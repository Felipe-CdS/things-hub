package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Felipe-CdS/things-hub/api"
	"github.com/Felipe-CdS/things-hub/internal/store"
)

type fixture struct {
	srv      *httptest.Server
	st       *store.Store
	logs     *bytes.Buffer
	mac      string
	notebook string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "thub.db"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	mac, err := st.AddSender(ctx, "mac", true, []store.Scope{{Source: "github", Account: "felipe-cds"}})
	if err != nil {
		t.Fatal(err)
	}
	notebook, err := st.AddSender(ctx, "notebook", false, []store.Scope{
		{Source: "slack", Account: "T024BE7LD"}, {Source: "github", Account: "acme"},
	})
	if err != nil {
		t.Fatal(err)
	}
	logs := &bytes.Buffer{}
	srv := httptest.NewServer(New(st, slog.New(slog.NewTextHandler(logs, nil))))
	t.Cleanup(func() { srv.Close(); st.Close() })
	return &fixture{srv: srv, st: st, logs: logs, mac: mac, notebook: notebook}
}

func (f *fixture) do(t *testing.T, method, path, token string, body any) (int, []byte) {
	t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, f.srv.URL+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

const slackBatch = `{
  "schema": "things-hub.batch.v1",
  "cursor": { "source": "slack", "account": "T024BE7LD", "value": "2026-10-13T12:00:00Z" },
  "events": [ {
    "source": "slack", "account": "T024BE7LD", "kind": "thread-question",
    "source_id": "C024BE91L/1760356800.123456", "op": "upsert",
    "revision": "1760357100.000200", "observed_at": "2026-10-13T12:05:11Z",
    "url": "https://acme.slack.com/archives/C024BE91L/p1760356800123456",
    "title": "Answer Ana: can the deploy move to Thursday?",
    "summary": "Ana asked in #deploys whether the Q4 deploy window can move to Thursday.",
    "from": "Ana Souza", "due": "2026-10-15", "labels": { "channel": "#deploys" }
  } ]
}`

func TestTokenScopes(t *testing.T) {
	f := newFixture(t)

	if code, _ := f.do(t, "GET", "/healthz", "", nil); code != http.StatusOK {
		t.Fatalf("healthz = %d", code)
	}
	if code, _ := f.do(t, "POST", "/v1/events", "", slackBatch); code != http.StatusUnauthorized {
		t.Fatalf("no token = %d", code)
	}
	if code, _ := f.do(t, "POST", "/v1/events", "thub_wrong", slackBatch); code != http.StatusUnauthorized {
		t.Fatalf("bad token = %d", code)
	}

	// The notebook writes its own workspace ...
	code, body := f.do(t, "POST", "/v1/events", f.notebook, slackBatch)
	var br api.BatchResponse
	json.Unmarshal(body, &br)
	if code != http.StatusOK || len(br.Results) != 1 || br.Results[0].Result != api.ResultAccepted || !br.CursorStored {
		t.Fatalf("notebook post = %d %s", code, body)
	}
	if code, body := f.do(t, "GET", "/v1/cursor?source=slack&account=T024BE7LD", f.notebook, nil); code != http.StatusOK ||
		!strings.Contains(string(body), `"value":"2026-10-13T12:00:00Z"`) {
		t.Fatalf("cursor = %d %s", code, body)
	}

	// ... but not the Mac's personal GitHub, and it can never read the queue.
	personal := `{"schema":"things-hub.batch.v1","events":[{"source":"github","account":"felipe-cds","kind":"assigned",
		"source_id":"felipe-cds/x#1","op":"upsert","revision":"2026-10-13T12:00:00Z","observed_at":"2026-10-13T12:00:00Z",
		"url":"https://github.com/felipe-cds/x/issues/1","title":"Fix x"}]}`
	code, body = f.do(t, "POST", "/v1/events", f.notebook, personal)
	if code != http.StatusOK || !strings.Contains(string(body), `"result":"rejected"`) {
		t.Fatalf("notebook personal post = %d %s", code, body)
	}
	for _, p := range []struct{ method, path string }{
		{"POST", "/v1/pull"}, {"POST", "/v1/ack"}, {"POST", "/v1/release"}, {"GET", "/v1/ledger"},
		{"PUT", "/v1/ledger/items"}, {"POST", "/v1/audit"}, {"GET", "/v1/audit?run=r"},
		{"PUT", "/v1/snapshots/r"}, {"GET", "/v1/snapshots/r"}, {"GET", "/v1/status"},
	} {
		if code, _ := f.do(t, p.method, p.path, f.notebook, "{}"); code != http.StatusForbidden {
			t.Fatalf("notebook %s %s = %d, want 403", p.method, p.path, code)
		}
	}
	if code, _ := f.do(t, "GET", "/v1/open?source=github&account=felipe-cds", f.notebook, nil); code != http.StatusForbidden {
		t.Fatalf("notebook open on personal account = %d", code)
	}
	if code, _ := f.do(t, "GET", "/v1/cursor?source=slack&account=T999", f.notebook, nil); code != http.StatusForbidden {
		t.Fatalf("notebook cursor on other workspace = %d", code)
	}
	foreignCursor := `{"schema":"things-hub.batch.v1","cursor":{"source":"slack","account":"T999","value":"x"},"events":[]}`
	if code, _ := f.do(t, "POST", "/v1/events", f.notebook, foreignCursor); code != http.StatusForbidden {
		t.Fatalf("foreign cursor = %d", code)
	}

	// The Mac reads the queue and sees the notebook's event.
	code, body = f.do(t, "POST", "/v1/pull", f.mac, `{"max":5}`)
	var pr api.PullResponse
	json.Unmarshal(body, &pr)
	if code != http.StatusOK || len(pr.Items) != 1 || pr.Items[0].Event == nil || pr.Items[0].Event.From != "Ana Souza" {
		t.Fatalf("mac pull = %d %s", code, body)
	}
	if code, _ := f.do(t, "GET", "/v1/status", f.mac, nil); code != http.StatusOK {
		t.Fatalf("mac status = %d", code)
	}

	// Revoking takes effect at once.
	if err := f.st.RevokeSender(context.Background(), "notebook"); err != nil {
		t.Fatal(err)
	}
	if code, _ := f.do(t, "POST", "/v1/events", f.notebook, slackBatch); code != http.StatusUnauthorized {
		t.Fatalf("revoked = %d", code)
	}

	// Logs carry keys and counts, never titles or summaries.
	logs := f.logs.String()
	for _, secret := range []string{"Answer Ana", "Q4 deploy", "Ana Souza", f.notebook, f.mac} {
		if strings.Contains(logs, secret) {
			t.Fatalf("logs contain %q:\n%s", secret, logs)
		}
	}
}

func TestEventValidation(t *testing.T) {
	f := newFixture(t)
	cases := []struct{ name, event, reason string }{
		{"unknown field", `{"source":"github","account":"felipe-cds","kind":"assigned","source_id":"a/b#1","op":"upsert",
			"revision":"2026-10-13T12:00:00Z","observed_at":"2026-10-13T12:00:00Z","url":"https://x.y/1","title":"t","titel":"t"}`, "unknown field"},
		{"bad kind", `{"source":"github","account":"felipe-cds","kind":"mention","source_id":"a/b#1","op":"upsert",
			"revision":"2026-10-13T12:00:00Z","observed_at":"2026-10-13T12:00:00Z","url":"https://x.y/1","title":"t"}`, "kind"},
		{"missing title", `{"source":"github","account":"felipe-cds","kind":"assigned","source_id":"a/b#1","op":"upsert",
			"revision":"2026-10-13T12:00:00Z","observed_at":"2026-10-13T12:00:00Z","url":"https://x.y/1"}`, "title"},
		{"long title", `{"source":"github","account":"felipe-cds","kind":"assigned","source_id":"a/b#1","op":"upsert",
			"revision":"2026-10-13T12:00:00Z","observed_at":"2026-10-13T12:00:00Z","url":"https://x.y/1","title":"` +
			strings.Repeat("x", 141) + `"}`, "title"},
		{"bad revision", `{"source":"github","account":"felipe-cds","kind":"assigned","source_id":"a/b#1","op":"upsert",
			"revision":"yesterday","observed_at":"2026-10-13T12:00:00Z","url":"https://x.y/1","title":"t"}`, "revision"},
		{"bad due", `{"source":"github","account":"felipe-cds","kind":"assigned","source_id":"a/b#1","op":"upsert",
			"revision":"2026-10-13T12:00:00Z","observed_at":"2026-10-13T12:00:00Z","url":"https://x.y/1","title":"t","due":"Friday"}`, "due"},
	}
	for _, c := range cases {
		code, body := f.do(t, "POST", "/v1/events", f.mac, `{"schema":"things-hub.batch.v1","events":[`+c.event+`]}`)
		if code != http.StatusOK || !strings.Contains(string(body), `"result":"rejected"`) || !strings.Contains(string(body), c.reason) {
			t.Errorf("%s: %d %s", c.name, code, body)
		}
	}
	if code, _ := f.do(t, "POST", "/v1/events", f.mac, `{"schema":"v0","events":[]}`); code != http.StatusBadRequest {
		t.Errorf("wrong schema = %d", code)
	}
	if code, _ := f.do(t, "POST", "/v1/events", f.mac, `{"schema":"things-hub.batch.v1","events":[],"extra":1}`); code != http.StatusBadRequest {
		t.Errorf("unknown envelope field = %d", code)
	}
	many := `{"schema":"things-hub.batch.v1","events":[` + strings.TrimSuffix(strings.Repeat(`{},`, api.MaxBatchEvents+1), ",") + `]}`
	if code, _ := f.do(t, "POST", "/v1/events", f.mac, many); code != http.StatusBadRequest {
		t.Errorf("too many events = %d", code)
	}
	huge := `{"schema":"things-hub.batch.v1","events":[],"pad":"` + strings.Repeat("x", api.MaxBatchBytes) + `"}`
	if code, _ := f.do(t, "POST", "/v1/events", f.mac, huge); code != http.StatusRequestEntityTooLarge {
		t.Errorf("huge batch = %d", code)
	}
}

func TestSnapshotRoundTrip(t *testing.T) {
	f := newFixture(t)
	snap := `{"areas":[{"uuid":"A1","title":"Work"}],"todos":[]}`
	if code, body := f.do(t, "PUT", "/v1/snapshots/2026-10-13T14-02-11Z", f.mac, snap); code != http.StatusOK {
		t.Fatalf("put = %d %s", code, body)
	}
	if code, body := f.do(t, "GET", "/v1/snapshots/2026-10-13T14-02-11Z", f.mac, nil); code != http.StatusOK || string(body) != snap {
		t.Fatalf("get = %d %s", code, body)
	}
	if code, _ := f.do(t, "GET", "/v1/snapshots/missing", f.mac, nil); code != http.StatusNotFound {
		t.Fatalf("missing = %d", code)
	}
}
