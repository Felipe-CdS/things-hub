package runner

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Felipe-CdS/things-hub/api"
	"github.com/Felipe-CdS/things-hub/internal/hub"
	"github.com/Felipe-CdS/things-hub/internal/plan"
	"github.com/Felipe-CdS/things-hub/internal/server"
	"github.com/Felipe-CdS/things-hub/internal/store"
	"github.com/Felipe-CdS/things-hub/internal/things"
	"github.com/Felipe-CdS/things-hub/internal/things/thingstest"
)

// env is an in-process thub-server (the real handler over a temporary
// database) and a fake Things.
type env struct {
	t    *testing.T
	st   *store.Store
	hub  *hub.Client
	fake *thingstest.Fake
	r    *Runner
	now  time.Time
	runs int
}

func newEnv(t *testing.T) *env {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "thub.db"))
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, st: st, fake: thingstest.New(), now: time.Now().UTC()}
	st.Now = func() time.Time { return e.now }
	tok, err := st.AddSender(context.Background(), "mac", true, []store.Scope{{Source: "github", Account: "felipe-cds"}})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(server.New(st, slog.New(slog.NewTextHandler(io.Discard, nil))))
	t.Cleanup(func() { srv.Close(); st.Close() })
	e.hub = hub.New(srv.URL, tok)
	e.r = New(e.hub, e.fake, io.Discard)
	e.r.Now = func() time.Time { return e.now }
	e.r.FindTimeout, e.r.FindInterval = 0, time.Millisecond
	return e
}

func (e *env) post(kind, id, rev, op string) string {
	e.t.Helper()
	ev := api.Event{Source: "github", Account: "felipe-cds", Kind: kind, SourceID: id, Op: op,
		Revision: rev, ObservedAt: rev}
	if op == api.OpUpsert {
		ev.URL, ev.Title = "https://github.com/"+strings.Replace(id, "#", "/pull/", 1), "Review PR: "+id
	}
	resp, err := e.hub.PostEvents(context.Background(), api.Batch{Schema: api.BatchSchema, Events: []api.Event{ev}})
	if err != nil {
		e.t.Fatal(err)
	}
	if r := resp.Results[0]; r.Result == api.ResultRejected {
		e.t.Fatalf("event rejected: %s", r.Reason)
	}
	return resp.Results[0].Key
}

func (e *env) pull(approved bool) *plan.Input {
	e.t.Helper()
	e.runs++
	e.now = e.now.Add(time.Minute)
	in, err := e.r.Pull(context.Background(), PullOptions{Dir: e.t.TempDir(), RunID: NewRunID(e.now),
		LayoutApproved: approved})
	if err != nil {
		e.t.Fatal(err)
	}
	return in
}

func newPlan(in *plan.Input) *plan.Plan {
	return &plan.Plan{Schema: plan.PlanSchema, RunID: in.RunID, LeaseID: in.LeaseID}
}

func (e *env) apply(in *plan.Input, p *plan.Plan, o ApplyOptions) *Report {
	e.t.Helper()
	rep, err := e.r.Apply(context.Background(), in, p, o)
	if err != nil {
		e.t.Fatal(err)
	}
	return rep
}

func createDecision(key string) plan.Decision {
	return plan.Decision{Key: key, Action: plan.ActCreate,
		Todo: &plan.Todo{Title: "Review PR: mic#12", Summary: "Review requested.", When: "today"}}
}

func (e *env) ledger(key string) []api.Thing {
	e.t.Helper()
	led, err := e.hub.Ledger(context.Background(), hub.LedgerQuery{Key: key})
	if err != nil {
		e.t.Fatal(err)
	}
	return led.Things
}

