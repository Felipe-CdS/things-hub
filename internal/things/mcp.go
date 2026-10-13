package things

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// RequiredTools are the things-mcp tools thub calls. FastMCP names tools
// after the Python functions; ToolNames also accepts the README's
// hyphenated spelling.
var RequiredTools = []string{
	"get_todos", "get_projects", "get_areas", "get_tags", "get_headings",
	"get_logbook", "get_trash", "search_todos", "search_advanced",
	"add_todo", "add_project", "update_todo", "update_project", "add_area", "update_area",
}

// MCP is a things-mcp session over stdio.
type MCP struct {
	session *mcp.ClientSession
	// names maps a canonical tool name to the name the server lists.
	names map[string]string
}

// DialMCP starts things-mcp with command (for example uvx things-mcp) and
// opens an MCP session to it.
func DialMCP(ctx context.Context, command []string) (*MCP, error) {
	if len(command) == 0 {
		return nil, errors.New("things-mcp command is empty")
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "thub", Version: "1"}, nil)
	cmd := exec.Command(command[0], command[1:]...)
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		return nil, fmt.Errorf("start things-mcp (%s): %w", strings.Join(command, " "), err)
	}
	m := &MCP{session: session, names: map[string]string{}}
	tools, err := m.ToolNames(ctx)
	if err != nil {
		session.Close()
		return nil, err
	}
	for _, t := range tools {
		m.names[strings.ReplaceAll(t, "-", "_")] = t
	}
	if missing := m.Missing(); len(missing) > 0 {
		session.Close()
		return nil, fmt.Errorf("things-mcp lacks tools: %s", strings.Join(missing, ", "))
	}
	return m, nil
}

// ToolNames lists the server's tool names.
func (m *MCP) ToolNames(ctx context.Context) ([]string, error) {
	var names []string
	for tool, err := range m.session.Tools(ctx, nil) {
		if err != nil {
			return nil, fmt.Errorf("list things-mcp tools: %w", err)
		}
		names = append(names, tool.Name)
	}
	sort.Strings(names)
	return names, nil
}

// Missing returns the required tools the server does not list.
func (m *MCP) Missing() []string {
	var out []string
	for _, t := range RequiredTools {
		if _, ok := m.names[t]; !ok {
			out = append(out, t)
		}
	}
	return out
}

// Close ends the session and stops things-mcp.
func (m *MCP) Close() error { return m.session.Close() }

// call runs a tool and returns its result, turning tool errors into Go
// errors.
func (m *MCP) call(ctx context.Context, tool string, args map[string]any) (*mcp.CallToolResult, error) {
	name := m.names[tool]
	if name == "" {
		name = tool
	}
	for k, v := range args {
		if v == nil {
			delete(args, k)
		}
	}
	res, err := m.session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return nil, fmt.Errorf("things-mcp %s: %w", tool, err)
	}
	if res.IsError {
		return nil, fmt.Errorf("things-mcp %s: %s", tool, resultText(res))
	}
	return res, nil
}

