package plan

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Felipe-CdS/things-hub/api"
	"github.com/Felipe-CdS/things-hub/internal/things"
)

const (
	keyA = "github:felipe-cds:review-requested:felipe-cds/mic#12"
	keyB = "github:felipe-cds:assigned:felipe-cds/mic#7"
)

func input() *Input {
	return &Input{
		Schema: InputSchema, RunID: "2026-10-13T14-02-11Z", LeaseID: "l_1",
		Items: []api.LeasedItem{
			{Key: keyA, Source: "github", Kind: "review-requested", SourceID: "felipe-cds/mic#12", Op: api.OpUpsert,
				Revision: "2026-10-13T11:50:02Z", Event: &api.Event{URL: "https://github.com/felipe-cds/mic/pull/12"}},
			{Key: keyB, Source: "github", Kind: "assigned", SourceID: "felipe-cds/mic#7", Op: api.OpUpsert,
				Revision: "2026-10-13T11:00:00Z"},
		},
		Snapshot: &things.Snapshot{
			Areas:    []things.Item{{UUID: "AREA-WORK", Type: things.TypeArea, Title: "Work"}, {UUID: "AREA-OLD", Type: things.TypeArea, Title: "Old"}},
			Projects: []things.Item{{UUID: "PROJ-1", Type: things.TypeProject, Title: "Site", Area: "AREA-WORK", Status: things.StatusIncomplete}},
			Headings: []things.Item{{UUID: "HEAD-1", Type: things.TypeHeading, Title: "Later", Project: "PROJ-1"}},
			Tags:     []things.Item{{Type: things.TypeTag, Title: "github"}},
			Todos: []things.Item{
				{UUID: "TODO-1", Type: things.TypeTodo, Title: "Old task", Area: "AREA-OLD", Status: things.StatusIncomplete},
				{UUID: "TODO-2", Type: things.TypeTodo, Title: "Other", Status: things.StatusIncomplete},
			},
		},
	}
}

func newPlan(in *Input) *Plan {
	return &Plan{Schema: PlanSchema, RunID: in.RunID, LeaseID: in.LeaseID}
}

func create(key, title string) Decision {
	return Decision{Key: key, Action: ActCreate, Todo: &Todo{Title: title}}
}

func errorsContain(t *testing.T, res *Result, want string) {
	t.Helper()
	for _, e := range res.Errors {
		if strings.Contains(e, want) {
			return
		}
	}
	t.Fatalf("no error containing %q in %q", want, res.Errors)
}

func TestValidPlanExpandsInOrder(t *testing.T) {
	in := input()
	p := newPlan(in)
	p.Organize = []Organize{
		{Op: OpCreateArea, Title: "University"},
		{Op: OpCreateTag, Title: "waiting"},
		{Op: OpMove, UUID: "TODO-1", List: "University"},
		{Op: OpDeleteArea, UUID: "AREA-OLD"},
	}
	p.Items = []Decision{
		{Key: keyA, Action: ActCreate, Todo: &Todo{Title: "Review PR", List: "Site", Heading: "Later",
			When: "today", Tags: []string{"github", "waiting"}}},
		{Key: keyB, Action: ActIgnore, Reason: "FYI"},
	}
	res := Check(in, p)
	if !res.OK(false) {
		t.Fatalf("errors %q over %q", res.Errors, res.OverLimits)
	}
	var ids []string
	for _, op := range res.Ops {
		ids = append(ids, op.ID+" "+op.Kind)
	}
	want := []string{
		"2026-10-13T14-02-11Z/o1 create_area", "2026-10-13T14-02-11Z/o2 create_tag",
		"2026-10-13T14-02-11Z/o3 update", "2026-10-13T14-02-11Z/o4 delete_area",
		"2026-10-13T14-02-11Z/i1 create_todo",
	}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Fatalf("ops %q", ids)
	}
	if res.Ops[2].List.Op != "2026-10-13T14-02-11Z/o1" {
		t.Fatalf("move targets %+v, want the area created by o1", res.Ops[2].List)
	}
	if res.Ops[4].Todo.HeadingID != "HEAD-1" || res.Ops[4].List.UUID != "PROJ-1" {
		t.Fatalf("create resolved to %+v %+v", res.Ops[4].List, res.Ops[4].Todo)
	}
	if !res.ReorganizesLayout || res.Counts.AreaChanges != 2 || res.Counts.NewTodos != 1 {
		t.Fatalf("counts %+v layout %v", res.Counts, res.ReorganizesLayout)
	}
	if len(res.Answers) != 2 || res.Answers[0].Outcome != api.OutcomeCreated || res.Answers[1].Outcome != api.OutcomeIgnored {
		t.Fatalf("answers %+v", res.Answers)
	}
}

