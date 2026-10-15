package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Felipe-CdS/things-hub/api"
	"github.com/Felipe-CdS/things-hub/internal/hub"
	"github.com/Felipe-CdS/things-hub/internal/plan"
	"github.com/Felipe-CdS/things-hub/internal/things"
)

// PullOptions configure a pull.
type PullOptions struct {
	// Dir is the run folder; input.json is written there.
	Dir            string
	RunID          string
	Max            int
	LeaseSeconds   int
	SnapshotDays   int
	Limits         plan.Limits
	LayoutApproved bool
}

// Pull takes the Things snapshot, stores it on the server, settles and
// refreshes the ledger, leases pending items and writes input.json.
func (r *Runner) Pull(ctx context.Context, o PullOptions) (*plan.Input, error) {
	if o.SnapshotDays == 0 {
		o.SnapshotDays = 30
	}
	if err := os.MkdirAll(o.Dir, 0o700); err != nil {
		return nil, err
	}
	snap, err := r.Things.Snapshot(ctx, o.SnapshotDays)
	if err != nil {
		return nil, fmt.Errorf("snapshot: %w", err)
	}
	body, err := json.Marshal(snap)
	if err != nil {
		return nil, err
	}
	if err := r.Hub.PutSnapshot(ctx, o.RunID, body); err != nil {
		return nil, fmt.Errorf("store snapshot: %w", err)
	}
	found, gone, err := r.Settle(ctx, o.RunID)
	if err != nil {
		return nil, err
	}
	if found+gone > 0 {
		r.printf("settled pending creates: %d found, %d gone\n", found, gone)
	}
	ledger, err := r.refreshLedger(ctx, snap)
	if err != nil {
		return nil, err
	}
	pulled, err := r.Hub.Pull(ctx, api.PullRequest{Max: o.Max, LeaseSeconds: o.LeaseSeconds})
	if err != nil {
		return nil, fmt.Errorf("pull: %w", err)
	}
	in := &plan.Input{
		Schema: plan.InputSchema, RunID: o.RunID, LeaseID: pulled.LeaseID, ExpiresAt: pulled.ExpiresAt,
		Items: pulled.Items, Ledger: ledger, Limits: o.Limits.WithDefaults(), Snapshot: snap,
		LayoutApproved: o.LayoutApproved,
	}
	if err := plan.WriteJSON(filepath.Join(o.Dir, "input.json"), in); err != nil {
		return nil, err
	}
	return in, nil
}

// refreshLedger records each ledger item's current state as Things shows
// it, and returns the refreshed ledger.
func (r *Runner) refreshLedger(ctx context.Context, snap *things.Snapshot) ([]api.Thing, error) {
	led, err := r.Hub.Ledger(ctx, hub.LedgerQuery{})
	if err != nil {
		return nil, fmt.Errorf("read ledger: %w", err)
	}
	var puts []api.Thing
	for i, t := range led.Things {
		if t.UUID == "" || t.State == api.ThingPendingCreate || t.Type == api.ThingTag {
			continue
		}
		var state string
		var observed json.RawMessage
		if it, ok := snap.Find(t.UUID); ok {
			state = ledgerState(&things.State{Status: it.Status, Trashed: it.Trashed})
			observed = mustJSON(it)
		} else {
			st, err := r.Things.Lookup(ctx, t.UUID)
			if err != nil {
				return nil, err
			}
			state = ledgerState(st)
		}
		if state != t.State {
			puts = append(puts, api.Thing{UUID: t.UUID, Marker: t.Marker, State: state, Observed: observed})
			led.Things[i].State = state
		}
	}
	if len(puts) > 0 {
		if _, err := r.Hub.PutLedger(ctx, api.LedgerPutRequest{Items: puts}); err != nil {
			return nil, fmt.Errorf("refresh ledger: %w", err)
		}
	}
	return led.Things, nil
}
