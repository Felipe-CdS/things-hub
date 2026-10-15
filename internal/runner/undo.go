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
	// Steps already undone, with what each wrote.
	done := map[string]json.RawMessage{}
	// Write-ahead rows of steps a killed undo started: the areas that had
	// the recreated area's title before its AddArea.
	ahead := map[string]json.RawMessage{}
	for _, a := range led.AppliedOps {
		switch a.Result {
		case resultOK, resultAlready:
			done[a.OpID] = a.Detail
		case resultStarted:
			ahead[a.OpID] = a.Detail
		}
	}
	audited := map[string]bool{}
	prev, err := r.Hub.Audit(ctx, undoRun)
	if err != nil {
		return nil, fmt.Errorf("read audit of %s: %w", undoRun, err)
	}
	for _, pe := range prev.Entries {
		audited[pe.OpID] = true
	}
	// Areas this undo recreated under a new uuid, so earlier moves out of
	// them go back into the new area.
	areas := map[string]string{}
	entries := slices.Clone(log.Entries)
	slices.SortFunc(entries, func(a, b api.AuditEntry) int { return int(b.Seq - a.Seq) })

	var steps []UndoStep
	for _, e := range entries {
		step := UndoStep{OpID: undoRun + "/" + strings.TrimPrefix(e.OpID, runID+"/"), Of: e.OpID,
			Action: e.Action, UUID: e.ThingUUID}
		if after, ok := done[step.OpID]; ok {
			var out struct{ UUID string }
			if e.Action == plan.KindDeleteArea && json.Unmarshal(after, &out) == nil && out.UUID != "" {
				areas[e.ThingUUID] = out.UUID
			}
			// A recorded step whose audit failed is done; only its audit is
			// left to write.
			if !audited[step.OpID] {
				if _, err := r.Hub.PostAudit(ctx, []api.AuditEntry{r.undoEntry(undoRun, step.OpID, e, after)}); err != nil {
					return steps, fmt.Errorf("audit %s: %w", step.OpID, err)
				}
			}
			step.Status, step.Describe = StatusReplayed, "already undone"
			steps = append(steps, step)
			continue
		}
		after, rows, err := r.inverse(ctx, e, &step, areas, ahead, led.Things)
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
		// The ledger goes first and in one write: a recreated area's or
		// item's new uuid must be durable before the step counts as undone.
		detail := mustJSON(after)
		req := api.LedgerPutRequest{Items: rows, AppliedOps: []api.AppliedOp{{OpID: step.OpID, RunID: undoRun,
			Result: resultOK, Detail: detail}}}
		if _, err := r.Hub.PutLedger(ctx, req); err != nil {
			return steps, fmt.Errorf("record %s: %w", step.OpID, err)
		}
		if _, err := r.Hub.PostAudit(ctx, []api.AuditEntry{r.undoEntry(undoRun, step.OpID, e, detail)}); err != nil {
			return steps, fmt.Errorf("audit %s: %w", step.OpID, err)
		}
		r.printf("%-9s %s\n", step.Status, step.Describe)
		steps = append(steps, step)
	}
	return steps, nil
}

