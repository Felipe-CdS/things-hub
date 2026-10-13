// Package things reads and writes Things 3 for the Mac tool: through
// things-mcp (a subprocess driven over MCP), a read-only view of the Things
// database for single-item lookups and the URL-scheme token, and AppleScript
// or URL-scheme fallbacks for what things-mcp cannot do.
package things

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"
)

// Item types as things.py reports them.
const (
	TypeTodo    = "to-do"
	TypeProject = "project"
	TypeHeading = "heading"
	TypeArea    = "area"
	TypeTag     = "tag"
)

// Item statuses as things.py reports them.
const (
	StatusIncomplete = "incomplete"
	StatusCompleted  = "completed"
	StatusCanceled   = "canceled"
)

// Item is one Things object as things-mcp returns it in structured content.
// Fields not used by thub are kept out; the JSON names follow things.py.
type Item struct {
	UUID         string          `json:"uuid"`
	Type         string          `json:"type"`
	Title        string          `json:"title"`
	Status       string          `json:"status,omitempty"`
	Trashed      bool            `json:"trashed,omitempty"`
	Notes        string          `json:"notes,omitempty"`
	Area         string          `json:"area,omitempty"`
	AreaTitle    string          `json:"area_title,omitempty"`
	Project      string          `json:"project,omitempty"`
	ProjectTitle string          `json:"project_title,omitempty"`
	Heading      string          `json:"heading,omitempty"`
	HeadingTitle string          `json:"heading_title,omitempty"`
	Tags         []string        `json:"tags,omitempty"`
	Start        string          `json:"start,omitempty"`
	StartDate    string          `json:"start_date,omitempty"`
	Deadline     string          `json:"deadline,omitempty"`
	StopDate     string          `json:"stop_date,omitempty"`
	Created      string          `json:"created,omitempty"`
	Modified     string          `json:"modified,omitempty"`
	Checklist    []ChecklistItem `json:"checklist,omitempty"`
}

// ChecklistItem is one checklist entry of a to-do.
type ChecklistItem struct {
	Title  string `json:"title"`
	Status string `json:"status,omitempty"`
}

