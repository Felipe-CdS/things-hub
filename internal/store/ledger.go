package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/Felipe-CdS/things-hub/api"
)

var markerRe = regexp.MustCompile(`^th-[0-9a-z]{10}$`)

func validRole(r string) bool {
	return r == api.RolePrimary || r == api.RoleMerged || r == api.RoleChild
}

func validThingType(t string) bool {
	switch t {
	case api.ThingTodo, api.ThingProject, api.ThingArea, api.ThingHeading, api.ThingTag:
		return true
	}
	return false
}

func validThingState(s string) bool {
	switch s {
	case api.ThingPendingCreate, api.ThingOpen, api.ThingCompleted, api.ThingCanceled, api.ThingTrashed, api.ThingGone:
		return true
	}
	return false
}

func validOrigin(o string) bool { return o == api.OriginHub || o == api.OriginAdopted }

// thingID finds a ledger row by marker, then by uuid.
func thingID(ctx context.Context, tx *sql.Tx, marker, uuid string) (int64, error) {
	var id int64
	var err error
	if marker != "" {
		err = tx.QueryRowContext(ctx, `SELECT id FROM things WHERE marker = ?`, marker).Scan(&id)
		if err == nil || !errors.Is(err, sql.ErrNoRows) || uuid == "" {
			if errors.Is(err, sql.ErrNoRows) {
				return 0, ErrNotFound
			}
			return id, err
		}
	}
	err = tx.QueryRowContext(ctx, `SELECT id FROM things WHERE uuid = ?`, uuid).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	return id, err
}

func putLink(ctx context.Context, tx *sql.Tx, key string, thing int64, role string, now time.Time) error {
	_, err := tx.ExecContext(ctx, `
INSERT INTO links (key, thing_id, role, created_at) VALUES (?, ?, ?, ?)
ON CONFLICT (key, thing_id) DO UPDATE SET role = excluded.role`, key, thing, role, ns(now))
	return err
}

func validJSON(name string, raw json.RawMessage) error {
	if len(raw) > 0 && !json.Valid(raw) {
		return inputErr("%s is not valid JSON", name)
	}
	return nil
}

// PutLedger upserts ledger rows and applied ops atomically.
func (st *Store) PutLedger(ctx context.Context, req api.LedgerPutRequest) (api.LedgerPutResponse, error) {
	resp := api.LedgerPutResponse{Items: make([]api.Thing, 0, len(req.Items))}
	err := st.tx(ctx, func(tx *sql.Tx) error {
		now := st.now()
		for i, t := range req.Items {
			id, err := putThing(ctx, tx, t, now)
			if err != nil {
				var in *InputError
				if errors.As(err, &in) {
					return inputErr("items[%d]: %s", i, in.Msg)
				}
				return err
			}
			stored, err := getThing(ctx, tx, id)
			if err != nil {
				return err
			}
			resp.Items = append(resp.Items, stored)
		}
		for i, op := range req.AppliedOps {
			if op.OpID == "" || op.RunID == "" || op.Result == "" {
				return inputErr("applied_ops[%d]: op_id, run_id and result are required", i)
			}
			if err := validJSON(fmt.Sprintf("applied_ops[%d].detail", i), op.Detail); err != nil {
				return err
			}
			at := op.AppliedAt
			if at.IsZero() {
				at = now
			}
			_, err := tx.ExecContext(ctx, `
INSERT INTO applied_ops (op_id, run_id, result, detail, applied_at) VALUES (?, ?, ?, ?, ?)
ON CONFLICT (op_id) DO UPDATE SET result = excluded.result, detail = excluded.detail, applied_at = excluded.applied_at`,
				op.OpID, op.RunID, op.Result, nullBytes(op.Detail), ns(at))
			if err != nil {
				return err
			}
			resp.AppliedOps++
		}
		return nil
	})
	return resp, err
}

