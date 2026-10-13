package classroom

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var ctx = context.Background()

var now = time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)

// fakeGoogle serves the OAuth token endpoint and the Classroom API.
type fakeGoogle struct {
	t   *testing.T
	srv *httptest.Server

	mu        sync.Mutex
	challenge string // code_challenge seen by the consent page
	refreshes int
	paths     []string
	// failPath answers 403 for this API path.
	failPath string
}

func newFakeGoogle(t *testing.T) *fakeGoogle {
	f := &fakeGoogle{t: t}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGoogle) config() Config {
	return Config{ClientID: "cid", ClientSecret: "csecret", AuthURL: f.srv.URL + "/auth",
		TokenURL: f.srv.URL + "/token", APIURL: f.srv.URL}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func (f *fakeGoogle) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Path == "/token" {
		f.token(w, r)
		return
	}
	if r.Method != http.MethodGet {
		f.t.Errorf("%s %s: the client must only read", r.Method, r.URL.Path)
	}
	if r.Header.Get("Authorization") != "Bearer at1" {
		writeJSON(w, 401, map[string]any{"error": map[string]any{"code": 401, "message": "no token", "status": "UNAUTHENTICATED"}})
		return
	}
	f.paths = append(f.paths, r.URL.Path)
	if r.URL.Path == f.failPath {
		writeJSON(w, 403, map[string]any{"error": map[string]any{"code": 403,
			"message": "The caller does not have permission", "status": "PERMISSION_DENIED"}})
		return
	}
	q := r.URL.Query()
	page := q.Get("pageToken")
	switch r.URL.Path {
	case "/v1/courses":
		if q.Get("studentId") != "me" || q.Get("courseStates") != "ACTIVE" {
			f.t.Errorf("courses query = %v", q)
		}
		if page == "" {
			writeJSON(w, 200, map[string]any{"courses": []any{
				map[string]any{"id": "c1", "name": "Cálculo I", "section": "Turma A",
					"alternateLink": "https://classroom.google.com/c/c1"},
			}, "nextPageToken": "p2"})
			return
		}
		writeJSON(w, 200, map[string]any{"courses": []any{
			map[string]any{"id": "c2", "name": "Física", "alternateLink": "https://classroom.google.com/c/c2"},
		}})
	case "/v1/courses/c1/courseWork":
		if q.Get("courseWorkStates") != "PUBLISHED" {
			f.t.Errorf("coursework query = %v", q)
		}
		writeJSON(w, 200, map[string]any{"courseWork": []any{
			map[string]any{"id": "w1", "title": " Lista 3 ", "description": "Exercícios 1 a 10.",
				"alternateLink": "https://classroom.google.com/c/c1/a/w1", "workType": "ASSIGNMENT", "maxPoints": 10,
				"dueDate":      map[string]any{"year": 2026, "month": 10, "day": 20},
				"dueTime":      map[string]any{"hours": 2, "minutes": 59},
				"creationTime": "2026-10-01T10:00:00.123Z", "updateTime": "2026-10-02T10:00:00Z"},
			map[string]any{"id": "w2", "title": "Lista 2", "alternateLink": "https://classroom.google.com/c/c1/a/w2",
				"dueDate": map[string]any{"year": 2026, "month": 10, "day": 1}, "dueTime": map[string]any{},
				"creationTime": "2026-09-20T10:00:00Z", "updateTime": "2026-09-20T10:00:00Z"},
		}})
	case "/v1/courses/c1/courseWork/-/studentSubmissions":
		if q.Get("userId") != "me" {
			f.t.Errorf("submissions query = %v", q)
		}
		writeJSON(w, 200, map[string]any{"studentSubmissions": []any{
			map[string]any{"courseWorkId": "w1", "state": "CREATED", "updateTime": "2026-10-05T09:00:00Z"},
			map[string]any{"courseWorkId": "w2", "state": "TURNED_IN", "late": true, "updateTime": "2026-10-02T03:00:00Z"},
		}})
	case "/v1/courses/c2/courseWork":
		if page == "" {
			writeJSON(w, 200, map[string]any{"courseWork": []any{}, "nextPageToken": "q2"})
			return
		}
		writeJSON(w, 200, map[string]any{"courseWork": []any{
			map[string]any{"id": "w3", "title": "Relatório", "dueDate": map[string]any{"year": 2026, "month": 11, "day": 3},
				"updateTime": "2026-10-10T10:00:00Z"},
			map[string]any{"id": "w4", "title": "Prova", "alternateLink": "https://classroom.google.com/c/c2/a/w4",
				"updateTime": "2026-09-01T10:00:00Z"},
		}})
	case "/v1/courses/c2/courseWork/-/studentSubmissions":
		writeJSON(w, 200, map[string]any{"studentSubmissions": []any{
			map[string]any{"courseWorkId": "w4", "state": "RETURNED", "updateTime": "2026-09-15T10:00:00Z"},
		}})
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeGoogle) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		f.t.Fatal(err)
	}
	if r.PostForm.Get("client_id") != "cid" || r.PostForm.Get("client_secret") != "csecret" {
		writeJSON(w, 401, map[string]any{"error": "invalid_client"})
		return
	}
	switch r.PostForm.Get("grant_type") {
	case "refresh_token":
		if r.PostForm.Get("refresh_token") != "rt-good" {
			writeJSON(w, 400, map[string]any{"error": "invalid_grant", "error_description": "Token has been expired or revoked."})
			return
		}
		f.refreshes++
		writeJSON(w, 200, map[string]any{"access_token": "at1", "token_type": "Bearer", "expires_in": 3600})
	case "authorization_code":
		sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
		if r.PostForm.Get("code") != "code-1" || base64.RawURLEncoding.EncodeToString(sum[:]) != f.challenge {
			writeJSON(w, 400, map[string]any{"error": "invalid_grant"})
			return
		}
		writeJSON(w, 200, map[string]any{"access_token": "at1", "token_type": "Bearer", "expires_in": 3600,
			"refresh_token": "rt-new"})
	default:
		writeJSON(w, 400, map[string]any{"error": "unsupported_grant_type"})
	}
}

