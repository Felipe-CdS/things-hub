// Package events checks and posts the event batches the Mac's collector
// skills write, keeping a batch in a spool folder when the inbox cannot be
// reached.
package events

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Felipe-CdS/things-hub/api"
	"github.com/Felipe-CdS/things-hub/internal/hub"
)

// Accounts are the source and account pairs this Mac may send, from the
// .env, never from a model.
type Accounts map[string]string

// Read decodes a batch file strictly.
func Read(path string) (api.Batch, []byte, error) {
	var b api.Batch
	raw, err := os.ReadFile(path)
	if err != nil {
		return b, nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&b); err != nil {
		return b, nil, fmt.Errorf("%s: %w", path, err)
	}
	return b, raw, nil
}

// Check validates a batch the way the server will, plus the Mac's own
// rules: each event's account is the configured one for its source, and a
// GitHub event names a repository the account owns.
func Check(b api.Batch, size int, allowed Accounts) []string {
	var errs []string
	if b.Schema != api.BatchSchema {
		errs = append(errs, fmt.Sprintf("schema must be %q", api.BatchSchema))
	}
	if len(b.Events) > api.MaxBatchEvents {
		errs = append(errs, fmt.Sprintf("%d events; at most %d per batch", len(b.Events), api.MaxBatchEvents))
	}
	if size > api.MaxBatchBytes {
		errs = append(errs, fmt.Sprintf("%d bytes; at most %d per batch", size, api.MaxBatchBytes))
	}
	if c := b.Cursor; c != nil && !strings.EqualFold(allowed[c.Source], c.Account) {
		errs = append(errs, fmt.Sprintf("cursor account %q is not the configured %s account", c.Account, c.Source))
	}
	keys := map[string]int{}
	for i, e := range b.Events {
		bad := func(msg string) { errs = append(errs, fmt.Sprintf("events[%d]: %s", i, msg)) }
		if err := e.Validate(); err != nil {
			bad(err.Error())
			continue
		}
		want, ok := allowed[e.Source]
		if !ok || want == "" {
			bad(fmt.Sprintf("source %s is not configured on this Mac", e.Source))
			continue
		}
		if !strings.EqualFold(e.Account, want) {
			bad(fmt.Sprintf("account %q is not the configured %s account %q", e.Account, e.Source, want))
			continue
		}
		if e.Source == api.SourceGitHub {
			owner, _, _ := strings.Cut(e.SourceID, "/")
			if !strings.EqualFold(owner, want) || !strings.Contains(e.SourceID, "#") {
				bad(fmt.Sprintf("source_id %q must be %s/<repo>#<number>: personal repositories only", e.SourceID, want))
				continue
			}
		}
		k := api.Key(e)
		if j, dup := keys[k]; dup {
			bad(fmt.Sprintf("same key as events[%d]: %s", j, k))
			continue
		}
		keys[k] = i
	}
	return errs
}

// Poster posts batches and spools what cannot be delivered.
type Poster struct {
	Hub   *hub.Client
	Spool string
	Now   func() time.Time
}

// Spooled is returned when a batch was kept for a later flush.
type Spooled struct {
	Path string
	Err  error
}

func (e *Spooled) Error() string {
	return fmt.Sprintf("inbox unreachable (%v); kept the batch in %s for thub flush", e.Err, e.Path)
}

func (e *Spooled) Unwrap() error { return e.Err }

// unreachable reports whether err means the server was never reached.
func unreachable(err error) bool {
	var ne net.Error
	var oe *net.OpError
	return errors.As(err, &oe) || errors.As(err, &ne) || errors.Is(err, context.DeadlineExceeded)
}

// Post sends a batch. A network failure keeps the raw batch in the spool.
func (p *Poster) Post(ctx context.Context, b api.Batch, raw []byte) (api.BatchResponse, error) {
	resp, err := p.Hub.PostEvents(ctx, b)
	if err == nil || !unreachable(err) {
		return resp, err
	}
	if err := os.MkdirAll(p.Spool, 0o700); err != nil {
		return resp, err
	}
	path := filepath.Join(p.Spool, p.Now().UTC().Format("20060102T150405.000000000Z")+".json")
	if werr := os.WriteFile(path, raw, 0o600); werr != nil {
		return resp, errors.Join(err, werr)
	}
	return resp, &Spooled{Path: path, Err: err}
}

// Flush resends spooled batches oldest first and removes each one the
// server answered. Resends are harmless: the server reports duplicates.
func (p *Poster) Flush(ctx context.Context) (sent int, err error) {
	entries, err := os.ReadDir(p.Spool)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, n := range names {
		path := filepath.Join(p.Spool, n)
		b, _, err := Read(path)
		if err != nil {
			return sent, err
		}
		if _, err := p.Hub.PostEvents(ctx, b); err != nil {
			return sent, fmt.Errorf("%s: %w", n, err)
		}
		if err := os.Remove(path); err != nil {
			return sent, err
		}
		sent++
	}
	return sent, nil
}