func putThing(ctx context.Context, tx *sql.Tx, t api.Thing, now time.Time) (int64, error) {
	if t.Marker == "" && t.UUID == "" {
		return 0, inputErr("marker or uuid is required")
	}
	if t.Marker != "" && !markerRe.MatchString(t.Marker) {
		return 0, inputErr("marker %q must match %s", t.Marker, markerRe)
	}
	if t.Type != "" && !validThingType(t.Type) {
		return 0, inputErr("unknown type %q", t.Type)
	}
	if t.Origin != "" && !validOrigin(t.Origin) {
		return 0, inputErr("unknown origin %q", t.Origin)
	}
	if t.State != "" && !validThingState(t.State) {
		return 0, inputErr("unknown state %q", t.State)
	}
	if err := validJSON("written", t.Written); err != nil {
		return 0, err
	}
	if err := validJSON("observed", t.Observed); err != nil {
		return 0, err
	}
	for _, l := range t.Links {
		if l.Key == "" || !validRole(l.Role) {
			return 0, inputErr("links need a key and a role of primary, merged or child")
		}
	}

	byMarker, byUUID := int64(0), int64(0)
	var err error
	if t.Marker != "" {
		if byMarker, err = thingID(ctx, tx, t.Marker, ""); err != nil && !errors.Is(err, ErrNotFound) {
			return 0, err
		}
	}
	if t.UUID != "" {
		if byUUID, err = thingID(ctx, tx, "", t.UUID); err != nil && !errors.Is(err, ErrNotFound) {
			return 0, err
		}
	}
	if byMarker != 0 && byUUID != 0 && byMarker != byUUID {
		return 0, inputErr("marker %s and uuid %s belong to different ledger rows", t.Marker, t.UUID)
	}
	id := max(byMarker, byUUID)
	nowNS := ns(now)

	if id == 0 {
		if t.Type == "" || t.Origin == "" || t.State == "" {
			return 0, inputErr("a new ledger row needs type, origin and state")
		}
		if t.Origin == api.OriginHub && t.Marker == "" {
			return 0, inputErr("a hub-made row needs a marker")
		}
		err = tx.QueryRowContext(ctx, `
INSERT INTO things (marker, uuid, type, origin, state, written, observed, op_id, run_id, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id`,
			nullStr(t.Marker), nullStr(t.UUID), t.Type, t.Origin, t.State, nullBytes(t.Written),
			nullBytes(t.Observed), nullStr(t.OpID), nullStr(t.RunID), nowNS, nowNS).Scan(&id)
		if err != nil {
			return 0, err
		}
	} else {
		var curMarker, curUUID sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT marker, uuid FROM things WHERE id = ?`, id).Scan(&curMarker, &curUUID); err != nil {
			return 0, err
		}
		if t.Marker != "" && curMarker.Valid && curMarker.String != t.Marker {
			return 0, inputErr("uuid %s already has marker %s", t.UUID, curMarker.String)
		}
		if t.UUID != "" && curUUID.Valid && curUUID.String != t.UUID {
			return 0, inputErr("marker %s already has uuid %s", t.Marker, curUUID.String)
		}
		_, err = tx.ExecContext(ctx, `
UPDATE things SET
	marker   = COALESCE(marker, ?),
	uuid     = COALESCE(uuid, ?),
	type     = COALESCE(?, type),
	origin   = COALESCE(?, origin),
	state    = COALESCE(?, state),
	written  = COALESCE(?, written),
	observed = COALESCE(?, observed),
	op_id    = COALESCE(?, op_id),
	run_id   = COALESCE(?, run_id),
	updated_at = ?
WHERE id = ?`,
			nullStr(t.Marker), nullStr(t.UUID), nullStr(t.Type), nullStr(t.Origin), nullStr(t.State),
			nullBytes(t.Written), nullBytes(t.Observed), nullStr(t.OpID), nullStr(t.RunID), nowNS, id)
		if err != nil {
			return 0, err
		}
	}
	if t.Links != nil {
		if _, err := tx.ExecContext(ctx, `DELETE FROM links WHERE thing_id = ?`, id); err != nil {
			return 0, err
		}
		for _, l := range t.Links {
			if err := putLink(ctx, tx, l.Key, id, l.Role, now); err != nil {
				return 0, err
			}
		}
	}
	return id, nil
}

const thingCols = `id, marker, uuid, type, origin, state, written, observed, op_id, run_id, created_at, updated_at`

type rowScanner interface{ Scan(...any) error }

func scanThing(r rowScanner) (int64, api.Thing, error) {
	var t api.Thing
	var id, created, updated int64
	var marker, uuid, written, observed, opID, runID sql.NullString
	err := r.Scan(&id, &marker, &uuid, &t.Type, &t.Origin, &t.State, &written, &observed, &opID, &runID, &created, &updated)
	if err != nil {
		return 0, t, err
	}
	t.Marker, t.UUID, t.OpID, t.RunID = marker.String, uuid.String, opID.String, runID.String
	if written.Valid {
		t.Written = json.RawMessage(written.String)
	}
	if observed.Valid {
		t.Observed = json.RawMessage(observed.String)
	}
	t.CreatedAt, t.UpdatedAt = fromNS(created), fromNS(updated)
	return id, t, nil
}

func thingLinks(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, id int64) ([]api.Link, error) {
	rows, err := q.QueryContext(ctx, `SELECT key, role FROM links WHERE thing_id = ? ORDER BY created_at, key`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []api.Link
	for rows.Next() {
		var l api.Link
		if err := rows.Scan(&l.Key, &l.Role); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func getThing(ctx context.Context, tx *sql.Tx, id int64) (api.Thing, error) {
	_, t, err := scanThing(tx.QueryRowContext(ctx, `SELECT `+thingCols+` FROM things WHERE id = ?`, id))
	if err != nil {
		return t, err
	}
	t.Links, err = thingLinks(ctx, tx, id)
	return t, err
}

// LedgerFilter narrows GET /v1/ledger. Run selects the applied ops to return.
type LedgerFilter struct {
	State, Key, Marker, UUID, Run string
}

// Ledger returns ledger rows with their links, and the applied ops of a run.
func (st *Store) Ledger(ctx context.Context, f LedgerFilter) (api.LedgerResponse, error) {
	resp := api.LedgerResponse{Things: []api.Thing{}}
	var where []string
	var args []any
	if f.State != "" {
		where, args = append(where, `state = ?`), append(args, f.State)
	}
	if f.Marker != "" {
		where, args = append(where, `marker = ?`), append(args, f.Marker)
	}
	if f.UUID != "" {
		where, args = append(where, `uuid = ?`), append(args, f.UUID)
	}
	if f.Key != "" {
		where, args = append(where, `id IN (SELECT thing_id FROM links WHERE key = ?)`), append(args, f.Key)
	}
	q := `SELECT ` + thingCols + ` FROM things`
	if len(where) > 0 {
		q += ` WHERE ` + strings.Join(where, ` AND `)
	}
	err := st.tx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, q+` ORDER BY id`, args...)
		if err != nil {
			return err
		}
		var ids []int64
		for rows.Next() {
			id, t, err := scanThing(rows)
			if err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
			resp.Things = append(resp.Things, t)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for i, id := range ids {
			if resp.Things[i].Links, err = thingLinks(ctx, tx, id); err != nil {
				return err
			}
		}
		if f.Run == "" {
			return nil
		}
		rows, err = tx.QueryContext(ctx,
			`SELECT op_id, run_id, result, detail, applied_at FROM applied_ops WHERE run_id = ? ORDER BY applied_at, op_id`, f.Run)
		if err != nil {
			return err
		}
		defer rows.Close()
		resp.AppliedOps = []api.AppliedOp{}
		for rows.Next() {
			var op api.AppliedOp
			var detail sql.NullString
			var at int64
			if err := rows.Scan(&op.OpID, &op.RunID, &op.Result, &detail, &at); err != nil {
				return err
			}
			if detail.Valid {
				op.Detail = json.RawMessage(detail.String)
			}
			op.AppliedAt = fromNS(at)
			resp.AppliedOps = append(resp.AppliedOps, op)
		}
		return rows.Err()
	})
	return resp, err
}
