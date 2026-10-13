package macconf

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadReadsEnvAndRefusesLoosePermissions(t *testing.T) {
	dir := t.TempDir()
	env := filepath.Join(dir, ".env")
	body := "# things-hub\nTHUB_URL=http://127.0.0.1:9999/\nexport THUB_TOKEN=\"thub_secret\"\nTHUB_GITHUB_LOGIN=Felipe-CdS\nTHUB_LIMIT_NEW_TODOS=7\n"
	if err := os.WriteFile(env, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("THUB_HOME", dir)
	t.Setenv("THUB_ENV_FILE", env)
	for _, k := range []string{"THUB_URL", "THUB_TOKEN", "THUB_GITHUB_LOGIN", "THUB_LIMIT_NEW_TODOS"} {
		os.Unsetenv(k)
	}
	var pe *PermissionError
	if _, err := Load(); !errors.As(err, &pe) {
		t.Fatalf("a mode-644 .env was accepted: %v", err)
	}
	if err := os.Chmod(env, 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.ServerURL != "http://127.0.0.1:9999" || c.Token != "thub_secret" || c.GitHubLogin != "felipe-cds" ||
		c.Limits.NewTodos != 7 || c.Limits.Deletes != 10 || c.RunsDir != filepath.Join(dir, "runs") {
		t.Fatalf("config %+v", c)
	}
	t.Setenv("THUB_TOKEN", "thub_from_env")
	if c, _ := Load(); c.Token != "thub_from_env" {
		t.Fatal("the environment did not override the .env")
	}
}

func TestStateRoundTrip(t *testing.T) {
	c := &Config{Home: t.TempDir()}
	s, err := c.LoadState()
	if err != nil || s.LayoutApproved {
		t.Fatalf("zero state: %+v %v", s, err)
	}
	if err := c.SaveState(State{LayoutApproved: true, LayoutRun: "r1"}); err != nil {
		t.Fatal(err)
	}
	if s, _ := c.LoadState(); !s.LayoutApproved || s.LayoutRun != "r1" {
		t.Fatalf("state %+v", s)
	}
}