func TestCreateThenRepeatedRunsNeverDuplicate(t *testing.T) {
	e := newEnv(t)
	key := e.post("review-requested", "felipe-cds/mic#12", "2026-10-13T10:00:00Z", api.OpUpsert)
	in := e.pull(false)
	if len(in.Items) != 1 {
		t.Fatalf("leased %d items", len(in.Items))
	}
	p := newPlan(in)
	p.Items = []plan.Decision{createDecision(key)}
	rep := e.apply(in, p, ApplyOptions{})
	if rep.Ops[0].Status != StatusApplied || rep.Acks[0].Result != api.AckOK {
		t.Fatalf("report %+v", rep)
	}
	todo, ok := e.fake.Get(rep.Ops[0].UUID)
	if !ok || !strings.Contains(todo.Notes, "— GitHub · felipe-cds/mic#12 · review requested") ||
		things.MarkerIn(todo.Notes) != rep.Ops[0].Marker {
		t.Fatalf("notes %q", todo.Notes)
	}
	rows := e.ledger(key)
	if len(rows) != 1 || rows[0].State != api.ThingOpen || rows[0].UUID != todo.UUID {
		t.Fatalf("ledger %+v", rows)
	}

	// A resend of the same revision is a duplicate: nothing to pull.
	e.post("review-requested", "felipe-cds/mic#12", "2026-10-13T10:00:00Z", api.OpUpsert)
	if in := e.pull(false); len(in.Items) != 0 {
		t.Fatalf("duplicate was queued again: %+v", in.Items)
	}

	// A newer revision comes back with its live link; a second create is refused.
	e.post("review-requested", "felipe-cds/mic#12", "2026-10-13T11:00:00Z", api.OpUpsert)
	in = e.pull(false)
	if len(in.Items) != 1 || len(in.Items[0].Things) != 1 || in.Items[0].Things[0].State != api.ThingOpen {
		t.Fatalf("re-pulled item %+v", in.Items)
	}
	p = newPlan(in)
	p.Items = []plan.Decision{createDecision(key)}
	if _, err := e.r.Apply(context.Background(), in, p, ApplyOptions{}); err == nil ||
		!strings.Contains(err.Error(), "already has a live Things item") {
		t.Fatalf("second create: %v", err)
	}
	p.Items = []plan.Decision{{Key: key, Action: plan.ActAttach, UUID: todo.UUID, AppendChecklist: []string{"Re-review"}}}
	e.apply(in, p, ApplyOptions{})
	if n := e.fake.Count(things.TypeTodo); n != 1 {
		t.Fatalf("%d to-dos, want 1", n)
	}
	if got, _ := e.fake.Get(todo.UUID); !slices.Equal(got.ChecklistTitles(), []string{"Re-review"}) {
		t.Fatalf("checklist %v", got.ChecklistTitles())
	}
}

// crashOn cancels the run's context the first time method is called, the
// way a killed thub apply stops mid-run.
func crashOn(f *thingstest.Fake, method string, cancel context.CancelFunc) {
	f.Fail = func(m string) error {
		if m == method {
			f.Fail = nil
			cancel()
			return thingstest.ErrCrash
		}
		return nil
	}
}

func TestCrashAfterWriteSettlesByMarker(t *testing.T) {
	e := newEnv(t)
	key := e.post("review-requested", "felipe-cds/mic#12", "2026-10-13T10:00:00Z", api.OpUpsert)
	in := e.pull(false)
	p := newPlan(in)
	p.Items = []plan.Decision{createDecision(key)}

	// The to-do reaches Things, then the run dies before reading its uuid.
	ctx, cancel := context.WithCancel(context.Background())
	crashOn(e.fake, "FindByMarker", cancel)
	if _, err := e.r.Apply(ctx, in, p, ApplyOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("crashed apply: %v", err)
	}
	rows := e.ledger(key)
	if len(rows) != 1 || rows[0].State != api.ThingPendingCreate {
		t.Fatalf("write-ahead row %+v", rows)
	}

	// Re-running the same plan settles the marker instead of creating again.
	rep := e.apply(in, p, ApplyOptions{})
	if n := e.fake.Count(things.TypeTodo); n != 1 {
		t.Fatalf("%d to-dos after re-run, want 1", n)
	}
	if rep.Ops[0].Status != StatusAlready || rep.Acks[0].Result != api.AckOK {
		t.Fatalf("re-run report %+v", rep)
	}
	if rows := e.ledger(key); rows[0].State != api.ThingOpen || rows[0].UUID == "" {
		t.Fatalf("settled row %+v", rows)
	}
}

func TestCrashedRunIsSettledByTheNextPull(t *testing.T) {
	e := newEnv(t)
	key := e.post("review-requested", "felipe-cds/mic#12", "2026-10-13T10:00:00Z", api.OpUpsert)
	in := e.pull(false)
	p := newPlan(in)
	p.Items = []plan.Decision{createDecision(key)}
	ctx, cancel := context.WithCancel(context.Background())
	crashOn(e.fake, "FindByMarker", cancel)
	e.r.Apply(ctx, in, p, ApplyOptions{})

	// The lease expires; the next run pulls the item again and must see the
	// to-do the crashed run wrote.
	e.now = e.now.Add(3 * time.Hour)
	in = e.pull(false)
	if len(in.Items) != 1 || len(in.Items[0].Things) != 1 || in.Items[0].Things[0].State != api.ThingOpen {
		t.Fatalf("re-pulled item %+v", in.Items)
	}
	p = newPlan(in)
	p.Items = []plan.Decision{createDecision(key)}
	if res := plan.Check(in, p); len(res.Errors) == 0 {
		t.Fatal("a create for the settled key was accepted")
	}
	if n := e.fake.Count(things.TypeTodo); n != 1 {
		t.Fatalf("%d to-dos, want 1", n)
	}
}

