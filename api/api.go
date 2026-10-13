// Package api holds the wire types shared by thub-server and its clients:
// the event envelope collectors send, and the request and response shapes of
// the queue, ledger, audit and snapshot endpoints the Mac tool uses.
package api

import (
	"encoding/json"
	"time"
)

// BatchSchema is the required value of Batch.Schema.
const BatchSchema = "things-hub.batch.v1"

// Batch limits enforced by POST /v1/events.
const (
	MaxBatchEvents = 200
	MaxBatchBytes  = 1 << 20
)

// Event field limits.
const (
	MaxTitleRunes    = 140
	MaxSummaryRunes  = 1000
	MaxLabels        = 10
	MaxLabelKeyRunes = 40
	MaxLabelValRunes = 200
	MaxFromRunes     = 200
	MaxSourceIDRunes = 300
	MaxURLRunes      = 2000
)

// Sources the server accepts.
const (
	SourceGitHub      = "github"
	SourceSlack       = "slack"
	SourceClassroom   = "classroom"
	SourceGeminiNotes = "gemini-notes"
	SourceEmail       = "email"
	SourceWiki        = "wiki"
)

// Event ops.
const (
	OpUpsert  = "upsert"
	OpResolve = "resolve"
)

// Batch is the body of POST /v1/events.
type Batch struct {
	Schema string `json:"schema"`
	// Cursor is opaque to the server. It is stored only when no event in the
	// batch was rejected.
	Cursor *Cursor `json:"cursor,omitempty"`
	Events []Event `json:"events"`
}

// Cursor is a collector's resume point for one source and account.
type Cursor struct {
	Source  string `json:"source"`
	Account string `json:"account"`
	Value   string `json:"value"`
}

// Event is one obligation reported by a collector. Unknown fields are
// rejected by the server.
type Event struct {
	Source   string `json:"source"`
	Account  string `json:"account"`
	Kind     string `json:"kind"`
	SourceID string `json:"source_id"`
	Op       string `json:"op"`
	// Revision is the time of the last source change that matters: RFC 3339,
	// or Unix seconds with an optional fraction (a Slack ts works as is). A
	// resolve should carry a revision newer than the last upsert, such as the
	// time the collector saw the item leave the source's result set.
	Revision   string            `json:"revision"`
	ObservedAt string            `json:"observed_at"`
	URL        string            `json:"url,omitempty"`
	Title      string            `json:"title,omitempty"`
	Summary    string            `json:"summary,omitempty"`
	From       string            `json:"from,omitempty"`
	OccurredAt string            `json:"occurred_at,omitempty"`
	Due        string            `json:"due,omitempty"`
	Labels     map[string]string `json:"labels,omitempty"`
}

// Per-event results of POST /v1/events.
const (
	ResultAccepted  = "accepted"
	ResultDuplicate = "duplicate"
	ResultStale     = "stale"
	ResultRejected  = "rejected"
)

// EventResult reports what happened to Batch.Events[Index].
type EventResult struct {
	Index  int    `json:"index"`
	Key    string `json:"key,omitempty"`
	Result string `json:"result"`
	Reason string `json:"reason,omitempty"`
	Seq    int64  `json:"seq,omitempty"`
}

// BatchResponse is the response of POST /v1/events.
type BatchResponse struct {
	Results      []EventResult `json:"results"`
	CursorStored bool          `json:"cursor_stored"`
}

// CursorResponse is the response of GET /v1/cursor. Value is null when no
// cursor is stored.
type CursorResponse struct {
	Source    string    `json:"source"`
	Account   string    `json:"account"`
	Value     *string   `json:"value"`
	UpdatedAt time.Time `json:"updated_at,omitzero"`
}

// OpenItem is an item whose latest event is an upsert.
type OpenItem struct {
	Key      string `json:"key"`
	Kind     string `json:"kind"`
	SourceID string `json:"source_id"`
	Revision string `json:"revision"`
}

// OpenResponse is the response of GET /v1/open.
type OpenResponse struct {
	Source  string     `json:"source"`
	Account string     `json:"account"`
	Items   []OpenItem `json:"items"`
}

// Lease defaults and bounds for POST /v1/pull.
const (
	DefaultPullMax      = 50
	MaxPullMax          = 500
	DefaultLeaseSeconds = 2 * 60 * 60
	MinLeaseSeconds     = 60
	MaxLeaseSeconds     = 24 * 60 * 60
)

// PullRequest is the body of POST /v1/pull. Zero values take the defaults.
type PullRequest struct {
	Max          int `json:"max,omitempty"`
	LeaseSeconds int `json:"lease_seconds,omitempty"`
}

// PullResponse is the response of POST /v1/pull. LeaseID is empty when no
// item was ready.
type PullResponse struct {
	LeaseID   string       `json:"lease_id,omitempty"`
	ExpiresAt time.Time    `json:"expires_at,omitzero"`
	Items     []LeasedItem `json:"items"`
}

