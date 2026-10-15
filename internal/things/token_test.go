package things

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// The fixture is a throwaway SQLite file with the one TMSettings row the
// token lives in; the real Things database is never opened.
func tokenFixture(t *testing.T, token string) *DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "main.sqlite")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`CREATE TABLE TMSettings (uuid TEXT, uriSchemeAuthenticationToken TEXT);
		INSERT INTO TMSettings VALUES (?, ?)`, settingsUUID, token); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	db, err := OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestAuthTokenPrefersEnvFile(t *testing.T) {
	ctx := context.Background()
	l := &Live{DB: tokenFixture(t, "db_token"), URLToken: "env_token"}
	if tok, err := l.authToken(ctx); err != nil || tok != "env_token" {
		t.Fatalf("authToken with .env token: %q %v", tok, err)
	}
}

func TestAuthTokenFallsBackToDatabase(t *testing.T) {
	ctx := context.Background()
	l := &Live{DB: tokenFixture(t, "db_token")}
	if tok, err := l.authToken(ctx); err != nil || tok != "db_token" {
		t.Fatalf("authToken without .env token: %q %v", tok, err)
	}
}

func TestMoveProjectSendsEnvToken(t *testing.T) {
	ctx := context.Background()
	var got []string
	l := &Live{DB: tokenFixture(t, "db_token"), URLToken: "env_token",
		Exec: func(_ context.Context, name string, args ...string) ([]byte, error) {
			got = append(got, args...)
			return nil, nil
		}}
	if err := l.MoveProject(ctx, "PROJ", "AREA"); err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 || !strings.Contains(got[len(got)-1], "auth-token=env_token") {
		t.Fatalf("MoveProject did not use the .env token: %q", got)
	}
}
