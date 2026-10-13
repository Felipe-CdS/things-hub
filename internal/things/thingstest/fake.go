// Package thingstest is an in-memory Things for tests: it never touches
// Things 3, records every write, and can delay marker visibility or fail a
// call to simulate Things' asynchronous writes and a crashed run.
package thingstest

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Felipe-CdS/things-hub/internal/things"
)

// ErrCrash is what a call returns when Fail picks it.
var ErrCrash = errors.New("simulated crash")

// Fake is an in-memory Things.
type Fake struct {
	mu    sync.Mutex
	items map[string]*things.Item
	order []string
	tags  []string
	n     int
	clock time.Time
	// Calls lists every write, as "<method> <uuid or title>".
	Calls []string
	// HideWrites keeps a new item out of FindByMarker for this many
	// searches, like Things' asynchronous URL-scheme writes.
	HideWrites int
	hidden     map[string]int
	// Fail is called before each method; a non-nil error is returned
	// instead of running it.
	Fail func(method string) error
}

// New returns an empty Fake.
func New() *Fake {
	return &Fake{items: map[string]*things.Item{}, hidden: map[string]int{},
		clock: time.Date(2026, 10, 13, 9, 0, 0, 0, time.Local)}
}

func (f *Fake) tick() string {
	f.clock = f.clock.Add(time.Second)
	return f.clock.Format(time.DateTime)
}

func (f *Fake) fail(method string) error {
	if f.Fail != nil {
		return f.Fail(method)
	}
	return nil
}

func (f *Fake) newUUID() string {
	f.n++
	return fmt.Sprintf("U%021d", f.n)
}

// Put adds an existing item and returns its uuid.
func (f *Fake) Put(it things.Item) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if it.UUID == "" {
		it.UUID = f.newUUID()
	}
	if it.Status == "" && it.Type != things.TypeArea {
		it.Status = things.StatusIncomplete
	}
	if it.Modified == "" {
		it.Modified = f.tick()
	}
	f.items[it.UUID] = &it
	f.order = append(f.order, it.UUID)
	return it.UUID
}

// PutTag adds an existing tag.
func (f *Fake) PutTag(title string) { f.mu.Lock(); f.tags = append(f.tags, title); f.mu.Unlock() }

// Get returns a copy of an item.
func (f *Fake) Get(uuid string) (things.Item, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	it, ok := f.items[uuid]
	if !ok {
		return things.Item{}, false
	}
	return *it, true
}

// Touch marks an item changed by the owner.
func (f *Fake) Touch(uuid string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.items[uuid].Modified = f.tick()
}

// WithMarker returns the items whose notes hold marker.
func (f *Fake) WithMarker(marker string) []things.Item {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []things.Item
	for _, u := range f.order {
		if things.MarkerIn(f.items[u].Notes) == marker {
			out = append(out, *f.items[u])
		}
	}
	return out
}

// Count returns how many items of typ exist, trashed or not.
func (f *Fake) Count(typ string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, it := range f.items {
		if it.Type == typ {
			n++
		}
	}
	return n
}

// Writes returns the recorded writes.
func (f *Fake) Writes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.Calls)
}

func (f *Fake) record(format string, args ...any) {
	f.Calls = append(f.Calls, fmt.Sprintf(format, args...))
}

func (f *Fake) all(match func(*things.Item) bool) []things.Item {
	out := []things.Item{}
	for _, u := range f.order {
		if it := f.items[u]; match(it) {
			out = append(out, *it)
		}
	}
	return out
}

