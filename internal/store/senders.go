package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/Felipe-CdS/things-hub/api"
)

// TokenPrefix starts every app token.
const TokenPrefix = "thub_"

var senderNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// Scope is a source and account pair a sender may write.
type Scope struct {
	Source  string
	Account string
}

// Sender is a token holder.
type Sender struct {
	Name       string
	Reader     bool
	Scopes     []Scope
	CreatedAt  time.Time
	LastSeenAt time.Time
	RevokedAt  time.Time
}

// CanWrite reports whether the sender may write events, cursors and open
// queries for the source and account.
func (s *Sender) CanWrite(source, account string) bool {
	account = strings.ToLower(account)
	for _, sc := range s.Scopes {
		if sc.Source == source && sc.Account == account {
			return true
		}
	}
	return false
}

// HashToken returns the stored form of a token.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return TokenPrefix + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b)), nil
}

func normalizeScopes(scopes []Scope) ([]Scope, error) {
	out := make([]Scope, 0, len(scopes))
	for _, sc := range scopes {
		if !api.KnownSource(sc.Source) {
			return nil, inputErr("unknown source %q", sc.Source)
		}
		if !api.ValidAccountPart(sc.Account) {
			return nil, inputErr("account %q is empty or contains ':' or spaces", sc.Account)
		}
		out = append(out, Scope{Source: sc.Source, Account: strings.ToLower(sc.Account)})
	}
	return out, nil
}

// AddSender creates a sender and returns its token. Only the token's hash is
// stored, so the token cannot be shown again.
func (st *Store) AddSender(ctx context.Context, name string, reader bool, scopes []Scope) (string, error) {
	if !senderNameRe.MatchString(name) {
		return "", inputErr("sender name must match %s", senderNameRe)
	}
	scopes, err := normalizeScopes(scopes)
	if err != nil {
		return "", err
	}
	token, err := newToken()
	if err != nil {
		return "", err
	}
	err = st.tx(ctx, func(tx *sql.Tx) error {
		var one int
		err := tx.QueryRowContext(ctx, `SELECT 1 FROM senders WHERE name = ?`, name).Scan(&one)
		if err == nil {
			return inputErr("sender %q already exists", name)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO senders (name, token_hash, reader, created_at) VALUES (?, ?, ?, ?)`,
			name, HashToken(token), reader, ns(st.now())); err != nil {
			return err
		}
		return insertScopes(ctx, tx, name, scopes)
	})
	if err != nil {
		return "", err
	}
	return token, nil
}

func insertScopes(ctx context.Context, tx *sql.Tx, name string, scopes []Scope) error {
	for _, sc := range scopes {
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO sender_scopes (sender, source, account) VALUES (?, ?, ?)`,
			name, sc.Source, sc.Account); err != nil {
			return err
		}
	}
	return nil
}

// AllowSender adds write scopes to a sender and, with reader, lets it pull,
// ack and use the ledger, audit, snapshots and status.
func (st *Store) AllowSender(ctx context.Context, name string, reader bool, scopes []Scope) error {
	scopes, err := normalizeScopes(scopes)
	if err != nil {
		return err
	}
	return st.tx(ctx, func(tx *sql.Tx) error {
		var revoked sql.NullInt64
		err := tx.QueryRowContext(ctx, `SELECT revoked_at FROM senders WHERE name = ?`, name).Scan(&revoked)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if revoked.Valid {
			return inputErr("sender %q is revoked", name)
		}
		if reader {
			if _, err := tx.ExecContext(ctx, `UPDATE senders SET reader = 1 WHERE name = ?`, name); err != nil {
				return err
			}
		}
		return insertScopes(ctx, tx, name, scopes)
	})
}

// RevokeSender invalidates a sender's token.
func (st *Store) RevokeSender(ctx context.Context, name string) error {
	res, err := st.db.ExecContext(ctx,
		`UPDATE senders SET revoked_at = ? WHERE name = ? AND revoked_at IS NULL`, ns(st.now()), name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		var one int
		if err := st.db.QueryRowContext(ctx, `SELECT 1 FROM senders WHERE name = ?`, name).Scan(&one); errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
	}
	return nil
}

// ListSenders returns every sender, revoked ones included.
func (st *Store) ListSenders(ctx context.Context) ([]Sender, error) {
	rows, err := st.db.QueryContext(ctx,
		`SELECT name, reader, created_at, last_seen_at, revoked_at FROM senders ORDER BY name`)
	if err != nil {
		return nil, err
	}
	var out []Sender
	for rows.Next() {
		var s Sender
		var created int64
		var seen, revoked sql.NullInt64
		if err := rows.Scan(&s.Name, &s.Reader, &created, &seen, &revoked); err != nil {
			rows.Close()
			return nil, err
		}
		s.CreatedAt, s.LastSeenAt, s.RevokedAt = fromNS(created), fromNullNS(seen), fromNullNS(revoked)
		out = append(out, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		if out[i].Scopes, err = st.scopes(ctx, out[i].Name); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (st *Store) scopes(ctx context.Context, name string) ([]Scope, error) {
	rows, err := st.db.QueryContext(ctx,
		`SELECT source, account FROM sender_scopes WHERE sender = ? ORDER BY source, account`, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Scope
	for rows.Next() {
		var sc Scope
		if err := rows.Scan(&sc.Source, &sc.Account); err != nil {
			return nil, err
		}
		out = append(out, sc)
	}
	return out, rows.Err()
}

// lastSeenEvery limits how often authentication writes last_seen_at.
const lastSeenEvery = time.Minute

// Authenticate returns the live sender holding token.
func (st *Store) Authenticate(ctx context.Context, token string) (*Sender, error) {
	if !strings.HasPrefix(token, TokenPrefix) {
		return nil, ErrUnauthorized
	}
	var s Sender
	var created int64
	var seen sql.NullInt64
	err := st.db.QueryRowContext(ctx,
		`SELECT name, reader, created_at, last_seen_at FROM senders WHERE token_hash = ? AND revoked_at IS NULL`,
		HashToken(token)).Scan(&s.Name, &s.Reader, &created, &seen)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrUnauthorized
	}
	if err != nil {
		return nil, err
	}
	s.CreatedAt, s.LastSeenAt = fromNS(created), fromNullNS(seen)
	if s.Scopes, err = st.scopes(ctx, s.Name); err != nil {
		return nil, err
	}
	now := st.now()
	if now.Sub(s.LastSeenAt) >= lastSeenEvery {
		if _, err := st.db.ExecContext(ctx, `UPDATE senders SET last_seen_at = ? WHERE name = ?`, ns(now), s.Name); err != nil {
			return nil, err
		}
		s.LastSeenAt = now
	}
	return &s, nil
}
