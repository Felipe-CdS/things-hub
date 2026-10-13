package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Felipe-CdS/things-hub/api"
	"github.com/Felipe-CdS/things-hub/internal/hub"
	"github.com/Felipe-CdS/things-hub/internal/plan"
	"github.com/Felipe-CdS/things-hub/internal/things"
)

// Applied-op results stored on the server.
const (
	resultStarted = "started"
	resultOK      = "ok"
	resultAlready = "already"
)

// Op statuses in the apply report.
const (
	StatusApplied  = "applied"
	StatusAlready  = "already"
	StatusReplayed = "replayed"
	StatusSkipped  = "skipped"
	StatusFailed   = "failed"
)

// ApplyOptions configure an apply.
type ApplyOptions struct {
	// Dir is the run folder holding input.json; the report is written there.
	Dir             string
	DryRun          bool
	AllowOverLimits bool
	ApproveLayout   bool
}

// OpReport is what happened to one op.
type OpReport struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Status   string `json:"status"`
	UUID     string `json:"uuid,omitempty"`
	Marker   string `json:"marker,omitempty"`
	Error    string `json:"error,omitempty"`
	Describe string `json:"describe"`
	// role links the item to the op's key on ack.
	role string
}

// Report is apply-result.json.
type Report struct {
	RunID     string          `json:"run_id"`
	DryRun    bool            `json:"dry_run"`
	Counts    plan.Counts     `json:"counts"`
	Ops       []OpReport      `json:"ops"`
	Acks      []api.AckResult `json:"acks,omitempty"`
	Released  int             `json:"released"`
	Warnings  []string        `json:"warnings,omitempty"`
	Settled   int             `json:"settled"`
	StartedAt time.Time       `json:"started_at"`
	EndedAt   time.Time       `json:"ended_at"`
}

// Totals counts op statuses.
func (rep *Report) Totals() map[string]int {
	out := map[string]int{}
	for _, o := range rep.Ops {
		out[o.Status]++
	}
	return out
}

// CheckError is a plan thub check refuses.
type CheckError struct{ Result *plan.Result }

func (e *CheckError) Error() string {
	var parts []string
	parts = append(parts, e.Result.Errors...)
	for _, o := range e.Result.OverLimits {
		parts = append(parts, "over the per-run limit: "+o)
	}
	return "plan refused:\n  " + strings.Join(parts, "\n  ")
}

// ErrLayoutPreview is returned for the first plan that reorganizes existing
// items until the owner approves it.
var ErrLayoutPreview = errors.New("this plan reorganizes existing items and the layout is not approved yet: " +
	"show the dry run to the owner, then apply with --approve-layout once they say yes")

// applyState is one apply's working state.
type applyState struct {
	r       *Runner
	runID   string
	dir     string
	applied map[string]string
	// rows are ledger rows by uuid and by op id.
	byUUID map[string]api.Thing
	byOp   map[string]api.Thing
	// made maps a create op id to the uuid it produced.
	made map[string]string
	// outcome per op id: done, skipped (conflict) or failed with an error.
	status map[string]OpReport
	// wrote holds the existing items this run already changed, whose
	// modified time no longer matches the snapshot.
	wrote map[string]bool
}