func TestCrashBeforeWriteCreatesOnceWithTheSameMarker(t *testing.T) {
	e := newEnv(t)
	key := e.post("review-requested", "felipe-cds/mic#12", "2026-10-13T10:00:00Z", api.OpUpsert)
	in := e.pull(false)
	p := newPlan(in)
	p.Items = []plan.Decision{createDecision(key)}
	ctx, cancel := context.WithCancel(context.Background())
	crashOn(e.fake, "AddTodo", cancel)
	e.r.Apply(ctx, in, p, ApplyOptions{})
	marker := e.ledger(key)[0].Marker

	rep := e.apply(in, p, ApplyOptions{})
	if got := e.fake.WithMarker(marker); len(got) != 1 {
		t.Fatalf("%d to-dos with the write-ahead marker %s, want 1", len(got), marker)
	}
	if rep.Ops[0].Marker != marker || e.fake.Count(things.TypeTodo) != 1 {
		t.Fatalf("report %+v", rep.Ops)
	}
}

func TestAppliedOpsReplayWritesNothing(t *testing.T) {
	e := newEnv(t)
	area := e.fake.Put(things.Item{Type: things.TypeArea, Title: "Work"})
	old := e.fake.Put(things.Item{Type: things.TypeTodo, Title: "Old task", Area: area})
	done := e.fake.Put(things.Item{Type: things.TypeTodo, Title: "Finished thing"})
	key := e.post("review-requested", "felipe-cds/mic#12", "2026-10-13T10:00:00Z", api.OpUpsert)
	in := e.pull(true)
	p := newPlan(in)
	p.Organize = []plan.Organize{
		{Op: plan.OpCreateTag, Title: "github"},
		{Op: plan.OpUpdate, UUID: old, Fields: &plan.Fields{When: ptr("2026-10-20")}},
		{Op: plan.OpComplete, UUID: done},
	}
	d := createDecision(key)
	d.Todo.Tags, d.Todo.List = []string{"github"}, "Work"
	p.Items = []plan.Decision{d}
	first := e.apply(in, p, ApplyOptions{})
	for _, o := range first.Ops {
		if o.Status != StatusApplied {
			t.Fatalf("first apply %+v", first.Ops)
		}
	}
	writes := e.fake.Writes()

	second := e.apply(in, p, ApplyOptions{})
	if got := e.fake.Writes(); !slices.Equal(got, writes) {
		t.Fatalf("replay wrote to Things: %v", got[len(writes):])
	}
	for _, o := range second.Ops {
		if o.Status != StatusReplayed {
			t.Fatalf("replay %+v", second.Ops)
		}
	}
	if second.Acks[0].Result != api.AckDuplicate {
		t.Fatalf("second ack %+v", second.Acks)
	}
}

func TestOwnerChangeSinceSnapshotIsSkippedAndDeferred(t *testing.T) {
	e := newEnv(t)
	todo := e.fake.Put(things.Item{Type: things.TypeTodo, Title: "Mine"})
	key := e.post("assigned", "felipe-cds/mic#7", "2026-10-13T10:00:00Z", api.OpUpsert)
	in := e.pull(false)
	p := newPlan(in)
	p.Items = []plan.Decision{{Key: key, Action: plan.ActUpdate, UUID: todo, Fields: &plan.Fields{Deadline: ptr("2026-10-30")}}}
	e.fake.Touch(todo)
	rep := e.apply(in, p, ApplyOptions{})
	if rep.Ops[0].Status != StatusSkipped || rep.Acks[0].State != api.StatePending {
		t.Fatalf("report %+v acks %+v", rep.Ops, rep.Acks)
	}
	if got, _ := e.fake.Get(todo); got.Deadline != "" {
		t.Fatal("the owner's change was clobbered")
	}
}

func TestFirstLayoutRunIsPreviewOnly(t *testing.T) {
	e := newEnv(t)
	todo := e.fake.Put(things.Item{Type: things.TypeTodo, Title: "Old"})
	in := e.pull(false)
	p := newPlan(in)
	p.Organize = []plan.Organize{{Op: plan.OpComplete, UUID: todo}}
	if _, err := e.r.Apply(context.Background(), in, p, ApplyOptions{}); !errors.Is(err, ErrLayoutPreview) {
		t.Fatalf("unapproved layout run: %v", err)
	}
	if len(e.fake.Writes()) != 0 {
		t.Fatal("a preview-only run wrote to Things")
	}
	e.apply(in, p, ApplyOptions{ApproveLayout: true})
	if got, _ := e.fake.Get(todo); got.Status != things.StatusCompleted {
		t.Fatalf("status %s", got.Status)
	}
}

