package things

import (
	"encoding/json"
	"testing"
)

func TestItemAcceptsThingsPyShapes(t *testing.T) {
	var items []Item
	raw := `[{"uuid":"A","type":"to-do","title":"x","trashed":1,"tags":["github"],"checklist":[{"title":"a","status":"incomplete"}]},
	         {"uuid":"B","type":"to-do","title":"y","tags":1,"checklist":1}]`
	if err := json.Unmarshal([]byte(raw), &items); err != nil {
		t.Fatal(err)
	}
	if !items[0].Trashed || items[0].Tags[0] != "github" || items[0].ChecklistTitles()[0] != "a" {
		t.Fatalf("first %+v", items[0])
	}
	if items[1].Trashed || items[1].Tags != nil || items[1].Checklist != nil {
		t.Fatalf("second %+v", items[1])
	}
}

func TestMarkerAndNotes(t *testing.T) {
	m := NewMarker()
	if MarkerIn("x "+m+" y") != m || len(m) != 13 {
		t.Fatalf("marker %q", m)
	}
	got := BuildNotes("Review requested.", &SourceLine{Source: "github", SourceID: "felipe-cds/mic#12",
		Kind: "review-requested", URL: "https://github.com/felipe-cds/mic/pull/12"}, "th-4k2m7x7qpa")
	want := "Review requested.\n\n— GitHub · felipe-cds/mic#12 · review requested\nhttps://github.com/felipe-cds/mic/pull/12\nth-4k2m7x7qpa"
	if got != want {
		t.Fatalf("notes\n%s\nwant\n%s", got, want)
	}
}