func resultText(res *mcp.CallToolResult) string {
	var parts []string
	for _, c := range res.Content {
		if t, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, t.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// listResult is the structured content of things-mcp's list tools.
type listResult struct {
	Items []Item `json:"items"`
	Total int    `json:"total"`
	Error string `json:"error"`
}

// list calls a read tool and decodes its structured items.
func (m *MCP) list(ctx context.Context, tool string, args map[string]any) ([]Item, error) {
	if args == nil {
		args = map[string]any{}
	}
	res, err := m.call(ctx, tool, args)
	if err != nil {
		return nil, err
	}
	if res.StructuredContent == nil {
		return nil, fmt.Errorf("things-mcp %s returned no structured content: %s", tool, resultText(res))
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		return nil, err
	}
	var out listResult
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("things-mcp %s: decode items: %w", tool, err)
	}
	if out.Error != "" {
		return nil, fmt.Errorf("things-mcp %s: %s", tool, out.Error)
	}
	if out.Items == nil {
		out.Items = []Item{}
	}
	return out.Items, nil
}

// snapshotLimit bounds the logbook read; things-mcp defaults it to 50.
const snapshotLimit = 100000

// Snapshot exports Things through the read tools.
func (m *MCP) Snapshot(ctx context.Context, days int) (*Snapshot, error) {
	s := &Snapshot{Schema: SnapshotSchema, TakenAt: time.Now().UTC()}
	var err error
	if s.Areas, err = m.list(ctx, "get_areas", nil); err != nil {
		return nil, err
	}
	if s.Projects, err = m.list(ctx, "get_projects", nil); err != nil {
		return nil, err
	}
	if s.Headings, err = m.list(ctx, "get_headings", nil); err != nil {
		return nil, err
	}
	if s.Tags, err = m.list(ctx, "get_tags", nil); err != nil {
		return nil, err
	}
	if s.Todos, err = m.list(ctx, "get_todos", map[string]any{"include_items": true}); err != nil {
		return nil, err
	}
	if s.Completed, err = m.list(ctx, "get_logbook", map[string]any{
		"period": fmt.Sprintf("%dd", days), "limit": snapshotLimit,
	}); err != nil {
		return nil, err
	}
	canceled, err := m.list(ctx, "search_advanced", map[string]any{"status": StatusCanceled})
	if err != nil {
		return nil, err
	}
	s.Canceled = stoppedWithin(canceled, days)
	trashed, err := m.list(ctx, "get_trash", nil)
	if err != nil {
		return nil, err
	}
	s.Trashed = trashed
	return s, nil
}

// stoppedWithin keeps items stopped in the last days, or with no stop date.
func stoppedWithin(items []Item, days int) []Item {
	cutoff := time.Now().AddDate(0, 0, -days)
	out := []Item{}
	for _, it := range items {
		t, err := time.ParseInLocation(time.DateTime, it.StopDate, time.Local)
		if it.StopDate == "" || err != nil || !t.Before(cutoff) {
			out = append(out, it)
		}
	}
	return out
}

// FindByMarker searches open to-dos and projects for the marker.
func (m *MCP) FindByMarker(ctx context.Context, marker string) (*Item, error) {
	items, err := m.list(ctx, "search_todos", map[string]any{"query": marker})
	if err != nil {
		return nil, err
	}
	for _, it := range items {
		if MarkerIn(it.Notes) == marker && !it.Trashed {
			return &it, nil
		}
	}
	return nil, nil
}

func optStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func optList(l []string) any {
	if len(l) == 0 {
		return nil
	}
	return l
}

// AddTodo creates a to-do through add_todo.
func (m *MCP) AddTodo(ctx context.Context, t NewTodo) error {
	_, err := m.call(ctx, "add_todo", map[string]any{
		"title": t.Title, "notes": optStr(t.Notes), "when": optStr(t.When), "deadline": optStr(t.Deadline),
		"tags": optList(t.Tags), "checklist_items": optList(t.Checklist), "list_id": optStr(t.ListID),
		"heading_id": optStr(t.HeadingID),
	})
	return err
}

// AddProject creates a project through add_project, without its todos
// parameter.
func (m *MCP) AddProject(ctx context.Context, p NewProject) error {
	_, err := m.call(ctx, "add_project", map[string]any{
		"title": p.Title, "notes": optStr(p.Notes), "when": optStr(p.When), "deadline": optStr(p.Deadline),
		"tags": optList(p.Tags), "area_id": optStr(p.AreaID),
	})
	return err
}

func changeArgs(uuid string, c Change) map[string]any {
	args := map[string]any{"id": uuid}
	set := func(k string, v *string) {
		if v != nil {
			args[k] = *v
		}
	}
	set("title", c.Title)
	set("notes", c.Notes)
	set("when", c.When)
	set("deadline", c.Deadline)
	if c.Tags != nil {
		args["tags"] = *c.Tags
	}
	if c.Completed != nil {
		args["completed"] = *c.Completed
	}
	if c.Canceled != nil {
		args["canceled"] = *c.Canceled
	}
	return args
}

// UpdateTodo changes a to-do through update_todo.
func (m *MCP) UpdateTodo(ctx context.Context, uuid string, c Change) error {
	args := changeArgs(uuid, c)
	if c.ListID != nil {
		args["list_id"] = *c.ListID
	}
	if c.HeadingID != nil {
		args["heading_id"] = *c.HeadingID
	}
	if c.Checklist != nil {
		args["checklist_items"] = *c.Checklist
	}
	if len(c.AppendChecklist) > 0 {
		args["append_checklist_items"] = c.AppendChecklist
	}
	_, err := m.call(ctx, "update_todo", args)
	return err
}

// UpdateProject changes a project through update_project. Moving a project
// to another area is MoveProject.
func (m *MCP) UpdateProject(ctx context.Context, uuid string, c Change) error {
	if c.ListID != nil || c.HeadingID != nil || c.Checklist != nil || len(c.AppendChecklist) > 0 {
		return fmt.Errorf("update_project: list, heading and checklist changes: %w", ErrNotSupported)
	}
	_, err := m.call(ctx, "update_project", changeArgs(uuid, c))
	return err
}

var areaIDRe = regexp.MustCompile(`\(id: ([^)\s]+)\)`)

// AddArea creates an area through add_area and returns the id it reports.
func (m *MCP) AddArea(ctx context.Context, title string) (string, error) {
	res, err := m.call(ctx, "add_area", map[string]any{"title": title})
	if err != nil {
		return "", err
	}
	match := areaIDRe.FindStringSubmatch(resultText(res))
	if match == nil {
		return "", fmt.Errorf("add_area returned no id: %q", resultText(res))
	}
	return match[1], nil
}

// RenameArea renames an area through update_area.
func (m *MCP) RenameArea(ctx context.Context, uuid, title string) error {
	_, err := m.call(ctx, "update_area", map[string]any{"id": uuid, "title": title})
	return err
}

// Areas lists the areas.
func (m *MCP) Areas(ctx context.Context) ([]Item, error) { return m.list(ctx, "get_areas", nil) }

// Tags lists the tags.
func (m *MCP) Tags(ctx context.Context) ([]Item, error) { return m.list(ctx, "get_tags", nil) }
