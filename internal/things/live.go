package things

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"strings"
)

// Exec runs a command and returns its standard output.
type Exec func(ctx context.Context, name string, args ...string) ([]byte, error)

// RunCommand is the default Exec.
func RunCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// Live is the Things client used on the Mac: things-mcp for reads and most
// writes, the Things database for lookups and the token, and AppleScript or
// the URL scheme for the gaps.
type Live struct {
	*MCP
	DB   *DB
	Exec Exec
	// URLToken is THINGS_AUTH_TOKEN from the thub .env. When empty, the
	// token comes from the Things database.
	URLToken string
}

// authToken returns the URL-scheme auth token: the .env's when set, else the
// Things database's. Callers must never print it.
func (l *Live) authToken(ctx context.Context) (string, error) {
	if l.URLToken != "" {
		return l.URLToken, nil
	}
	return l.DB.AuthToken(ctx)
}

// Lookup reads one item from the Things database.
func (l *Live) Lookup(ctx context.Context, uuid string) (*State, error) {
	return l.DB.Lookup(ctx, uuid)
}

// Close stops things-mcp and closes the database.
func (l *Live) Close() error {
	return errors.Join(l.MCP.Close(), l.DB.Close())
}

// osascript runs an AppleScript whose values come only from argv, so no
// value is ever spliced into the script text.
func (l *Live) osascript(ctx context.Context, body string, args ...string) error {
	script := "on run argv\ntell application \"Things3\"\n" + body + "\nend tell\nend run"
	_, err := l.Exec(ctx, "osascript", append([]string{"-e", script}, args...)...)
	return err
}

// AddTag creates a tag: things-mcp has no tool for it.
func (l *Live) AddTag(ctx context.Context, title string) error {
	return l.osascript(ctx, `make new tag with properties {name:(item 1 of argv)}`, title)
}

// DeleteTag deletes a tag by name.
func (l *Live) DeleteTag(ctx context.Context, title string) error {
	return l.osascript(ctx, `delete tag (item 1 of argv)`, title)
}

var appleClass = map[string]string{TypeTodo: "to do", TypeProject: "project", TypeArea: "area"}

// Delete moves a to-do or project to the Trash, or deletes an area.
// Things' AppleScript delete never empties the Trash.
func (l *Live) Delete(ctx context.Context, uuid, typ string) error {
	class := appleClass[typ]
	if class == "" {
		return fmt.Errorf("delete %s: %w", typ, ErrNotSupported)
	}
	return l.osascript(ctx, `delete `+class+` id (item 1 of argv)`, uuid)
}

// Restore moves a trashed to-do or project back out of the Trash: first to
// the Inbox (to-dos) or Anytime (projects), then into listID when given.
func (l *Live) Restore(ctx context.Context, uuid, typ, listID string) error {
	var body string
	switch typ {
	case TypeTodo:
		body = `set t to to do id (item 1 of argv)
move t to list "Inbox"
if (item 2 of argv) is not "" then
	try
		set project of t to project id (item 2 of argv)
	on error
		set area of t to area id (item 2 of argv)
	end try
end if`
	case TypeProject:
		body = `set p to project id (item 1 of argv)
move p to list "Anytime"
if (item 2 of argv) is not "" then set area of p to area id (item 2 of argv)`
	default:
		return fmt.Errorf("restore %s: %w", typ, ErrNotSupported)
	}
	return l.osascript(ctx, body, uuid, listID)
}

// MoveProject moves a project to another area with the URL scheme's
// update-project area-id, which things-mcp does not pass.
func (l *Live) MoveProject(ctx context.Context, uuid, areaID string) error {
	token, err := l.authToken(ctx)
	if err != nil {
		return err
	}
	if token == "" {
		return errors.New("the Things URL auth token is empty: set THINGS_AUTH_TOKEN in the .env or turn on Settings → General → Enable Things URLs")
	}
	q := url.Values{"id": {uuid}, "area-id": {areaID}, "auth-token": {token}}
	u := "things:///update-project?" + strings.ReplaceAll(q.Encode(), "+", "%20")
	_, err = l.Exec(ctx, "open", "-g", u)
	return err
}

var _ Client = (*Live)(nil)