func TestRefusals(t *testing.T) {
	cases := []struct {
		name string
		edit func(in *Input, p *Plan)
		want string
	}{
		{"unknown uuid", func(in *Input, p *Plan) {
			p.Organize = []Organize{{Op: OpComplete, UUID: "NOPE"}}
		}, "unknown uuid NOPE"},
		{"unknown area", func(in *Input, p *Plan) {
			p.Items = []Decision{{Key: keyA, Action: ActCreate, Todo: &Todo{Title: "x", List: "Garden"}}}
		}, `unknown area or project "Garden"`},
		{"unknown tag", func(in *Input, p *Plan) {
			p.Items = []Decision{{Key: keyA, Action: ActCreate, Todo: &Todo{Title: "x", Tags: []string{"later"}}}}
		}, `unknown tag "later"`},
		{"tag created later in the plan", func(in *Input, p *Plan) {
			p.Organize = []Organize{{Op: OpCreateTag, Title: "github"}}
		}, `tag "github" already exists`},
		{"second create for a linked key", func(in *Input, p *Plan) {
			in.Items[0].Things = []api.LinkedThing{{Role: api.RolePrimary, UUID: "TODO-2", Type: api.ThingTodo, State: api.ThingOpen}}
			p.Items = []Decision{create(keyA, "again")}
		}, "already has a live Things item"},
		{"a pending create counts as live", func(in *Input, p *Plan) {
			in.Items[0].Things = []api.LinkedThing{{Role: api.RolePrimary, Marker: "th-aaaaaaaaaa", Type: api.ThingTodo, State: api.ThingPendingCreate}}
			p.Items = []Decision{create(keyA, "again")}
		}, "already has a live Things item"},
		{"two decisions for one key", func(in *Input, p *Plan) {
			p.Items = []Decision{create(keyA, "one"), create(keyA, "two")}
		}, "a second decision for the same key"},
		{"key not leased", func(in *Input, p *Plan) {
			p.Items = []Decision{create("github:felipe-cds:assigned:felipe-cds/x#1", "x")}
		}, "not among the leased items"},
		{"non-empty area", func(in *Input, p *Plan) {
			p.Organize = []Organize{{Op: OpDeleteArea, UUID: "AREA-WORK"}}
		}, "is not empty"},
		{"empty the trash", func(in *Input, p *Plan) {
			p.Organize = []Organize{{Op: "empty_trash"}}
		}, "never allowed"},
		{"delete without reason", func(in *Input, p *Plan) {
			p.Organize = []Organize{{Op: OpDelete, UUID: "TODO-2"}}
		}, "reason is required"},
		{"ignore without reason", func(in *Input, p *Plan) {
			p.Items = []Decision{{Key: keyA, Action: ActIgnore}}
		}, "reason is required"},
		{"bad when", func(in *Input, p *Plan) {
			p.Items = []Decision{{Key: keyA, Action: ActCreate, Todo: &Todo{Title: "x", When: "next week"}}}
		}, "when \"next week\""},
		{"wrong lease", func(in *Input, p *Plan) { p.LeaseID = "l_2" }, "lease_id"},
		{"wrong revision", func(in *Input, p *Plan) {
			d := create(keyA, "x")
			d.Revision = "2026-10-13T00:00:00Z"
			p.Items = []Decision{d}
		}, "not the leased revision"},
		{"use of a deleted item", func(in *Input, p *Plan) {
			p.Organize = []Organize{{Op: OpDelete, UUID: "TODO-2", Reason: "dup"}, {Op: OpComplete, UUID: "TODO-2"}}
		}, "is in the Trash"},
		{"github resolve must complete", func(in *Input, p *Plan) {
			in.Items[0].Op = api.OpResolve
			in.Items[0].Things = []api.LinkedThing{{Role: api.RolePrimary, UUID: "TODO-2", Type: api.ThingTodo, State: api.ThingOpen}}
			p.Items = []Decision{{Key: keyA, Action: ActIgnore, Reason: "merged"}}
		}, "use resolve"},
		{"notes must keep the marker", func(in *Input, p *Plan) {
			in.Snapshot.Todos[1].Notes = "x\nth-abcdefghij"
			p.Organize = []Organize{{Op: OpUpdate, UUID: "TODO-2", Fields: &Fields{Notes: ptr("new notes")}}}
		}, "drop the item's marker"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := input()
			p := newPlan(in)
			c.edit(in, p)
			res := Check(in, p)
			if res.OK(true) {
				t.Fatal("plan was accepted")
			}
			errorsContain(t, res, c.want)
		})
	}
}

func TestDeleteAreaAfterMovingItsContents(t *testing.T) {
	in := input()
	p := newPlan(in)
	p.Organize = []Organize{
		{Op: OpMove, UUID: "TODO-1", List: "Work"},
		{Op: OpDeleteArea, UUID: "AREA-OLD"},
	}
	if res := Check(in, p); !res.OK(false) {
		t.Fatalf("errors %q", res.Errors)
	}
}