// Apply checks the plan and, unless DryRun, carries it out.
func (r *Runner) Apply(ctx context.Context, in *plan.Input, p *plan.Plan, o ApplyOptions) (*Report, error) {
	res := plan.Check(in, p)
	rep := &Report{RunID: p.RunID, DryRun: o.DryRun, Counts: res.Counts, Warnings: res.Warnings, StartedAt: r.Now()}
	if !res.OK(o.AllowOverLimits) {
		return rep, &CheckError{Result: res}
	}
	if o.DryRun {
		for _, op := range res.Ops {
			rep.Ops = append(rep.Ops, OpReport{ID: op.ID, Kind: op.Kind, Status: "planned", UUID: op.UUID, Describe: op.Describe})
		}
		rep.EndedAt = r.Now()
		return rep, nil
	}
	if res.ReorganizesLayout && !in.LayoutApproved && !o.ApproveLayout {
		return rep, ErrLayoutPreview
	}

	found, gone, err := r.Settle(ctx, p.RunID)
	if err != nil {
		return rep, err
	}
	rep.Settled = found + gone
	led, err := r.Hub.Ledger(ctx, hub.LedgerQuery{Run: p.RunID})
	if err != nil {
		return rep, fmt.Errorf("read ledger: %w", err)
	}
	s := &applyState{r: r, runID: p.RunID, dir: o.Dir, applied: map[string]string{},
		byUUID: map[string]api.Thing{}, byOp: map[string]api.Thing{}, made: map[string]string{},
		status: map[string]OpReport{}, wrote: map[string]bool{}}
	for _, a := range led.AppliedOps {
		s.applied[a.OpID] = a.Result
	}
	for _, t := range led.Things {
		if t.UUID != "" {
			s.byUUID[t.UUID] = t
		}
		if t.RunID == p.RunID && t.OpID != "" {
			s.byOp[t.OpID] = t
			if t.UUID != "" {
				s.made[t.OpID] = t.UUID
			}
		}
	}

	for _, op := range res.Ops {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		or := s.run(ctx, op)
		s.status[op.ID] = or
		if op.Existing && (or.Status == StatusApplied || or.Status == StatusReplayed) {
			s.wrote[op.UUID] = true
		}
		rep.Ops = append(rep.Ops, or)
		r.printf("%-9s %s\n", or.Status, op.Describe)
		if or.Error != "" {
			r.printf("          %s\n", or.Error)
		}
	}

	if in.LeaseID != "" {
		acks := s.acks(res.Answers)
		if len(acks) > 0 {
			ar, err := r.Hub.Ack(ctx, api.AckRequest{LeaseID: in.LeaseID, Acks: acks})
			if err != nil {
				return rep, fmt.Errorf("ack: %w", err)
			}
			rep.Acks = ar.Results
		}
		if len(res.Unplanned) > 0 {
			rr, err := r.Hub.Release(ctx, api.ReleaseRequest{LeaseID: in.LeaseID, Keys: res.Unplanned})
			if err != nil {
				return rep, fmt.Errorf("release: %w", err)
			}
			rep.Released = rr.Released
		}
	}
	rep.EndedAt = r.Now()
	if o.Dir != "" {
		if err := plan.WriteJSON(filepath.Join(o.Dir, "apply-result.json"), rep); err != nil {
			return rep, err
		}
	}
	return rep, nil
}

// acks turns answers and op outcomes into acks. An answer whose op failed
// is failed; one whose op was skipped for a conflict is deferred.
func (s *applyState) acks(answers []plan.Answer) []api.Ack {
	var out []api.Ack
	for _, a := range answers {
		ack := api.Ack{Key: a.Key, Revision: a.Revision, Outcome: a.Outcome, Note: a.Note, NotBefore: a.NotBefore}
		for _, id := range a.Ops {
			or := s.status[id]
			switch or.Status {
			case StatusFailed:
				ack.Outcome, ack.Note, ack.NotBefore = api.OutcomeFailed, or.Error, ""
			case StatusSkipped:
				if ack.Outcome != api.OutcomeFailed {
					ack.Outcome, ack.Note, ack.NotBefore = api.OutcomeDeferred, or.Error, ""
				}
			}
		}
		if ack.Outcome != api.OutcomeFailed && ack.Outcome != api.OutcomeDeferred {
			for _, id := range a.Ops {
				or := s.status[id]
				role := or.role
				switch {
				case or.Marker != "":
					ack.Items = append(ack.Items, api.ThingRef{Marker: or.Marker, Role: role})
				case or.UUID != "":
					ack.Items = append(ack.Items, api.ThingRef{UUID: or.UUID, Role: role})
				}
			}
		}
		if len(ack.Note) > 500 {
			ack.Note = ack.Note[:500]
		}
		out = append(out, ack)
	}
	return out
}

func (s *applyState) report(op plan.Op, status string) OpReport {
	return OpReport{ID: op.ID, Kind: op.Kind, Status: status, UUID: op.UUID, Describe: op.Describe, role: op.Role}
}

func (s *applyState) fail(op plan.Op, err error) OpReport {
	or := s.report(op, StatusFailed)
	or.Error = err.Error()
	return or
}

