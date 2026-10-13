package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/Felipe-CdS/things-hub/api"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newStore(t *testing.T) (*Store, *clock) {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "thub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	c := &clock{t: time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)}
	st.Now = c.now
	return st, c
}

var ctx = context.Background()

func mac(t *testing.T, st *Store) *Sender {
	t.Helper()
	token, err := st.AddSender(ctx, "mac", true, []Scope{{Source: "github", Account: "Felipe-CdS"}})
	if err != nil {
		t.Fatal(err)
	}
	s, err := st.Authenticate(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func ghEvent(id, revision string) api.Event {
	return api.Event{
		Source: "github", Account: "Felipe-CdS", Kind: "review-requested", SourceID: id,
		Op: "upsert", Revision: revision, ObservedAt: "2026-10-13T12:00:00Z",
		URL: "https://github.com/felipe-cds/mic/pull/12", Title: "Review PR: microcode loader",
	}
}

func ingest(t *testing.T, st *Store, s *Sender, cursor *api.Cursor, events ...api.Event) api.BatchResponse {
	t.Helper()
	in := make([]IncomingEvent, len(events))
	for i, e := range events {
		in[i].Event = e
	}
	resp, err := st.Ingest(ctx, s, cursor, in)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func results(resp api.BatchResponse) []string {
	out := make([]string, len(resp.Results))
	for i, r := range resp.Results {
		out[i] = r.Result
	}
	return out
}

func wantResults(t *testing.T, resp api.BatchResponse, want ...string) {
	t.Helper()
	got := results(resp)
	if len(got) != len(want) {
		t.Fatalf("results = %v, want %v (%+v)", got, want, resp.Results)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("results = %v, want %v (%+v)", got, want, resp.Results)
		}
	}
}

func pull(t *testing.T, st *Store, holder string, max, lease int) api.PullResponse {
	t.Helper()
	resp, err := st.Pull(ctx, holder, api.PullRequest{Max: max, LeaseSeconds: lease})
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func ack(t *testing.T, st *Store, holder, lease string, acks ...api.Ack) []api.AckResult {
	t.Helper()
	resp, err := st.Ack(ctx, holder, api.AckRequest{LeaseID: lease, Acks: acks})
	if err != nil {
		t.Fatal(err)
	}
	return resp.Results
}

func itemState(t *testing.T, st *Store, key string) (state string, attempts int) {
	t.Helper()
	if err := st.db.QueryRow(`SELECT state, attempts FROM items WHERE key = ?`, key).Scan(&state, &attempts); err != nil {
		t.Fatal(err)
	}
	return state, attempts
}

const key12 = "github:felipe-cds:review-requested:felipe-cds/mic#12"

func TestKeyAndDedupe(t *testing.T) {
	st, c := newStore(t)
	s := mac(t, st)

	resp := ingest(t, st, s, nil, ghEvent("felipe-cds/mic#12", "2026-10-13T11:50:02Z"))
	wantResults(t, resp, api.ResultAccepted)
	if resp.Results[0].Key != key12 {
		t.Fatalf("key = %q, want %q", resp.Results[0].Key, key12)
	}

	// Same revision, spelled differently, is a duplicate; older is stale.
	wantResults(t, ingest(t, st, s, nil,
		ghEvent("felipe-cds/mic#12", "2026-10-13T11:50:02.000Z"),
		ghEvent("felipe-cds/mic#12", "2026-10-13T11:00:00Z"),
	), api.ResultDuplicate, api.ResultStale)

	// The first revision is processed and done.
	p := pull(t, st, "mac", 10, 600)
	if len(p.Items) != 1 || p.Items[0].Event == nil || p.Items[0].Event.Title == "" {
		t.Fatalf("pull = %+v", p)
	}
	r := ack(t, st, "mac", p.LeaseID, api.Ack{Key: key12, Revision: "2026-10-13T11:50:02Z", Outcome: api.OutcomeCreated})
	if r[0].Result != api.AckOK || r[0].State != api.StateDone {
		t.Fatalf("ack = %+v", r)
	}

	// A resend is still a duplicate and does not requeue the item.
	c.advance(time.Hour)
	wantResults(t, ingest(t, st, s, nil, ghEvent("felipe-cds/mic#12", "2026-10-13T11:50:02Z")), api.ResultDuplicate)
	if state, _ := itemState(t, st, key12); state != api.StateDone {
		t.Fatalf("state after duplicate = %s", state)
	}

	// A newer revision puts it back in the queue with the previous outcome.
	wantResults(t, ingest(t, st, s, nil, ghEvent("felipe-cds/mic#12", "2026-10-13T13:00:00Z")), api.ResultAccepted)
	p = pull(t, st, "mac", 10, 600)
	if len(p.Items) != 1 || p.Items[0].Revision != "2026-10-13T13:00:00Z" {
		t.Fatalf("pull = %+v", p)
	}
	if prev := p.Items[0].Previous; prev == nil || prev.Outcome != api.OutcomeCreated || prev.Revision != "2026-10-13T11:50:02Z" {
		t.Fatalf("previous = %+v", p.Items[0].Previous)
	}
}

func TestUnixRevisions(t *testing.T) {
	st, _ := newStore(t)
	token, err := st.AddSender(ctx, "notebook", false, []Scope{{Source: "slack", Account: "T024BE7LD"}})
	if err != nil {
		t.Fatal(err)
	}
	s, _ := st.Authenticate(ctx, token)
	e := api.Event{
		Source: "slack", Account: "T024BE7LD", Kind: "thread-question", SourceID: "C024BE91L/1760356800.123456",
		Op: "upsert", Revision: "1760357100.000200", ObservedAt: "2026-10-13T12:05:11Z",
		URL: "https://acme.slack.com/archives/C024BE91L/p1760356800123456", Title: "Answer Ana",
	}
	older, newer := e, e
	older.Revision, newer.Revision = "1760357100.0002", "1760357100.000201"
	resp := ingest(t, st, s, nil, e, older, newer)
	wantResults(t, resp, api.ResultAccepted, api.ResultDuplicate, api.ResultAccepted)
	if want := "slack:t024be7ld:thread-question:C024BE91L/1760356800.123456"; resp.Results[0].Key != want {
		t.Fatalf("key = %q, want %q", resp.Results[0].Key, want)
	}
}

func TestRejectsAndCursor(t *testing.T) {
	st, _ := newStore(t)
	s := mac(t, st)
	cur := &api.Cursor{Source: "github", Account: "felipe-cds", Value: "2026-10-13T12:00:00Z"}

	bad := ghEvent("felipe-cds/mic#13", "2026-10-13T11:50:02Z")
	bad.Kind = "review-requsted"
	resp := ingest(t, st, s, cur, ghEvent("felipe-cds/mic#12", "2026-10-13T11:50:02Z"), bad)
	wantResults(t, resp, api.ResultAccepted, api.ResultRejected)
	if resp.CursorStored {
		t.Fatal("cursor stored although an event was rejected")
	}
	got, err := st.Cursor(ctx, s, "github", "Felipe-CdS")
	if err != nil || got.Value != nil {
		t.Fatalf("cursor = %+v, %v", got, err)
	}

	resp = ingest(t, st, s, cur, ghEvent("felipe-cds/mic#14", "2026-10-13T11:50:02Z"))
	if !resp.CursorStored {
		t.Fatal("cursor not stored")
	}
	got, err = st.Cursor(ctx, s, "github", "felipe-cds")
	if err != nil || got.Value == nil || *got.Value != cur.Value {
		t.Fatalf("cursor = %+v, %v", got, err)
	}

	// Writing someone else's account is rejected per event, and a cursor
	// for it is refused as a whole.
	other := ghEvent("someorg/repo#1", "2026-10-13T11:50:02Z")
	other.Account = "someorg"
	wantResults(t, ingest(t, st, s, nil, other), api.ResultRejected)
	_, err = st.Ingest(ctx, s, &api.Cursor{Source: "github", Account: "someorg", Value: "x"}, nil)
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("foreign cursor err = %v", err)
	}
}

func TestOpenAndResolve(t *testing.T) {
	st, _ := newStore(t)
	s := mac(t, st)
	ingest(t, st, s, nil, ghEvent("felipe-cds/mic#12", "2026-10-13T11:00:00Z"), ghEvent("felipe-cds/mic#13", "2026-10-13T11:00:00Z"))
	resolve := api.Event{Source: "github", Account: "felipe-cds", Kind: "review-requested", SourceID: "felipe-cds/mic#13",
		Op: "resolve", Revision: "2026-10-13T12:00:00Z", ObservedAt: "2026-10-13T12:00:00Z"}
	wantResults(t, ingest(t, st, s, nil, resolve), api.ResultAccepted)
	open, err := st.Open(ctx, s, "github", "felipe-cds")
	if err != nil {
		t.Fatal(err)
	}
	if len(open.Items) != 1 || open.Items[0].SourceID != "felipe-cds/mic#12" || open.Items[0].Kind != "review-requested" {
		t.Fatalf("open = %+v", open)
	}
}

func TestLeaseExpiryAndRepull(t *testing.T) {
	st, c := newStore(t)
	s := mac(t, st)
	ingest(t, st, s, nil, ghEvent("felipe-cds/mic#12", "2026-10-13T11:00:00Z"), ghEvent("felipe-cds/mic#13", "2026-10-13T11:00:00Z"))

	first := pull(t, st, "mac", 10, 60)
	if len(first.Items) != 2 {
		t.Fatalf("first pull = %d items", len(first.Items))
	}
	// A second pull during the lease gets nothing.
	if p := pull(t, st, "mac", 10, 60); len(p.Items) != 0 || p.LeaseID != "" {
		t.Fatalf("pull during lease = %+v", p)
	}

	// After expiry the items come back under a new lease.
	c.advance(61 * time.Second)
	second := pull(t, st, "mac", 1, 600)
	if len(second.Items) != 1 || second.LeaseID == first.LeaseID {
		t.Fatalf("second pull = %+v", second)
	}
	taken := second.Items[0].Key

	// The old lease can no longer ack the item the new lease took ...
	r := ack(t, st, "mac", first.LeaseID, api.Ack{Key: taken, Revision: "2026-10-13T11:00:00Z", Outcome: api.OutcomeCreated})
	if r[0].Result != api.AckRejected {
		t.Fatalf("ack by expired lease of a re-pulled item = %+v", r)
	}
	// ... but may still ack the one no other lease took.
	var untaken string
	for _, it := range first.Items {
		if it.Key != taken {
			untaken = it.Key
		}
	}
	r = ack(t, st, "mac", first.LeaseID, api.Ack{Key: untaken, Revision: "2026-10-13T11:00:00Z", Outcome: api.OutcomeIgnored})
	if r[0].Result != api.AckOK || r[0].State != api.StateDone {
		t.Fatalf("late ack = %+v", r)
	}

	// Release returns the rest.
	rel, err := st.Release(ctx, "mac", api.ReleaseRequest{LeaseID: second.LeaseID})
	if err != nil || rel.Released != 1 {
		t.Fatalf("release = %+v, %v", rel, err)
	}
	if p := pull(t, st, "mac", 10, 600); len(p.Items) != 1 || p.Items[0].Key != taken {
		t.Fatalf("pull after release = %+v", p)
	}
}

func TestAckIdempotentAndNewerRevisionDuringLease(t *testing.T) {
	st, _ := newStore(t)
	s := mac(t, st)
	ingest(t, st, s, nil, ghEvent("felipe-cds/mic#12", "2026-10-13T11:00:00Z"))
	p := pull(t, st, "mac", 10, 600)

	// A newer revision arrives while the item is leased.
	wantResults(t, ingest(t, st, s, nil, ghEvent("felipe-cds/mic#12", "2026-10-13T11:30:00Z")), api.ResultAccepted)
	if again := pull(t, st, "mac", 10, 600); len(again.Items) != 0 {
		t.Fatalf("leased item handed out twice: %+v", again)
	}

	a := api.Ack{Key: key12, Revision: "2026-10-13T11:00:00Z", Outcome: api.OutcomeCreated}
	r := ack(t, st, "mac", p.LeaseID, a)
	if r[0].Result != api.AckOK || r[0].State != api.StatePending {
		t.Fatalf("ack with newer revision pending = %+v", r)
	}
	r = ack(t, st, "mac", p.LeaseID, a)
	if r[0].Result != api.AckDuplicate {
		t.Fatalf("repeated ack = %+v", r)
	}
	if state, _ := itemState(t, st, key12); state != api.StatePending {
		t.Fatalf("state after repeated ack = %s", state)
	}

	p2 := pull(t, st, "mac", 10, 600)
	if len(p2.Items) != 1 || p2.Items[0].Revision != "2026-10-13T11:30:00Z" {
		t.Fatalf("re-pull = %+v", p2)
	}
	// An ack for a revision the server never stored is refused.
	r = ack(t, st, "mac", p2.LeaseID, api.Ack{Key: key12, Revision: "2026-10-13T12:00:00Z", Outcome: api.OutcomeUpdated})
	if r[0].Result != api.AckRejected {
		t.Fatalf("ack ahead of stored revision = %+v", r)
	}
	r = ack(t, st, "mac", p2.LeaseID, api.Ack{Key: key12, Revision: "2026-10-13T11:30:00Z", Outcome: api.OutcomeUpdated})
	if r[0].Result != api.AckOK || r[0].State != api.StateDone {
		t.Fatalf("final ack = %+v", r)
	}

	// Another sender's lease is not usable.
	if _, err := st.Ack(ctx, "other", api.AckRequest{LeaseID: p2.LeaseID}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("foreign ack err = %v", err)
	}
	if _, err := st.Ack(ctx, "mac", api.AckRequest{LeaseID: "l_nope"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown lease err = %v", err)
	}
}

func TestParkAfterThreeFailures(t *testing.T) {
	st, c := newStore(t)
	s := mac(t, st)
	ingest(t, st, s, nil, ghEvent("felipe-cds/mic#12", "2026-10-13T11:00:00Z"))
	fail := api.Ack{Key: key12, Revision: "2026-10-13T11:00:00Z", Outcome: api.OutcomeFailed, Note: "marker not found"}

	for i := 1; i <= api.MaxAttempts; i++ {
		p := pull(t, st, "mac", 10, 600)
		if len(p.Items) != 1 {
			t.Fatalf("attempt %d: pull = %+v", i, p)
		}
		r := ack(t, st, "mac", p.LeaseID, fail)
		// A repeated failure ack does not count twice.
		ack(t, st, "mac", p.LeaseID, fail)
		state, attempts := itemState(t, st, key12)
		if attempts != i {
			t.Fatalf("attempt %d: attempts = %d", i, attempts)
		}
		want := api.StatePending
		if i == api.MaxAttempts {
			want = api.StateParked
		}
		if state != want || r[0].State != want {
			t.Fatalf("attempt %d: state = %s / %s, want %s", i, state, r[0].State, want)
		}
	}
	if p := pull(t, st, "mac", 10, 600); len(p.Items) != 0 {
		t.Fatalf("parked item pulled: %+v", p)
	}
	status, err := st.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.Queue.Parked != 1 || len(status.Parked) != 1 || status.Parked[0].Key != key12 || status.Parked[0].LastNote != "marker not found" {
		t.Fatalf("status = %+v", status)
	}

	// A newer revision gives a parked item a fresh start.
	c.advance(time.Minute)
	ingest(t, st, s, nil, ghEvent("felipe-cds/mic#12", "2026-10-13T12:00:00Z"))
	if state, attempts := itemState(t, st, key12); state != api.StatePending || attempts != 0 {
		t.Fatalf("after new revision: %s, %d", state, attempts)
	}
}

func TestDeferredNotBefore(t *testing.T) {
	st, c := newStore(t)
	s := mac(t, st)
	ingest(t, st, s, nil, ghEvent("felipe-cds/mic#12", "2026-10-13T11:00:00Z"))
	p := pull(t, st, "mac", 10, 600)
	r := ack(t, st, "mac", p.LeaseID, api.Ack{Key: key12, Revision: "2026-10-13T11:00:00Z",
		Outcome: api.OutcomeDeferred, NotBefore: c.t.Add(time.Hour).Format(time.RFC3339)})
	if r[0].Result != api.AckOK || r[0].State != api.StatePending {
		t.Fatalf("defer = %+v", r)
	}
	if p := pull(t, st, "mac", 10, 600); len(p.Items) != 0 {
		t.Fatalf("deferred item pulled early")
	}
	c.advance(time.Hour)
	if p := pull(t, st, "mac", 10, 600); len(p.Items) != 1 {
		t.Fatalf("deferred item not pulled after not_before")
	}
}

func TestLedgerAndLinks(t *testing.T) {
	st, _ := newStore(t)
	s := mac(t, st)
	ingest(t, st, s, nil, ghEvent("felipe-cds/mic#12", "2026-10-13T11:00:00Z"))
	p := pull(t, st, "mac", 10, 600)

	// Write-ahead: the create is recorded by marker before Things is touched.
	_, err := st.PutLedger(ctx, api.LedgerPutRequest{Items: []api.Thing{{
		Marker: "th-4k2m9x7qpa", Type: api.ThingTodo, Origin: api.OriginHub, State: api.ThingPendingCreate,
		Written: []byte(`{"title":"Review PR"}`), OpID: "run1/1", RunID: "run1",
		Links: []api.Link{{Key: key12, Role: api.RolePrimary}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	pending, err := st.Ledger(ctx, LedgerFilter{State: api.ThingPendingCreate})
	if err != nil || len(pending.Things) != 1 {
		t.Fatalf("pending creates = %+v, %v", pending, err)
	}

	// Settling fills in the uuid; the op is recorded once.
	resp, err := st.PutLedger(ctx, api.LedgerPutRequest{
		Items:      []api.Thing{{Marker: "th-4k2m9x7qpa", UUID: "UUID-1", State: api.ThingOpen}},
		AppliedOps: []api.AppliedOp{{OpID: "run1/1", RunID: "run1", Result: "applied"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := resp.Items[0]
	if got.UUID != "UUID-1" || got.State != api.ThingOpen || got.Type != api.ThingTodo || len(got.Links) != 1 || string(got.Written) != `{"title":"Review PR"}` {
		t.Fatalf("settled row = %+v", got)
	}
	// A different uuid for the same marker is refused.
	if _, err := st.PutLedger(ctx, api.LedgerPutRequest{Items: []api.Thing{{Marker: "th-4k2m9x7qpa", UUID: "UUID-2"}}}); err == nil {
		t.Fatal("conflicting uuid accepted")
	}

	r := ack(t, st, "mac", p.LeaseID, api.Ack{Key: key12, Revision: "2026-10-13T11:00:00Z", Outcome: api.OutcomeCreated,
		Items: []api.ThingRef{{UUID: "UUID-1", Role: api.RolePrimary}}})
	if r[0].Result != api.AckOK {
		t.Fatalf("ack = %+v", r)
	}
	led, err := st.Ledger(ctx, LedgerFilter{Key: key12, Run: "run1"})
	if err != nil || len(led.Things) != 1 || len(led.AppliedOps) != 1 {
		t.Fatalf("ledger by key = %+v, %v", led, err)
	}

	// The next pull of the key shows its linked Things item.
	ingest(t, st, s, nil, ghEvent("felipe-cds/mic#12", "2026-10-13T12:00:00Z"))
	p = pull(t, st, "mac", 10, 600)
	if len(p.Items) != 1 || len(p.Items[0].Things) != 1 || p.Items[0].Things[0].UUID != "UUID-1" {
		t.Fatalf("pull things = %+v", p.Items)
	}
}

func TestRejectedAckWritesNoLinks(t *testing.T) {
	st, _ := newStore(t)
	s := mac(t, st)
	ingest(t, st, s, nil, ghEvent("felipe-cds/mic#12", "2026-10-13T11:00:00Z"))
	p := pull(t, st, "mac", 10, 600)
	if _, err := st.PutLedger(ctx, api.LedgerPutRequest{Items: []api.Thing{{Marker: "th-4k2m9x7qpa", UUID: "UUID-1", Type: api.ThingTodo, Origin: api.OriginHub, State: api.ThingOpen}}}); err != nil {
		t.Fatal(err)
	}

	r := ack(t, st, "mac", p.LeaseID, api.Ack{Key: key12, Revision: "2026-10-13T11:00:00Z", Outcome: api.OutcomeCreated,
		Items: []api.ThingRef{{UUID: "UUID-1", Role: api.RolePrimary}, {UUID: "UUID-missing", Role: api.RoleChild}}})
	if r[0].Result != api.AckRejected {
		t.Fatalf("ack = %+v", r)
	}
	led, err := st.Ledger(ctx, LedgerFilter{Key: key12})
	if err != nil || len(led.Things) != 0 {
		t.Fatalf("ledger by key after rejected ack = %+v, %v", led, err)
	}
}

func TestEmptyLinksOverJSONClearLinks(t *testing.T) {
	st, _ := newStore(t)
	if _, err := st.PutLedger(ctx, api.LedgerPutRequest{Items: []api.Thing{{
		Marker: "th-4k2m9x7qpa", Type: api.ThingTodo, Origin: api.OriginHub, State: api.ThingOpen, Links: []api.Link{{Key: key12, Role: api.RolePrimary}},
	}}}); err != nil {
		t.Fatal(err)
	}

	body, err := json.Marshal(api.LedgerPutRequest{Items: []api.Thing{{Marker: "th-4k2m9x7qpa", Links: []api.Link{}}}})
	if err != nil {
		t.Fatal(err)
	}
	var req api.LedgerPutRequest
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	resp, err := st.PutLedger(ctx, req)
	if err != nil || len(resp.Items) != 1 || len(resp.Items[0].Links) != 0 {
		t.Fatalf("unlink = %+v, %v", resp, err)
	}
	led, err := st.Ledger(ctx, LedgerFilter{Key: key12})
	if err != nil || len(led.Things) != 0 {
		t.Fatalf("ledger by key after unlink = %+v, %v", led, err)
	}
}

func TestAuditAndSnapshots(t *testing.T) {
	st, c := newStore(t)
	entry := api.AuditEntry{RunID: "run1", OpID: "run1/1", Action: "create", ThingUUID: "UUID-1",
		After: []byte(`{"title":"x"}`), At: c.t}
	r, err := st.AppendAudit(ctx, []api.AuditEntry{entry, entry})
	if err != nil || r.Appended != 1 || r.Duplicate != 1 {
		t.Fatalf("append = %+v, %v", r, err)
	}
	a, err := st.Audit(ctx, "run1")
	if err != nil || len(a.Entries) != 1 || string(a.Entries[0].After) != `{"title":"x"}` {
		t.Fatalf("audit = %+v, %v", a, err)
	}

	for i := 0; i < api.SnapshotKeep+2; i++ {
		c.advance(time.Minute)
		if err := st.PutSnapshot(ctx, fmt.Sprintf("run%02d", i), []byte(`{"areas":[]}`)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.Snapshot(ctx, "run00"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("oldest snapshot err = %v", err)
	}
	if body, err := st.Snapshot(ctx, "run31"); err != nil || string(body) != `{"areas":[]}` {
		t.Fatalf("newest snapshot = %s, %v", body, err)
	}
	if err := st.PutSnapshot(ctx, "run-x", []byte(`not json`)); err == nil {
		t.Fatal("invalid snapshot accepted")
	}
}

func TestRetentionPrune(t *testing.T) {
	st, c := newStore(t)
	s := mac(t, st)
	ingest(t, st, s, nil, ghEvent("felipe-cds/mic#12", "2026-10-13T11:00:00Z"), ghEvent("felipe-cds/mic#13", "2026-10-13T11:00:00Z"))
	p := pull(t, st, "mac", 1, 600)
	ack(t, st, "mac", p.LeaseID, api.Ack{Key: p.Items[0].Key, Revision: "2026-10-13T11:00:00Z", Outcome: api.OutcomeCreated, Note: "made a to-do"})
	doneKey := p.Items[0].Key

	c.advance(91 * 24 * time.Hour)
	ingest(t, st, s, nil, ghEvent("felipe-cds/mic#14", "2026-10-13T11:00:00Z"))

	r, err := st.Prune(ctx, 90)
	if err != nil {
		t.Fatal(err)
	}
	if r.EventPayloads != 2 || r.ItemPayloads != 1 || r.Leases != 1 {
		t.Fatalf("prune = %+v", r)
	}
	var nullPayloads, keptPayloads int
	st.db.QueryRow(`SELECT COUNT(*) FROM events WHERE payload IS NULL`).Scan(&nullPayloads)
	st.db.QueryRow(`SELECT COUNT(*) FROM events WHERE payload IS NOT NULL`).Scan(&keptPayloads)
	if nullPayloads != 2 || keptPayloads != 1 {
		t.Fatalf("events pruned = %d, kept = %d", nullPayloads, keptPayloads)
	}
	var doneNote, donePayload any
	st.db.QueryRow(`SELECT payload, last_note FROM items WHERE key = ?`, doneKey).Scan(&donePayload, &doneNote)
	if donePayload != nil || doneNote != nil {
		t.Fatalf("done item kept payload %v / note %v", donePayload, doneNote)
	}

	// Keys and revisions stay, so dedupe still works, and the still-pending
	// item keeps its payload for the Mac.
	wantResults(t, ingest(t, st, s, nil,
		ghEvent("felipe-cds/mic#12", "2026-10-13T11:00:00Z"),
		ghEvent("felipe-cds/mic#13", "2026-10-13T11:00:00Z"),
	), api.ResultDuplicate, api.ResultDuplicate)
	p = pull(t, st, "mac", 10, 600)
	if len(p.Items) != 2 {
		t.Fatalf("pull after prune = %+v", p)
	}
	for _, it := range p.Items {
		if it.Event == nil {
			t.Fatalf("pending item %s lost its payload", it.Key)
		}
	}
}

func TestSenderScopesAndRevoke(t *testing.T) {
	st, _ := newStore(t)
	token, err := st.AddSender(ctx, "notebook", false, []Scope{{Source: "slack", Account: "T024BE7LD"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddSender(ctx, "notebook", false, nil); err == nil {
		t.Fatal("duplicate sender accepted")
	}
	if _, err := st.AddSender(ctx, "x", false, []Scope{{Source: "jira", Account: "a"}}); err == nil {
		t.Fatal("unknown source accepted")
	}
	var stored string
	st.db.QueryRow(`SELECT token_hash FROM senders WHERE name = 'notebook'`).Scan(&stored)
	if stored != HashToken(token) || stored == token {
		t.Fatal("token not stored as its SHA-256")
	}
	s, err := st.Authenticate(ctx, token)
	if err != nil || s.Reader || !s.CanWrite("slack", "t024be7ld") || s.CanWrite("github", "acme") {
		t.Fatalf("sender = %+v, %v", s, err)
	}
	if err := st.AllowSender(ctx, "notebook", false, []Scope{{Source: "github", Account: "Acme"}}); err != nil {
		t.Fatal(err)
	}
	if s, _ = st.Authenticate(ctx, token); !s.CanWrite("github", "acme") {
		t.Fatal("allow did not add the scope")
	}
	if err := st.RevokeSender(ctx, "notebook"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Authenticate(ctx, token); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("revoked token err = %v", err)
	}
	if err := st.RevokeSender(ctx, "nobody"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoke unknown err = %v", err)
	}
}
