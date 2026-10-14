package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/Felipe-CdS/things-hub/api"
	"github.com/Felipe-CdS/things-hub/internal/hub"
	"github.com/Felipe-CdS/things-hub/internal/plan"
	"github.com/Felipe-CdS/things-hub/internal/things"
)

// UndoRunID is the run id an undo of runID records its changes under.
// Re-running the same undo replays only what is left.
func UndoRunID(runID string) string { return "undo-" + runID }

// UndoStep is one inverse write.
type UndoStep struct {
	OpID     string `json:"op_id"`
	Of       string `json:"of"`
	Action   string `json:"action"`
	UUID     string `json:"uuid,omitempty"`
	Status   string `json:"status"`
	Describe string `json:"describe"`
	Error    string `json:"error,omitempty"`
}

// Undo replays a run's audit log newest-first and writes each change's
// inverse: before state back for updates and moves, reopen for completions
// and cancels, cancel for creates, out of the Trash for deletes.
func (r *Runner) Undo(ctx context.Context, runID string) ([]UndoStep, error) {
	log, err := r.Hub.Audit(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("read audit of %s: %w", runID, err)
	}
	if len(log.Entries) == 0 {
		return nil, fmt.Errorf("run %s has no audit entries", runID)
	}
	undoRun := UndoRunID(runID)
	led, err := r.Hub.Ledger(ctx, hub.LedgerQuery{Run: undoRun})
	if err != nil {
		return nil, err
	}
	applied := map[string]bool{}
	// Write-ahead rows of steps a killed undo started: the areas that had
	// the recreated area's title before its AddArea, or the marker of an
	// item recreated from the Trash.
	ahead := map[string]json.RawMessage{}
	for _, a := range led.AppliedOps {
		applied[a.OpID] = a.Result == resultOK || a.Result == resultAlready
		if a.Result == resultStarted {
			ahead[a.OpID] = a.Detail
		}
	}
	// Areas this undo recreated under a new uuid, so earlier moves out of
	// them go back into the new area. A resumed undo reads them back from
	// its own audit, which is written before a step is marked done.
	areas := map[string]string{}
	audited := map[string]bool{}
	prev, err := r.Hub.Audit(ctx, undoRun)
	if err != nil {
		return nil, fmt.Errorf("read audit of %s: %w", undoRun, err)
	}
	for _, pe := range prev.Entries {
		audited[pe.OpID] = true
		var out struct{ UUID string }
		if pe.Action == "undo_"+plan.KindDeleteArea && json.Unmarshal(pe.After, &out) == nil && out.UUID != "" {
			areas[pe.ThingUUID] = out.UUID
		}
	}
	entries := slices.Clone(log.Entries)
	slices.SortFunc(entries, func(a, b api.AuditEntry) int { return int(b.Seq - a.Seq) })

	var steps []UndoStep
	for _, e := range entries {
		step := UndoStep{OpID: undoRun + "/" + strings.TrimPrefix(e.OpID, runID+"/"), Of: e.OpID,
			Action: e.Action, UUID: e.ThingUUID}
		if applied[step.OpID] || audited[step.OpID] {
			// An audited step whose ledger write failed is done in Things;
			// only its applied op is left to record.
			if !applied[step.OpID] {
				req := api.LedgerPutRequest{AppliedOps: []api.AppliedOp{{OpID: step.OpID, RunID: undoRun, Result: resultOK}}}
				if _, err := r.Hub.PutLedger(ctx, req); err != nil {
					return steps, fmt.Errorf("record %s: %w", step.OpID, err)
				}
			}
			step.Status, step.Describe = StatusReplayed, "already undone"
			steps = append(steps, step)
			continue
		}
		after, state, err := r.inverse(ctx, e, &step, areas, ahead)
		if err != nil {
			step.Status, step.Error = StatusFailed, err.Error()
			r.printf("%-9s %s\n          %s\n", step.Status, step.Describe, step.Error)
			steps = append(steps, step)
			continue
		}
		if step.Status == StatusSkipped {
			r.printf("%-9s %s\n          %s\n", step.Status, step.Describe, step.Error)
			steps = append(steps, step)
			continue
		}
		if step.Status == "" {
			step.Status = StatusApplied
		}
		var rows []api.Thing
		if e.ThingUUID != "" && state != "" {
			rows = []api.Thing{{UUID: e.ThingUUID, Marker: e.Marker, State: state}}
		}
		// The audit goes first: it carries a recreated area's new uuid,
		// which must be durable before the step counts as undone.
		entry := api.AuditEntry{RunID: undoRun, OpID: step.OpID, Action: "undo_" + e.Action, Key: e.Key,
			ThingUUID: e.ThingUUID, Marker: e.Marker, ThingType: e.ThingType, Before: e.After,
			After: mustJSON(after), At: r.Now().UTC()}
		if _, err := r.Hub.PostAudit(ctx, []api.AuditEntry{entry}); err != nil {
			return steps, fmt.Errorf("audit %s: %w", step.OpID, err)
		}
		req := api.LedgerPutRequest{Items: rows, AppliedOps: []api.AppliedOp{{OpID: step.OpID, RunID: undoRun, Result: resultOK}}}
		if _, err := r.Hub.PutLedger(ctx, req); err != nil {
			return steps, fmt.Errorf("record %s: %w", step.OpID, err)
		}
		r.printf("%-9s %s\n", step.Status, step.Describe)
		steps = append(steps, step)
	}
	return steps, nil
}