// target resolves an op's list to a uuid.
func (s *applyState) target(t plan.Target) (string, error) {
	if t.UUID != "" || t.Op == "" {
		return t.UUID, nil
	}
	if u := s.made[t.Op]; u != "" {
		return u, nil
	}
	return "", fmt.Errorf("its list is created by %s, which did not complete", t.Op)
}

// record stores the audit entry, on the server and in the run folder, then
// the ledger rows and the applied op atomically. The server keeps the first
// audit entry per op, so a replay after a failed ledger write never doubles it.
func (s *applyState) record(ctx context.Context, op plan.Op, rows []api.Thing, result string, detail any, audit *api.AuditEntry) error {
	if audit != nil {
		audit.RunID, audit.OpID, audit.At = s.runID, op.ID, s.r.Now().UTC()
		if audit.Key == "" {
			audit.Key = op.Key
		}
		if _, err := s.r.Hub.PostAudit(ctx, []api.AuditEntry{*audit}); err != nil {
			return fmt.Errorf("audit %s: %w", op.ID, err)
		}
		if s.dir != "" {
			if err := appendJSONL(filepath.Join(s.dir, "audit.jsonl"), audit); err != nil {
				return err
			}
		}
	}
	req := api.LedgerPutRequest{Items: rows,
		AppliedOps: []api.AppliedOp{{OpID: op.ID, RunID: s.runID, Result: result, Detail: mustJSON(detail)}}}
	if _, err := s.r.Hub.PutLedger(ctx, req); err != nil {
		return fmt.Errorf("record %s: %w", op.ID, err)
	}
	s.applied[op.ID] = result
	for _, t := range rows {
		if t.UUID != "" {
			s.byUUID[t.UUID] = t
		}
	}
	return nil
}

// adopt returns the ledger row for an existing item: unchanged identity
// for a known row, origin adopted for a new one.
func (s *applyState) adopt(uuid, typ, state string) api.Thing {
	if t, ok := s.byUUID[uuid]; ok {
		return api.Thing{UUID: uuid, Marker: t.Marker, State: state}
	}
	return api.Thing{UUID: uuid, Type: ledgerType(typ), Origin: api.OriginAdopted, State: state}
}

func (s *applyState) run(ctx context.Context, op plan.Op) OpReport {
	switch s.applied[op.ID] {
	case resultOK, resultAlready:
		or := s.report(op, StatusReplayed)
		if t, ok := s.byOp[op.ID]; ok {
			or.UUID, or.Marker = t.UUID, t.Marker
		}
		return or
	}
	var or OpReport
	var err error
	switch op.Kind {
	case plan.KindCreateTodo, plan.KindCreateProject:
		or, err = s.create(ctx, op)
	case plan.KindCreateArea:
		or, err = s.createArea(ctx, op)
	case plan.KindCreateTag:
		or, err = s.createTag(ctx, op)
	default:
		or, err = s.change(ctx, op)
	}
	if err != nil {
		if ctx.Err() != nil {
			return s.fail(op, ctx.Err())
		}
		return s.fail(op, err)
	}
	return or
}

// poll calls try until it reports done or FindTimeout passes, since Things
// writes asynchronously.
func (s *applyState) poll(ctx context.Context, try func() (bool, error)) (bool, error) {
	deadline := s.r.Now().Add(s.r.FindTimeout)
	for {
		done, err := try()
		if err != nil || done {
			return done, err
		}
		if !s.r.Now().Before(deadline) {
			return false, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(s.r.FindInterval):
		}
	}
}

// findMarker polls for a marker until FindTimeout.
func (s *applyState) findMarker(ctx context.Context, marker string) (*things.Item, error) {
	var it *things.Item
	_, err := s.poll(ctx, func() (bool, error) {
		var err error
		it, err = s.r.Things.FindByMarker(ctx, marker)
		return it != nil, err
	})
	return it, err
}

