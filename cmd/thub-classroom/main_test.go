package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Felipe-CdS/things-hub/internal/classroom"
)

func testApp(t *testing.T, envBody string) (*app, *bytes.Buffer) {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte(envBody), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("THUB_ENV_FILE", path)
	for _, k := range []string{"THUB_GOOGLE_CLIENT_ID", "THUB_GOOGLE_CLIENT_SECRET",
		"THUB_CLASSROOM_REFRESH_TOKEN", "THUB_CLASSROOM_ACCOUNT"} {
		t.Setenv(k, "")
	}
	var out bytes.Buffer
	a, err := newApp(&out)
	if err != nil {
		t.Fatal(err)
	}
	a.now = func() time.Time { return time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC) }
	return a, &out
}

func TestSettingsErrors(t *testing.T) {
	ctx := context.Background()
	a, _ := testApp(t, "THUB_GOOGLE_CLIENT_ID=cid\nTHUB_GOOGLE_CLIENT_SECRET=cs\n")
	if err := a.run(ctx, []string{"assignments"}); err == nil || !strings.Contains(err.Error(), "THUB_CLASSROOM_ACCOUNT") {
		t.Fatalf("missing account: %v", err)
	}
	a.env["THUB_CLASSROOM_ACCOUNT"] = "university"
	if err := a.run(ctx, []string{"assignments"}); err == nil || !strings.Contains(err.Error(), "thub-classroom login") {
		t.Fatalf("missing refresh token: %v", err)
	}
	if err := a.run(ctx, []string{"coursework"}); err == nil {
		t.Fatal("coursework without a course id succeeded")
	}
	if err := a.run(ctx, []string{"post"}); err == nil {
		t.Fatal("unknown command succeeded")
	}
	t.Setenv("THUB_CLASSROOM_ACCOUNT", "School")
	b, err := newApp(&bytes.Buffer{})
	if err != nil || b.env["THUB_CLASSROOM_ACCOUNT"] != "School" || b.env["THUB_GOOGLE_CLIENT_ID"] != "cid" {
		t.Fatalf("environment over env file = %v, %v", b.env, err)
	}
}

func TestAssignmentsCommand(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/token":
			w.Write([]byte(`{"access_token":"at","token_type":"Bearer","expires_in":3600}`))
		case "/v1/courses":
			w.Write([]byte(`{"courses":[{"id":"c1","name":"Cálculo I"}]}`))
		case "/v1/courses/c1/courseWork":
			w.Write([]byte(`{"courseWork":[{"id":"w1","title":"Lista 3","updateTime":"2026-10-02T10:00:00Z"}]}`))
		case "/v1/courses/c1/courseWork/-/studentSubmissions":
			w.Write([]byte(`{"studentSubmissions":[{"courseWorkId":"w1","state":"TURNED_IN","updateTime":"2026-10-05T10:00:00Z"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	a, out := testApp(t, "THUB_GOOGLE_CLIENT_ID=cid\nTHUB_GOOGLE_CLIENT_SECRET=cs\n"+
		"THUB_CLASSROOM_REFRESH_TOKEN=rt\nTHUB_CLASSROOM_ACCOUNT=University\n")
	a.cfg = classroom.Config{TokenURL: srv.URL + "/token", APIURL: srv.URL}
	if err := a.run(context.Background(), []string{"assignments"}); err != nil {
		t.Fatal(err)
	}
	var got classroom.Assignments
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("%v:\n%s", err, out)
	}
	if got.Account != "university" || len(got.Assignments) != 1 {
		t.Fatalf("output = %+v", got)
	}
	if a := got.Assignments[0]; a.SourceID != "c1/w1" || !a.Done || a.Revision != "2026-10-05T10:00:00Z" {
		t.Fatalf("assignment = %+v", a)
	}
}