// whenOf maps a snapshot item's schedule back to a when value.
func whenOf(it things.Item) string {
	if it.StartDate != "" {
		return it.StartDate
	}
	if it.Start == "Someday" {
		return "someday"
	}
	return "anytime"
}

// restoreChange builds the change that puts back what ch changed.
func restoreChange(ch things.Change, before things.Item) things.Change {
	var out things.Change
	str := func(s string) *string { return &s }
	if ch.Title != nil {
		out.Title = str(before.Title)
	}
	if ch.Notes != nil {
		out.Notes = str(before.Notes)
	}
	if ch.When != nil {
		out.When = str(whenOf(before))
	}
	if ch.Deadline != nil {
		out.Deadline = str(before.Deadline)
	}
	if ch.Tags != nil {
		tags := slices.Clone(before.Tags)
		if tags == nil {
			tags = []string{}
		}
		out.Tags = &tags
	}
	if ch.ListID != nil && before.ListID() != "" {
		out.ListID = str(before.ListID())
	}
	if ch.HeadingID != nil && before.Heading != "" {
		out.HeadingID = str(before.Heading)
	}
	if ch.Checklist != nil || len(ch.AppendChecklist) > 0 {
		list := before.ChecklistTitles()
		out.Checklist = &list
	}
	return out
}

