package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"github.com/Felipe-CdS/things-hub/api"
)

// IncomingEvent is one decoded event of a batch, or why it failed to decode.
type IncomingEvent struct {
	Event     api.Event
	DecodeErr error
}

// Ingest stores a batch for sender. Each event is accepted, duplicate, stale
// or rejected; the cursor is stored only when none was rejected. A cursor for
// a source and account the sender may not write is ErrForbidden and nothing
// is stored.
func (st *Store) Ingest(ctx context.Context, sender *Sender, cursor *api.Cursor, events []IncomingEvent) (api.BatchResponse, error) {
	resp := api.BatchResponse{Results: make([]api.EventResult, len(events))}
	if cursor != nil && !sender.CanWrite(cursor.Source, cursor.Account) {
		return resp, ErrForbidden
	}
	err := st.tx(ctx, func(tx *sql.Tx) error {
		rejected := false
		now := ns(st.now())
		for i, in := range events {
			res, err := st.ingestOne(ctx, tx, sender, in, now)
			if err != nil {
				return err
			}
			res.Index = i
			if res.Result == api.ResultRejected {
				rejected = true
			}
			resp.Results[i] = res
		}
		if cursor == nil || rejected {
			return nil
		}
		_, err := tx.ExecContext(ctx, `
INSERT INTO cursors (sender, source, account, value, updated_at) VALUES (?, ?, ?, ?, ?)
ON CONFLICT (sender, source, account) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
			sender.Name, cursor.Source, strings.ToLower(cursor.Account), cursor.Value, now)
		resp.CursorStored = err == nil
		return err
	})
	return resp, err
}

func (st *Store) ingestOne(ctx context.Context, tx *sql.Tx, sender *Sender, in IncomingEvent, now int64) (api.EventResult, error) {
	reject := func(reason string) (api.EventResult, error) {
		return api.EventResult{Result: api.ResultRejected, Reason: reason}, nil
	}
	if in.DecodeErr != nil {
		return reject(in.DecodeErr.Error())
	}
	e := in.Event
	if err := e.Validate(); err != nil {
		return reject(err.Error())
	}
	if !sender.CanWrite(e.Source, e.Account) {
		return reject("sender may not write " + e.Source + " events for account " + strings.ToLower(e.Account))
	}
	e.Account = strings.ToLower(e.Account)
	key := api.Key(e)
	rev, _ := api.ParseRevision(e.Revision)
	revNS := ns(rev)
	res := api.EventResult{Key: key}

	var seq int64
	err := tx.QueryRowContext(ctx, `SELECT seq FROM events WHERE key = ? AND revision_ns = ?`, key, revNS).Scan(&seq)
	if err == nil {
		res.Result, res.Seq = api.ResultDuplicate, seq
		return res, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return res, err
	}
	var storedNS int64
	err = tx.QueryRowContext(ctx, `SELECT revision_ns FROM items WHERE key = ?`, key).Scan(&storedNS)
	found := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return res, err
	}
	if found && revNS <= storedNS {
		res.Result = api.ResultStale
		if revNS == storedNS {
			res.Result = api.ResultDuplicate
		}
		return res, nil
	}

	payload, err := json.Marshal(e)
	if err != nil {
		return res, err
	}
	err = tx.QueryRowContext(ctx, `
INSERT INTO events (key, revision, revision_ns, op, sender, source, account, received_at, payload)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING seq`,
		key, e.Revision, revNS, e.Op, sender.Name, e.Source, e.Account, now, string(payload)).Scan(&seq)
	if err != nil {
		return res, err
	}
	if !found {
		_, err = tx.ExecContext(ctx, `
INSERT INTO items (key, source, account, kind, source_id, sender, op, revision, revision_ns, event_seq,
	payload, state, queued_at, first_seen, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending', ?, ?, ?)`,
			key, e.Source, e.Account, e.Kind, e.SourceID, sender.Name, e.Op, e.Revision, revNS, seq,
			string(payload), now, now, now)
	} else {
		// A newer revision puts a done or parked item back in the queue and
		// clears a deferral; a leased item stays with its lease, and its
		// ack sends it back to pending.
		_, err = tx.ExecContext(ctx, `
UPDATE items SET
	sender = ?, op = ?, revision = ?, revision_ns = ?, event_seq = ?, payload = ?, updated_at = ?,
	not_before = CASE WHEN state = 'leased' THEN not_before ELSE NULL END,
	attempts   = CASE WHEN state = 'parked' THEN 0 ELSE attempts END,
	queued_at  = CASE WHEN state IN ('done', 'parked') THEN ? ELSE queued_at END,
	state      = CASE WHEN state IN ('done', 'parked') THEN 'pending' ELSE state END
WHERE key = ?`,
			sender.Name, e.Op, e.Revision, revNS, seq, string(payload), now, now, key)
	}
	if err != nil {
		return res, err
	}
	res.Result, res.Seq = api.ResultAccepted, seq
	return res, nil
}

// Cursor returns the cursor sender stored for source and account, or nil.
func (st *Store) Cursor(ctx context.Context, sender *Sender, source, account string) (*api.CursorResponse, error) {
	if !sender.CanWrite(source, account) {
		return nil, ErrForbidden
	}
	account = strings.ToLower(account)
	out := &api.CursorResponse{Source: source, Account: account}
	var value string
	var updated int64
	err := st.db.QueryRowContext(ctx,
		`SELECT value, updated_at FROM cursors WHERE sender = ? AND source = ? AND account = ?`,
		sender.Name, source, account).Scan(&value, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	out.Value, out.UpdatedAt = &value, fromNS(updated)
	return out, nil
}

// Open lists the items for source and account whose latest event is an
// upsert, so a collector can resolve the ones that left the source.
func (st *Store) Open(ctx context.Context, sender *Sender, source, account string) (*api.OpenResponse, error) {
	if !sender.CanWrite(source, account) {
		return nil, ErrForbidden
	}
	account = strings.ToLower(account)
	rows, err := st.db.QueryContext(ctx, `
SELECT key, kind, source_id, revision FROM items
WHERE source = ? AND account = ? AND op = 'upsert' ORDER BY key`, source, account)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := &api.OpenResponse{Source: source, Account: account, Items: []api.OpenItem{}}
	for rows.Next() {
		var it api.OpenItem
		if err := rows.Scan(&it.Key, &it.Kind, &it.SourceID, &it.Revision); err != nil {
			return nil, err
		}
		out.Items = append(out.Items, it)
	}
	return out, rows.Err()
}