// create writes a to-do or project with a marker: write-ahead to the
// ledger, write to Things, find it by marker, record its uuid.
func (s *applyState) create(ctx context.Context, op plan.Op) (OpReport, error) {
	or := s.report(op, StatusApplied)
	list, err := s.target(op.List)
	if err != nil {
		return or, err
	}
	row, retry := s.byOp[op.ID]
	marker := row.Marker
	if retry && row.UUID != "" {
		// Settled since the crash: only the applied op and its audit are missing.
		or.UUID, or.Marker, or.Status = row.UUID, marker, StatusAlready
		s.made[op.ID] = row.UUID
		audit := &api.AuditEntry{Action: "create", ThingUUID: row.UUID, Marker: marker, ThingType: ledgerType(op.Type), After: row.Observed}
		return or, s.record(ctx, op, nil, resultOK, map[string]string{"uuid": row.UUID, "marker": marker}, audit)
	}
	if !retry {
		marker = things.NewMarker()
		ahead := api.Thing{Marker: marker, Type: ledgerType(op.Type), Origin: api.OriginHub,
			State: api.ThingPendingCreate, OpID: op.ID, RunID: s.runID, Written: mustJSON(op)}
		if op.Key != "" {
			ahead.Links = []api.Link{{Key: op.Key, Role: op.Role}}
		}
		if _, err := s.r.Hub.PutLedger(ctx, api.LedgerPutRequest{Items: []api.Thing{ahead}}); err != nil {
			return or, fmt.Errorf("write-ahead: %w", err)
		}
		s.byOp[op.ID] = ahead
	} else if it, err := s.findMarker(ctx, marker); err != nil {
		return or, err
	} else if it != nil {
		return s.created(ctx, op, marker, it, StatusAlready)
	}
	or.Marker = marker
	notes := things.BuildNotes(op.Summary, op.Source, marker)
	if op.Kind == plan.KindCreateTodo {
		t := *op.Todo
		t.Notes, t.ListID = notes, list
		err = s.r.Things.AddTodo(ctx, t)
	} else {
		p := *op.Project
		p.Notes, p.AreaID = notes, list
		err = s.r.Things.AddProject(ctx, p)
	}
	if err != nil {
		return or, err
	}
	it, err := s.findMarker(ctx, marker)
	if err != nil {
		return or, err
	}
	if it == nil {
		return or, fmt.Errorf("marker %s did not show up within %s; the next run settles it", marker, s.r.FindTimeout)
	}
	return s.created(ctx, op, marker, it, StatusApplied)
}

func (s *applyState) created(ctx context.Context, op plan.Op, marker string, it *things.Item, status string) (OpReport, error) {
	or := s.report(op, status)
	or.UUID, or.Marker = it.UUID, marker
	s.made[op.ID] = it.UUID
	row := api.Thing{Marker: marker, UUID: it.UUID, State: api.ThingOpen, Observed: mustJSON(it)}
	s.byOp[op.ID] = row
	audit := &api.AuditEntry{Action: "create", ThingUUID: it.UUID, Marker: marker, ThingType: ledgerType(op.Type), After: mustJSON(it)}
	return or, s.record(ctx, op, []api.Thing{row}, resultOK, map[string]string{"uuid": it.UUID, "marker": marker}, audit)
}

func (s *applyState) createArea(ctx context.Context, op plan.Op) (OpReport, error) {
	or := s.report(op, StatusApplied)
	row, retry := s.byOp[op.ID]
	if !retry {
		row = api.Thing{Marker: things.NewMarker(), Type: api.ThingArea, Origin: api.OriginHub,
			State: api.ThingPendingCreate, OpID: op.ID, RunID: s.runID, Written: mustJSON(op)}
		if _, err := s.r.Hub.PutLedger(ctx, api.LedgerPutRequest{Items: []api.Thing{row}}); err != nil {
			return or, fmt.Errorf("write-ahead: %w", err)
		}
		s.byOp[op.ID] = row
	}
	var uuid string
	if retry {
		// The area may exist from before the crash: find it by title.
		areas, err := s.r.Things.Areas(ctx)
		if err != nil {
			return or, err
		}
		for _, a := range areas {
			if a.Title == op.Title {
				uuid, or.Status = a.UUID, StatusAlready
			}
		}
	}
	if uuid == "" {
		var err error
		if uuid, err = s.r.Things.AddArea(ctx, op.Title); err != nil {
			return or, err
		}
	}
	or.UUID, or.Marker = uuid, row.Marker
	s.made[op.ID] = uuid
	done := api.Thing{Marker: row.Marker, UUID: uuid, State: api.ThingOpen}
	audit := &api.AuditEntry{Action: "create_area", ThingUUID: uuid, Marker: row.Marker, ThingType: api.ThingArea,
		After: mustJSON(things.Item{UUID: uuid, Type: things.TypeArea, Title: op.Title})}
	return or, s.record(ctx, op, []api.Thing{done}, resultOK, map[string]string{"uuid": uuid}, audit)
}