// inverse writes the inverse of one audit entry and returns what it wrote
// and the item's new ledger state.
func (r *Runner) inverse(ctx context.Context, e api.AuditEntry, step *UndoStep, areas map[string]string,
	ahead map[string]json.RawMessage) (any, string, error) {
	var before things.Item
	if len(e.Before) > 0 && string(e.Before) != "null" {
		if err := json.Unmarshal(e.Before, &before); err != nil {
			return nil, "", fmt.Errorf("decode before: %w", err)
		}
	}
	if a, ok := areas[before.Area]; ok && e.Action != plan.KindDeleteArea {
		before.Area = a
	}
	typ := before.Type
	if typ == "" {
		var after things.Item
		_ = json.Unmarshal(e.After, &after)
		typ = after.Type
	}
	t := true
	f := false
	update := func(ch things.Change) error {
		if typ == things.TypeProject {
			return r.Things.UpdateProject(ctx, e.ThingUUID, ch)
		}
		return r.Things.UpdateTodo(ctx, e.ThingUUID, ch)
	}

	switch e.Action {
	case "create":
		step.Describe = fmt.Sprintf("⊘ cancel created %s %s", typ, e.ThingUUID)
		st, err := r.Things.Lookup(ctx, e.ThingUUID)
		if err != nil {
			return nil, "", err
		}
		if st == nil || st.Trashed || st.Status != things.StatusIncomplete {
			step.Status = StatusAlready
			return nil, ledgerState(st), nil
		}
		ch := things.Change{Canceled: &t}
		return ch, api.ThingCanceled, update(ch)
	case plan.KindUpdate:
		ch, err := DecodeChange(e.After)
		if err != nil {
			return nil, "", err
		}
		back := restoreChange(ch, before)
		step.Describe = fmt.Sprintf("~ restore %s %q", typ, before.Title)
		if back.Empty() {
			step.Status = StatusAlready
			return nil, "", nil
		}
		return back, api.ThingOpen, update(back)
	case plan.KindComplete:
		step.Describe = fmt.Sprintf("↺ reopen %s %q", typ, before.Title)
		ch := things.Change{Completed: &f}
		return ch, api.ThingOpen, update(ch)
	case plan.KindCancel:
		step.Describe = fmt.Sprintf("↺ reopen %s %q", typ, before.Title)
		ch := things.Change{Canceled: &f}
		return ch, api.ThingOpen, update(ch)
	case plan.KindDelete:
		step.Describe = fmt.Sprintf("↺ restore %s %q from the Trash", typ, before.Title)
		return r.undelete(ctx, UndoRunID(e.RunID), step.OpID, e.ThingUUID, before, ahead)
	case plan.KindMoveProject:
		step.Describe = fmt.Sprintf("> move project %q back", before.Title)
		if before.Area == "" {
			return nil, "", fmt.Errorf("project %q had no area; move it by hand", before.Title)
		}
		return map[string]string{"area": before.Area}, api.ThingOpen, r.Things.MoveProject(ctx, e.ThingUUID, before.Area)
	case plan.KindRenameArea:
		step.Describe = fmt.Sprintf("~ rename area back to %q", before.Title)
		return before, api.ThingOpen, r.Things.RenameArea(ctx, e.ThingUUID, before.Title)
	case "create_area":
		step.Describe = fmt.Sprintf("✗ delete created area %s", e.ThingUUID)
		return r.deleteCreatedArea(ctx, e.ThingUUID)
	case "create_tag":
		var tag things.Item
		_ = json.Unmarshal(e.After, &tag)
		step.Describe = fmt.Sprintf("✗ delete created tag %q", tag.Title)
		return r.deleteCreatedTag(ctx, tag.Title, step)
	case plan.KindDeleteArea:
		step.Describe = fmt.Sprintf("+ recreate area %q (new uuid; its contents were moved before)", before.Title)
		uuid, err := r.recreateArea(ctx, UndoRunID(e.RunID), step.OpID, before.Title, ahead)
		if err == nil && uuid != "" {
			areas[e.ThingUUID] = uuid
		}
		return map[string]string{"uuid": uuid}, "", err
	}
	return nil, "", fmt.Errorf("no inverse for action %q", e.Action)
}

// recreateArea adds an area titled title, write-ahead like apply: it first
// records the areas that already have that title, so a resumed undo adopts
// the area its killed run created instead of adding a second one.
func (r *Runner) recreateArea(ctx context.Context, runID, opID, title string, ahead map[string]json.RawMessage) (string, error) {
	list, err := r.Things.Areas(ctx)
	if err != nil {
		return "", err
	}
	known := []string{}
	for _, a := range list {
		if a.Title == title {
			known = append(known, a.UUID)
		}
	}
	if raw, ok := ahead[opID]; ok {
		var had []string
		if err := json.Unmarshal(raw, &had); err != nil {
			return "", fmt.Errorf("decode write-ahead of %s: %w", opID, err)
		}
		for _, u := range known {
			if !slices.Contains(had, u) {
				return u, nil
			}
		}
		known = had
	}
	req := api.LedgerPutRequest{AppliedOps: []api.AppliedOp{{OpID: opID, RunID: runID, Result: resultStarted,
		Detail: mustJSON(known)}}}
	if _, err := r.Hub.PutLedger(ctx, req); err != nil {
		return "", fmt.Errorf("record %s: %w", opID, err)
	}
	return r.Things.AddArea(ctx, title)
}

