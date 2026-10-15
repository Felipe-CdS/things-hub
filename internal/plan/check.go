package plan

import (
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Felipe-CdS/things-hub/api"
	"github.com/Felipe-CdS/things-hub/internal/things"
)

// Op kinds thub apply carries out, in plan order.
const (
	KindCreateTodo    = "create_todo"
	KindCreateProject = "create_project"
	KindUpdate        = "update"
	KindComplete      = "complete"
	KindCancel        = "cancel"
	KindDelete        = "delete"
	KindCreateArea    = "create_area"
	KindRenameArea    = "rename_area"
	KindDeleteArea    = "delete_area"
	KindCreateTag     = "create_tag"
	KindMoveProject   = "move_project"
)

// Target names a project or area: an existing uuid, or the op that creates
// it earlier in the plan. Both empty means the Inbox or no area.
type Target struct {
	UUID string `json:"uuid,omitempty"`
	Op   string `json:"op,omitempty"`
}

// IsZero reports whether the target is empty.
func (t Target) IsZero() bool { return t.UUID == "" && t.Op == "" }

// Op is one Things write thub apply carries out.
type Op struct {
	// ID is <run-id>/o<n> for organize[n-1], <run-id>/i<n> for items[n-1]
	// and <run-id>/i<n>.<k> for the k-th step of a new project; .c<n>
	// suffixes the n-th open to-do completed or canceled with its project.
	ID   string `json:"id"`
	Kind string `json:"kind"`
	// Key and Role link the written item to a source key.
	Key  string `json:"key,omitempty"`
	Role string `json:"role,omitempty"`
	// UUID and Type name the existing item the op changes.
	UUID string `json:"uuid,omitempty"`
	Type string `json:"type,omitempty"`
	// Title is the new to-do, project, area or tag title.
	Title string `json:"title,omitempty"`
	// Notes are a new item's notes without the marker, which apply adds.
	Summary string             `json:"summary,omitempty"`
	Source  *things.SourceLine `json:"source,omitempty"`
	// Todo holds the fields of a new to-do; its list is List.
	Todo *things.NewTodo `json:"todo,omitempty"`
	// Project holds the fields of a new project; its area is List.
	Project *things.NewProject `json:"project,omitempty"`
	// List is where a new or moved item goes.
	List Target `json:"list,omitzero"`
	// Change is an update; its ListID comes from List when set.
	Change *things.Change `json:"change,omitempty"`
	// Existing marks an op on an item that existed before the run.
	Existing bool `json:"existing,omitempty"`
	// Before is the item as the snapshot saw it.
	Before *things.Item `json:"before,omitempty"`
	// Describe is the dry-run line.
	Describe string `json:"describe"`
}

// Ack outcome each action maps to when its ops succeed.
var actionOutcome = map[string]string{
	ActCreate: api.OutcomeCreated, ActProject: api.OutcomeCreated, ActAttach: api.OutcomeAttached,
	ActUpdate: api.OutcomeUpdated, ActResolve: api.OutcomeResolved, ActIgnore: api.OutcomeIgnored,
	ActDefer: api.OutcomeDeferred,
}

// Answer is how a leased key is acked after its ops ran.
type Answer struct {
	Key       string   `json:"key"`
	Revision  string   `json:"revision"`
	Action    string   `json:"action"`
	Outcome   string   `json:"outcome"`
	Ops       []string `json:"ops,omitempty"`
	Note      string   `json:"note,omitempty"`
	NotBefore string   `json:"not_before,omitempty"`
}

// Counts are what the plan changes, compared with the limits.
type Counts struct {
	NewTodos    int `json:"new_todos"`
	NewProjects int `json:"new_projects"`
	Deletes     int `json:"deletes"`
	Moves       int `json:"moves"`
	AreaChanges int `json:"area_changes"`
	Completes   int `json:"completes"`
	Ignored     int `json:"ignored"`
	Deferred    int `json:"deferred"`
}

// Result is a checked plan: its ops in order, how each key is acked, and
// what is wrong with it.
type Result struct {
	Ops       []Op     `json:"ops"`
	Answers   []Answer `json:"answers"`
	Unplanned []string `json:"unplanned,omitempty"`
	Counts    Counts   `json:"counts"`
	Errors    []string `json:"errors,omitempty"`
	Warnings  []string `json:"warnings,omitempty"`
	// OverLimits lists the limits the plan exceeds.
	OverLimits []string `json:"over_limits,omitempty"`
	// ReorganizesLayout is true when organize ops change existing items.
	ReorganizesLayout bool `json:"reorganizes_layout,omitempty"`
}