func TestFetchAssignments(t *testing.T) {
	g := newFakeGoogle(t)
	c := NewClient(ctx, g.config(), "rt-good")
	got, err := c.FetchAssignments(ctx, "university", now)
	if err != nil {
		t.Fatal(err)
	}
	if got.Account != "university" || got.Courses != 2 || got.FetchedAt != "2026-10-13T12:00:00Z" {
		t.Fatalf("header = %+v", got)
	}
	want := []Assignment{
		{SourceID: "c1/w1", CourseID: "c1", Course: "Cálculo I", Section: "Turma A", CourseWorkID: "w1",
			Title: "Lista 3", Description: "Exercícios 1 a 10.", URL: "https://classroom.google.com/c/c1/a/w1",
			WorkType: "ASSIGNMENT", MaxPoints: 10, Due: "2026-10-20T02:59:00Z", Created: "2026-10-01T10:00:00.123Z",
			Revision: "2026-10-05T09:00:00Z", State: "CREATED"},
		{SourceID: "c1/w2", CourseID: "c1", Course: "Cálculo I", Section: "Turma A", CourseWorkID: "w2",
			Title: "Lista 2", URL: "https://classroom.google.com/c/c1/a/w2", Due: "2026-10-01T00:00:00Z",
			Created: "2026-09-20T10:00:00Z", Revision: "2026-10-02T03:00:00Z", State: "TURNED_IN", Late: true, Done: true},
		{SourceID: "c2/w3", CourseID: "c2", Course: "Física", CourseWorkID: "w3", Title: "Relatório",
			URL: "https://classroom.google.com/c/c2", Due: "2026-11-03", Revision: "2026-10-10T10:00:00Z", State: "NONE"},
		{SourceID: "c2/w4", CourseID: "c2", Course: "Física", CourseWorkID: "w4", Title: "Prova",
			URL: "https://classroom.google.com/c/c2/a/w4", Revision: "2026-09-15T10:00:00Z", State: "RETURNED", Done: true},
	}
	if len(got.Assignments) != len(want) {
		t.Fatalf("got %d assignments: %+v", len(got.Assignments), got.Assignments)
	}
	for i := range want {
		if got.Assignments[i] != want[i] {
			t.Errorf("assignment %d =\n%+v\nwant\n%+v", i, got.Assignments[i], want[i])
		}
	}
	if g.refreshes != 1 {
		t.Errorf("refreshed %d times, want once", g.refreshes)
	}
}

