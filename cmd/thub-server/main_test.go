package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func admin(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := run(args, &out)
	return out.String(), err
}

func TestAdminCommands(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("THUB_DB", filepath.Join(dir, "thub.db"))

	if _, err := admin(t, "sender", "list"); err == nil {
		t.Fatal("list on a missing database succeeded")
	}
	out, err := admin(t, "sender", "add", "mac", "--source", "github", "--account", "Felipe-CdS", "--reader")
	if err != nil || !strings.Contains(out, "thub_") {
		t.Fatalf("add = %q, %v", out, err)
	}
	if _, err := admin(t, "sender", "add", "nb", "--source", "slack"); err == nil {
		t.Fatal("unpaired --source accepted")
	}
	if _, err := admin(t, "sender", "add", "nb", "--source", "slack", "--account", "T1"); err != nil {
		t.Fatal(err)
	}
	if _, err := admin(t, "sender", "allow", "nb", "--source", "github", "--account", "acme"); err != nil {
		t.Fatal(err)
	}
	if _, err := admin(t, "sender", "allow", "ghost", "--reader"); err == nil {
		t.Fatal("allow on unknown sender succeeded")
	}
	if _, err := admin(t, "sender", "revoke", "nb"); err != nil {
		t.Fatal(err)
	}
	out, err = admin(t, "sender", "list")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"github/felipe-cds", "github/acme,slack/t1", "true"} {
		if !strings.Contains(out, want) {
			t.Fatalf("list missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "thub_") {
		t.Fatalf("list shows a token:\n%s", out)
	}

	backup := filepath.Join(dir, "backup.db")
	if _, err := admin(t, "backup", backup); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(backup); err != nil || fi.Size() == 0 {
		t.Fatalf("backup file: %v", err)
	}
	if _, err := admin(t, "backup", backup); err == nil {
		t.Fatal("backup over an existing file succeeded")
	}
	if out, err := admin(t, "prune", "--days", "30"); err != nil || !strings.Contains(out, "retention 30 days") {
		t.Fatalf("prune = %q, %v", out, err)
	}
	if _, err := admin(t, "nope"); err == nil {
		t.Fatal("unknown command succeeded")
	}
}

func TestConfigFromEnv(t *testing.T) {
	t.Setenv("THUB_DB", "")
	t.Setenv("THUB_LISTEN", "")
	t.Setenv("THUB_RETENTION_DAYS", "")
	c, err := loadConfig()
	if err != nil || c.db != "/data/thub.db" || c.listen != ":8787" || c.retentionDays != 90 {
		t.Fatalf("defaults = %+v, %v", c, err)
	}
	t.Setenv("THUB_RETENTION_DAYS", "zero")
	if _, err := loadConfig(); err == nil {
		t.Fatal("bad retention accepted")
	}
}
