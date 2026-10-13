// Package macconf loads the Mac tool's settings and secrets from a .env
// file and the environment, and keeps its small local state.
package macconf

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Felipe-CdS/things-hub/internal/plan"
)

// Config is the Mac tool's configuration.
type Config struct {
	// Home holds the .env, state.json, runs and spool.
	Home string
	// EnvFile is the .env that was read, if any.
	EnvFile     string
	ServerURL   string
	Token       string
	CFAccessID  string
	CFAccessKey string
	GitHubLogin string
	ThingsMCP   []string
	RunsDir     string
	Limits      plan.Limits
}

// DefaultHome is ~/Library/Application Support/things-hub.
func DefaultHome() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".things-hub"
	}
	return filepath.Join(home, "Library", "Application Support", "things-hub")
}

// PermissionError is a .env readable by others.
type PermissionError struct {
	Path string
	Mode os.FileMode
}

func (e *PermissionError) Error() string {
	return fmt.Sprintf("%s has mode %04o and holds secrets: run chmod 600 %q", e.Path, e.Mode.Perm(), e.Path)
}

// envFile picks the .env: THUB_ENV_FILE, else <home>/.env. The repository's
// own .env belongs to the server (make run), so thub never reads ./.env.
func envFile(home string) string {
	if p := os.Getenv("THUB_ENV_FILE"); p != "" {
		return p
	}
	return filepath.Join(home, ".env")
}

// ReadEnv parses a .env file: KEY=VALUE lines, # comments, optional export
// prefix and matching single or double quotes. It refuses a file group or
// others can read or write.
func ReadEnv(path string) (map[string]string, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return nil, &PermissionError{Path: path, Mode: fi.Mode()}
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("%s:%d: expected KEY=VALUE", path, n)
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		out[k] = v
	}
	return out, sc.Err()
}

// Load reads the .env (when it exists) and the environment, which wins.
func Load() (*Config, error) {
	home := os.Getenv("THUB_HOME")
	if home == "" {
		home = DefaultHome()
	}
	c := &Config{Home: home}
	vals := map[string]string{}
	path := envFile(home)
	file, err := ReadEnv(path)
	switch {
	case err == nil:
		c.EnvFile, vals = path, file
	case errors.Is(err, os.ErrNotExist) && os.Getenv("THUB_ENV_FILE") == "":
	default:
		return nil, err
	}
	get := func(k string) string {
		if v, ok := os.LookupEnv(k); ok {
			return v
		}
		return vals[k]
	}
	c.ServerURL = strings.TrimRight(get("THUB_URL"), "/")
	if c.ServerURL == "" {
		c.ServerURL = "http://127.0.0.1:8787"
	}
	c.Token = get("THUB_TOKEN")
	c.CFAccessID, c.CFAccessKey = get("CF_ACCESS_CLIENT_ID"), get("CF_ACCESS_CLIENT_SECRET")
	c.GitHubLogin = strings.ToLower(get("THUB_GITHUB_LOGIN"))
	c.ThingsMCP = strings.Fields(get("THUB_THINGS_MCP"))
	if len(c.ThingsMCP) == 0 {
		c.ThingsMCP = []string{"uvx", "things-mcp"}
	}
	c.RunsDir = get("THUB_RUNS_DIR")
	if c.RunsDir == "" {
		c.RunsDir = filepath.Join(home, "runs")
	}
	for k, dst := range map[string]*int{
		"THUB_LIMIT_NEW_TODOS": &c.Limits.NewTodos, "THUB_LIMIT_DELETES": &c.Limits.Deletes,
		"THUB_LIMIT_MOVES": &c.Limits.Moves, "THUB_LIMIT_AREA_CHANGES": &c.Limits.AreaChanges,
	} {
		v := get(k)
		if v == "" {
			continue
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return nil, fmt.Errorf("%s must be a positive number, got %q", k, v)
		}
		*dst = n
	}
	c.Limits = c.Limits.WithDefaults()
	return c, nil
}

// State is state.json: what thub remembers between runs.
type State struct {
	// LayoutApproved is set once the owner said yes to the first run that
	// reorganizes existing items; later runs apply directly.
	LayoutApproved   bool      `json:"layout_approved"`
	LayoutApprovedAt time.Time `json:"layout_approved_at,omitzero"`
	LayoutRun        string    `json:"layout_run,omitempty"`
}

func (c *Config) statePath() string { return filepath.Join(c.Home, "state.json") }

// LoadState reads state.json; a missing file is the zero state.
func (c *Config) LoadState() (State, error) {
	var s State
	raw, err := os.ReadFile(c.statePath())
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	return s, json.Unmarshal(raw, &s)
}

// SaveState writes state.json.
func (c *Config) SaveState(s State) error {
	if err := os.MkdirAll(c.Home, 0o700); err != nil {
		return err
	}
	return plan.WriteJSON(c.statePath(), s)
}

// SpoolDir holds batches that could not be posted.
func (c *Config) SpoolDir() string { return filepath.Join(c.Home, "spool") }