func TestOld(t *testing.T) {
	const longAgo = "2026-09-20T10:00:00Z"
	co := course{ID: "c1", Name: "Cálculo I"}
	cases := []struct {
		name string
		cw   courseWork
		s    submission
		want bool
	}{
		{"undated, created and unchanged long ago", courseWork{ID: "w", CreationTime: longAgo, UpdateTime: longAgo},
			submission{State: "CREATED", UpdateTime: longAgo}, true},
		{"undated, unsubmitted yesterday", courseWork{ID: "w", CreationTime: longAgo, UpdateTime: longAgo},
			submission{State: "RECLAIMED_BY_STUDENT", UpdateTime: "2026-10-12T10:00:00Z"}, false},
		{"undated, created recently", courseWork{ID: "w", CreationTime: "2026-10-10T10:00:00Z", UpdateTime: "2026-10-10T10:00:00Z"},
			submission{State: StateNone}, false},
		{"long overdue, unchanged", courseWork{ID: "w", DueDate: &date{2026, 9, 25}, CreationTime: longAgo, UpdateTime: longAgo},
			submission{State: StateNone}, true},
		{"long overdue, coursework edited lately", courseWork{ID: "w", DueDate: &date{2026, 9, 25}, CreationTime: longAgo,
			UpdateTime: "2026-10-11T10:00:00Z"}, submission{State: StateNone}, false},
		{"due soon, created long ago", courseWork{ID: "w", DueDate: &date{2026, 10, 20}, CreationTime: longAgo, UpdateTime: longAgo},
			submission{State: StateNone}, false},
		{"no times at all", courseWork{ID: "w"}, submission{State: StateNone}, false},
	}
	for _, tc := range cases {
		if got := join(co, tc.cw, tc.s, now); got.Old != tc.want {
			t.Errorf("%s: old = %v, want %v (%+v)", tc.name, got.Old, tc.want, got)
		}
	}
	unarchived := course{ID: "c1", Name: "Cálculo I", UpdateTime: "2026-10-12T10:00:00Z"}
	for _, i := range []int{0, 3} {
		tc := cases[i]
		if got := join(unarchived, tc.cw, tc.s, now); got.Old || got.Revision != longAgo {
			t.Errorf("%s, in a course unarchived yesterday: %+v", tc.name, got)
		}
	}
}

func TestFetchFailsWhole(t *testing.T) {
	g := newFakeGoogle(t)
	g.failPath = "/v1/courses/c2/courseWork/-/studentSubmissions"
	_, err := NewClient(ctx, g.config(), "rt-good").FetchAssignments(ctx, "university", now)
	if err == nil || !strings.Contains(err.Error(), "403 PERMISSION_DENIED: The caller does not have permission") {
		t.Fatalf("err = %v", err)
	}
}

func TestRefreshRefused(t *testing.T) {
	g := newFakeGoogle(t)
	_, err := NewClient(ctx, g.config(), "rt-revoked").Courses(ctx)
	if !errors.Is(err, ErrLoginExpired) {
		t.Fatalf("err = %v, want ErrLoginExpired", err)
	}
}

