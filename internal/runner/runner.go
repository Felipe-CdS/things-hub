// Package runner carries out the deterministic steps of an on-demand Mac
// run: pull (snapshot, ledger refresh, lease), apply (settle, write-ahead,
// Things writes, audit, ack) and undo.
package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/Felipe-CdS/things-hub/api"
	"github.com/Felipe-CdS/things-hub/internal/hub"
	"github.com/Felipe-CdS/things-hub/internal/things"
)

// Runner holds the clients a run uses.
type Runner struct {
	Hub    *hub.Client
	Things things.Client
	Out    io.Writer
	Now    func() time.Time
	// FindTimeout bounds how long a create waits for its marker to show up
	// (Things writes asynchronously); FindInterval is the poll interval.
	FindTimeout  time.Duration
	FindInterval time.Duration
	// SettleAge is how old a pending_create row of another run must be
	// before a marker that is still missing marks it gone.
	SettleAge time.Duration
}

// New returns a runner with the default timings.
func New(h *hub.Client, t things.Client, out io.Writer) *Runner {
	return &Runner{Hub: h, Things: t, Out: out, Now: time.Now,
		FindTimeout: 10 * time.Second, FindInterval: 500 * time.Millisecond, SettleAge: 2 * time.Minute}
}

func (r *Runner) printf(format string, args ...any) {
	if r.Out != nil {
		fmt.Fprintf(r.Out, format, args...)
	}
}

// NewRunID returns a run id from the time, such as 2026-10-13T14-02-11Z.
func NewRunID(t time.Time) string {
	return t.UTC().Format("2006-01-02T15-04-05Z")
}

func mustJSON(v any) json.RawMessage {
	if v == nil {
		return nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return raw
}

// ledgerState maps a fresh read of an item to its ledger state.
func ledgerState(st *things.State) string {
	switch {
	case st == nil:
		return api.ThingGone
	case st.Trashed:
		return api.ThingTrashed
	case st.Status == things.StatusCompleted:
		return api.ThingCompleted
	case st.Status == things.StatusCanceled:
		return api.ThingCanceled
	}
	return api.ThingOpen
}

// ledgerType maps a things.py type to a ledger type.
func ledgerType(t string) string {
	switch t {
	case things.TypeTodo:
		return api.ThingTodo
	case things.TypeProject:
		return api.ThingProject
	case things.TypeHeading:
		return api.ThingHeading
	case things.TypeArea:
		return api.ThingArea
	case things.TypeTag:
		return api.ThingTag
	}
	return t
}

// Settle resolves every pending_create ledger row by its marker: found rows
// get their uuid and become open; rows of other runs whose marker is still
// missing after SettleAge become gone, since their create never reached
// Things. Rows of currentRun that are not found stay pending, and apply
// creates them again with the same marker.
func (r *Runner) Settle(ctx context.Context, currentRun string) (found, gone int, err error) {
	led, err := r.Hub.Ledger(ctx, hub.LedgerQuery{State: api.ThingPendingCreate})
	if err != nil {
		return 0, 0, fmt.Errorf("read pending creates: %w", err)
	}
	var puts []api.Thing
	for _, t := range led.Things {
		if t.Marker == "" || t.Type == api.ThingArea || t.Type == api.ThingTag {
			continue
		}
		it, err := r.Things.FindByMarker(ctx, t.Marker)
		if err != nil {
			return found, gone, err
		}
		if it != nil {
			puts = append(puts, api.Thing{Marker: t.Marker, UUID: it.UUID, State: api.ThingOpen, Observed: mustJSON(it)})
			found++
			continue
		}
		if t.RunID != currentRun && r.Now().Sub(t.UpdatedAt) > r.SettleAge {
			puts = append(puts, api.Thing{Marker: t.Marker, State: api.ThingGone})
			gone++
		}
	}
	if len(puts) > 0 {
		if _, err := r.Hub.PutLedger(ctx, api.LedgerPutRequest{Items: puts}); err != nil {
			return found, gone, fmt.Errorf("settle ledger: %w", err)
		}
	}
	return found, gone, nil
}

// RunDir returns the folder of a run under root.
func RunDir(root, runID string) string { return filepath.Join(root, runID) }

func appendJSONL(path string, v any) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = f.Write(append(raw, '\n'))
	return err
}