// Snapshot exports the fake's state.
func (f *Fake) Snapshot(_ context.Context, _ int) (*things.Snapshot, error) {
	if err := f.fail("Snapshot"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	open := func(typ string) func(*things.Item) bool {
		return func(it *things.Item) bool {
			return it.Type == typ && !it.Trashed && it.Status == things.StatusIncomplete
		}
	}
	s := &things.Snapshot{Schema: things.SnapshotSchema, TakenAt: f.clock,
		Areas:    f.all(func(it *things.Item) bool { return it.Type == things.TypeArea }),
		Projects: f.all(open(things.TypeProject)),
		Headings: f.all(open(things.TypeHeading)),
		Todos:    f.all(open(things.TypeTodo)),
		Completed: f.all(func(it *things.Item) bool {
			return !it.Trashed && it.Status == things.StatusCompleted
		}),
		Canceled: f.all(func(it *things.Item) bool {
			return !it.Trashed && it.Status == things.StatusCanceled
		}),
		Trashed: f.all(func(it *things.Item) bool { return it.Trashed }),
	}
	for _, t := range f.tags {
		s.Tags = append(s.Tags, things.Item{Type: things.TypeTag, Title: t})
	}
	return s, nil
}

// FindByMarker searches open, untrashed to-dos and projects.
func (f *Fake) FindByMarker(_ context.Context, marker string) (*things.Item, error) {
	if err := f.fail("FindByMarker"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, u := range f.order {
		it := f.items[u]
		if things.MarkerIn(it.Notes) != marker || it.Trashed || it.Status != things.StatusIncomplete {
			continue
		}
		if f.hidden[u] > 0 {
			f.hidden[u]--
			return nil, nil
		}
		c := *it
		return &c, nil
	}
	return nil, nil
}

// Lookup reads one item.
func (f *Fake) Lookup(_ context.Context, uuid string) (*things.State, error) {
	if err := f.fail("Lookup"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	it, ok := f.items[uuid]
	if !ok {
		return nil, nil
	}
	st := &things.State{UUID: uuid, Type: it.Type, Status: it.Status, Trashed: it.Trashed,
		Modified: it.Modified, Checklist: it.ChecklistTitles()}
	if it.Type == things.TypeArea {
		for _, o := range f.items {
			if o.Area == uuid && !o.Trashed {
				st.Contents++
			}
		}
	}
	return st, nil
}

// Areas lists areas.
func (f *Fake) Areas(_ context.Context) ([]things.Item, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.all(func(it *things.Item) bool { return it.Type == things.TypeArea }), nil
}

// Tags lists tags.
func (f *Fake) Tags(_ context.Context) ([]things.Item, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []things.Item{}
	for _, t := range f.tags {
		out = append(out, things.Item{Type: things.TypeTag, Title: t})
	}
	return out, nil
}

func (f *Fake) place(it *things.Item, list string) {
	it.Area, it.Project = "", ""
	if l, ok := f.items[list]; ok {
		if l.Type == things.TypeProject {
			it.Project = list
		} else {
			it.Area = list
		}
	}
}

func (f *Fake) create(it things.Item) {
	it.UUID = f.newUUID()
	it.Status = things.StatusIncomplete
	it.Modified = f.tick()
	f.items[it.UUID] = &it
	f.order = append(f.order, it.UUID)
	if f.HideWrites > 0 {
		f.hidden[it.UUID] = f.HideWrites
	}
}

// AddTodo creates a to-do.
func (f *Fake) AddTodo(_ context.Context, t things.NewTodo) error {
	if err := f.fail("AddTodo"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	it := things.Item{Type: things.TypeTodo, Title: t.Title, Notes: t.Notes, Tags: t.Tags,
		Deadline: t.Deadline, Heading: t.HeadingID}
	f.place(&it, t.ListID)
	for _, c := range t.Checklist {
		it.Checklist = append(it.Checklist, things.ChecklistItem{Title: c, Status: things.StatusIncomplete})
	}
	f.create(it)
	f.record("AddTodo %s", t.Title)
	return nil
}

// AddProject creates a project.
func (f *Fake) AddProject(_ context.Context, p things.NewProject) error {
	if err := f.fail("AddProject"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	it := things.Item{Type: things.TypeProject, Title: p.Title, Notes: p.Notes, Tags: p.Tags, Area: p.AreaID}
	f.create(it)
	f.record("AddProject %s", p.Title)
	return nil
}

func (f *Fake) update(method, uuid string, c things.Change) error {
	if err := f.fail(method); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	it, ok := f.items[uuid]
	if !ok {
		return fmt.Errorf("%s: no item %s", method, uuid)
	}
	if c.Title != nil {
		it.Title = *c.Title
	}
	if c.Notes != nil {
		it.Notes = *c.Notes
	}
	if c.When != nil {
		switch *c.When {
		case "anytime":
			it.Start, it.StartDate = "Anytime", ""
		case "someday":
			it.Start, it.StartDate = "Someday", ""
		default:
			it.Start, it.StartDate = "Anytime", *c.When
		}
	}
	if c.Deadline != nil {
		it.Deadline = *c.Deadline
	}
	if c.Tags != nil {
		it.Tags = slices.Clone(*c.Tags)
	}
	if c.ListID != nil {
		f.place(it, *c.ListID)
	}
	if c.HeadingID != nil {
		it.Heading = *c.HeadingID
	}
	if c.Checklist != nil {
		it.Checklist = nil
		for _, t := range *c.Checklist {
			it.Checklist = append(it.Checklist, things.ChecklistItem{Title: t, Status: things.StatusIncomplete})
		}
	}
	for _, t := range c.AppendChecklist {
		it.Checklist = append(it.Checklist, things.ChecklistItem{Title: t, Status: things.StatusIncomplete})
	}
	// Like Things, completing or canceling a project with open to-dos is
	// ignored.
	stopsOpenProject := it.Type == things.TypeProject && f.hasOpenTodos(uuid) &&
		((c.Completed != nil && *c.Completed) || (c.Canceled != nil && *c.Canceled))
	if c.Completed != nil && !stopsOpenProject {
		it.Status = things.StatusIncomplete
		if *c.Completed {
			it.Status = things.StatusCompleted
		}
	}
	if c.Canceled != nil && !stopsOpenProject {
		it.Status = things.StatusIncomplete
		if *c.Canceled {
			it.Status = things.StatusCanceled
		}
	}
	it.Modified = f.tick()
	f.record("%s %s %s", method, uuid, describe(c))
	return nil
}

func (f *Fake) hasOpenTodos(project string) bool {
	for _, it := range f.items {
		if it.Type == things.TypeTodo && it.Project == project && !it.Trashed && it.Status == things.StatusIncomplete {
			return true
		}
	}
	return false
}

func describe(c things.Change) string {
	var parts []string
	if c.Completed != nil {
		parts = append(parts, fmt.Sprintf("completed=%v", *c.Completed))
	}
	if c.Canceled != nil {
		parts = append(parts, fmt.Sprintf("canceled=%v", *c.Canceled))
	}
	if c.When != nil {
		parts = append(parts, "when="+*c.When)
	}
	if c.Deadline != nil {
		parts = append(parts, "deadline="+*c.Deadline)
	}
	if c.ListID != nil {
		parts = append(parts, "list="+*c.ListID)
	}
	if c.Title != nil {
		parts = append(parts, "title="+*c.Title)
	}
	if len(c.AppendChecklist) > 0 {
		parts = append(parts, "checklist+="+strings.Join(c.AppendChecklist, "|"))
	}
	if c.Checklist != nil {
		parts = append(parts, "checklist="+strings.Join(*c.Checklist, "|"))
	}
	return strings.Join(parts, " ")
}

// UpdateTodo changes a to-do.
func (f *Fake) UpdateTodo(_ context.Context, uuid string, c things.Change) error {
	return f.update("UpdateTodo", uuid, c)
}

// UpdateProject changes a project.
func (f *Fake) UpdateProject(_ context.Context, uuid string, c things.Change) error {
	return f.update("UpdateProject", uuid, c)
}

// AddArea creates an area.
func (f *Fake) AddArea(_ context.Context, title string) (string, error) {
	if err := f.fail("AddArea"); err != nil {
		return "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	it := things.Item{UUID: f.newUUID(), Type: things.TypeArea, Title: title}
	f.items[it.UUID] = &it
	f.order = append(f.order, it.UUID)
	f.record("AddArea %s", title)
	return it.UUID, nil
}

// RenameArea renames an area.
func (f *Fake) RenameArea(_ context.Context, uuid, title string) error {
	if err := f.fail("RenameArea"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.items[uuid].Title = title
	f.record("RenameArea %s %s", uuid, title)
	return nil
}

// AddTag creates a tag.
func (f *Fake) AddTag(_ context.Context, title string) error {
	if err := f.fail("AddTag"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tags = append(f.tags, title)
	f.record("AddTag %s", title)
	return nil
}

// DeleteTag deletes a tag.
func (f *Fake) DeleteTag(_ context.Context, title string) error {
	if err := f.fail("DeleteTag"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tags = slices.DeleteFunc(f.tags, func(t string) bool { return t == title })
	f.record("DeleteTag %s", title)
	return nil
}

// MoveProject moves a project to an area.
func (f *Fake) MoveProject(_ context.Context, uuid, areaID string) error {
	if err := f.fail("MoveProject"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.items[uuid].Area = areaID
	f.items[uuid].Modified = f.tick()
	f.record("MoveProject %s %s", uuid, areaID)
	return nil
}

// Delete trashes a to-do or project, or removes an area.
func (f *Fake) Delete(_ context.Context, uuid, typ string) error {
	if err := f.fail("Delete"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	it, ok := f.items[uuid]
	if !ok {
		return fmt.Errorf("delete: no item %s", uuid)
	}
	if typ == things.TypeArea {
		delete(f.items, uuid)
		f.order = slices.DeleteFunc(f.order, func(u string) bool { return u == uuid })
	} else {
		it.Trashed = true
		it.Modified = f.tick()
	}
	f.record("Delete %s", uuid)
	return nil
}

// Restore brings a trashed item back.
func (f *Fake) Restore(_ context.Context, uuid, _, listID string) error {
	if err := f.fail("Restore"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	it := f.items[uuid]
	it.Trashed = false
	if listID != "" {
		f.place(it, listID)
	}
	it.Modified = f.tick()
	f.record("Restore %s", uuid)
	return nil
}

// Close does nothing.
func (f *Fake) Close() error { return nil }

var _ things.Client = (*Fake)(nil)
