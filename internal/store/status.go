package store

import (
	"context"
	"database/sql"
	"time"

	"github.com/Felipe-CdS/things-hub/api"
)

// PruneResult counts what a prune removed.
type PruneResult struct {
	EventPayloads int64
	ItemPayloads  int64
	Leases        int64
	Snapshots     int64
}

// Prune drops event payloads older than retentionDays, the payloads and
// notes of items done for that long, old leases no item refers to, and
// snapshots beyond the newest api.SnapshotKeep. Keys and revisions stay, so
// dedupe keeps working.
func (st *Store) Prune(ctx context.Context, retentionDays int) (PruneResult, error) {
	var r PruneResult
	if retentionDays < 1 {
		return r, inputErr("retention must be at least 1 day")
	}
	cutoff := ns(st.now().Add(-time.Duration(retentionDays) * 24 * time.Hour))
	err := st.tx(ctx, func(tx *sql.Tx) error {
		count := func(dst *int64, q string, args ...any) error {
			res, err := tx.ExecContext(ctx, q, args...)
			if err != nil {
				return err
			}
			*dst, _ = res.RowsAffected()
			return nil
		}
		if err := count(&r.EventPayloads,
			`UPDATE events SET payload = NULL WHERE received_at < ? AND payload IS NOT NULL`, cutoff); err != nil {
			return err
		}
		if err := count(&r.ItemPayloads, `
UPDATE items SET payload = NULL, last_note = NULL
WHERE state = 'done' AND updated_at < ? AND COALESCE(last_outcome_at, 0) < ?
	AND (payload IS NOT NULL OR last_note IS NOT NULL)`, cutoff, cutoff); err != nil {
			return err
		}
		if err := count(&r.Leases, `
DELETE FROM leases WHERE expires_at < ?
	AND id NOT IN (SELECT lease_id FROM items WHERE lease_id IS NOT NULL)`, cutoff); err != nil {
			return err
		}
		n, err := trimSnapshots(ctx, tx)
		r.Snapshots = n
		return err
	})
	return r, err
}

// Status reports queue depth, the oldest pending item, the last event per
// sender and account, parked items and live leases.
func (st *Store) Status(ctx context.Context) (api.StatusResponse, error) {
	now := st.now()
	resp := api.StatusResponse{Now: now, Senders: []api.SenderActivity{}, Parked: []api.ParkedItem{}, Leases: []api.LeaseInfo{}}
	err := st.tx(ctx, func(tx *sql.Tx) error {
		n := ns(now)
		err := tx.QueryRowContext(ctx, `
SELECT
	COALESCE(SUM(state = 'pending' OR (state = 'leased' AND lease_expires <= ?1)), 0),
	COALESCE(SUM(state = 'pending' AND not_before > ?1), 0),
	COALESCE(SUM(state = 'leased' AND lease_expires > ?1), 0),
	COALESCE(SUM(state = 'done'), 0),
	COALESCE(SUM(state = 'parked'), 0)
FROM items`, n).Scan(&resp.Queue.Pending, &resp.Queue.Deferred, &resp.Queue.Leased, &resp.Queue.Done, &resp.Queue.Parked)
		if err != nil {
			return err
		}
		var key string
		var queued int64
		err = tx.QueryRowContext(ctx, `SELECT key, queued_at FROM items WHERE `+readyWhere+`
ORDER BY queued_at, key LIMIT 1`, n).Scan(&key, &queued)
		if err == nil {
			resp.OldestPending = &api.QueuedItem{Key: key, QueuedAt: fromNS(queued)}
		} else if err != sql.ErrNoRows {
			return err
		}

		rows, err := tx.QueryContext(ctx, `
SELECT sender, source, account, MAX(received_at), MAX(seq), COUNT(*) FROM events
GROUP BY sender, source, account ORDER BY sender, source, account`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var a api.SenderActivity
			var last int64
			if err := rows.Scan(&a.Sender, &a.Source, &a.Account, &last, &a.LastSeq, &a.Events); err != nil {
				rows.Close()
				return err
			}
			a.LastEventAt = fromNS(last)
			resp.Senders = append(resp.Senders, a)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		rows, err = tx.QueryContext(ctx,
			`SELECT key, attempts, last_note, last_outcome_at FROM items WHERE state = 'parked' ORDER BY key`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var p api.ParkedItem
			var note sql.NullString
			var at sql.NullInt64
			if err := rows.Scan(&p.Key, &p.Attempts, &note, &at); err != nil {
				rows.Close()
				return err
			}
			p.LastNote, p.LastOutcomeAt = note.String, fromNullNS(at)
			resp.Parked = append(resp.Parked, p)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		rows, err = tx.QueryContext(ctx, `
SELECT l.id, l.holder, l.created_at, l.expires_at, COUNT(i.key) FROM leases l
JOIN items i ON i.lease_id = l.id AND i.state = 'leased'
WHERE l.expires_at > ? AND l.released_at IS NULL
GROUP BY l.id ORDER BY l.created_at`, n)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var l api.LeaseInfo
			var created, expires int64
			if err := rows.Scan(&l.ID, &l.Holder, &created, &expires, &l.Items); err != nil {
				return err
			}
			l.CreatedAt, l.ExpiresAt = fromNS(created), fromNS(expires)
			resp.Leases = append(resp.Leases, l)
		}
		return rows.Err()
	})
	return resp, err
}