func TestResolvedGitHubItemCompletesItsTodo(t *testing.T) {
	e := newEnv(t)
	key := e.post("review-requested", "felipe-cds/mic#12", "2026-10-13T10:00:00Z", api.OpUpsert)
	in := e.pull(false)
	p := newPlan(in)
	p.Items = []plan.Decision{createDecision(key)}
	uuid := e.apply(in, p, ApplyOptions{}).Ops[0].UUID

	e.post("review-requested", "felipe-cds/mic#12", "2026-10-13T12:00:00Z", api.OpResolve)
	in = e.pull(false)
	p = newPlan(in)
	p.Items = []plan.Decision{{Key: key, Action: plan.ActIgnore, Reason: "merged"}}
	if res := plan.Check(in, p); len(res.Errors) == 0 {
		t.Fatal("ignoring a resolved GitHub item with an open to-do was accepted")
	}
	p.Items = []plan.Decision{{Key: key, Action: plan.ActResolve}}
	rep := e.apply(in, p, ApplyOptions{})
	if got, _ := e.fake.Get(uuid); got.Status != things.StatusCompleted {
		t.Fatalf("to-do is %s after resolve; report %+v", got.Status, rep.Ops)
	}
	if rows := e.ledger(key); rows[0].State != api.ThingCompleted {
		t.Fatalf("ledger %+v", rows)
	}
}

func TestUndoReversesNewestFirst(t *testing.T) {
	e := newEnv(t)
	work := e.fake.Put(things.Item{Type: things.TypeArea, Title: "Work"})
	home := e.fake.Put(things.Item{Type: things.TypeArea, Title: "Home"})
	proj := e.fake.Put(things.Item{Type: things.TypeProject, Title: "Garden", Area: work})
	moved := e.fake.Put(things.Item{Type: things.TypeTodo, Title: "Plan", Area: work, StartDate: "2026-10-14", Start: "Anytime"})
	done := e.fake.Put(things.Item{Type: things.TypeTodo, Title: "Call"})
	trash := e.fake.Put(things.Item{Type: things.TypeTodo, Title: "Dup", Area: home})
	key := e.post("review-requested", "felipe-cds/mic#12", "2026-10-13T10:00:00Z", api.OpUpsert)
	in := e.pull(true)
	p := newPlan(in)
	p.Organize = []plan.Organize{
		{Op: plan.OpUpdate, UUID: moved, Fields: &plan.Fields{When: ptr("2026-10-20"), List: ptr("Home")}},
		{Op: plan.OpComplete, UUID: done},
		{Op: plan.OpDelete, UUID: trash, Reason: "duplicate"},
		{Op: plan.OpMoveProject, UUID: proj, Area: "Home"},
	}
	p.Items = []plan.Decision{createDecision(key)}
	rep := e.apply(in, p, ApplyOptions{})
	created := rep.Ops[len(rep.Ops)-1].UUID
	before := len(e.fake.Writes())

	steps, err := e.r.Undo(context.Background(), in.RunID)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range steps {
		if s.Status != StatusApplied {
			t.Fatalf("undo step %+v", s)
		}
	}
	got := e.fake.Writes()[before:]
	want := []string{
		"UpdateTodo " + created + " canceled=true",
		"MoveProject " + proj + " " + work,
		"Restore " + trash,
		"UpdateTodo " + done + " completed=false",
		"UpdateTodo " + moved + " when=2026-10-14 list=" + work,
	}
	if !slices.Equal(got, want) {
		t.Fatalf("undo writes\n got %q\nwant %q", got, want)
	}
	if it, _ := e.fake.Get(moved); it.Area != work || it.StartDate != "2026-10-14" {
		t.Fatalf("moved to-do not restored: %+v", it)
	}
	if it, _ := e.fake.Get(trash); it.Trashed {
		t.Fatal("deleted to-do still in the Trash")
	}

	// Undo is itself idempotent: a second undo writes nothing.
	n := len(e.fake.Writes())
	if _, err := e.r.Undo(context.Background(), in.RunID); err != nil {
		t.Fatal(err)
	}
	if len(e.fake.Writes()) != n {
		t.Fatal("a repeated undo wrote again")
	}
}

func TestMissingTail(t *testing.T) {
	for _, c := range []struct{ have, add, want []string }{
		{nil, []string{"a", "b"}, []string{"a", "b"}},
		{[]string{"x", "a"}, []string{"a", "b"}, []string{"b"}},
		{[]string{"x", "a", "b"}, []string{"a", "b"}, nil},
		{[]string{"b"}, []string{"a", "b"}, []string{"a", "b"}},
	} {
		if got := missingTail(c.have, c.add); !slices.Equal(got, c.want) {
			t.Errorf("missingTail(%v, %v) = %v, want %v", c.have, c.add, got, c.want)
		}
	}
}

func ptr[T any](v T) *T { return &v }
