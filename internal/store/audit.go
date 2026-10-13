package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"

	"github.com/Felipe-CdS/things-hub/api"
)

var runIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,99}$`)

// ValidRunID reports whether id can name a run.
func ValidRunID(id string) bool { return runIDRe.MatchString(id) }

// AppendAudit appends audit entries. An entry whose run and op id are
// already stored is counted as a duplicate and left unchanged.
func (st *Store) AppendAudit(ctx context.Context, entries []api.AuditEntry) (api.AuditPostResponse, error) {
	var resp api.AuditPostResponse
	for i, e := range entries {
		if !ValidRunID(e.RunID) || e.OpID == "" || e.Action == "" {
			return resp, inputErr("entries[%d]: run_id, op_id and action are required", i)
		}
		if e.At.IsZero() {
			return resp, inputErr("entries[%d]: at is required", i)
		}
		if err := validJSON(fmt.Sprintf("entries[%d].before", i), e.Before); err != nil {
			return resp, err
		}
		if err := validJSON(fmt.Sprintf("entries[%d].after", i), e.After); err != nil {
			return resp, err
		}
	}
	err := st.tx(ctx, func(tx *sql.Tx) error {
		now := ns(st.now())
		for _, e := range entries {
			res, err := tx.ExecContext(ctx, `
INSERT INTO audit (run_id, op_id, action, key, thing_uuid, marker, thing_type, before, after, at, recorded_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (run_id, op_id) DO NOTHING`,
				e.RunID, e.OpID, e.Action, nullStr(e.Key), nullStr(e.ThingUUID), nullStr(e.Marker),
				nullStr(e.ThingType), nullBytes(e.Before), nullBytes(e.After), ns(e.At), now)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n == 1 {
				resp.Appended++
			} else {
				resp.Duplicate++
			}
		}
		return nil
	})
	return resp, err
}

// Audit returns a run's audit entries in the order they were recorded.
func (st *Store) Audit(ctx context.Context, run string) (api.AuditResponse, error) {
	resp := api.AuditResponse{RunID: run, Entries: []api.AuditEntry{}}
	if !ValidRunID(run) {
		return resp, inputErr("run must match %s", runIDRe)
	}
	rows, err := st.db.QueryContext(ctx, `
SELECT seq, run_id, op_id, action, key, thing_uuid, marker, thing_type, before, after, at, recorded_at
FROM audit WHERE run_id = ? ORDER BY seq`, run)
	if err != nil {
		return resp, err
	}
	defer rows.Close()
	for rows.Next() {
		var e api.AuditEntry
		var key, uuid, marker, typ, before, after sql.NullString
		var at, recorded int64
		if err := rows.Scan(&e.Seq, &e.RunID, &e.OpID, &e.Action, &key, &uuid, &marker, &typ,
			&before, &after, &at, &recorded); err != nil {
			return resp, err
		}
		e.Key, e.ThingUUID, e.Marker, e.ThingType = key.String, uuid.String, marker.String, typ.String
		if before.Valid {
			e.Before = json.RawMessage(before.String)
		}
		if after.Valid {
			e.After = json.RawMessage(after.String)
		}
		e.At, e.RecordedAt = fromNS(at), fromNS(recorded)
		resp.Entries = append(resp.Entries, e)
	}
	return resp, rows.Err()
}

// PutSnapshot stores the Things snapshot of a run, replacing an earlier one
// for the same run, and keeps only the newest api.SnapshotKeep runs.
func (st *Store) PutSnapshot(ctx context.Context, run string, body []byte) error {
	if !ValidRunID(run) {
		return inputErr("run id must match %s", runIDRe)
	}
	if !json.Valid(body) {
		return inputErr("snapshot is not valid JSON")
	}
	return st.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
INSERT INTO snapshots (run_id, body, created_at) VALUES (?, ?, ?)
ON CONFLICT (run_id) DO UPDATE SET body = excluded.body, created_at = excluded.created_at`,
			run, body, ns(st.now()))
		if err != nil {
			return err
		}
		_, err = trimSnapshots(ctx, tx)
		return err
	})
}

func trimSnapshots(ctx context.Context, tx *sql.Tx) (int64, error) {
	res, err := tx.ExecContext(ctx, `
DELETE FROM snapshots WHERE run_id NOT IN (
	SELECT run_id FROM snapshots ORDER BY created_at DESC, run_id DESC LIMIT ?)`, api.SnapshotKeep)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Snapshot returns the stored snapshot of a run.
func (st *Store) Snapshot(ctx context.Context, run string) ([]byte, error) {
	if !ValidRunID(run) {
		return nil, inputErr("run id must match %s", runIDRe)
	}
	var body []byte
	err := st.db.QueryRowContext(ctx, `SELECT body FROM snapshots WHERE run_id = ?`, run).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return body, err
}
