package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base32"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/Felipe-CdS/things-hub/api"
)

func newLeaseID() (string, error) {
	b := make([]byte, 15)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "l_" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b)), nil
}

// readyWhere selects the items a pull may take: pending and not deferred, or
// leased with an expired lease.
const readyWhere = `((state = 'pending' AND (not_before IS NULL OR not_before <= ?1))
	OR (state = 'leased' AND lease_expires <= ?1))`

// Pull leases up to max ready items to holder, oldest first.
func (st *Store) Pull(ctx context.Context, holder string, req api.PullRequest) (api.PullResponse, error) {
	resp := api.PullResponse{Items: []api.LeasedItem{}}
	max, lease := req.Max, req.LeaseSeconds
	if max == 0 {
		max = api.DefaultPullMax
	}
	if lease == 0 {
		lease = api.DefaultLeaseSeconds
	}
	if max < 1 || max > api.MaxPullMax {
		return resp, inputErr("max must be 1 to %d", api.MaxPullMax)
	}
	if lease < api.MinLeaseSeconds || lease > api.MaxLeaseSeconds {
		return resp, inputErr("lease_seconds must be %d to %d", api.MinLeaseSeconds, api.MaxLeaseSeconds)
	}
	err := st.tx(ctx, func(tx *sql.Tx) error {
		now := st.now()
		rows, err := tx.QueryContext(ctx, `SELECT key FROM items WHERE `+readyWhere+`
ORDER BY queued_at, key LIMIT ?2`, ns(now), max)
		if err != nil {
			return err
		}
		var keys []string
		for rows.Next() {
			var k string
			if err := rows.Scan(&k); err != nil {
				rows.Close()
				return err
			}
			keys = append(keys, k)
		}
		rows.Close()
		if err := rows.Err(); err != nil || len(keys) == 0 {
			return err
		}
		id, err := newLeaseID()
		if err != nil {
			return err
		}
		expires := now.Add(time.Duration(lease) * time.Second)
		if _, err := tx.ExecContext(ctx, `INSERT INTO leases (id, holder, created_at, expires_at) VALUES (?, ?, ?, ?)`,
			id, holder, ns(now), ns(expires)); err != nil {
			return err
		}
		for _, k := range keys {
			if _, err := tx.ExecContext(ctx,
				`UPDATE items SET state = 'leased', lease_id = ?, lease_expires = ? WHERE key = ?`,
				id, ns(expires), k); err != nil {
				return err
			}
			it, err := leasedItem(ctx, tx, k)
			if err != nil {
				return err
			}
			resp.Items = append(resp.Items, it)
		}
		resp.LeaseID, resp.ExpiresAt = id, expires
		return nil
	})
	return resp, err
}

func leasedItem(ctx context.Context, tx *sql.Tx, key string) (api.LeasedItem, error) {
	it := api.LeasedItem{Key: key}
	var payload, outcome, outcomeRev, note sql.NullString
	var outcomeAt sql.NullInt64
	err := tx.QueryRowContext(ctx, `
SELECT source, account, kind, source_id, op, revision, payload, attempts,
	last_outcome, last_outcome_rev, last_note, last_outcome_at
FROM items WHERE key = ?`, key).Scan(&it.Source, &it.Account, &it.Kind, &it.SourceID, &it.Op,
		&it.Revision, &payload, &it.Attempts, &outcome, &outcomeRev, &note, &outcomeAt)
	if err != nil {
		return it, err
	}
	if payload.Valid {
		var e api.Event
		if err := json.Unmarshal([]byte(payload.String), &e); err != nil {
			return it, err
		}
		it.Event = &e
	}
	if outcome.Valid {
		it.Previous = &api.Outcome{Outcome: outcome.String, Revision: outcomeRev.String,
			Note: note.String, At: fromNullNS(outcomeAt)}
	}
	it.Things, err = linkedThings(ctx, tx, key)
	return it, err
}

