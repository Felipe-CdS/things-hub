// Command thub-server is the things-hub inbox: it accepts collector events,
// queues them for the Mac, and stores the Things ledger, audit log and
// snapshots. Administration runs as subcommands inside the container.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/Felipe-CdS/things-hub/internal/server"
	"github.com/Felipe-CdS/things-hub/internal/store"
)

const usage = `usage: thub-server [command]

commands:
  serve                                    run the HTTP API (default)
  sender add <name> [--source S --account A]... [--reader]
                                           create a sender and print its token once
  sender list                              list senders and their scopes
  sender allow <name> [--source S --account A]... [--reader]
                                           add write scopes or the reader scope
  sender revoke <name>                     invalidate a sender's token
  backup <path>                            write a consistent copy of the database
  prune [--days N]                         drop payloads older than the retention
  healthcheck                              exit 0 when /healthz answers

environment:
  THUB_DB              database path (default /data/thub.db)
  THUB_LISTEN          listen address (default :8787)
  THUB_RETENTION_DAYS  payload retention in days (default 90)
`

type config struct {
	db            string
	listen        string
	retentionDays int
}

func loadConfig() (config, error) {
	c := config{db: "/data/thub.db", listen: ":8787", retentionDays: 90}
	if v := os.Getenv("THUB_DB"); v != "" {
		c.db = v
	}
	if v := os.Getenv("THUB_LISTEN"); v != "" {
		c.listen = v
	}
	if v := os.Getenv("THUB_RETENTION_DAYS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return c, fmt.Errorf("THUB_RETENTION_DAYS must be a positive number of days, got %q", v)
		}
		c.retentionDays = n
	}
	return c, nil
}

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "thub-server:", err)
		os.Exit(1)
	}
}

func run(args []string, out io.Writer) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	cmd := "serve"
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "serve":
		return serve(cfg)
	case "sender":
		return senderCmd(cfg, args, out)
	case "backup":
		return backup(cfg, args, out)
	case "prune":
		return prune(cfg, args, out)
	case "healthcheck":
		return healthcheck(cfg)
	case "help", "-h", "--help":
		fmt.Fprint(out, usage)
		return nil
	}
	return fmt.Errorf("unknown command %q\n\n%s", cmd, usage)
}

