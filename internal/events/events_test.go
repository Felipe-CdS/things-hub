package events

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Felipe-CdS/things-hub/api"
	"github.com/Felipe-CdS/things-hub/internal/hub"
)

func event(sourceID string) api.Event {
	return api.Event{Source: "github", Account: "felipe-cds", Kind: "assigned", SourceID: sourceID, Op: api.OpUpsert,
		Revision: "2026-10-13T10:00:00Z", ObservedAt: "2026-10-13T10:00:00Z",
		URL: "https://github.com/felipe-cds/mic/issues/7", Title: "Fix: mic#7"}
}

func TestCheck(t *testing.T) {
	ok := Accounts{"github": "felipe-cds"}
	good := api.Batch{Schema: api.BatchSchema, Events: []api.Event{event("Felipe-CdS/mic#7")}}
	if errs := Check(good, 100, ok); len(errs) != 0 {
		t.Fatalf("good batch refused: %q", errs)
	}
	org := event("acme/api#3")
	other := event("felipe-cds/mic#8")
	other.Account = "someone"
	slack := event("C1/1.2")
	slack.Source, slack.Kind = "slack", "mention"
	bad := api.Batch{Schema: api.BatchSchema, Events: []api.Event{org, other, slack, event("felipe-cds/mic#7"), event("felipe-cds/mic#7")}}
	errs := strings.Join(Check(bad, 100, ok), "\n")
	for _, want := range []string{"events[0]: source_id \"acme/api#3\"", "events[1]: account \"someone\"",
		"events[2]: source slack is not configured", "events[4]: same key as events[3]"} {
		if !strings.Contains(errs, want) {
			t.Errorf("missing %q in\n%s", want, errs)
		}
	}
}

func TestPostSpoolsWhenUnreachableAndFlushResends(t *testing.T) {
	dir := t.TempDir()
	p := &Poster{Hub: hub.New("http://127.0.0.1:1", "thub_x"), Spool: filepath.Join(dir, "spool"), Now: time.Now}
	b := api.Batch{Schema: api.BatchSchema, Events: []api.Event{event("felipe-cds/mic#7")}}
	_, err := p.Post(context.Background(), b, []byte(`{"schema":"things-hub.batch.v1","events":[]}`))
	var sp *Spooled
	if !errors.As(err, &sp) {
		t.Fatalf("post to a dead server: %v", err)
	}
	if fi, err := os.Stat(sp.Path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("spooled file: %v %v", fi, err)
	}
	if n, err := p.Flush(context.Background()); err == nil || n != 0 {
		t.Fatalf("flush to a dead server: %d %v", n, err)
	}
}