// OK reports whether the plan may be applied: no errors, and within the
// limits unless allowOver.
func (r *Result) OK(allowOver bool) bool {
	return len(r.Errors) == 0 && (allowOver || len(r.OverLimits) == 0)
}

var (
	dateRe     = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	reminderRe = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2}|today|tomorrow|evening)@\d{1,2}:\d{2}$`)
)

const (
	maxTitleRunes   = 255
	maxSummaryRunes = 2000
	checklistHint   = 5
)

func validWhen(s string) bool {
	switch s {
	case "today", "tomorrow", "evening", "anytime", "someday":
		return true
	}
	if reminderRe.MatchString(s) {
		return true
	}
	return validDate(s)
}

func validDate(s string) bool {
	if !dateRe.MatchString(s) {
		return false
	}
	_, err := time.Parse(time.DateOnly, s)
	return err == nil
}

// container is a project or area a list reference can name.
type container struct {
	Target
	Type  string
	Title string
}

// checker holds the simulated state of Things while the plan is walked in
// order: organize ops first, then items.
type checker struct {
	in   *Input
	p    *Plan
	res  *Result
	snap *things.Snapshot

	containers []container
	tags       map[string]string // lowercase title -> title
	deleted    map[string]bool
	stopped    map[string]bool
	// areaOf is the area each top-level item sits in, for empty-area checks.
	areaOf map[string]string
	// projectOf is the project each to-do sits in, directly or under a heading.
	projectOf map[string]string
	// headingProject maps a heading uuid to its project.
	headingProject map[string]string
	where          string
}

func (c *checker) errf(format string, args ...any) {
	c.res.Errors = append(c.res.Errors, c.where+": "+fmt.Sprintf(format, args...))
}

func (c *checker) warnf(format string, args ...any) {
	c.res.Warnings = append(c.res.Warnings, c.where+": "+fmt.Sprintf(format, args...))
}

// Check validates a plan against its input and expands it into ops.
func Check(in *Input, p *Plan) *Result {
	res := &Result{Ops: []Op{}, Answers: []Answer{}}
	snap := in.Snapshot
	if snap == nil {
		snap = &things.Snapshot{}
	}
	c := &checker{in: in, p: p, res: res, snap: snap, tags: map[string]string{},
		deleted: map[string]bool{}, stopped: map[string]bool{}, areaOf: map[string]string{},
		projectOf: map[string]string{}, headingProject: map[string]string{}}
	for _, a := range snap.Areas {
		c.containers = append(c.containers, container{Target{UUID: a.UUID}, things.TypeArea, a.Title})
	}
	for _, pr := range snap.Projects {
		c.containers = append(c.containers, container{Target{UUID: pr.UUID}, things.TypeProject, pr.Title})
		if pr.Area != "" {
			c.areaOf[pr.UUID] = pr.Area
		}
	}
	for _, t := range snap.Tags {
		c.tags[strings.ToLower(t.Title)] = t.Title
	}
	for _, h := range snap.Headings {
		c.headingProject[h.UUID] = h.Project
	}
	for _, list := range [][]things.Item{snap.Todos, snap.Completed, snap.Canceled} {
		for _, t := range list {
			if t.Area != "" && t.Project == "" && t.Type == things.TypeTodo {
				c.areaOf[t.UUID] = t.Area
			}
			if t.Project != "" {
				c.projectOf[t.UUID] = t.Project
			} else if p := c.headingProject[t.Heading]; p != "" {
				c.projectOf[t.UUID] = p
			}
		}
	}

	c.where = "plan"
	if p.Schema != PlanSchema {
		c.errf("schema must be %q", PlanSchema)
	}
	if p.RunID != in.RunID {
		c.errf("run_id %q does not match the input's %q", p.RunID, in.RunID)
	}
	if p.LeaseID != in.LeaseID {
		c.errf("lease_id %q does not match the input's %q", p.LeaseID, in.LeaseID)
	}

	for i, o := range p.Organize {
		c.where = fmt.Sprintf("organize[%d] (%s)", i, o.Op)
		c.organize(fmt.Sprintf("%s/o%d", p.RunID, i+1), o)
	}
	seen := map[string]bool{}
	for i, d := range p.Items {
		c.where = fmt.Sprintf("items[%d] (%s %s)", i, d.Action, d.Key)
		if seen[d.Key] {
			c.errf("a second decision for the same key")
			continue
		}
		seen[d.Key] = true
		c.decision(fmt.Sprintf("%s/i%d", p.RunID, i+1), d)
	}
	for _, it := range in.Items {
		if !seen[it.Key] {
			res.Unplanned = append(res.Unplanned, it.Key)
		}
	}
	if len(res.Unplanned) > 0 {
		c.where = "plan"
		c.warnf("%d leased item(s) have no decision and will be released: %s",
			len(res.Unplanned), strings.Join(res.Unplanned, ", "))
	}

	lim := in.Limits.WithDefaults()
	over := func(name string, n, max int) {
		if n > max {
			res.OverLimits = append(res.OverLimits, fmt.Sprintf("%s: %d, limit %d", name, n, max))
		}
	}
	over("new to-dos", res.Counts.NewTodos, lim.NewTodos)
	over("deletes", res.Counts.Deletes, lim.Deletes)
	over("moves and changes of existing items", res.Counts.Moves, lim.Moves)
	over("area creations and deletions", res.Counts.AreaChanges, lim.AreaChanges)
	return res
}

func (c *checker) add(op Op) {
	c.res.Ops = append(c.res.Ops, op)
}

// existing returns the snapshot item uuid names, if it is still live in the
// simulated plan.
func (c *checker) existing(uuid string, types ...string) (things.Item, bool) {
	if uuid == "" {
		c.errf("uuid is required")
		return things.Item{}, false
	}
	it, ok := c.snap.Find(uuid)
	if !ok {
		c.errf("unknown uuid %s (not in the snapshot)", uuid)
		return it, false
	}
	if c.deleted[uuid] || it.Trashed {
		c.errf("%s %q is in the Trash", it.Type, it.Title)
		return it, false
	}
	for _, t := range types {
		if it.Type == t {
			return it, true
		}
	}
	c.errf("%s is a %s, not a %s", uuid, it.Type, strings.Join(types, " or "))
	return it, false
}

func (c *checker) liveContainers() []container {
	var out []container
	for _, k := range c.containers {
		if !c.deleted[k.UUID] && !c.deleted[k.Op] {
			out = append(out, k)
		}
	}
	return out
}

// list resolves a project or area reference. types narrows what it may be.
func (c *checker) list(ref string, types ...string) (container, bool) {
	if ref == "" {
		return container{}, true
	}
	allowed := func(k container) bool {
		for _, t := range types {
			if k.Type == t {
				return true
			}
		}
		return false
	}
	live := c.liveContainers()
	for _, k := range live {
		if k.UUID == ref {
			if !allowed(k) {
				c.errf("%q is a %s, not a %s", ref, k.Type, strings.Join(types, " or "))
				return k, false
			}
			return k, true
		}
	}
	var matches []container
	for _, k := range live {
		if allowed(k) && strings.EqualFold(k.Title, ref) {
			matches = append(matches, k)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], true
	case 0:
		c.errf("unknown %s %q", strings.Join(types, " or "), ref)
	default:
		c.errf("%q names %d lists; use a uuid", ref, len(matches))
	}
	return container{}, false
}

func (c *checker) heading(ref string, list container) (string, bool) {
	if ref == "" {
		return "", true
	}
	if list.Type != things.TypeProject || list.UUID == "" {
		c.errf("heading %q needs an existing project as the list", ref)
		return "", false
	}
	for _, h := range c.snap.Headings {
		if h.Project == list.UUID && (h.UUID == ref || strings.EqualFold(h.Title, ref)) {
			return h.UUID, true
		}
	}
	c.errf("unknown heading %q in project %q (thub cannot create headings in an existing project)", ref, list.Title)
	return "", false
}

func (c *checker) checkTags(tags []string) []string {
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		name, ok := c.tags[strings.ToLower(t)]
		if !ok {
			c.errf("unknown tag %q (add a create_tag op first)", t)
			continue
		}
		out = append(out, name)
	}
	return out
}

func (c *checker) title(s string) bool {
	if strings.TrimSpace(s) == "" {
		c.errf("title is empty")
		return false
	}
	if utf8.RuneCountInString(s) > maxTitleRunes {
		c.errf("title is longer than %d characters", maxTitleRunes)
		return false
	}
	return true
}

func (c *checker) summary(s string) {
	if utf8.RuneCountInString(s) > maxSummaryRunes {
		c.errf("summary is longer than %d characters", maxSummaryRunes)
	}
}

func (c *checker) dates(when, deadline string) {
	if when != "" && !validWhen(when) {
		c.errf("when %q is not today, tomorrow, evening, anytime, someday or YYYY-MM-DD[@HH:MM]", when)
	}
	if deadline != "" && !validDate(deadline) {
		c.errf("deadline %q is not YYYY-MM-DD", deadline)
	}
}

// change turns Fields into a Things change, resolving the list.
func (c *checker) change(f *Fields, it things.Item) (*things.Change, Target, bool) {
	if f == nil {
		c.errf("fields are required")
		return nil, Target{}, false
	}
	ch := &things.Change{Title: f.Title, Notes: f.Notes, When: f.When, Deadline: f.Deadline, Checklist: f.Checklist}
	ok := true
	if f.Title != nil && !c.title(*f.Title) {
		ok = false
	}
	if f.When != nil && *f.When != "" && !validWhen(*f.When) {
		c.errf("when %q is not today, tomorrow, evening, anytime, someday or YYYY-MM-DD[@HH:MM]", *f.When)
		ok = false
	}
	if f.When != nil && *f.When == "" {
		c.errf("when cannot be cleared; use anytime or someday")
		ok = false
	}
	if f.Deadline != nil && *f.Deadline != "" && !validDate(*f.Deadline) {
		c.errf("deadline %q is not YYYY-MM-DD", *f.Deadline)
		ok = false
	}
	if f.Tags != nil {
		tags := c.checkTags(*f.Tags)
		ch.Tags = &tags
	}
	if f.Notes != nil && it.Type != "" && things.MarkerIn(it.Notes) != "" && things.MarkerIn(*f.Notes) != things.MarkerIn(it.Notes) {
		c.errf("notes would drop the item's marker %s", things.MarkerIn(it.Notes))
		ok = false
	}
	var target Target
	if it.Type == things.TypeProject && (f.List != nil || f.Heading != nil || f.Checklist != nil) {
		c.errf("a project's list, heading or checklist cannot be changed here; use move_project for its area")
		return nil, target, false
	}
	if f.List != nil {
		k, found := c.list(*f.List, things.TypeArea, things.TypeProject)
		if !found {
			return nil, target, false
		}
		if k.IsZero() {
			c.errf("list cannot be empty: moving to the Inbox is not supported")
			return nil, target, false
		}
		target = k.Target
		if f.Heading != nil {
			h, found := c.heading(*f.Heading, k)
			if !found {
				return nil, target, false
			}
			ch.HeadingID = &h
		}
	} else if f.Heading != nil {
		proj, _ := c.list(it.Project, things.TypeProject)
		h, found := c.heading(*f.Heading, proj)
		if !found {
			return nil, target, false
		}
		ch.HeadingID = &h
	}
	if ch.Empty() && target.IsZero() {
		c.errf("fields change nothing")
		return nil, target, false
	}
	return ch, target, ok
}

func (c *checker) organize(id string, o Organize) {
	switch o.Op {
	case OpCreateArea:
		if !c.title(o.Title) {
			return
		}
		if _, dup := c.findContainer(o.Title, things.TypeArea); dup {
			c.errf("area %q already exists", o.Title)
			return
		}
		c.containers = append(c.containers, container{Target{Op: id}, things.TypeArea, o.Title})
		c.res.Counts.AreaChanges++
		c.add(Op{ID: id, Kind: KindCreateArea, Type: things.TypeArea, Title: o.Title,
			Describe: fmt.Sprintf("+ create area %q", o.Title)})
	case OpRenameArea:
		it, ok := c.existing(o.UUID, things.TypeArea)
		if !ok || !c.title(o.Title) {
			return
		}
		for i := range c.containers {
			if c.containers[i].UUID == o.UUID {
				c.containers[i].Title = o.Title
			}
		}
		c.existingChange()
		c.add(Op{ID: id, Kind: KindRenameArea, UUID: o.UUID, Type: things.TypeArea, Title: o.Title,
			Existing: true, Before: &it, Describe: fmt.Sprintf("~ rename area %q → %q", it.Title, o.Title)})
	case OpDeleteArea:
		it, ok := c.existing(o.UUID, things.TypeArea)
		if !ok {
			return
		}
		if n := c.areaContents(o.UUID); n > 0 {
			c.errf("area %q is not empty (%d item(s)); move or delete its contents first", it.Title, n)
			return
		}
		c.deleted[o.UUID] = true
		c.res.Counts.AreaChanges++
		c.res.ReorganizesLayout = true
		c.add(Op{ID: id, Kind: KindDeleteArea, UUID: o.UUID, Type: things.TypeArea, Existing: true,
			Before: &it, Describe: fmt.Sprintf("✗ delete empty area %q: %s", it.Title, o.Reason)})
	case OpCreateTag:
		if !c.title(o.Title) {
			return
		}
		if _, dup := c.tags[strings.ToLower(o.Title)]; dup {
			c.errf("tag %q already exists", o.Title)
			return
		}
		c.tags[strings.ToLower(o.Title)] = o.Title
		c.add(Op{ID: id, Kind: KindCreateTag, Type: things.TypeTag, Title: o.Title,
			Describe: fmt.Sprintf("+ create tag %q", o.Title)})
	case OpCreateProject:
		if !c.title(o.Title) {
			return
		}
		c.summary(o.Summary)
		area, ok := c.list(o.Area, things.TypeArea)
		if !ok {
			return
		}
		c.containers = append(c.containers, container{Target{Op: id}, things.TypeProject, o.Title})
		c.res.Counts.NewProjects++
		c.add(Op{ID: id, Kind: KindCreateProject, Type: things.TypeProject, Title: o.Title, Summary: o.Summary,
			Project: &things.NewProject{Title: o.Title}, List: area.Target,
			Describe: fmt.Sprintf("+ create project %q%s", o.Title, in(area.Title))})
	case OpMove:
		it, ok := c.existing(o.UUID, things.TypeTodo)
		if !ok {
			return
		}
		f := &Fields{List: &o.List}
		ch, target, ok := c.change(f, it)
		if !ok {
			return
		}
		c.trackMove(o.UUID, target)
		c.existingChange()
		c.add(Op{ID: id, Kind: KindUpdate, UUID: o.UUID, Type: it.Type, Change: ch, List: target, Existing: true,
			Before: &it, Describe: fmt.Sprintf("> move %q → %s", it.Title, c.targetTitle(target))})
	case OpMoveProject:
		it, ok := c.existing(o.UUID, things.TypeProject)
		if !ok {
			return
		}
		area, ok := c.list(o.Area, things.TypeArea)
		if !ok {
			return
		}
		if area.IsZero() {
			c.errf("area is required")
			return
		}
		c.trackMove(o.UUID, area.Target)
		c.existingChange()
		c.add(Op{ID: id, Kind: KindMoveProject, UUID: o.UUID, Type: things.TypeProject, List: area.Target,
			Existing: true, Before: &it, Describe: fmt.Sprintf("> move project %q → area %q", it.Title, area.Title)})
	case OpUpdate:
		it, ok := c.existing(o.UUID, things.TypeTodo, things.TypeProject)
		if !ok {
			return
		}
		ch, target, ok := c.change(o.Fields, it)
		if !ok {
			return
		}
		c.trackMove(o.UUID, target)
		c.existingChange()
		c.add(Op{ID: id, Kind: KindUpdate, UUID: o.UUID, Type: it.Type, Change: ch, List: target, Existing: true,
			Before: &it, Describe: fmt.Sprintf("~ update %s %q: %s", it.Type, it.Title, c.describeChange(ch, target))})
	case OpComplete, OpCancel:
		it, ok := c.existing(o.UUID, things.TypeTodo, things.TypeProject)
		if !ok {
			return
		}
		if it.Status != things.StatusIncomplete || c.stopped[o.UUID] {
			c.errf("%s %q is not open", it.Type, it.Title)
			return
		}
		kind := KindComplete
		if o.Op == OpCancel {
			kind = KindCancel
		}
		c.stop(Op{ID: id, Kind: kind}, it, because(o.Reason), c.existingChange)
	case OpDelete:
		it, ok := c.existing(o.UUID, things.TypeTodo, things.TypeProject)
		if !ok {
			return
		}
		if strings.TrimSpace(o.Reason) == "" {
			c.errf("reason is required")
			return
		}
		c.deleted[o.UUID] = true
		delete(c.areaOf, o.UUID)
		c.res.Counts.Deletes++
		c.res.ReorganizesLayout = true
		c.add(Op{ID: id, Kind: KindDelete, UUID: o.UUID, Type: it.Type, Existing: true, Before: &it,
			Describe: fmt.Sprintf("✗ delete %s %q to the Trash: %s", it.Type, it.Title, o.Reason)})
	case "empty_trash":
		c.errf("emptying the Trash is never allowed")
	default:
		c.errf("unknown op %q", o.Op)
	}
}

// stop adds op, a complete or cancel of the open item it, with count
// called once per item it stops. Things ignores completing or canceling a
// project that still has open to-dos, so a project's open to-dos are
// stopped first, as ops <op id>.c<n> linked to the same key as children.
func (c *checker) stop(op Op, it things.Item, why string, count func()) {
	sign := "✓ complete"
	if op.Kind == KindCancel {
		sign = "⊘ cancel"
	}
	if it.Type == things.TypeProject {
		n := 0
		for _, t := range c.snap.Todos {
			if t.Status != things.StatusIncomplete || c.stopped[t.UUID] || c.deleted[t.UUID] ||
				c.projectOf[t.UUID] != it.UUID {
				continue
			}
			n++
			c.stopped[t.UUID] = true
			count()
			child := op
			child.ID, child.UUID, child.Type, child.Existing, child.Before = fmt.Sprintf("%s.c%d", op.ID, n), t.UUID, t.Type, true, &t
			if child.Key != "" {
				child.Role = api.RoleChild
			}
			child.Describe = fmt.Sprintf("%s %s %q in project %q%s", sign, t.Type, t.Title, it.Title, why)
			c.add(child)
		}
	}
	c.stopped[it.UUID] = true
	count()
	op.UUID, op.Type, op.Existing, op.Before = it.UUID, it.Type, true, &it
	op.Describe = fmt.Sprintf("%s %s %q%s", sign, it.Type, it.Title, why)
	c.add(op)
}

// existingChange counts a move or change of an item that existed before
// the run, which also marks the plan as reorganizing the layout.
func (c *checker) existingChange() {
	c.res.Counts.Moves++
	c.res.ReorganizesLayout = true
}

func (c *checker) findContainer(title, typ string) (container, bool) {
	for _, k := range c.liveContainers() {
		if k.Type == typ && strings.EqualFold(k.Title, title) {
			return k, true
		}
	}
	return container{}, false
}

func (c *checker) trackMove(uuid string, t Target) {
	if t.IsZero() {
		return
	}
	for _, k := range c.containers {
		if k.Target == t {
			if k.Type == things.TypeArea && k.UUID != "" {
				c.areaOf[uuid] = k.UUID
			} else {
				delete(c.areaOf, uuid)
			}
			if k.Type == things.TypeProject && k.UUID != "" {
				c.projectOf[uuid] = k.UUID
			} else {
				delete(c.projectOf, uuid)
			}
		}
	}
}

func (c *checker) areaContents(area string) int {
	n := 0
	for item, a := range c.areaOf {
		if a == area && !c.deleted[item] {
			n++
		}
	}
	return n
}

func (c *checker) targetTitle(t Target) string {
	for _, k := range c.containers {
		if k.Target == t {
			return fmt.Sprintf("%s %q", k.Type, k.Title)
		}
	}
	return "Inbox"
}

func (c *checker) describeChange(ch *things.Change, t Target) string {
	var parts []string
	add := func(name string, v *string) {
		if v != nil {
			parts = append(parts, fmt.Sprintf("%s=%q", name, *v))
		}
	}
	add("title", ch.Title)
	add("when", ch.When)
	add("deadline", ch.Deadline)
	if ch.Notes != nil {
		parts = append(parts, "notes")
	}
	if ch.Tags != nil {
		parts = append(parts, "tags="+strings.Join(*ch.Tags, ","))
	}
	if !t.IsZero() {
		parts = append(parts, "list="+c.targetTitle(t))
	}
	if ch.HeadingID != nil {
		parts = append(parts, "heading")
	}
	if ch.Checklist != nil {
		parts = append(parts, fmt.Sprintf("checklist=%d item(s)", len(*ch.Checklist)))
	}
	if len(ch.AppendChecklist) > 0 {
		parts = append(parts, fmt.Sprintf("checklist+=%q", ch.AppendChecklist))
	}
	return strings.Join(parts, ", ")
}

func in(title string) string {
	if title == "" {
		return ""
	}
	return fmt.Sprintf(" in %q", title)
}

func because(reason string) string {
	if reason == "" {
		return ""
	}
	return ": " + reason
}

// live reports whether a linked ledger row still stands for a Things item.
func live(t api.LinkedThing) bool {
	return t.State == api.ThingPendingCreate || t.State == api.ThingOpen
}

func (c *checker) source(item api.LeasedItem) *things.SourceLine {
	src := &things.SourceLine{Source: item.Source, SourceID: item.SourceID, Kind: item.Kind}
	if item.Event != nil {
		src.URL = item.Event.URL
	}
	return src
}

func (c *checker) newTodo(t Todo, list container) (*things.NewTodo, bool) {
	if !c.title(t.Title) {
		return nil, false
	}
	c.summary(t.Summary)
	c.dates(t.When, t.Deadline)
	head, ok := c.heading(t.Heading, list)
	if !ok {
		return nil, false
	}
	if len(t.Checklist) > checklistHint+2 {
		c.warnf("checklist has %d steps; POLICY.md prefers a project above about %d", len(t.Checklist), checklistHint)
	}
	return &things.NewTodo{Title: t.Title, When: t.When, Deadline: t.Deadline, Tags: c.checkTags(t.Tags),
		Checklist: t.Checklist, HeadingID: head}, true
}

func (c *checker) decision(id string, d Decision) {
	item, ok := c.in.Item(d.Key)
	if !ok {
		c.errf("key is not among the leased items")
		return
	}
	if d.Revision != "" && d.Revision != item.Revision {
		c.errf("revision %q is not the leased revision %q", d.Revision, item.Revision)
		return
	}
	ans := Answer{Key: d.Key, Revision: item.Revision, Action: d.Action, Outcome: actionOutcome[d.Action], Note: d.Reason}
	var linked []api.LinkedThing
	for _, t := range item.Things {
		if live(t) {
			linked = append(linked, t)
		}
	}
	creates := d.Action == ActCreate || d.Action == ActProject
	if creates && len(linked) > 0 {
		c.errf("key already has a live Things item (%s); attach, update or resolve it instead", linked[0].UUID+" "+linked[0].Marker)
		return
	}
	if creates && item.Op == api.OpResolve {
		c.errf("the source resolved this item; resolve, ignore or defer it")
		return
	}
	if item.Op == api.OpResolve && item.Source == api.SourceGitHub && d.Action != ActResolve && d.Action != ActDefer {
		for _, t := range linked {
			if t.Type == api.ThingTodo || t.Type == api.ThingProject {
				c.errf("POLICY.md: a GitHub item resolved at the source completes its open Things item; use resolve")
				return
			}
		}
	}
	nOps := len(c.res.Ops)

	switch d.Action {
	case ActCreate:
		if d.Todo == nil {
			c.errf("todo is required")
			return
		}
		list, ok := c.list(d.Todo.List, things.TypeArea, things.TypeProject)
		if !ok {
			return
		}
		nt, ok := c.newTodo(*d.Todo, list)
		if !ok {
			return
		}
		c.res.Counts.NewTodos++
		c.add(Op{ID: id, Kind: KindCreateTodo, Key: d.Key, Role: api.RolePrimary, Type: things.TypeTodo,
			Title: d.Todo.Title, Summary: d.Todo.Summary, Source: c.source(item), Todo: nt, List: list.Target,
			Describe: fmt.Sprintf("+ create to-do %q%s%s", d.Todo.Title, in(list.Title), todoExtras(nt))})
	case ActProject:
		if d.Project == nil {
			c.errf("project is required")
			return
		}
		if len(d.Todos) == 0 {
			c.errf("a project needs its steps in todos")
			return
		}
		if len(d.Todos) <= checklistHint {
			c.warnf("%d steps; POLICY.md prefers one to-do with a checklist up to about %d", len(d.Todos), checklistHint)
		}
		if !c.title(d.Project.Title) {
			return
		}
		c.summary(d.Project.Summary)
		c.dates(d.Project.When, d.Project.Deadline)
		area, ok := c.list(d.Project.Area, things.TypeArea)
		if !ok {
			return
		}
		np := &things.NewProject{Title: d.Project.Title, When: d.Project.When, Deadline: d.Project.Deadline,
			Tags: c.checkTags(d.Project.Tags)}
		c.containers = append(c.containers, container{Target{Op: id}, things.TypeProject, d.Project.Title})
		c.res.Counts.NewProjects++
		c.add(Op{ID: id, Kind: KindCreateProject, Key: d.Key, Role: api.RolePrimary, Type: things.TypeProject,
			Title: d.Project.Title, Summary: d.Project.Summary, Source: c.source(item), Project: np, List: area.Target,
			Describe: fmt.Sprintf("+ create project %q%s with %d to-dos", d.Project.Title, in(area.Title), len(d.Todos))})
		parent := container{Target{Op: id}, things.TypeProject, d.Project.Title}
		for k, t := range d.Todos {
			if t.List != "" || t.Heading != "" {
				c.errf("todos[%d]: a step goes into the new project; leave list and heading empty", k)
				continue
			}
			nt, ok := c.newTodo(t, container{})
			if !ok {
				continue
			}
			c.res.Counts.NewTodos++
			c.add(Op{ID: fmt.Sprintf("%s.%d", id, k+1), Kind: KindCreateTodo, Key: d.Key, Role: api.RoleChild,
				Type: things.TypeTodo, Title: t.Title, Summary: t.Summary, Source: c.source(item), Todo: nt,
				List: parent.Target, Describe: fmt.Sprintf("  + step %q%s", t.Title, todoExtras(nt))})
		}
	case ActAttach:
		it, ok := c.existing(d.UUID, things.TypeTodo)
		if !ok {
			return
		}
		ch := &things.Change{AppendChecklist: d.AppendChecklist}
		var target Target
		if d.Fields != nil {
			var fch *things.Change
			if fch, target, ok = c.change(d.Fields, it); !ok {
				return
			}
			fch.AppendChecklist = d.AppendChecklist
			if fch.Checklist != nil && len(d.AppendChecklist) > 0 {
				c.errf("set either fields.checklist or append_checklist, not both")
				return
			}
			ch = fch
		}
		c.trackMove(d.UUID, target)
		op := Op{ID: id, Kind: KindUpdate, Key: d.Key, Role: api.RoleMerged, UUID: d.UUID, Type: it.Type,
			Change: ch, List: target, Existing: true, Before: &it,
			Describe: fmt.Sprintf("~ attach to %q", it.Title)}
		if !ch.Empty() || !target.IsZero() {
			op.Describe += ": " + c.describeChange(ch, target)
		} else {
			op.Change = nil
		}
		c.add(op)
	case ActUpdate:
		it, ok := c.existing(d.UUID, things.TypeTodo, things.TypeProject)
		if !ok {
			return
		}
		ch, target, ok := c.change(d.Fields, it)
		if !ok {
			return
		}
		c.trackMove(d.UUID, target)
		c.res.Counts.Moves++
		c.add(Op{ID: id, Kind: KindUpdate, Key: d.Key, Role: api.RolePrimary, UUID: d.UUID, Type: it.Type,
			Change: ch, List: target, Existing: true, Before: &it,
			Describe: fmt.Sprintf("~ update %s %q: %s", it.Type, it.Title, c.describeChange(ch, target))})
	case ActResolve:
		if d.Complete && d.Cancel {
			c.errf("set complete or cancel, not both")
			return
		}
		uuids := []string{d.UUID}
		if d.UUID == "" {
			uuids = nil
			for _, t := range linked {
				if t.UUID != "" && t.Role != api.RoleChild && (t.Type == api.ThingTodo || t.Type == api.ThingProject) {
					uuids = append(uuids, t.UUID)
				}
			}
			if len(uuids) == 0 {
				c.errf("no live Things item is linked to this key; give a uuid or use ignore")
				return
			}
		}
		kind := KindComplete
		if d.Cancel {
			kind = KindCancel
		}
		for k, u := range uuids {
			it, ok := c.existing(u, things.TypeTodo, things.TypeProject)
			if !ok {
				continue
			}
			if c.stopped[u] {
				c.warnf("%s %q is already completed or canceled earlier in this plan; nothing to write", it.Type, it.Title)
				continue
			}
			if it.Status != things.StatusIncomplete {
				c.warnf("%s %q is already %s; nothing to write", it.Type, it.Title, it.Status)
				continue
			}
			opID := id
			if len(uuids) > 1 {
				opID = fmt.Sprintf("%s.%d", id, k+1)
			}
			c.stop(Op{ID: opID, Kind: kind, Key: d.Key, Role: api.RolePrimary}, it, " (source resolved)",
				func() { c.res.Counts.Completes++ })
		}
	case ActIgnore:
		if strings.TrimSpace(d.Reason) == "" {
			c.errf("reason is required")
			return
		}
		c.res.Counts.Ignored++
	case ActDefer:
		if strings.TrimSpace(d.Reason) == "" {
			c.errf("reason is required")
			return
		}
		if d.NotBefore != "" {
			if _, err := time.Parse(time.RFC3339, d.NotBefore); err != nil {
				c.errf("not_before is not RFC 3339")
				return
			}
		}
		ans.NotBefore = d.NotBefore
		c.res.Counts.Deferred++
	default:
		c.errf("unknown action %q", d.Action)
		return
	}
	for _, op := range c.res.Ops[nOps:] {
		ans.Ops = append(ans.Ops, op.ID)
	}
	c.res.Answers = append(c.res.Answers, ans)
}

func todoExtras(t *things.NewTodo) string {
	var parts []string
	if t.When != "" {
		parts = append(parts, "when "+t.When)
	}
	if t.Deadline != "" {
		parts = append(parts, "deadline "+t.Deadline)
	}
	if len(t.Tags) > 0 {
		parts = append(parts, "tags "+strings.Join(t.Tags, ","))
	}
	if len(t.Checklist) > 0 {
		parts = append(parts, fmt.Sprintf("%d checklist item(s)", len(t.Checklist)))
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, "; ") + ")"
}