// undelete moves an item out of the Trash, and recreates it from its before
// state when the move did not take. The recreate is write-ahead like apply:
// its marker is recorded first, so a resumed undo finds the copy its killed
// run created instead of adding a second one.
func (r *Runner) undelete(ctx context.Context, runID, opID, uuid string, before things.Item,
	ahead map[string]json.RawMessage) (any, string, error) {
	st, err := r.Things.Lookup(ctx, uuid)
	if err != nil {
		return nil, "", err
	}
	if st != nil && !st.Trashed {
		return nil, ledgerState(st), nil
	}
	var marker string
	if raw, ok := ahead[opID]; ok {
		if err := json.Unmarshal(raw, &marker); err != nil {
			return nil, "", fmt.Errorf("decode write-ahead of %s: %w", opID, err)
		}
		it, err := r.findMarker(ctx, marker)
		if err != nil {
			return nil, "", err
		}
		if it != nil {
			return map[string]string{"recreated_from": uuid, "uuid": it.UUID, "marker": marker}, api.ThingGone, nil
		}
	} else if st != nil {
		if err := r.Things.Restore(ctx, uuid, before.Type, before.ListID()); err == nil {
			restored, err := r.poll(ctx, func() (bool, error) {
				st, err := r.Things.Lookup(ctx, uuid)
				return st != nil && !st.Trashed, err
			})
			if err != nil {
				return nil, "", err
			}
			if restored {
				return map[string]string{"restored": uuid}, api.ThingOpen, nil
			}
		}
	}
	// Fallback: recreate from the snapshot taken before the delete.
	if marker == "" {
		marker = things.NewMarker()
		req := api.LedgerPutRequest{AppliedOps: []api.AppliedOp{{OpID: opID, RunID: runID, Result: resultStarted,
			Detail: mustJSON(marker)}}}
		if _, err := r.Hub.PutLedger(ctx, req); err != nil {
			return nil, "", fmt.Errorf("record %s: %w", opID, err)
		}
	}
	notes := things.BuildNotes(before.Notes, nil, marker)
	if old := things.MarkerIn(before.Notes); old != "" {
		notes = strings.ReplaceAll(before.Notes, old, marker)
	}
	if before.Type == things.TypeProject {
		err = r.Things.AddProject(ctx, things.NewProject{Title: before.Title, Notes: notes,
			Deadline: before.Deadline, Tags: before.Tags, AreaID: before.Area})
	} else {
		err = r.Things.AddTodo(ctx, things.NewTodo{Title: before.Title, Notes: notes, When: whenOf(before),
			Deadline: before.Deadline, Tags: before.Tags, Checklist: before.ChecklistTitles(), ListID: before.ListID(),
			HeadingID: before.Heading})
	}
	if err != nil {
		return nil, "", err
	}
	it, err := r.findMarker(ctx, marker)
	if err != nil {
		return nil, "", err
	}
	if it == nil {
		return nil, "", fmt.Errorf("recreated %s %q (marker %s) did not show up within %s; re-run the undo",
			before.Type, before.Title, marker, r.FindTimeout)
	}
	return map[string]string{"recreated_from": uuid, "uuid": it.UUID, "marker": marker}, api.ThingGone, nil
}

func (r *Runner) deleteCreatedArea(ctx context.Context, uuid string) (any, string, error) {
	snap, err := r.Things.Snapshot(ctx, 30)
	if err != nil {
		return nil, "", err
	}
	for _, list := range [][]things.Item{snap.Todos, snap.Projects} {
		for _, it := range list {
			if it.Area == uuid {
				return nil, "", fmt.Errorf("the area is not empty (%s %q); move its contents first", it.Type, it.Title)
			}
		}
	}
	return map[string]string{"state": api.ThingGone}, api.ThingGone, r.Things.Delete(ctx, uuid, things.TypeArea)
}

func (r *Runner) deleteCreatedTag(ctx context.Context, title string, step *UndoStep) (any, string, error) {
	snap, err := r.Things.Snapshot(ctx, 30)
	if err != nil {
		return nil, "", err
	}
	for _, list := range [][]things.Item{snap.Todos, snap.Projects, snap.Areas} {
		for _, it := range list {
			if slices.Contains(it.Tags, title) {
				step.Status = StatusSkipped
				step.Error = fmt.Sprintf("tag %q is still used by %q; left in place", title, it.Title)
				return nil, "", nil
			}
		}
	}
	return map[string]string{"deleted_tag": title}, "", r.Things.DeleteTag(ctx, title)
}