func (s *applyState) createTag(ctx context.Context, op plan.Op) (OpReport, error) {
	or := s.report(op, StatusApplied)
	tags, err := s.r.Things.Tags(ctx)
	if err != nil {
		return or, err
	}
	result := resultOK
	if slices.ContainsFunc(tags, func(t things.Item) bool { return strings.EqualFold(t.Title, op.Title) }) {
		or.Status, result = StatusAlready, resultAlready
	} else if err := s.r.Things.AddTag(ctx, op.Title); err != nil {
		return or, err
	}
	audit := &api.AuditEntry{Action: "create_tag", ThingType: api.ThingTag,
		After: mustJSON(things.Item{Type: things.TypeTag, Title: op.Title})}
	if result == resultAlready {
		audit = nil
	}
	return or, s.record(ctx, op, nil, result, map[string]string{"title": op.Title}, audit)
}

// change writes to an item that existed before the run. It re-reads the
// item first: one the owner changed since the snapshot is skipped and left
// for the next run, unless this op already started before a crash.
func (s *applyState) change(ctx context.Context, op plan.Op) (OpReport, error) {
	or := s.report(op, StatusApplied)
	st, err := s.r.Things.Lookup(ctx, op.UUID)
	if err != nil {
		return or, err
	}
	resumed := s.applied[op.ID] == resultStarted
	if st == nil && resumed && op.Kind == plan.KindDeleteArea {
		return or, s.landed(ctx, op, api.ThingGone)
	}
	if st == nil {
		return or, fmt.Errorf("%s %s no longer exists", op.Type, op.UUID)
	}
	if op.Kind != plan.KindDelete && op.Kind != plan.KindDeleteArea && st.Trashed {
		return or, fmt.Errorf("%s %s is in the Trash", op.Type, op.UUID)
	}
	if !resumed && !s.wrote[op.UUID] && op.Before != nil && op.Before.Modified != "" && st.Modified != op.Before.Modified {
		or.Status, or.Error = StatusSkipped, fmt.Sprintf("changed in Things since the snapshot (%s → %s); left for the next run", op.Before.Modified, st.Modified)
		return or, nil
	}

	var already bool
	state := api.ThingOpen
	switch op.Kind {
	case plan.KindComplete:
		already, state = st.Status == things.StatusCompleted, api.ThingCompleted
	case plan.KindCancel:
		already, state = st.Status == things.StatusCanceled, api.ThingCanceled
	case plan.KindDelete:
		already, state = st.Trashed, api.ThingTrashed
	case plan.KindDeleteArea:
		n := st.Contents
		if _, err := s.poll(ctx, func() (bool, error) {
			a, err := s.r.Things.Lookup(ctx, op.UUID)
			if a != nil {
				n = a.Contents
			}
			return n == 0, err
		}); err != nil {
			return or, err
		}
		if n > 0 {
			return or, fmt.Errorf("area %s is not empty in Things (%d project(s) or to-do(s)); left in place", op.UUID, n)
		}
		state = api.ThingGone
	}
	if already && resumed {
		return or, s.landed(ctx, op, state)
	}
	if already {
		or.Status = StatusAlready
		return or, s.record(ctx, op, []api.Thing{s.adopt(op.UUID, op.Type, state)}, resultAlready, nil, nil)
	}
	if !resumed {
		if err := s.record(ctx, op, nil, resultStarted, nil, nil); err != nil {
			return or, err
		}
	}

	var after any
	switch op.Kind {
	case plan.KindUpdate:
		if op.Change == nil {
			// An attach that only links the key.
			break
		}
		ch := *op.Change
		if !op.List.IsZero() {
			list, err := s.target(op.List)
			if err != nil {
				return or, err
			}
			ch.ListID = &list
		}
		ch.AppendChecklist = missingTail(st.Checklist, ch.AppendChecklist)
		if !ch.Empty() {
			if op.Type == things.TypeProject {
				err = s.r.Things.UpdateProject(ctx, op.UUID, ch)
			} else {
				err = s.r.Things.UpdateTodo(ctx, op.UUID, ch)
			}
		}
		after = ch
	case plan.KindComplete, plan.KindCancel:
		t := true
		ch := things.Change{Completed: &t}
		if op.Kind == plan.KindCancel {
			ch = things.Change{Canceled: &t}
		}
		if op.Type == things.TypeProject {
			err = s.r.Things.UpdateProject(ctx, op.UUID, ch)
		} else {
			err = s.r.Things.UpdateTodo(ctx, op.UUID, ch)
		}
		if err == nil {
			err = s.waitStatus(ctx, op, state)
		}
		after = map[string]string{"status": state}
	case plan.KindDelete, plan.KindDeleteArea:
		err = s.r.Things.Delete(ctx, op.UUID, op.Type)
		after = map[string]string{"state": state}
	case plan.KindRenameArea:
		err = s.r.Things.RenameArea(ctx, op.UUID, op.Title)
		after = things.Item{UUID: op.UUID, Type: things.TypeArea, Title: op.Title}
	case plan.KindMoveProject:
		area, terr := s.target(op.List)
		if terr != nil {
			return or, terr
		}
		err = s.r.Things.MoveProject(ctx, op.UUID, area)
		after = map[string]string{"area": area}
	default:
		return or, fmt.Errorf("unknown op kind %q", op.Kind)
	}
	if err != nil {
		return or, err
	}
	return or, s.recordChange(ctx, op, state, after)
}