// LeasedItem is one queue item handed out by a pull.
type LeasedItem struct {
	Key      string `json:"key"`
	Source   string `json:"source"`
	Account  string `json:"account"`
	Kind     string `json:"kind"`
	SourceID string `json:"source_id"`
	Op       string `json:"op"`
	Revision string `json:"revision"`
	// Event is the latest event for the key; nil once retention pruned it.
	Event    *Event        `json:"event,omitempty"`
	Attempts int           `json:"attempts"`
	Previous *Outcome      `json:"previous,omitempty"`
	Things   []LinkedThing `json:"things,omitempty"`
}

// Outcome is the last ack recorded for an item.
type Outcome struct {
	Outcome  string    `json:"outcome"`
	Revision string    `json:"revision,omitempty"`
	Note     string    `json:"note,omitempty"`
	At       time.Time `json:"at"`
}

// LinkedThing is a ledger row linked to a key.
type LinkedThing struct {
	Role   string `json:"role"`
	Marker string `json:"marker,omitempty"`
	UUID   string `json:"uuid,omitempty"`
	Type   string `json:"type"`
	Origin string `json:"origin"`
	State  string `json:"state"`
}

// Ack outcomes.
const (
	OutcomeIgnored  = "ignored"
	OutcomeCreated  = "created"
	OutcomeAttached = "attached"
	OutcomeUpdated  = "updated"
	OutcomeResolved = "resolved"
	OutcomeDeferred = "deferred"
	OutcomeFailed   = "failed"
)

// MaxAttempts is the number of failed acks after which an item is parked.
const MaxAttempts = 3

// AckRequest is the body of POST /v1/ack.
type AckRequest struct {
	LeaseID string `json:"lease_id"`
	Acks    []Ack  `json:"acks"`
}

// Ack is the outcome for one leased item at the revision the Mac processed.
type Ack struct {
	Key      string `json:"key"`
	Revision string `json:"revision"`
	Outcome  string `json:"outcome"`
	// Items are Things ledger rows to link to the key.
	Items []ThingRef `json:"items,omitempty"`
	Note  string     `json:"note,omitempty"`
	// NotBefore (RFC 3339) delays a deferred item's next pull.
	NotBefore string `json:"not_before,omitempty"`
}

// ThingRef names a ledger row by marker or uuid, with the link role.
type ThingRef struct {
	Marker string `json:"marker,omitempty"`
	UUID   string `json:"uuid,omitempty"`
	Role   string `json:"role"`
}

// Per-ack results.
const (
	AckOK        = "ok"
	AckDuplicate = "duplicate"
	AckRejected  = "rejected"
)