// undoEntry is the audit entry of the step undoing e.
func (r *Runner) undoEntry(undoRun, opID string, e api.AuditEntry, after json.RawMessage) api.AuditEntry {
	return api.AuditEntry{RunID: undoRun, OpID: opID, Action: "undo_" + e.Action, Key: e.Key,
		ThingUUID: e.ThingUUID, Marker: e.Marker, ThingType: e.ThingType, Before: e.After,
		After: after, At: r.Now().UTC()}
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
// and the ledger rows that change with it.
func (r *Runner) inverse(ctx context.Context, e api.AuditEntry, step *UndoStep, areas map[string]string,
	ahead map[string]json.RawMessage, rows []api.Thing) (any, []api.Thing, error) {
	var before things.Item
	if len(e.Before) > 0 && string(e.Before) != "null" {
		if err := json.Unmarshal(e.Before, &before); err != nil {
			return nil, nil, fmt.Errorf("decode before: %w", err)
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
	own := func(state string) []api.Thing {
		if e.ThingUUID == "" || state == "" {
			return nil
		}
		return []api.Thing{{UUID: e.ThingUUID, Marker: e.Marker, State: state}}
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
			return nil, nil, err
		}
		if st == nil || st.Trashed || st.Status != things.StatusIncomplete {
			step.Status = StatusAlready
			return nil, own(ledgerState(st)), nil
		}
		ch := things.Change{Canceled: &t}
		return ch, own(api.ThingCanceled), update(ch)
	case plan.KindUpdate:
		ch, err := DecodeChange(e.After)
		if err != nil {
			return nil, nil, err
		}
		back := restoreChange(ch, before)
		step.Describe = fmt.Sprintf("~ restore %s %q", typ, before.Title)
		if back.Empty() {
			step.Status = StatusAlready
			return nil, nil, nil
		}
		return back, own(api.ThingOpen), update(back)
	case plan.KindComplete:
		step.Describe = fmt.Sprintf("↺ reopen %s %q", typ, before.Title)
		ch := things.Change{Completed: &f}
		return ch, own(api.ThingOpen), update(ch)
	case plan.KindCancel:
		step.Describe = fmt.Sprintf("↺ reopen %s %q", typ, before.Title)
		ch := things.Change{Canceled: &f}
		return ch, own(api.ThingOpen), update(ch)
	case plan.KindDelete:
		step.Describe = fmt.Sprintf("↺ restore %s %q from the Trash", typ, before.Title)
		return r.undelete(ctx, UndoRunID(e.RunID), step.OpID, e, before, rows)
	case plan.KindMoveProject:
		step.Describe = fmt.Sprintf("> move project %q back", before.Title)
		if before.Area == "" {
			return nil, nil, fmt.Errorf("project %q had no area; move it by hand", before.Title)
		}
		return map[string]string{"area": before.Area}, own(api.ThingOpen), r.Things.MoveProject(ctx, e.ThingUUID, before.Area)
	case plan.KindRenameArea:
		step.Describe = fmt.Sprintf("~ rename area back to %q", before.Title)
		return before, own(api.ThingOpen), r.Things.RenameArea(ctx, e.ThingUUID, before.Title)
	case "create_area":
		step.Describe = fmt.Sprintf("✗ delete created area %s", e.ThingUUID)
		after, state, err := r.deleteCreatedArea(ctx, e.ThingUUID)
		return after, own(state), err
	case "create_tag":
		var tag things.Item
		_ = json.Unmarshal(e.After, &tag)
		step.Describe = fmt.Sprintf("✗ delete created tag %q", tag.Title)
		after, err := r.deleteCreatedTag(ctx, tag.Title, step)
		return after, nil, err
	case plan.KindDeleteArea:
		step.Describe = fmt.Sprintf("+ recreate area %q (new uuid; its contents were moved before)", before.Title)
		uuid, err := r.recreateArea(ctx, UndoRunID(e.RunID), step.OpID, before.Title, ahead)
		if err == nil && uuid != "" {
			areas[e.ThingUUID] = uuid
		}
		return map[string]string{"uuid": uuid}, nil, err
	}
	return nil, nil, fmt.Errorf("no inverse for action %q", e.Action)
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
// a pending ledger row with the copy's marker goes first, so a resumed undo
// finds the copy its killed run created instead of adding a second one.
// The copy takes over the original row's key links.
func (r *Runner) undelete(ctx context.Context, runID, opID string, e api.AuditEntry, before things.Item,
	rows []api.Thing) (any, []api.Thing, error) {
	uuid := e.ThingUUID
	st, err := r.Things.Lookup(ctx, uuid)
	if err != nil {
		return nil, nil, err
	}
	if st != nil && !st.Trashed {
		return nil, []api.Thing{{UUID: uuid, Marker: e.Marker, State: ledgerState(st)}}, nil
	}
	links := []api.Link{}
	var copied api.Thing
	for _, t := range rows {
		if t.UUID == uuid {
			links = append(links, t.Links...)
		}
		if t.OpID == opID && t.RunID == runID {
			copied = t
		}
	}
	if copied.Marker != "" {
		it, err := r.findMarker(ctx, copied.Marker)
		if err != nil {
			return nil, nil, err
		}
		if it != nil {
			return recreated(uuid, e.Marker, copied.Marker, it, links)
		}
	} else if st != nil {
		if err := r.Things.Restore(ctx, uuid, before.Type, before.ListID()); err == nil {
			restored, err := r.poll(ctx, func() (bool, error) {
				st, err := r.Things.Lookup(ctx, uuid)
				return st != nil && !st.Trashed, err
			})
			if err != nil {
				return nil, nil, err
			}
			if restored {
				return map[string]string{"restored": uuid}, []api.Thing{{UUID: uuid, Marker: e.Marker, State: api.ThingOpen}}, nil
			}
		}
	}
	// Fallback: recreate from the snapshot taken before the delete.
	marker := copied.Marker
	if marker == "" {
		marker = things.NewMarker()
		ahead := api.Thing{Marker: marker, Type: ledgerType(before.Type), Origin: api.OriginHub,
			State: api.ThingPendingCreate, OpID: opID, RunID: runID, Written: mustJSON(before)}
		if _, err := r.Hub.PutLedger(ctx, api.LedgerPutRequest{Items: []api.Thing{ahead}}); err != nil {
			return nil, nil, fmt.Errorf("write-ahead: %w", err)
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
		return nil, nil, err
	}
	it, err := r.findMarker(ctx, marker)
	if err != nil {
		return nil, nil, err
	}
	if it == nil {
		return nil, nil, fmt.Errorf("recreated %s %q (marker %s) did not show up within %s; re-run the undo",
			before.Type, before.Title, marker, r.FindTimeout)
	}
	return recreated(uuid, e.Marker, marker, it, links)
}

// recreated returns the ledger rows of a deleted item's copy: the copy open
// with the original's key links, the original gone with none.
func recreated(uuid, oldMarker, marker string, it *things.Item, links []api.Link) (any, []api.Thing, error) {
	return map[string]string{"recreated_from": uuid, "uuid": it.UUID, "marker": marker}, []api.Thing{
		{UUID: uuid, Marker: oldMarker, State: api.ThingGone, Links: []api.Link{}},
		{Marker: marker, UUID: it.UUID, State: api.ThingOpen, Observed: mustJSON(it), Links: links},
	}, nil
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

func (r *Runner) deleteCreatedTag(ctx context.Context, title string, step *UndoStep) (any, error) {
	snap, err := r.Things.Snapshot(ctx, 30)
	if err != nil {
		return nil, err
	}
	for _, list := range [][]things.Item{snap.Todos, snap.Projects, snap.Areas} {
		for _, it := range list {
			if slices.Contains(it.Tags, title) {
				step.Status = StatusSkipped
				step.Error = fmt.Sprintf("tag %q is still used by %q; left in place", title, it.Title)
				return nil, nil
			}
		}
	}
	return map[string]string{"deleted_tag": title}, r.Things.DeleteTag(ctx, title)
}
