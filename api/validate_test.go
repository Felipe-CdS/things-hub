package api

import (
	"testing"
	"time"
)

func TestParseRevision(t *testing.T) {
	cases := []struct {
		in   string
		want time.Time
	}{
		{"2026-10-13T11:50:02Z", time.Date(2026, 10, 13, 11, 50, 2, 0, time.UTC)},
		{"2026-10-13T08:50:02.5-03:00", time.Date(2026, 10, 13, 11, 50, 2, 5e8, time.UTC)},
		{"1760357100.000200", time.Unix(1760357100, 200000).UTC()},
		{"1760357100", time.Unix(1760357100, 0).UTC()},
	}
	for _, c := range cases {
		got, err := ParseRevision(c.in)
		if err != nil || !got.Equal(c.want) {
			t.Errorf("ParseRevision(%q) = %v, %v; want %v", c.in, got, err, c.want)
		}
	}
	for _, bad := range []string{"", "yesterday", "1760357100.", "1760357100.0000000001", "2026-10-13"} {
		if _, err := ParseRevision(bad); err == nil {
			t.Errorf("ParseRevision(%q) succeeded", bad)
		}
	}
}

func TestKey(t *testing.T) {
	e := Event{Source: "github", Account: "Felipe-CdS", Kind: "review-requested", SourceID: "Felipe-CdS/mic#12"}
	if got, want := Key(e), "github:felipe-cds:review-requested:Felipe-CdS/mic#12"; got != want {
		t.Fatalf("Key = %q, want %q", got, want)
	}
}
