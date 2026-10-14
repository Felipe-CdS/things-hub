// Command thub-classroom is the Classroom routine's helper on the Mac: it
// does the one-time Google login and the read-only Classroom API calls, and
// prints JSON. It never writes to Classroom and never talks to the inbox;
// the things-hub-classroom skill turns its output into events and posts them
// with thub-send.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"time"

	"github.com/Felipe-CdS/things-hub/api"
	"github.com/Felipe-CdS/things-hub/internal/classroom"
)

const usage = `usage: thub-classroom <command>

commands:
  login [--announcements] [--no-browser]
                         log in to Google once in the browser and store the
                         refresh token in the env file
  assignments            every published coursework of the active courses,
                         joined with your submission (what the skill reads)

settings (environment, or the env file ${THUB_ENV_FILE:-.env}, mode 600):
  THUB_GOOGLE_CLIENT_ID         the Desktop app OAuth client id
  THUB_GOOGLE_CLIENT_SECRET     its client secret
  THUB_CLASSROOM_REFRESH_TOKEN  written by login
  THUB_CLASSROOM_ACCOUNT        the event account alias, such as university
`

type app struct {
	env         map[string]string
	envFile     string
	cfg         classroom.Config
	out         io.Writer
	now         func() time.Time
	openBrowser func(string) error
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	a, err := newApp(os.Stdout)
	if err == nil {
		err = a.run(ctx, os.Args[1:])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "thub-classroom:", err)
		os.Exit(1)
	}
}

// newApp reads the env file; variables already in the environment win.
func newApp(out io.Writer) (*app, error) {
	path := os.Getenv("THUB_ENV_FILE")
	if path == "" {
		path = ".env"
	}
	env, err := classroom.LoadEnvFile(path)
	if err != nil {
		return nil, err
	}
	for _, k := range []string{"THUB_GOOGLE_CLIENT_ID", "THUB_GOOGLE_CLIENT_SECRET",
		"THUB_CLASSROOM_REFRESH_TOKEN", "THUB_CLASSROOM_ACCOUNT"} {
		if v := os.Getenv(k); v != "" {
			env[k] = v
		}
	}
	return &app{env: env, envFile: path, out: out, now: time.Now, openBrowser: openBrowser}, nil
}

func openBrowser(url string) error { return exec.Command("open", url).Run() }

func (a *app) run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		fmt.Fprint(a.out, usage)
		return errors.New("a command is required")
	}
	cmd, args := args[0], args[1:]
	a.cfg.ClientID, a.cfg.ClientSecret = a.env["THUB_GOOGLE_CLIENT_ID"], a.env["THUB_GOOGLE_CLIENT_SECRET"]
	switch cmd {
	case "login":
		return a.login(ctx, args)
	case "assignments":
		if len(args) != 0 {
			return errors.New("assignments takes no arguments")
		}
		account := strings.ToLower(a.env["THUB_CLASSROOM_ACCOUNT"])
		if !api.ValidAccountPart(account) {
			return errors.New("THUB_CLASSROOM_ACCOUNT must be set to an alias without ':' or spaces, such as university")
		}
		c, err := a.client(ctx)
		if err != nil {
			return err
		}
		res, err := c.FetchAssignments(ctx, account, a.now())
		if err != nil {
			return err
		}
		return a.print(res)
	case "help", "-h", "--help":
		fmt.Fprint(a.out, usage)
		return nil
	}
	return fmt.Errorf("unknown command %q\n\n%s", cmd, usage)
}

func (a *app) client(ctx context.Context) (*classroom.Client, error) {
	if a.cfg.ClientID == "" || a.cfg.ClientSecret == "" {
		return nil, errors.New("THUB_GOOGLE_CLIENT_ID and THUB_GOOGLE_CLIENT_SECRET must be set")
	}
	rt := a.env["THUB_CLASSROOM_REFRESH_TOKEN"]
	if rt == "" {
		return nil, errors.New("THUB_CLASSROOM_REFRESH_TOKEN is not set; run `thub-classroom login` first")
	}
	return classroom.NewClient(ctx, a.cfg, rt), nil
}

func (a *app) print(v any) error {
	enc := json.NewEncoder(a.out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func (a *app) login(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	ann := fs.Bool("announcements", false, "also ask for read-only access to announcements")
	noBrowser := fs.Bool("no-browser", false, "only print the login page's URL")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("login takes no arguments")
	}
	opts := classroom.LoginOptions{Announcements: *ann, Out: a.out, OpenBrowser: a.openBrowser}
	if *noBrowser {
		opts.OpenBrowser = nil
	}
	tok, err := classroom.Login(ctx, a.cfg, opts)
	if err != nil {
		return err
	}
	if err := classroom.SetEnvFileKey(a.envFile, "THUB_CLASSROOM_REFRESH_TOKEN", tok.RefreshToken); err != nil {
		return fmt.Errorf("store the refresh token in %s: %w", a.envFile, err)
	}
	fmt.Fprintf(a.out, "Logged in. The refresh token is stored in %s (mode 600).\n", a.envFile)
	courses, err := classroom.ClientFromToken(ctx, a.cfg, tok).Courses(ctx)
	if err != nil {
		return fmt.Errorf("logged in, but Classroom refused the first read: %w\n"+
			"If it says access is blocked or disabled by an administrator, the school does not allow this app; see docs/classroom-setup.md", err)
	}
	fmt.Fprintf(a.out, "Classroom answered: %d active course(s).\n", len(courses))
	return nil
}
