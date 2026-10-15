// Package plan holds what an on-demand run passes between its steps: the
// input thub pull writes for the agent, the plan the agent writes back, its
// validation against the snapshot and the per-run limits, and the ordered
// list of Things operations thub apply carries out.
package plan

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/Felipe-CdS/things-hub/api"
	"github.com/Felipe-CdS/things-hub/internal/things"
)

// Schemas of the run files.
const (
	InputSchema = "things-hub.input.v1"
	PlanSchema  = "things-hub.plan.v1"
)

// Input is input.json: what thub pull hands the agent.
type Input struct {
	Schema    string    `json:"schema"`
	RunID     string    `json:"run_id"`
	LeaseID   string    `json:"lease_id,omitempty"`
	ExpiresAt time.Time `json:"lease_expires_at,omitzero"`
	// Items are the leased queue items with their linked Things items.
	Items []api.LeasedItem `json:"items"`
	// Ledger is every hub-made or adopted Things item the server knows,
	// with its state refreshed from the snapshot.
	Ledger   []api.Thing      `json:"ledger"`
	Limits   Limits           `json:"limits"`
	Snapshot *things.Snapshot `json:"snapshot"`
	// LayoutApproved is true once the owner said yes to the first run that
	// reorganizes existing items.
	LayoutApproved bool `json:"layout_approved"`
}

// Item returns the leased item with key.
func (in *Input) Item(key string) (api.LeasedItem, bool) {
	for _, it := range in.Items {
		if it.Key == key {
			return it, true
		}
	}
	return api.LeasedItem{}, false
}

// Item actions: each answers one leased key.
const (
	ActCreate  = "create"
	ActProject = "project"
	ActAttach  = "attach"
	ActUpdate  = "update"
	ActResolve = "resolve"
	ActIgnore  = "ignore"
	ActDefer   = "defer"
)

// Organizing ops: each acts on Things directly, with no key.
const (
	OpCreateArea    = "create_area"
	OpRenameArea    = "rename_area"
	OpDeleteArea    = "delete_area"
	OpCreateTag     = "create_tag"
	OpCreateProject = "create_project"
	OpMove          = "move"
	OpMoveProject   = "move_project"
	OpUpdate        = "update"
	OpComplete      = "complete"
	OpCancel        = "cancel"
	OpDelete        = "delete"
)

// Plan is plan.json: the agent's decisions.
type Plan struct {
	Schema   string     `json:"schema"`
	RunID    string     `json:"run_id"`
	LeaseID  string     `json:"lease_id,omitempty"`
	Items    []Decision `json:"items"`
	Organize []Organize `json:"organize"`
}

// Decision answers one leased key.
type Decision struct {
	Key      string `json:"key"`
	Revision string `json:"revision,omitempty"`
	Action   string `json:"action"`
	Reason   string `json:"reason,omitempty"`
	// Todo is the to-do to create (create).
	Todo *Todo `json:"todo,omitempty"`
	// Project and Todos are the project to create and its steps (project).
	Project *Project `json:"project,omitempty"`
	Todos   []Todo   `json:"todos,omitempty"`
	// UUID is the existing item (attach, update, resolve).
	UUID string `json:"uuid,omitempty"`
	// AppendChecklist is added to the to-do's checklist (attach).
	AppendChecklist []string `json:"append_checklist,omitempty"`
	// Fields change the item (update, attach).
	Fields *Fields `json:"fields,omitempty"`
	// Complete or Cancel the item (resolve). Complete is the default.
	Complete bool `json:"complete,omitempty"`
	Cancel   bool `json:"cancel,omitempty"`
	// NotBefore (RFC 3339) holds a deferred item back.
	NotBefore string `json:"not_before,omitempty"`
}

// Todo is a to-do the hub creates. Notes are built by thub from Summary,
// the source and the marker.
type Todo struct {
	Title   string `json:"title"`
	Summary string `json:"summary,omitempty"`
	// List is a project or area: its uuid, or the title of one in the
	// snapshot or created earlier in the plan. Empty means the Inbox.
	List      string   `json:"list,omitempty"`
	Heading   string   `json:"heading,omitempty"`
	When      string   `json:"when,omitempty"`
	Deadline  string   `json:"deadline,omitempty"`
	Tags      []string `json:"tags,omitempty"`
	Checklist []string `json:"checklist,omitempty"`
}

// Project is a project the hub creates.
type Project struct {
	Title   string `json:"title"`
	Summary string `json:"summary,omitempty"`
	// Area is an area uuid or title; empty means no area.
	Area     string   `json:"area,omitempty"`
	When     string   `json:"when,omitempty"`
	Deadline string   `json:"deadline,omitempty"`
	Tags     []string `json:"tags,omitempty"`
}

// Fields are changes to an existing to-do or project. Nil leaves a field
// as is; an empty When or Deadline clears it.
type Fields struct {
	Title     *string   `json:"title,omitempty"`
	Notes     *string   `json:"notes,omitempty"`
	When      *string   `json:"when,omitempty"`
	Deadline  *string   `json:"deadline,omitempty"`
	Tags      *[]string `json:"tags,omitempty"`
	List      *string   `json:"list,omitempty"`
	Heading   *string   `json:"heading,omitempty"`
	Checklist *[]string `json:"checklist,omitempty"`
}

// Organize is one organizing op.
type Organize struct {
	Op     string  `json:"op"`
	UUID   string  `json:"uuid,omitempty"`
	Title  string  `json:"title,omitempty"`
	List   string  `json:"list,omitempty"`
	Area   string  `json:"area,omitempty"`
	Fields *Fields `json:"fields,omitempty"`
	// Summary is the notes of a created project.
	Summary string `json:"summary,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

// Limits are the per-run caps thub check enforces.
type Limits struct {
	// NewTodos caps to-dos created, including a new project's steps.
	NewTodos int `json:"new_todos"`
	// Deletes caps to-dos and projects moved to the Trash.
	Deletes int `json:"deletes"`
	// Moves caps moves, reschedules and other changes to items that existed
	// before the run.
	Moves int `json:"moves"`
	// AreaChanges caps area creations and deletions.
	AreaChanges int `json:"area_changes"`
}

// DefaultLimits are the POLICY.md defaults.
var DefaultLimits = Limits{NewTodos: 25, Deletes: 10, Moves: 50, AreaChanges: 3}

// WithDefaults fills zero limits with the defaults.
func (l Limits) WithDefaults() Limits {
	if l.NewTodos == 0 {
		l.NewTodos = DefaultLimits.NewTodos
	}
	if l.Deletes == 0 {
		l.Deletes = DefaultLimits.Deletes
	}
	if l.Moves == 0 {
		l.Moves = DefaultLimits.Moves
	}
	if l.AreaChanges == 0 {
		l.AreaChanges = DefaultLimits.AreaChanges
	}
	return l
}

// ReadJSON decodes a JSON file strictly: unknown fields are errors, which
// catches the agent's typos early.
func ReadJSON(path string, v any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// WriteJSON writes v indented, through a temporary file.
func WriteJSON(path string, v any) error {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