// AckResult reports what happened to one Ack.
type AckResult struct {
	Key    string `json:"key"`
	Result string `json:"result"`
	State  string `json:"state,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// AckResponse is the response of POST /v1/ack.
type AckResponse struct {
	Results []AckResult `json:"results"`
}

// ReleaseRequest is the body of POST /v1/release. With Keys empty, every item
// still held by the lease is returned to the queue.
type ReleaseRequest struct {
	LeaseID string   `json:"lease_id"`
	Keys    []string `json:"keys,omitempty"`
}

// ReleaseResponse is the response of POST /v1/release.
type ReleaseResponse struct {
	Released int `json:"released"`
}

// Item states.
const (
	StatePending = "pending"
	StateLeased  = "leased"
	StateDone    = "done"
	StateParked  = "parked"
)

// Ledger values.
const (
	ThingTodo    = "todo"
	ThingProject = "project"
	ThingArea    = "area"
	ThingHeading = "heading"
	ThingTag     = "tag"

	OriginHub     = "hub"
	OriginAdopted = "adopted"

	ThingPendingCreate = "pending_create"
	ThingOpen          = "open"
	ThingCompleted     = "completed"
	ThingCanceled      = "canceled"
	ThingTrashed       = "trashed"
	ThingGone          = "gone"

	RolePrimary = "primary"
	RoleMerged  = "merged"
	RoleChild   = "child"
)

// Thing is one ledger row: a Things item the hub made (identified by its
// marker until the uuid is known) or adopted (identified by uuid).
//
// In PUT /v1/ledger/items a row is matched by marker, then by uuid. Empty
// fields keep the stored value; Links nil keeps the stored links and a
// non-nil slice replaces them.
type Thing struct {
	Marker    string          `json:"marker,omitempty"`
	UUID      string          `json:"uuid,omitempty"`
	Type      string          `json:"type,omitempty"`
	Origin    string          `json:"origin,omitempty"`
	State     string          `json:"state,omitempty"`
	Written   json.RawMessage `json:"written,omitempty"`
	Observed  json.RawMessage `json:"observed,omitempty"`
	OpID      string          `json:"op_id,omitempty"`
	RunID     string          `json:"run_id,omitempty"`
	Links     []Link          `json:"links"`
	CreatedAt time.Time       `json:"created_at,omitzero"`
	UpdatedAt time.Time       `json:"updated_at,omitzero"`
}

// Link ties a source key to a ledger row.
type Link struct {
	Key  string `json:"key"`
	Role string `json:"role"`
}

// AppliedOp records that a planned Things write was carried out, so it is
// applied at most once.
type AppliedOp struct {
	OpID      string          `json:"op_id"`
	RunID     string          `json:"run_id"`
	Result    string          `json:"result"`
	Detail    json.RawMessage `json:"detail,omitempty"`
	AppliedAt time.Time       `json:"applied_at,omitzero"`
}

// LedgerResponse is the response of GET /v1/ledger. AppliedOps is filled
// only when the run query parameter is given.
type LedgerResponse struct {
	Things     []Thing     `json:"things"`
	AppliedOps []AppliedOp `json:"applied_ops,omitempty"`
}

// LedgerPutRequest is the body of PUT /v1/ledger/items. It applies
// atomically.
type LedgerPutRequest struct {
	Items      []Thing     `json:"items,omitempty"`
	AppliedOps []AppliedOp `json:"applied_ops,omitempty"`
}

// LedgerPutResponse is the response of PUT /v1/ledger/items.
type LedgerPutResponse struct {
	Items      []Thing `json:"items"`
	AppliedOps int     `json:"applied_ops"`
}

// AuditEntry is one applied change with its before and after state.
type AuditEntry struct {
	Seq        int64           `json:"seq,omitempty"`
	RunID      string          `json:"run_id"`
	OpID       string          `json:"op_id"`
	Action     string          `json:"action"`
	Key        string          `json:"key,omitempty"`
	ThingUUID  string          `json:"thing_uuid,omitempty"`
	Marker     string          `json:"marker,omitempty"`
	ThingType  string          `json:"thing_type,omitempty"`
	Before     json.RawMessage `json:"before,omitempty"`
	After      json.RawMessage `json:"after,omitempty"`
	At         time.Time       `json:"at"`
	RecordedAt time.Time       `json:"recorded_at,omitzero"`
}

// AuditPostRequest is the body of POST /v1/audit. Entries are unique by run
// and op id, so a resend is harmless.
type AuditPostRequest struct {
	Entries []AuditEntry `json:"entries"`
}

// AuditPostResponse is the response of POST /v1/audit.
type AuditPostResponse struct {
	Appended  int `json:"appended"`
	Duplicate int `json:"duplicate"`
}

// AuditResponse is the response of GET /v1/audit.
type AuditResponse struct {
	RunID   string       `json:"run_id"`
	Entries []AuditEntry `json:"entries"`
}

// Snapshot limits. The server keeps the last SnapshotKeep runs.
const (
	MaxSnapshotBytes = 32 << 20
	SnapshotKeep     = 30
)

// SnapshotPutResponse is the response of PUT /v1/snapshots/{run}. GET
// returns the stored snapshot body as is.
type SnapshotPutResponse struct {
	RunID string `json:"run_id"`
	Bytes int    `json:"bytes"`
}

// StatusResponse is the response of GET /v1/status.
type StatusResponse struct {
	Now           time.Time        `json:"now"`
	Queue         QueueCounts      `json:"queue"`
	OldestPending *QueuedItem      `json:"oldest_pending,omitempty"`
	Senders       []SenderActivity `json:"senders"`
	Parked        []ParkedItem     `json:"parked"`
	Leases        []LeaseInfo      `json:"leases"`
}

// QueueCounts counts items by state. Pending includes items whose lease
// expired; Deferred counts the pending items held by a not-before time.
type QueueCounts struct {
	Pending  int `json:"pending"`
	Deferred int `json:"deferred"`
	Leased   int `json:"leased"`
	Done     int `json:"done"`
	Parked   int `json:"parked"`
}

// QueuedItem names a pending item and when it was queued.
type QueuedItem struct {
	Key      string    `json:"key"`
	QueuedAt time.Time `json:"queued_at"`
}

// SenderActivity is the last event received per sender, source and account.
type SenderActivity struct {
	Sender      string    `json:"sender"`
	Source      string    `json:"source"`
	Account     string    `json:"account"`
	LastEventAt time.Time `json:"last_event_at"`
	LastSeq     int64     `json:"last_seq"`
	Events      int       `json:"events"`
}

// ParkedItem is an item that failed MaxAttempts times.
type ParkedItem struct {
	Key           string    `json:"key"`
	Attempts      int       `json:"attempts"`
	LastNote      string    `json:"last_note,omitempty"`
	LastOutcomeAt time.Time `json:"last_outcome_at,omitzero"`
}

// LeaseInfo describes a live lease.
type LeaseInfo struct {
	ID        string    `json:"id"`
	Holder    string    `json:"holder"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	Items     int       `json:"items"`
}

// ErrorResponse is the body of every non-2xx response.
type ErrorResponse struct {
	Error string `json:"error"`
}
