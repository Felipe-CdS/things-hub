package things

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// The Things database locations things.py searches, newest layout first.
var dbGlobs = []string{
	"Library/Group Containers/JLMPQHK86H.com.culturedcode.ThingsMac/ThingsData-*/Things Database.thingsdatabase/main.sqlite",
	"Library/Group Containers/JLMPQHK86H.com.culturedcode.ThingsMac/Things Database.thingsdatabase/main.sqlite",
}

// FindDB returns the path of the Things database: THINGSDB when set (as
// things.py honors it), otherwise the first match under the home folder.
func FindDB() (string, error) {
	if p := os.Getenv("THINGSDB"); p != "" {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	for _, g := range dbGlobs {
		matches, err := filepath.Glob(filepath.Join(home, g))
		if err != nil {
			return "", err
		}
		if len(matches) > 0 {
			return matches[0], nil
		}
	}
	return "", errors.New("Things database not found; is Things 3 installed and opened once?")
}

// DB is a read-only view of the Things database, the same source things-mcp
// reads through things.py. thub uses it only for the URL-scheme token and
// single-item lookups, which things-mcp has no tool for.
type DB struct{ db *sql.DB }

// OpenDB opens the Things database read-only.
func OpenDB(path string) (*DB, error) {
	u := url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return &DB{db: db}, nil
}

// Close closes the database.
func (d *DB) Close() error { return d.db.Close() }

// settingsUUID is the row of TMSettings that holds the URL-scheme token.
const settingsUUID = "RhAzEf6qDxCD5PmnZVtBZR"

// AuthToken returns the Things URL-scheme auth token ("" when Enable Things
// URLs is off or the token is not set yet). Callers must never print it.
func (d *DB) AuthToken(ctx context.Context) (string, error) {
	var tok sql.NullString
	err := d.db.QueryRowContext(ctx,
		`SELECT uriSchemeAuthenticationToken FROM TMSettings WHERE uuid = ?`, settingsUUID).Scan(&tok)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read Things settings: %w", err)
	}
	return tok.String, nil
}

// taskTypes maps TMTask.type to things.py's names.
var taskTypes = map[int]string{0: TypeTodo, 1: TypeProject, 2: TypeHeading}

// taskStatuses maps TMTask.status to things.py's names.
var taskStatuses = map[int]string{0: StatusIncomplete, 2: StatusCanceled, 3: StatusCompleted}

// Lookup reads one to-do, project, heading or area. The modified time uses
// the same expression as things.py, so it compares equal to a snapshot's.
func (d *DB) Lookup(ctx context.Context, uuid string) (*State, error) {
	var typ, status int
	var trashed sql.NullInt64
	var modified sql.NullString
	err := d.db.QueryRowContext(ctx, `
SELECT type, status, trashed, datetime(userModificationDate, "unixepoch", "localtime")
FROM TMTask WHERE uuid = ?`, uuid).Scan(&typ, &status, &trashed, &modified)
	if errors.Is(err, sql.ErrNoRows) {
		var n int
		err = d.db.QueryRowContext(ctx, `SELECT count(*) FROM TMArea WHERE uuid = ?`, uuid).Scan(&n)
		if err != nil {
			return nil, err
		}
		if n == 0 {
			return nil, nil
		}
		return &State{UUID: uuid, Type: TypeArea, Status: StatusIncomplete}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("look up %s: %w", uuid, err)
	}
	st := &State{UUID: uuid, Type: taskTypes[typ], Status: taskStatuses[status],
		Trashed: trashed.Int64 == 1, Modified: modified.String}
	rows, err := d.db.QueryContext(ctx, `SELECT title FROM TMChecklistItem WHERE task = ? ORDER BY "index"`, uuid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var title sql.NullString
		if err := rows.Scan(&title); err != nil {
			return nil, err
		}
		st.Checklist = append(st.Checklist, title.String)
	}
	return st, rows.Err()
}
