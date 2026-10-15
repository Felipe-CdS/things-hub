// Command thub is the Mac side of things-hub: it posts collector events to
// the inbox, pulls queued items with a Things snapshot, checks and applies
// the agent's plan to Things 3, and undoes a run.
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
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/Felipe-CdS/things-hub/api"
	"github.com/Felipe-CdS/things-hub/internal/events"
	"github.com/Felipe-CdS/things-hub/internal/hub"
	"github.com/Felipe-CdS/things-hub/internal/macconf"
	"github.com/Felipe-CdS/things-hub/internal/plan"
	"github.com/Felipe-CdS/things-hub/internal/runner"
	"github.com/Felipe-CdS/things-hub/internal/things"
)

const usage = `usage: thub <command>

run steps (see RUN.md):
  doctor                         check the .env, inbox, gh login, things-mcp and the Things URL token
  pull [--out DIR] [--max N]     snapshot Things, lease pending items, write DIR/input.json
  check <plan.json>              validate a plan against its input.json and the per-run limits
  apply [--dry-run] <plan.json>  apply a plan to Things (--dry-run prints the diff only)
        [--allow-over-limits] [--approve-layout]
  undo <run-id>                  write back the inverse of a run's changes, newest first
  status [run-dir]               queue status, and a run's apply result

collector helpers (used by skills):
  cursor <source> [account]      print the stored cursor
  open <source> [account]        print the items still open, for resolve events
  check-batch <batch.json>       validate an event batch without sending it
  post <batch.json>              validate and send an event batch (kept in the spool if unreachable)
  flush                          resend spooled batches

settings come from a mode-600 .env: THUB_ENV_FILE, else
~/Library/Application Support/things-hub/.env (see .env.example).
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:], os.Stdout)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "thub:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 {
		fmt.Fprint(out, usage)
		return errors.New("a command is required")
	}
	cmd, args := args[0], args[1:]
	if cmd == "help" || cmd == "-h" || cmd == "--help" {
		fmt.Fprint(out, usage)
		return nil
	}
	if cmd == "check" {
		return checkCmd(args, out)
	}
	cfg, err := macconf.Load()
	if err != nil {
		return err
	}
	switch cmd {
	case "doctor":
		return doctor(ctx, cfg, out)
	case "pull":
		return pullCmd(ctx, cfg, args, out)
	case "apply":
		return applyCmd(ctx, cfg, args, out)
	case "undo":
		return undoCmd(ctx, cfg, args, out)
	case "status":
		return statusCmd(ctx, cfg, args, out)
	case "cursor", "open":
		return sourceCmd(ctx, cfg, cmd, args, out)
	case "check-batch":
		return checkBatchCmd(cfg, args, out)
	case "post":
		return postCmd(ctx, cfg, args, out)
	case "flush":
		return flushCmd(ctx, cfg, out)
	}
	return fmt.Errorf("unknown command %q\n\n%s", cmd, usage)
}

// parseInterleaved parses flags that may come before or after positionals.
func parseInterleaved(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		pos, args = append(pos, args[0]), args[1:]
	}
}

func hubClient(cfg *macconf.Config) (*hub.Client, error) {
	if cfg.Token == "" {
		return nil, fmt.Errorf("THUB_TOKEN is not set: add it to %s (see .env.example)", envName(cfg))
	}
	c := hub.New(cfg.ServerURL, cfg.Token)
	c.CFAccessID, c.CFAccessSecret = cfg.CFAccessID, cfg.CFAccessKey
	return c, nil
}

func envName(cfg *macconf.Config) string {
	if cfg.EnvFile != "" {
		return cfg.EnvFile
	}
	return filepath.Join(cfg.Home, ".env")
}

func openThings(ctx context.Context, cfg *macconf.Config) (*things.Live, error) {
	path, err := things.FindDB()
	if err != nil {
		return nil, err
	}
	db, err := things.OpenDB(path)
	if err != nil {
		return nil, err
	}
	m, err := things.DialMCP(ctx, cfg.ThingsMCP)
	if err != nil {
		db.Close()
		return nil, err
	}
	return &things.Live{MCP: m, DB: db, Exec: things.RunCommand}, nil
}

func accounts(cfg *macconf.Config) events.Accounts {
	return events.Accounts{api.SourceGitHub: cfg.GitHubLogin}
}

// doctor checks everything a run needs and never prints a secret.
func doctor(ctx context.Context, cfg *macconf.Config, out io.Writer) error {
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	failed := 0
	report := func(name string, err error, detail string) {
		if err != nil {
			failed++
			fmt.Fprintf(w, "FAIL\t%s\t%v\n", name, err)
			return
		}
		fmt.Fprintf(w, "ok\t%s\t%s\n", name, detail)
	}

	if cfg.EnvFile == "" {
		report(".env", fmt.Errorf("no .env found; create %s from .env.example with mode 600", envName(cfg)), "")
	} else {
		report(".env", nil, cfg.EnvFile+" (mode 600)")
	}
	if cfg.Token == "" {
		report("inbox token", errors.New("THUB_TOKEN is empty"), "")
	} else {
		report("inbox token", nil, "THUB_TOKEN is set")
	}

	hc, err := hubClient(cfg)
	if err == nil {
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err = hc.Health(cctx)
		cancel()
		if err != nil {
			err = fmt.Errorf("%s: %w (phase 1: is the server running, or the SSH forward up?)", cfg.ServerURL, err)
		}
		report("inbox reachable", err, cfg.ServerURL)
		if err == nil {
			cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			st, err := hc.Status(cctx)
			cancel()
			report("inbox reader access", err, fmt.Sprintf("%d pending, %d leased, %d parked", st.Queue.Pending, st.Queue.Leased, st.Queue.Parked))
		}
	}

	login, err := exec.CommandContext(ctx, "gh", "api", "user", "--jq", ".login").Output()
	switch {
	case err != nil:
		report("gh login", fmt.Errorf("gh api user: %w (run gh auth login)", err), "")
	case cfg.GitHubLogin == "":
		report("gh login", errors.New("THUB_GITHUB_LOGIN is empty in the .env"), "")
	case !strings.EqualFold(strings.TrimSpace(string(login)), cfg.GitHubLogin):
		report("gh login", fmt.Errorf("gh is logged in as %s, the .env says %s", strings.TrimSpace(string(login)), cfg.GitHubLogin), "")
	default:
		report("gh login", nil, cfg.GitHubLogin)
	}

	cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	m, err := things.DialMCP(cctx, cfg.ThingsMCP)
	if err == nil {
		names, _ := m.ToolNames(cctx)
		m.Close()
		report("things-mcp", nil, fmt.Sprintf("%s: %d tools, all %d required present", strings.Join(cfg.ThingsMCP, " "), len(names), len(things.RequiredTools)))
	} else {
		report("things-mcp", err, "")
	}
	cancel()

	path, err := things.FindDB()
	if err == nil {
		var db *things.DB
		if db, err = things.OpenDB(path); err == nil {
			var tok string
			tok, err = db.AuthToken(ctx)
			db.Close()
			if err == nil && tok == "" {
				err = errors.New("empty: turn on Things → Settings → General → Enable Things URLs (restart Things if it stays empty)")
			}
		}
	}
	report("Things URL auth token", err, "present (value not shown)")

	_, err = exec.LookPath("osascript")
	report("osascript", err, "for tags, deletes and project moves")
	w.Flush()
	if failed > 0 {
		return fmt.Errorf("%d check(s) failed", failed)
	}
	return nil
}

func pullCmd(ctx context.Context, cfg *macconf.Config, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("pull", flag.ContinueOnError)
	dir := fs.String("out", "", "run folder (default <runs dir>/<run id>)")
	max := fs.Int("max", 0, "lease at most this many items (default 50)")
	lease := fs.Int("lease-seconds", 0, "lease length (default 2 hours)")
	if _, err := parseInterleaved(fs, args); err != nil {
		return err
	}
	hc, err := hubClient(cfg)
	if err != nil {
		return err
	}
	runID := runner.NewRunID(time.Now())
	if *dir == "" {
		*dir = runner.RunDir(cfg.RunsDir, runID)
	} else {
		runID = filepath.Base(filepath.Clean(*dir))
	}
	state, err := cfg.LoadState()
	if err != nil {
		return err
	}
	tc, err := openThings(ctx, cfg)
	if err != nil {
		return err
	}
	defer tc.Close()
	r := runner.New(hc, tc, out)
	in, err := r.Pull(ctx, runner.PullOptions{Dir: *dir, RunID: runID, Max: *max, LeaseSeconds: *lease,
		Limits: cfg.Limits, LayoutApproved: state.LayoutApproved})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "run %s\nfolder %s\n", in.RunID, *dir)
	if len(in.Items) == 0 {
		fmt.Fprintln(out, "nothing new: no pending items (an organizing-only plan may still be written)")
		return nil
	}
	fmt.Fprintf(out, "leased %d item(s) until %s (lease %s)\n", len(in.Items), in.ExpiresAt.Local().Format(time.DateTime), in.LeaseID)
	for _, it := range in.Items {
		title := ""
		if it.Event != nil {
			title = it.Event.Title
		}
		fmt.Fprintf(out, "  %-7s %s  %s\n", it.Op, it.Key, title)
	}
	return nil
}

func checkCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	input := fs.String("input", "", "input.json (default: next to the plan)")
	allowOver := fs.Bool("allow-over-limits", false, "accept a plan above the per-run limits")
	pos, err := parseInterleaved(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("usage: thub check <plan.json>")
	}
	in, p, err := readRun(pos[0], *input)
	if err != nil {
		return err
	}
	res := plan.Check(in, p)
	printCheck(out, in, res)
	if !res.OK(*allowOver) {
		return errors.New("plan refused")
	}
	fmt.Fprintln(out, "plan ok")
	return nil
}

func readRun(planPath, inputPath string) (*plan.Input, *plan.Plan, error) {
	if inputPath == "" {
		inputPath = filepath.Join(filepath.Dir(planPath), "input.json")
	}
	var in plan.Input
	if err := plan.ReadJSON(inputPath, &in); err != nil {
		return nil, nil, err
	}
	var p plan.Plan
	if err := plan.ReadJSON(planPath, &p); err != nil {
		return nil, nil, err
	}
	return &in, &p, nil
}

func printCheck(out io.Writer, in *plan.Input, res *plan.Result) {
	for _, e := range res.Errors {
		fmt.Fprintln(out, "error:", e)
	}
	for _, o := range res.OverLimits {
		fmt.Fprintln(out, "over limit:", o)
	}
	for _, w := range res.Warnings {
		fmt.Fprintln(out, "warning:", w)
	}
	c := res.Counts
	fmt.Fprintf(out, "counts: %d new to-dos, %d new projects, %d deletes, %d moves/changes of existing items, "+
		"%d area changes, %d completions, %d ignored, %d deferred\n",
		c.NewTodos, c.NewProjects, c.Deletes, c.Moves, c.AreaChanges, c.Completes, c.Ignored, c.Deferred)
	if res.ReorganizesLayout && !in.LayoutApproved {
		fmt.Fprintln(out, "layout: this plan reorganizes existing items; the first such run is preview-only until the owner says yes")
	}
}

// printDiff writes the dry-run diff.
func printDiff(out io.Writer, in *plan.Input, p *plan.Plan, res *plan.Result) {
	fmt.Fprintf(out, "dry run %s: %d Things change(s)\n", p.RunID, len(res.Ops))
	for _, op := range res.Ops {
		fmt.Fprintln(out, op.Describe)
	}
	for _, d := range p.Items {
		switch d.Action {
		case plan.ActIgnore:
			fmt.Fprintf(out, "· ignore %s: %s\n", d.Key, d.Reason)
		case plan.ActDefer:
			fmt.Fprintf(out, "… defer %s: %s\n", d.Key, d.Reason)
		}
	}
	for _, k := range res.Unplanned {
		fmt.Fprintf(out, "↩ release %s (no decision)\n", k)
	}
	printCheck(out, in, res)
}

func applyCmd(ctx context.Context, cfg *macconf.Config, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("apply", flag.ContinueOnError)
	dry := fs.Bool("dry-run", false, "print the diff of every change and write nothing")
	input := fs.String("input", "", "input.json (default: next to the plan)")
	allowOver := fs.Bool("allow-over-limits", false, "apply a plan above the per-run limits (after a one-time yes)")
	approve := fs.Bool("approve-layout", false, "the owner approved this first layout-reorganizing run")
	pos, err := parseInterleaved(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("usage: thub apply [--dry-run] <plan.json>")
	}
	in, p, err := readRun(pos[0], *input)
	if err != nil {
		return err
	}
	state, err := cfg.LoadState()
	if err != nil {
		return err
	}
	in.LayoutApproved = in.LayoutApproved || state.LayoutApproved
	dir := filepath.Dir(pos[0])

	if *dry {
		res := plan.Check(in, p)
		var sb strings.Builder
		printDiff(&sb, in, p, res)
		fmt.Fprint(out, sb.String())
		if err := os.WriteFile(filepath.Join(dir, "dry-run.txt"), []byte(sb.String()), 0o600); err != nil {
			return err
		}
		if !res.OK(*allowOver) {
			return errors.New("plan refused")
		}
		return nil
	}

	hc, err := hubClient(cfg)
	if err != nil {
		return err
	}
	tc, err := openThings(ctx, cfg)
	if err != nil {
		return err
	}
	defer tc.Close()
	r := runner.New(hc, tc, out)
	rep, err := r.Apply(ctx, in, p, runner.ApplyOptions{Dir: dir, AllowOverLimits: *allowOver, ApproveLayout: *approve})
	if err != nil {
		var ce *runner.CheckError
		if errors.As(err, &ce) {
			printCheck(out, in, ce.Result)
		}
		return err
	}
	if *approve && !state.LayoutApproved {
		state.LayoutApproved, state.LayoutApprovedAt, state.LayoutRun = true, time.Now().UTC(), p.RunID
		if err := cfg.SaveState(state); err != nil {
			return err
		}
	}
	printReport(out, rep)
	return nil
}

func printReport(out io.Writer, rep *runner.Report) {
	t := rep.Totals()
	fmt.Fprintf(out, "run %s: %d applied, %d already done, %d replayed, %d skipped (changed in Things), %d failed; "+
		"%d acked, %d released\n", rep.RunID, t[runner.StatusApplied], t[runner.StatusAlready], t[runner.StatusReplayed],
		t[runner.StatusSkipped], t[runner.StatusFailed], len(rep.Acks), rep.Released)
	for _, a := range rep.Acks {
		if a.Result == api.AckRejected {
			fmt.Fprintf(out, "ack rejected: %s: %s\n", a.Key, a.Reason)
		}
	}
}

func undoCmd(ctx context.Context, cfg *macconf.Config, args []string, out io.Writer) error {
	if len(args) != 1 {
		return errors.New("usage: thub undo <run-id>")
	}
	hc, err := hubClient(cfg)
	if err != nil {
		return err
	}
	tc, err := openThings(ctx, cfg)
	if err != nil {
		return err
	}
	defer tc.Close()
	steps, err := runner.New(hc, tc, out).Undo(ctx, args[0])
	if err != nil {
		return err
	}
	failed := 0
	for _, s := range steps {
		if s.Status == runner.StatusFailed {
			failed++
		}
	}
	fmt.Fprintf(out, "undo of %s: %d step(s), %d failed (recorded as run %s)\n", args[0], len(steps), failed, runner.UndoRunID(args[0]))
	if failed > 0 {
		return fmt.Errorf("%d undo step(s) failed; re-run thub undo %s after fixing them", failed, args[0])
	}
	return nil
}

func statusCmd(ctx context.Context, cfg *macconf.Config, args []string, out io.Writer) error {
	if len(args) > 1 {
		return errors.New("usage: thub status [run-dir]")
	}
	hc, err := hubClient(cfg)
	if err != nil {
		return err
	}
	st, err := hc.Status(ctx)
	if err != nil {
		return err
	}
	q := st.Queue
	fmt.Fprintf(out, "queue: %d pending (%d deferred), %d leased, %d done, %d parked\n", q.Pending, q.Deferred, q.Leased, q.Done, q.Parked)
	if st.OldestPending != nil {
		fmt.Fprintf(out, "oldest pending: %s since %s\n", st.OldestPending.Key, st.OldestPending.QueuedAt.Local().Format(time.DateTime))
	}
	for _, p := range st.Parked {
		fmt.Fprintf(out, "parked: %s after %d attempts: %s\n", p.Key, p.Attempts, p.LastNote)
	}
	for _, l := range st.Leases {
		fmt.Fprintf(out, "lease %s: %d item(s), expires %s\n", l.ID, l.Items, l.ExpiresAt.Local().Format(time.DateTime))
	}
	for _, s := range st.Senders {
		fmt.Fprintf(out, "sender %s %s/%s: last event %s\n", s.Sender, s.Source, s.Account, s.LastEventAt.Local().Format(time.DateTime))
	}
	if len(args) == 1 {
		var rep runner.Report
		if err := plan.ReadJSON(filepath.Join(args[0], "apply-result.json"), &rep); err != nil {
			return err
		}
		printReport(out, &rep)
		created, changed, completed, deleted := 0, 0, 0, 0
		for _, o := range rep.Ops {
			if o.Status != runner.StatusApplied {
				continue
			}
			switch o.Kind {
			case plan.KindCreateTodo, plan.KindCreateProject, plan.KindCreateArea, plan.KindCreateTag:
				created++
			case plan.KindComplete, plan.KindCancel:
				completed++
			case plan.KindDelete, plan.KindDeleteArea:
				deleted++
			default:
				changed++
			}
		}
		fmt.Fprintf(out, "Things: %d created, %d changed, %d completed or canceled, %d deleted; %d ignored, %d deferred\n",
			created, changed, completed, deleted, rep.Counts.Ignored, rep.Counts.Deferred)
	}
	return nil
}

func sourceCmd(ctx context.Context, cfg *macconf.Config, cmd string, args []string, out io.Writer) error {
	if len(args) < 1 || len(args) > 2 {
		return fmt.Errorf("usage: thub %s <source> [account]", cmd)
	}
	source := args[0]
	account := accounts(cfg)[source]
	if len(args) == 2 {
		account = args[1]
	}
	if account == "" {
		return fmt.Errorf("no account configured for %s; pass it", source)
	}
	hc, err := hubClient(cfg)
	if err != nil {
		return err
	}
	var v any
	if cmd == "open" {
		v, err = hc.Open(ctx, source, account)
	} else {
		v, err = hc.Cursor(ctx, source, account)
	}
	if err != nil {
		return err
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func checkBatchCmd(cfg *macconf.Config, args []string, out io.Writer) error {
	if len(args) != 1 {
		return errors.New("usage: thub check-batch <batch.json>")
	}
	b, raw, err := events.Read(args[0])
	if err != nil {
		return err
	}
	if errs := events.Check(b, len(raw), accounts(cfg)); len(errs) > 0 {
		for _, e := range errs {
			fmt.Fprintln(out, "error:", e)
		}
		return errors.New("batch refused")
	}
	fmt.Fprintf(out, "batch ok: %d event(s)\n", len(b.Events))
	return nil
}

func postCmd(ctx context.Context, cfg *macconf.Config, args []string, out io.Writer) error {
	if err := checkBatchCmd(cfg, args, out); err != nil {
		return err
	}
	b, raw, err := events.Read(args[0])
	if err != nil {
		return err
	}
	hc, err := hubClient(cfg)
	if err != nil {
		return err
	}
	p := &events.Poster{Hub: hc, Spool: cfg.SpoolDir(), Now: time.Now}
	resp, err := p.Post(ctx, b, raw)
	if err != nil {
		return err
	}
	counts := map[string]int{}
	for _, r := range resp.Results {
		counts[r.Result]++
		if r.Result == api.ResultRejected {
			fmt.Fprintf(out, "rejected events[%d] %s: %s\n", r.Index, r.Key, r.Reason)
		}
	}
	fmt.Fprintf(out, "posted %d event(s): %d accepted, %d duplicate, %d stale, %d rejected; cursor stored: %v\n",
		len(resp.Results), counts[api.ResultAccepted], counts[api.ResultDuplicate], counts[api.ResultStale],
		counts[api.ResultRejected], resp.CursorStored)
	if counts[api.ResultRejected] > 0 {
		return errors.New("some events were rejected")
	}
	return nil
}

func flushCmd(ctx context.Context, cfg *macconf.Config, out io.Writer) error {
	hc, err := hubClient(cfg)
	if err != nil {
		return err
	}
	p := &events.Poster{Hub: hc, Spool: cfg.SpoolDir(), Now: time.Now}
	n, err := p.Flush(ctx)
	fmt.Fprintf(out, "flushed %d spooled batch(es)\n", n)
	return err
}