func serve(cfg config) error {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	st, err := store.Open(cfg.db)
	if err != nil {
		return err
	}
	defer st.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go pruneLoop(ctx, st, cfg.retentionDays, log)

	srv := &http.Server{
		Addr:              cfg.listen,
		Handler:           server.New(st, log),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       2 * time.Minute,
		WriteTimeout:      2 * time.Minute,
		IdleTimeout:       2 * time.Minute,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Info("listening", "addr", cfg.listen, "db", cfg.db, "retention_days", cfg.retentionDays)
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

// pruneLoop prunes at start and then daily.
func pruneLoop(ctx context.Context, st *store.Store, days int, log *slog.Logger) {
	t := time.NewTicker(24 * time.Hour)
	defer t.Stop()
	for {
		r, err := st.Prune(ctx, days)
		if err != nil {
			log.Error("prune", "err", err)
		} else {
			log.Info("prune", "event_payloads", r.EventPayloads, "item_payloads", r.ItemPayloads,
				"leases", r.Leases, "snapshots", r.Snapshots)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// scopeFlags collects repeated --source and --account pairs.
type scopeFlags struct{ sources, accounts []string }

func (s *scopeFlags) register(fs *flag.FlagSet) {
	fs.Func("source", "source the sender may write (repeatable, paired with --account)", func(v string) error {
		s.sources = append(s.sources, v)
		return nil
	})
	fs.Func("account", "account the sender may write (repeatable, paired with --source)", func(v string) error {
		s.accounts = append(s.accounts, v)
		return nil
	})
}

func (s *scopeFlags) scopes() ([]store.Scope, error) {
	if len(s.sources) != len(s.accounts) {
		return nil, errors.New("each --source needs a matching --account")
	}
	out := make([]store.Scope, len(s.sources))
	for i := range s.sources {
		out[i] = store.Scope{Source: s.sources[i], Account: s.accounts[i]}
	}
	return out, nil
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

func openStore(cfg config) (*store.Store, error) {
	if _, err := os.Stat(cfg.db); err != nil {
		return nil, fmt.Errorf("database %s: %w", cfg.db, err)
	}
	return store.Open(cfg.db)
}

func senderCmd(cfg config, args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("sender needs a subcommand: add, list, allow or revoke")
	}
	sub, args := args[0], args[1:]
	fs := flag.NewFlagSet("sender "+sub, flag.ContinueOnError)
	var sf scopeFlags
	var reader bool
	if sub == "add" || sub == "allow" {
		sf.register(fs)
		fs.BoolVar(&reader, "reader", false, "may pull, ack and use the ledger, audit, snapshots and status")
	}
	pos, err := parseInterleaved(fs, args)
	if err != nil {
		return err
	}
	scopes, err := sf.scopes()
	if err != nil {
		return err
	}
	ctx := context.Background()
	// add may create the database; the others need an existing one.
	open := func() (*store.Store, error) {
		if sub == "add" {
			return store.Open(cfg.db)
		}
		return openStore(cfg)
	}
	needName := func() (string, error) {
		if len(pos) != 1 {
			return "", fmt.Errorf("sender %s needs exactly one name", sub)
		}
		return pos[0], nil
	}

	switch sub {
	case "add":
		name, err := needName()
		if err != nil {
			return err
		}
		st, err := open()
		if err != nil {
			return err
		}
		defer st.Close()
		token, err := st.AddSender(ctx, name, reader, scopes)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "sender %s created. Its token is shown only once:\n%s\n", name, token)
		return nil
	case "allow":
		name, err := needName()
		if err != nil {
			return err
		}
		if len(scopes) == 0 && !reader {
			return errors.New("sender allow needs --source and --account, or --reader")
		}
		st, err := open()
		if err != nil {
			return err
		}
		defer st.Close()
		if err := st.AllowSender(ctx, name, reader, scopes); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("sender %s does not exist", name)
			}
			return err
		}
		fmt.Fprintf(out, "sender %s updated\n", name)
		return nil
	case "revoke":
		name, err := needName()
		if err != nil {
			return err
		}
		st, err := open()
		if err != nil {
			return err
		}
		defer st.Close()
		if err := st.RevokeSender(ctx, name); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("sender %s does not exist", name)
			}
			return err
		}
		fmt.Fprintf(out, "sender %s revoked\n", name)
		return nil
	case "list":
		if len(pos) != 0 {
			return errors.New("sender list takes no arguments")
		}
		st, err := open()
		if err != nil {
			return err
		}
		defer st.Close()
		senders, err := st.ListSenders(ctx)
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tREADER\tWRITES\tCREATED\tLAST SEEN\tREVOKED")
		for _, s := range senders {
			var writes []string
			for _, sc := range s.Scopes {
				writes = append(writes, sc.Source+"/"+sc.Account)
			}
			fmt.Fprintf(tw, "%s\t%t\t%s\t%s\t%s\t%s\n", s.Name, s.Reader, orDash(strings.Join(writes, ",")),
				stamp(s.CreatedAt), stamp(s.LastSeenAt), stamp(s.RevokedAt))
		}
		return tw.Flush()
	}
	return fmt.Errorf("unknown sender subcommand %q", sub)
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func stamp(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format(time.RFC3339)
}

func backup(cfg config, args []string, out io.Writer) error {
	if len(args) != 1 {
		return errors.New("backup needs exactly one destination path")
	}
	if _, err := os.Stat(args[0]); err == nil {
		return fmt.Errorf("%s already exists", args[0])
	}
	st, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.Backup(context.Background(), args[0]); err != nil {
		return err
	}
	fmt.Fprintf(out, "backup written to %s\n", args[0])
	return nil
}

func prune(cfg config, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("prune", flag.ContinueOnError)
	days := fs.Int("days", cfg.retentionDays, "retention in days")
	if pos, err := parseInterleaved(fs, args); err != nil {
		return err
	} else if len(pos) != 0 {
		return errors.New("prune takes no arguments")
	}
	st, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer st.Close()
	r, err := st.Prune(context.Background(), *days)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "pruned %d event payloads, %d item payloads, %d leases, %d snapshots (retention %d days)\n",
		r.EventPayloads, r.ItemPayloads, r.Leases, r.Snapshots, *days)
	return nil
}

func healthcheck(cfg config) error {
	host, port, err := net.SplitHostPort(cfg.listen)
	if err != nil {
		return err
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("http://" + net.JoinHostPort(host, port) + "/healthz")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz returned %s", resp.Status)
	}
	return nil
}