// UnmarshalJSON accepts things.py's loose shapes: trashed as 1 or a bool,
// tags and checklist as lists or as a bare 1 when items were not included.
func (it *Item) UnmarshalJSON(b []byte) error {
	type plain Item
	var raw struct {
		plain
		Trashed   json.RawMessage `json:"trashed"`
		Tags      json.RawMessage `json:"tags"`
		Checklist json.RawMessage `json:"checklist"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	*it = Item(raw.plain)
	switch strings.TrimSpace(string(raw.Trashed)) {
	case "1", "true":
		it.Trashed = true
	}
	if len(raw.Tags) > 0 && raw.Tags[0] == '[' {
		if err := json.Unmarshal(raw.Tags, &it.Tags); err != nil {
			return err
		}
	}
	if len(raw.Checklist) > 0 && raw.Checklist[0] == '[' {
		if err := json.Unmarshal(raw.Checklist, &it.Checklist); err != nil {
			return err
		}
	}
	return nil
}

// ChecklistTitles returns the titles of the item's checklist.
func (it Item) ChecklistTitles() []string {
	out := make([]string, len(it.Checklist))
	for i, c := range it.Checklist {
		out[i] = c.Title
	}
	return out
}

// ListID is the project or area the item lives in, project first.
func (it Item) ListID() string {
	if it.Project != "" {
		return it.Project
	}
	return it.Area
}

// SnapshotSchema is the schema of a Snapshot.
const SnapshotSchema = "things-hub.snapshot.v1"

// Snapshot is the whole of Things taken at the start of a run.
type Snapshot struct {
	Schema    string    `json:"schema"`
	TakenAt   time.Time `json:"taken_at"`
	Areas     []Item    `json:"areas"`
	Projects  []Item    `json:"projects"`
	Headings  []Item    `json:"headings"`
	Tags      []Item    `json:"tags"`
	Todos     []Item    `json:"todos"`
	Completed []Item    `json:"completed"`
	Canceled  []Item    `json:"canceled"`
	Trashed   []Item    `json:"trashed"`
}

// Find returns the item with uuid from any list of the snapshot.
func (s *Snapshot) Find(uuid string) (Item, bool) {
	if s == nil || uuid == "" {
		return Item{}, false
	}
	for _, list := range [][]Item{s.Todos, s.Projects, s.Areas, s.Headings, s.Completed, s.Canceled, s.Trashed} {
		for _, it := range list {
			if it.UUID == uuid {
				return it, true
			}
		}
	}
	return Item{}, false
}

// State is a fresh read of one item, used right before writing to it.
type State struct {
	UUID      string
	Type      string
	Status    string
	Trashed   bool
	Modified  string
	Checklist []string
}

// NewTodo is a to-do to create. Notes carry the marker.
type NewTodo struct {
	Title     string
	Notes     string
	When      string
	Deadline  string
	Tags      []string
	Checklist []string
	ListID    string
	HeadingID string
}

// NewProject is a project to create, without to-dos: each to-do is created
// on its own so that it carries its own marker.
type NewProject struct {
	Title    string
	Notes    string
	When     string
	Deadline string
	Tags     []string
	AreaID   string
}

// Change is an update to an existing to-do or project. Nil fields are left
// unchanged; an empty string clears a date. Lists and checklists apply to
// to-dos only.
type Change struct {
	Title           *string   `json:"title,omitempty"`
	Notes           *string   `json:"notes,omitempty"`
	When            *string   `json:"when,omitempty"`
	Deadline        *string   `json:"deadline,omitempty"`
	Tags            *[]string `json:"tags,omitempty"`
	ListID          *string   `json:"list_id,omitempty"`
	HeadingID       *string   `json:"heading_id,omitempty"`
	Checklist       *[]string `json:"checklist,omitempty"`
	AppendChecklist []string  `json:"append_checklist,omitempty"`
	Completed       *bool     `json:"completed,omitempty"`
	Canceled        *bool     `json:"canceled,omitempty"`
}

// Empty reports whether the change sets nothing.
func (c Change) Empty() bool {
	return c.Title == nil && c.Notes == nil && c.When == nil && c.Deadline == nil && c.Tags == nil &&
		c.ListID == nil && c.HeadingID == nil && c.Checklist == nil && len(c.AppendChecklist) == 0 &&
		c.Completed == nil && c.Canceled == nil
}

// Client is everything thub needs from Things. The live client drives
// things-mcp and the fallbacks; tests use a fake.
type Client interface {
	// Snapshot exports areas, projects, headings, tags, open to-dos, and
	// to-dos completed, canceled or trashed in the last days.
	Snapshot(ctx context.Context, days int) (*Snapshot, error)
	// FindByMarker returns the open, non-trashed to-do or project whose
	// notes hold marker, or nil.
	FindByMarker(ctx context.Context, marker string) (*Item, error)
	// Lookup re-reads one to-do, project or area, or returns nil when it
	// no longer exists.
	Lookup(ctx context.Context, uuid string) (*State, error)
	// Areas and Tags list what exists now, so creating one is idempotent.
	Areas(ctx context.Context) ([]Item, error)
	Tags(ctx context.Context) ([]Item, error)

	AddTodo(ctx context.Context, t NewTodo) error
	AddProject(ctx context.Context, p NewProject) error
	UpdateTodo(ctx context.Context, uuid string, c Change) error
	UpdateProject(ctx context.Context, uuid string, c Change) error
	// AddArea creates an area and returns its uuid.
	AddArea(ctx context.Context, title string) (string, error)
	RenameArea(ctx context.Context, uuid, title string) error

	// Fallbacks for what things-mcp cannot do.
	AddTag(ctx context.Context, title string) error
	DeleteTag(ctx context.Context, title string) error
	MoveProject(ctx context.Context, uuid, areaID string) error
	// Delete moves a to-do or project to the Trash, or deletes an empty
	// area. It never empties the Trash.
	Delete(ctx context.Context, uuid, typ string) error
	// Restore brings a trashed to-do or project back into listID (or the
	// Inbox when empty).
	Restore(ctx context.Context, uuid, typ, listID string) error

	Close() error
}

// ErrNotSupported is returned by an operation a client cannot carry out.
var ErrNotSupported = errors.New("not supported")

var markerRe = regexp.MustCompile(`\bth-[a-z2-7]{10}\b`)

// NewMarker returns a fresh marker: "th-" and 10 random base32 characters.
func NewMarker() string {
	b := make([]byte, 7)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	s := strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))
	return "th-" + s[:10]
}

// MarkerIn returns the marker found in notes, or "".
func MarkerIn(notes string) string {
	return markerRe.FindString(notes)
}

var sourceNames = map[string]string{
	"github": "GitHub", "slack": "Slack", "classroom": "Classroom",
	"gemini-notes": "Gemini notes", "email": "Email", "wiki": "Wiki",
}

// SourceLine describes where a hub to-do came from.
type SourceLine struct {
	Source   string
	SourceID string
	Kind     string
	URL      string
}

// BuildNotes writes a hub item's notes: the summary, the source line and
// link, and the marker last.
func BuildNotes(summary string, src *SourceLine, marker string) string {
	var parts []string
	if s := strings.TrimSpace(summary); s != "" {
		parts = append(parts, s, "")
	}
	if src != nil {
		name := sourceNames[src.Source]
		if name == "" {
			name = src.Source
		}
		parts = append(parts, "— "+name+" · "+src.SourceID+" · "+strings.ReplaceAll(src.Kind, "-", " "))
		if src.URL != "" {
			parts = append(parts, src.URL)
		}
	}
	parts = append(parts, marker)
	return strings.Join(parts, "\n")
}
