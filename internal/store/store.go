// Package store keeps thub-server's state in SQLite: senders, events, the
// item queue with leases, cursors, the Things ledger, the audit log and
// snapshots.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// Errors the HTTP layer maps to status codes.
var (
	ErrNotFound     = errors.New("not found")
	ErrUnauthorized = errors.New("unauthorized")
	ErrForbidden    = errors.New("forbidden")
	ErrConflict     = errors.New("conflict")
)

// InputError is a client mistake in a request body or query.
type InputError struct{ Msg string }

func (e *InputError) Error() string { return e.Msg }

func inputErr(format string, args ...any) error {
	return &InputError{Msg: fmt.Sprintf(format, args...)}
}

// Store is the SQLite-backed state of thub-server.
type Store struct {
	db *sql.DB
	// Now is the clock; tests replace it.
	Now func() time.Time
}

// Open opens or creates the database at path in WAL mode and migrates it.
func Open(path string) (*Store, error) {
	dsn := path + "?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)&_pragma=temp_store(MEMORY)" +
		"&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// One connection serializes writers inside this process; busy_timeout
	// covers the admin commands that open the same file from another one.
	db.SetMaxOpenConns(1)
	st := &Store{db: db, Now: time.Now}
	if err := st.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return st, nil
}

// Close closes the database.
func (st *Store) Close() error { return st.db.Close() }

// Ping checks that the database answers.
func (st *Store) Ping(ctx context.Context) error {
	var one int
	return st.db.QueryRowContext(ctx, `SELECT 1`).Scan(&one)
}

// Backup writes a consistent copy of the database to path with VACUUM INTO.
// path must not exist yet.
func (st *Store) Backup(ctx context.Context, path string) error {
	_, err := st.db.ExecContext(ctx, `VACUUM INTO ?`, path)
	return err
}

func (st *Store) now() time.Time { return st.Now().UTC() }

func (st *Store) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := st.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

func ns(t time.Time) int64 { return t.UnixNano() }

func fromNS(v int64) time.Time { return time.Unix(0, v).UTC() }

func fromNullNS(v sql.NullInt64) time.Time {
	if !v.Valid {
		return time.Time{}
	}
	return fromNS(v.Int64)
}

func nullStr(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }

func nullBytes(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return string(b)
}

var migrations = []string{
	`
CREATE TABLE senders (
	name         TEXT PRIMARY KEY,
	token_hash   TEXT NOT NULL UNIQUE,
	reader       INTEGER NOT NULL DEFAULT 0,
	created_at   INTEGER NOT NULL,
	last_seen_at INTEGER,
	revoked_at   INTEGER
);
CREATE TABLE sender_scopes (
	sender  TEXT NOT NULL REFERENCES senders(name),
	source  TEXT NOT NULL,
	account TEXT NOT NULL,
	PRIMARY KEY (sender, source, account)
);
CREATE TABLE events (
	seq         INTEGER PRIMARY KEY AUTOINCREMENT,
	key         TEXT NOT NULL,
	revision    TEXT NOT NULL,
	revision_ns INTEGER NOT NULL,
	op          TEXT NOT NULL,
	sender      TEXT NOT NULL,
	source      TEXT NOT NULL,
	account     TEXT NOT NULL,
	received_at INTEGER NOT NULL,
	payload     TEXT,
	UNIQUE (key, revision_ns)
);
CREATE INDEX events_received ON events (received_at);
CREATE INDEX events_sender ON events (sender, source, account);
CREATE TABLE items (
	key                   TEXT PRIMARY KEY,
	source                TEXT NOT NULL,
	account               TEXT NOT NULL,
	kind                  TEXT NOT NULL,
	source_id             TEXT NOT NULL,
	sender                TEXT NOT NULL,
	op                    TEXT NOT NULL,
	revision              TEXT NOT NULL,
	revision_ns           INTEGER NOT NULL,
	event_seq             INTEGER NOT NULL,
	payload               TEXT,
	state                 TEXT NOT NULL CHECK (state IN ('pending', 'leased', 'done', 'parked')),
	queued_at             INTEGER NOT NULL,
	not_before            INTEGER,
	processed_revision    TEXT,
	processed_revision_ns INTEGER,
	lease_id              TEXT,
	lease_expires         INTEGER,
	attempts              INTEGER NOT NULL DEFAULT 0,
	last_outcome          TEXT,
	last_outcome_rev      TEXT,
	last_note             TEXT,
	last_outcome_at       INTEGER,
	ack_lease_id          TEXT,
	ack_revision_ns       INTEGER,
	first_seen            INTEGER NOT NULL,
	updated_at            INTEGER NOT NULL
);
CREATE INDEX items_queue ON items (state, queued_at);
CREATE INDEX items_lease ON items (lease_id);
CREATE INDEX items_account ON items (source, account);
CREATE TABLE cursors (
	sender     TEXT NOT NULL,
	source     TEXT NOT NULL,
	account    TEXT NOT NULL,
	value      TEXT NOT NULL,
	updated_at INTEGER NOT NULL,
	PRIMARY KEY (sender, source, account)
);
CREATE TABLE leases (
	id          TEXT PRIMARY KEY,
	holder      TEXT NOT NULL,
	created_at  INTEGER NOT NULL,
	expires_at  INTEGER NOT NULL,
	released_at INTEGER
);
CREATE TABLE things (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	marker     TEXT UNIQUE,
	uuid       TEXT UNIQUE,
	type       TEXT NOT NULL,
	origin     TEXT NOT NULL,
	state      TEXT NOT NULL,
	written    TEXT,
	observed   TEXT,
	op_id      TEXT,
	run_id     TEXT,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	CHECK (marker IS NOT NULL OR uuid IS NOT NULL)
);
CREATE INDEX things_state ON things (state);
CREATE TABLE links (
	key        TEXT NOT NULL,
	thing_id   INTEGER NOT NULL REFERENCES things(id) ON DELETE CASCADE,
	role       TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	PRIMARY KEY (key, thing_id)
);
CREATE INDEX links_thing ON links (thing_id);
CREATE TABLE applied_ops (
	op_id      TEXT PRIMARY KEY,
	run_id     TEXT NOT NULL,
	result     TEXT NOT NULL,
	detail     TEXT,
	applied_at INTEGER NOT NULL
);
CREATE INDEX applied_ops_run ON applied_ops (run_id);
CREATE TABLE audit (
	seq         INTEGER PRIMARY KEY AUTOINCREMENT,
	run_id      TEXT NOT NULL,
	op_id       TEXT NOT NULL,
	action      TEXT NOT NULL,
	key         TEXT,
	thing_uuid  TEXT,
	marker      TEXT,
	thing_type  TEXT,
	before      TEXT,
	after       TEXT,
	at          INTEGER NOT NULL,
	recorded_at INTEGER NOT NULL,
	UNIQUE (run_id, op_id)
);
CREATE TABLE snapshots (
	run_id     TEXT PRIMARY KEY,
	body       BLOB NOT NULL,
	created_at INTEGER NOT NULL
);
`,
}

func (st *Store) migrate(ctx context.Context) error {
	var version int
	if err := st.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	for i := version; i < len(migrations); i++ {
		err := st.tx(ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, i+1))
			return err
		})
		if err != nil {
			return fmt.Errorf("migrate to schema %d: %w", i+1, err)
		}
	}
	return nil
}