func TestLimits(t *testing.T) {
	in := input()
	in.Items = nil
	for i := range 16 {
		in.Items = append(in.Items, api.LeasedItem{Key: fmt.Sprintf("github:felipe-cds:assigned:felipe-cds/r#%d", i),
			Source: "github", Op: api.OpUpsert, Revision: "2026-10-13T10:00:00Z"})
	}
	p := newPlan(in)
	for _, it := range in.Items[:15] {
		p.Items = append(p.Items, create(it.Key, "t"))
	}
	res := Check(in, p)
	if !res.OK(false) || res.Counts.NewTodos != 15 {
		t.Fatalf("15 new to-dos refused: %q %q", res.Errors, res.OverLimits)
	}
	if len(res.Unplanned) != 1 {
		t.Fatalf("unplanned %v", res.Unplanned)
	}

	p.Items = append(p.Items, create(in.Items[15].Key, "t"))
	res = Check(in, p)
	if res.OK(false) || len(res.OverLimits) != 1 || !strings.Contains(res.OverLimits[0], "new to-dos: 16, limit 15") {
		t.Fatalf("16 new to-dos: over %q", res.OverLimits)
	}
	if !res.OK(true) {
		t.Fatal("--allow-over-limits did not accept it")
	}

	in.Limits = Limits{NewTodos: 20}
	if res := Check(in, p); !res.OK(false) {
		t.Fatalf("a raised limit still refused: %q", res.OverLimits)
	}

	// A project's steps count as new to-dos.
	in.Limits = Limits{NewTodos: 3}
	p.Items = []Decision{{Key: in.Items[0].Key, Action: ActProject, Project: &Project{Title: "Big"},
		Todos: []Todo{{Title: "1"}, {Title: "2"}, {Title: "3"}, {Title: "4"}, {Title: "5"}, {Title: "6"}}}}
	res = Check(in, p)
	if res.Counts.NewTodos != 6 || res.OK(false) {
		t.Fatalf("project steps: counts %+v over %q", res.Counts, res.OverLimits)
	}
}

func TestOtherLimits(t *testing.T) {
	in := input()
	in.Limits = Limits{Deletes: 1, Moves: 1, AreaChanges: 1}
	p := newPlan(in)
	p.Organize = []Organize{
		{Op: OpDelete, UUID: "TODO-1", Reason: "a"}, {Op: OpDelete, UUID: "TODO-2", Reason: "b"},
		{Op: OpMoveProject, UUID: "PROJ-1", Area: "Old"}, {Op: OpRenameArea, UUID: "AREA-WORK", Title: "Job"},
		{Op: OpCreateArea, Title: "A"}, {Op: OpCreateArea, Title: "B"},
	}
	res := Check(in, p)
	if len(res.Errors) != 0 || len(res.OverLimits) != 3 {
		t.Fatalf("errors %q over %q", res.Errors, res.OverLimits)
	}
}

func TestProjectStepsAndPolicyWarnings(t *testing.T) {
	in := input()
	p := newPlan(in)
	p.Items = []Decision{{Key: keyA, Action: ActProject, Project: &Project{Title: "Ship", Area: "Work"},
		Todos: []Todo{{Title: "a"}, {Title: "b"}}}}
	res := Check(in, p)
	if !res.OK(false) || len(res.Ops) != 3 || res.Ops[1].ID != in.RunID+"/i1.1" || res.Ops[1].List.Op != in.RunID+"/i1" ||
		res.Ops[1].Role != api.RoleChild {
		t.Fatalf("ops %+v errors %q", res.Ops, res.Errors)
	}
	if len(res.Warnings) == 0 || !strings.Contains(strings.Join(res.Warnings, " "), "checklist up to about 5") {
		t.Fatalf("warnings %q", res.Warnings)
	}
}

func TestResolveDefaultsToLinkedItems(t *testing.T) {
	in := input()
	in.Items[0].Op = api.OpResolve
	in.Items[0].Things = []api.LinkedThing{{Role: api.RolePrimary, UUID: "TODO-2", Type: api.ThingTodo, State: api.ThingOpen}}
	p := newPlan(in)
	p.Items = []Decision{{Key: keyA, Action: ActResolve}}
	res := Check(in, p)
	if !res.OK(false) || len(res.Ops) != 1 || res.Ops[0].Kind != KindComplete || res.Ops[0].UUID != "TODO-2" {
		t.Fatalf("ops %+v errors %q", res.Ops, res.Errors)
	}
	if res.ReorganizesLayout {
		t.Fatal("resolving a hub to-do marked the plan as a layout change")
	}
}

func ptr[T any](v T) *T { return &v }