// landed records a resumed op whose write reached Things before the crash
// as a fresh write would, so undo reverses it.
func (s *applyState) landed(ctx context.Context, op plan.Op, state string) error {
	after := map[string]string{"state": state}
	if op.Kind == plan.KindComplete || op.Kind == plan.KindCancel {
		after = map[string]string{"status": state}
	}
	return s.recordChange(ctx, op, state, after)
}

func (s *applyState) recordChange(ctx context.Context, op plan.Op, state string, after any) error {
	audit := &api.AuditEntry{Action: op.Kind, ThingUUID: op.UUID, ThingType: ledgerType(op.Type),
		Before: mustJSON(op.Before), After: mustJSON(after)}
	return s.record(ctx, op, []api.Thing{s.adopt(op.UUID, op.Type, state)}, resultOK, nil, audit)
}

// waitStatus re-reads a completed or canceled item until Things shows it
// so: Things ignores the write for a project that still has open to-dos.
func (s *applyState) waitStatus(ctx context.Context, op plan.Op, state string) error {
	want := things.StatusCompleted
	if state == api.ThingCanceled {
		want = things.StatusCanceled
	}
	var got string
	ok, err := s.poll(ctx, func() (bool, error) {
		st, err := s.r.Things.Lookup(ctx, op.UUID)
		if st != nil {
			got = st.Status
		}
		return got == want, err
	})
	if err != nil || ok {
		return err
	}
	return fmt.Errorf("%s %s is still %s in Things after the write (open to-dos added since the snapshot?)", op.Type, op.UUID, got)
}

// missingTail drops the items an earlier, interrupted apply already
// appended, so a replay never doubles a checklist.
func missingTail(have, add []string) []string {
	if len(add) == 0 {
		return nil
	}
	for n := len(add); n > 0; n-- {
		if len(have) >= n && slices.Equal(have[len(have)-n:], add[:n]) {
			return add[n:]
		}
	}
	return add
}

// DecodeChange reads the change an update audit entry recorded.
func DecodeChange(raw json.RawMessage) (things.Change, error) {
	var ch things.Change
	if len(raw) == 0 {
		return ch, nil
	}
	err := json.Unmarshal(raw, &ch)
	return ch, err
}