func linkedThings(ctx context.Context, tx *sql.Tx, key string) ([]api.LinkedThing, error) {
	rows, err := tx.QueryContext(ctx, `
SELECT l.role, t.marker, t.uuid, t.type, t.origin, t.state
FROM links l JOIN things t ON t.id = l.thing_id WHERE l.key = ? ORDER BY l.created_at, t.id`, key)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []api.LinkedThing
	for rows.Next() {
		var lt api.LinkedThing
		var marker, uuid sql.NullString
		if err := rows.Scan(&lt.Role, &marker, &uuid, &lt.Type, &lt.Origin, &lt.State); err != nil {
			return nil, err
		}
		lt.Marker, lt.UUID = marker.String, uuid.String
		out = append(out, lt)
	}
	return out, rows.Err()
}

// checkLease confirms the lease exists and belongs to holder.
func checkLease(ctx context.Context, tx *sql.Tx, id, holder string) error {
	if id == "" {
		return inputErr("lease_id is required")
	}
	var h string
	err := tx.QueryRowContext(ctx, `SELECT holder FROM leases WHERE id = ?`, id).Scan(&h)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if h != holder {
		return ErrForbidden
	}
	return nil
}

func validOutcome(o string) bool {
	switch o {
	case api.OutcomeIgnored, api.OutcomeCreated, api.OutcomeAttached, api.OutcomeUpdated,
		api.OutcomeResolved, api.OutcomeDeferred, api.OutcomeFailed:
		return true
	}
	return false
}

// Ack records per-item outcomes for a lease. Acks are idempotent: repeating
// one returns duplicate and changes nothing.
func (st *Store) Ack(ctx context.Context, holder string, req api.AckRequest) (api.AckResponse, error) {
	resp := api.AckResponse{Results: make([]api.AckResult, 0, len(req.Acks))}
	err := st.tx(ctx, func(tx *sql.Tx) error {
		if err := checkLease(ctx, tx, req.LeaseID, holder); err != nil {
			return err
		}
		now := st.now()
		for _, a := range req.Acks {
			res, err := ackOne(ctx, tx, req.LeaseID, a, now)
			if err != nil {
				return err
			}
			resp.Results = append(resp.Results, res)
		}
		return nil
	})
	return resp, err
}