func TestLogin(t *testing.T) {
	g := newFakeGoogle(t)
	var out strings.Builder
	browser := func(page string) error {
		u, err := url.Parse(page)
		if err != nil {
			return err
		}
		q := u.Query()
		for k, want := range map[string]string{"access_type": "offline", "prompt": "select_account consent",
			"code_challenge_method": "S256", "client_id": "cid", "response_type": "code"} {
			if q.Get(k) != want {
				t.Errorf("auth URL %s = %q, want %q", k, q.Get(k), want)
			}
		}
		for _, s := range strings.Fields(q.Get("scope")) {
			if !strings.HasSuffix(s, ".readonly") {
				t.Errorf("scope %s is not read-only", s)
			}
		}
		if n := len(strings.Fields(q.Get("scope"))); n != 4 {
			t.Errorf("asked for %d scopes, want 4 with announcements", n)
		}
		redirect := q.Get("redirect_uri")
		if !strings.HasPrefix(redirect, "http://127.0.0.1:") {
			t.Errorf("redirect_uri = %s", redirect)
		}
		g.mu.Lock()
		g.challenge = q.Get("code_challenge")
		g.mu.Unlock()
		// The browser follows the redirect back with the code.
		go func() {
			resp, err := http.Get(redirect + "?" + url.Values{"code": {"code-1"}, "state": {q.Get("state")}}.Encode())
			if err == nil {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
		}()
		return nil
	}
	tok, err := Login(ctx, g.config(), LoginOptions{Announcements: true, OpenBrowser: browser, Out: &out, Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if tok.RefreshToken != "rt-new" {
		t.Fatalf("refresh token = %q", tok.RefreshToken)
	}
	if !strings.Contains(out.String(), g.srv.URL+"/auth?") {
		t.Fatalf("login did not print the consent page:\n%s", out.String())
	}
}

func TestLoginRefusedByGoogle(t *testing.T) {
	g := newFakeGoogle(t)
	browser := func(page string) error {
		u, _ := url.Parse(page)
		q := u.Query()
		go http.Get(q.Get("redirect_uri") + "?" + url.Values{"error": {"admin_policy_enforced"}, "state": {q.Get("state")}}.Encode())
		return nil
	}
	_, err := Login(ctx, g.config(), LoginOptions{OpenBrowser: browser, Out: io.Discard, Timeout: 10 * time.Second})
	if err == nil || !strings.Contains(err.Error(), "admin_policy_enforced") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoginWrongState(t *testing.T) {
	g := newFakeGoogle(t)
	browser := func(page string) error {
		u, _ := url.Parse(page)
		go http.Get(u.Query().Get("redirect_uri") + "?code=code-1&state=forged")
		return nil
	}
	_, err := Login(ctx, g.config(), LoginOptions{OpenBrowser: browser, Out: io.Discard, Timeout: 10 * time.Second})
	if err == nil || !strings.Contains(err.Error(), "wrong state") {
		t.Fatalf("err = %v", err)
	}
}

func TestEnvFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	if env, err := LoadEnvFile(path); err != nil || len(env) != 0 {
		t.Fatalf("missing file = %v, %v", env, err)
	}
	body := "# things-hub\nTHUB_URL=http://127.0.0.1:8787\nexport THUB_GOOGLE_CLIENT_ID='cid.apps'\n" +
		"THUB_CLASSROOM_REFRESH_TOKEN=old\nTHUB_CLASSROOM_ACCOUNT=\"university\"\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadEnvFile(path); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("world-readable file accepted: %v", err)
	}
	os.Chmod(path, 0o600)
	if err := SetEnvFileKey(path, "THUB_CLASSROOM_REFRESH_TOKEN", "1//0new-token_x"); err != nil {
		t.Fatal(err)
	}
	env, err := LoadEnvFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{"THUB_URL": "http://127.0.0.1:8787", "THUB_GOOGLE_CLIENT_ID": "cid.apps",
		"THUB_CLASSROOM_REFRESH_TOKEN": "1//0new-token_x", "THUB_CLASSROOM_ACCOUNT": "university"} {
		if env[k] != want {
			t.Errorf("%s = %q, want %q", k, env[k], want)
		}
	}
	data, _ := os.ReadFile(path)
	if strings.Count(string(data), "THUB_CLASSROOM_REFRESH_TOKEN") != 1 || !strings.HasPrefix(string(data), "# things-hub\n") {
		t.Errorf("file after set:\n%s", data)
	}

	fresh := filepath.Join(t.TempDir(), ".env")
	if err := SetEnvFileKey(fresh, "K", "v"); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(fresh); fi.Mode().Perm() != 0o600 {
		t.Errorf("new env file mode = %v", fi.Mode().Perm())
	}
	if err := SetEnvFileKey(fresh, "K", "a b"); err == nil {
		t.Error("value with a space accepted")
	}
}
