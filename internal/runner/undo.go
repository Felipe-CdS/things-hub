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
	for _, a := range led.AppliedOps {
		applied[a.OpID] = a.Result == resultOK || a.Result == resultAlready
	}
	// Areas this undo recreated under a new uuid, so earlier moves out of
	// them go back into the new area. A resumed undo reads them back from
	// its own audit.
	areas := map[string]string{}
	prev, err := r.Hub.Audit(ctx, undoRun)
	if err != nil {
		return nil, fmt.Errorf("read audit of %s: %w", undoRun, err)
	}
	for _, pe := range prev.Entries {
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
		if applied[step.OpID] {
			step.Status, step.Describe = StatusReplayed, "already undone"
			steps = append(steps, step)
			continue
		}
		after, state, err := r.inverse(ctx, e, &step, areas)
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
		req := api.LedgerPutRequest{Items: rows, AppliedOps: []api.AppliedOp{{OpID: step.OpID, RunID: undoRun, Result: resultOK}}}
		if _, err := r.Hub.PutLedger(ctx, req); err != nil {
			return steps, fmt.Errorf("record %s: %w", step.OpID, err)
		}
		entry := api.AuditEntry{RunID: undoRun, OpID: step.OpID, Action: "undo_" + e.Action, Key: e.Key,
			ThingUUID: e.ThingUUID, Marker: e.Marker, ThingType: e.ThingType, Before: e.After,
			After: mustJSON(after), At: r.Now().UTC()}
		if _, err := r.Hub.PostAudit(ctx, []api.AuditEntry{entry}); err != nil {
			return steps, fmt.Errorf("audit %s: %w", step.OpID, err)
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
func (r *Runner) inverse(ctx context.Context, e api.AuditEntry, step *UndoStep, areas map[string]string) (any, string, error) {
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
		return r.undelete(ctx, e.ThingUUID, before)
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
		uuid, err := r.Things.AddArea(ctx, before.Title)
		if err == nil && uuid != "" {
			areas[e.ThingUUID] = uuid
		}
		return map[string]string{"uuid": uuid}, "", err
	}
	return nil, "", fmt.Errorf("no inverse for action %q", e.Action)
}

// undelete moves an item out of the Trash, and recreates it from its before
// state when the move did not take.
func (r *Runner) undelete(ctx context.Context, uuid string, before things.Item) (any, string, error) {
	st, err := r.Things.Lookup(ctx, uuid)
	if err != nil {
		return nil, "", err
	}
	if st != nil && !st.Trashed {
		return nil, ledgerState(st), nil
	}
	if st != nil {
		if err := r.Things.Restore(ctx, uuid, before.Type, before.ListID()); err == nil {
			if st, err = r.Things.Lookup(ctx, uuid); err == nil && st != nil && !st.Trashed {
				return map[string]string{"restored": uuid}, api.ThingOpen, nil
			}
		}
	}
	// Fallback: recreate from the snapshot taken before the delete.
	if before.Type == things.TypeProject {
		err = r.Things.AddProject(ctx, things.NewProject{Title: before.Title, Notes: before.Notes,
			Deadline: before.Deadline, Tags: before.Tags, AreaID: before.Area})
	} else {
		err = r.Things.AddTodo(ctx, things.NewTodo{Title: before.Title, Notes: before.Notes, When: whenOf(before),
			Deadline: before.Deadline, Tags: before.Tags, Checklist: before.ChecklistTitles(), ListID: before.ListID(),
			HeadingID: before.Heading})
	}
	if err != nil {
		return nil, "", err
	}
	return map[string]string{"recreated_from": uuid}, api.ThingGone, nil
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