func ackOne(ctx context.Context, tx *sql.Tx, leaseID string, a api.Ack, now time.Time) (api.AckResult, error) {
	res := api.AckResult{Key: a.Key, Result: api.AckRejected}
	if !validOutcome(a.Outcome) {
		res.Reason = "unknown outcome " + a.Outcome
		return res, nil
	}
	rev, err := api.ParseRevision(a.Revision)
	if err != nil {
		res.Reason = err.Error()
		return res, nil
	}
	revNS := ns(rev)
	var notBefore sql.NullInt64
	if a.NotBefore != "" {
		if a.Outcome != api.OutcomeDeferred {
			res.Reason = "not_before is only for deferred"
			return res, nil
		}
		t, err := time.Parse(time.RFC3339Nano, a.NotBefore)
		if err != nil {
			res.Reason = "not_before is not RFC 3339"
			return res, nil
		}
		notBefore = sql.NullInt64{Int64: ns(t), Valid: true}
	}
	for _, ref := range a.Items {
		if ref.Marker == "" && ref.UUID == "" {
			res.Reason = "item needs a marker or uuid"
			return res, nil
		}
		if !validRole(ref.Role) {
			res.Reason = "unknown link role " + ref.Role
			return res, nil
		}
	}

	var state string
	var itemRevNS, attempts int64
	var itemLease, ackLease sql.NullString
	var ackRevNS sql.NullInt64
	err = tx.QueryRowContext(ctx, `
SELECT state, revision_ns, attempts, lease_id, ack_lease_id, ack_revision_ns FROM items WHERE key = ?`,
		a.Key).Scan(&state, &itemRevNS, &attempts, &itemLease, &ackLease, &ackRevNS)
	if errors.Is(err, sql.ErrNoRows) {
		res.Reason = "unknown key"
		return res, nil
	}
	if err != nil {
		return res, err
	}
	res.State = state
	if ackLease.String == leaseID && ackRevNS.Valid && ackRevNS.Int64 == revNS && itemLease.String != leaseID {
		res.Result = api.AckDuplicate
		return res, nil
	}
	if state != api.StateLeased || itemLease.String != leaseID {
		res.Reason = "item is not held by this lease"
		return res, nil
	}
	if revNS > itemRevNS {
		res.Reason = "revision is newer than the stored one"
		return res, nil
	}

	ids := make([]int64, len(a.Items))
	for i, ref := range a.Items {
		ids[i], err = thingID(ctx, tx, ref.Marker, ref.UUID)
		if errors.Is(err, ErrNotFound) {
			res.Reason = "item " + ref.Marker + ref.UUID + " is not in the ledger"
			return res, nil
		}
		if err != nil {
			return res, err
		}
	}
	for i, ref := range a.Items {
		if err := putLink(ctx, tx, a.Key, ids[i], ref.Role, now); err != nil {
			return res, err
		}
	}

	newer := revNS < itemRevNS
	nowNS := ns(now)
	switch a.Outcome {
	case api.OutcomeDeferred:
		state = api.StatePending
		_, err = tx.ExecContext(ctx, `UPDATE items SET state = 'pending', queued_at = ?, not_before = ? WHERE key = ?`,
			nowNS, notBefore, a.Key)
	case api.OutcomeFailed:
		attempts++
		state = api.StatePending
		if attempts >= api.MaxAttempts {
			state = api.StateParked
		}
		_, err = tx.ExecContext(ctx, `UPDATE items SET state = ?, attempts = ?, queued_at = ? WHERE key = ?`,
			state, attempts, nowNS, a.Key)
	default:
		state = api.StateDone
		if newer {
			state = api.StatePending
		}
		_, err = tx.ExecContext(ctx, `
UPDATE items SET state = ?, attempts = 0, processed_revision = ?, processed_revision_ns = ?,
	queued_at = CASE WHEN ? = 'pending' THEN ? ELSE queued_at END
WHERE key = ?`, state, a.Revision, revNS, state, nowNS, a.Key)
	}
	if err != nil {
		return res, err
	}
	_, err = tx.ExecContext(ctx, `
UPDATE items SET lease_id = NULL, lease_expires = NULL, last_outcome = ?, last_outcome_rev = ?,
	last_note = ?, last_outcome_at = ?, ack_lease_id = ?, ack_revision_ns = ?
WHERE key = ?`, a.Outcome, a.Revision, nullStr(a.Note), nowNS, leaseID, revNS, a.Key)
	if err != nil {
		return res, err
	}
	res.Result, res.State = api.AckOK, state
	return res, nil
}

// Release returns a lease's unacked items to the queue: all of them, or only
// keys when given.
func (st *Store) Release(ctx context.Context, holder string, req api.ReleaseRequest) (api.ReleaseResponse, error) {
	var resp api.ReleaseResponse
	err := st.tx(ctx, func(tx *sql.Tx) error {
		if err := checkLease(ctx, tx, req.LeaseID, holder); err != nil {
			return err
		}
		release := func(where string, args ...any) error {
			res, err := tx.ExecContext(ctx, `UPDATE items SET state = 'pending', lease_id = NULL, lease_expires = NULL
WHERE state = 'leased' AND lease_id = ?`+where, append([]any{req.LeaseID}, args...)...)
			if err != nil {
				return err
			}
			n, _ := res.RowsAffected()
			resp.Released += int(n)
			return nil
		}
		if len(req.Keys) == 0 {
			if err := release(""); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, `UPDATE leases SET released_at = ? WHERE id = ? AND released_at IS NULL`,
				ns(st.now()), req.LeaseID)
			return err
		}
		for _, k := range req.Keys {
			if err := release(` AND key = ?`, k); err != nil {
				return err
			}
		}
		return nil
	})
	return resp, err
}
