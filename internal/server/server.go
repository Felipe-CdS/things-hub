// Package server is thub-server's HTTP API.
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/Felipe-CdS/things-hub/api"
	"github.com/Felipe-CdS/things-hub/internal/store"
)

// maxJSONBytes bounds request bodies other than events and snapshots.
const maxJSONBytes = 8 << 20

// Server serves the API over a store.
type Server struct {
	st  *store.Store
	log *slog.Logger
	mux *http.ServeMux
}

// New returns the API handler.
func New(st *store.Store, log *slog.Logger) *Server {
	s := &Server{st: st, log: log, mux: http.NewServeMux()}
	s.mux.HandleFunc("GET /healthz", s.healthz)
	s.mux.Handle("POST /v1/events", s.auth(false, s.postEvents))
	s.mux.Handle("GET /v1/cursor", s.auth(false, s.getCursor))
	s.mux.Handle("GET /v1/open", s.auth(false, s.getOpen))
	s.mux.Handle("POST /v1/pull", s.auth(true, s.pull))
	s.mux.Handle("POST /v1/ack", s.auth(true, s.ack))
	s.mux.Handle("POST /v1/release", s.auth(true, s.release))
	s.mux.Handle("GET /v1/ledger", s.auth(true, s.getLedger))
	s.mux.Handle("PUT /v1/ledger/items", s.auth(true, s.putLedger))
	s.mux.Handle("POST /v1/audit", s.auth(true, s.postAudit))
	s.mux.Handle("GET /v1/audit", s.auth(true, s.getAudit))
	s.mux.Handle("PUT /v1/snapshots/{run}", s.auth(true, s.putSnapshot))
	s.mux.Handle("GET /v1/snapshots/{run}", s.auth(true, s.getSnapshot))
	s.mux.Handle("GET /v1/status", s.auth(true, s.status))
	return s
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

type senderKey struct{}

// ServeHTTP logs each request by method, path, status and sender. Bodies are
// never logged.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	var sender string
	r = r.WithContext(context.WithValue(r.Context(), senderKey{}, &sender))
	s.mux.ServeHTTP(rec, r)
	if r.URL.Path == "/healthz" && rec.status == http.StatusOK {
		return
	}
	s.log.Info("request", "method", r.Method, "path", r.URL.Path, "status", rec.status,
		"sender", sender, "ms", time.Since(start).Milliseconds())
}

type handler func(w http.ResponseWriter, r *http.Request, sender *store.Sender)

// auth requires a live bearer token, and with reader, the reader scope.
func (s *Server) auth(reader bool, h handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok {
			s.fail(w, store.ErrUnauthorized)
			return
		}
		sender, err := s.st.Authenticate(r.Context(), strings.TrimSpace(token))
		if err != nil {
			s.fail(w, err)
			return
		}
		if name, ok := r.Context().Value(senderKey{}).(*string); ok {
			*name = sender.Name
		}
		if reader && !sender.Reader {
			s.fail(w, fmt.Errorf("%w: sender %s may not read the queue", store.ErrForbidden, sender.Name))
			return
		}
		h(w, r, sender)
	})
}

func (s *Server) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		s.log.Warn("write response", "err", err)
	}
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	var in *store.InputError
	var tooBig *http.MaxBytesError
	status := http.StatusInternalServerError
	msg := "internal error"
	switch {
	case errors.As(err, &in):
		status, msg = http.StatusBadRequest, in.Msg
	case errors.As(err, &tooBig):
		status, msg = http.StatusRequestEntityTooLarge, fmt.Sprintf("body is larger than %d bytes", tooBig.Limit)
	case errors.Is(err, store.ErrUnauthorized):
		status, msg = http.StatusUnauthorized, "missing, unknown or revoked token"
	case errors.Is(err, store.ErrForbidden):
		status, msg = http.StatusForbidden, err.Error()
	case errors.Is(err, store.ErrNotFound):
		status, msg = http.StatusNotFound, err.Error()
	case errors.Is(err, store.ErrConflict):
		status, msg = http.StatusConflict, err.Error()
	default:
		s.log.Error("request failed", "err", err)
	}
	s.writeJSON(w, status, api.ErrorResponse{Error: msg})
}

// decode reads a JSON body strictly: unknown fields and trailing data are
// errors.
func decode(w http.ResponseWriter, r *http.Request, limit int64, v any) error {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		return err
	}
	return decodeStrict(body, v)
}

func decodeStrict(body []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return &store.InputError{Msg: "invalid JSON: " + err.Error()}
	}
	if dec.More() {
		return &store.InputError{Msg: "invalid JSON: trailing data"}
	}
	return nil
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	if err := s.st.Ping(r.Context()); err != nil {
		s.log.Error("healthz", "err", err)
		s.writeJSON(w, http.StatusServiceUnavailable, api.ErrorResponse{Error: "database unavailable"})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

type batchEnvelope struct {
	Schema string            `json:"schema"`
	Cursor *api.Cursor       `json:"cursor,omitempty"`
	Events []json.RawMessage `json:"events"`
}

func (s *Server) postEvents(w http.ResponseWriter, r *http.Request, sender *store.Sender) {
	var env batchEnvelope
	if err := decode(w, r, api.MaxBatchBytes, &env); err != nil {
		s.fail(w, err)
		return
	}
	if env.Schema != api.BatchSchema {
		s.fail(w, &store.InputError{Msg: fmt.Sprintf("schema must be %q", api.BatchSchema)})
		return
	}
	if len(env.Events) > api.MaxBatchEvents {
		s.fail(w, &store.InputError{Msg: fmt.Sprintf("at most %d events per batch", api.MaxBatchEvents)})
		return
	}
	in := make([]store.IncomingEvent, len(env.Events))
	for i, raw := range env.Events {
		if err := decodeStrict(raw, &in[i].Event); err != nil {
			in[i].DecodeErr = err
		}
	}
	resp, err := s.st.Ingest(r.Context(), sender, env.Cursor, in)
	if err != nil {
		if errors.Is(err, store.ErrForbidden) {
			err = fmt.Errorf("%w: sender %s may not write the cursor for %s/%s", err, sender.Name,
				env.Cursor.Source, strings.ToLower(env.Cursor.Account))
		}
		s.fail(w, err)
		return
	}
	counts := map[string]int{}
	for _, res := range resp.Results {
		counts[res.Result]++
		if res.Result == api.ResultRejected {
			s.log.Info("event rejected", "sender", sender.Name, "index", res.Index, "key", res.Key, "reason", res.Reason)
		}
	}
	s.log.Info("batch", "sender", sender.Name, "events", len(resp.Results),
		"accepted", counts[api.ResultAccepted], "duplicate", counts[api.ResultDuplicate],
		"stale", counts[api.ResultStale], "rejected", counts[api.ResultRejected], "cursor_stored", resp.CursorStored)
	s.writeJSON(w, http.StatusOK, resp)
}

func sourceAccount(r *http.Request) (string, string, error) {
	q := r.URL.Query()
	source, account := q.Get("source"), q.Get("account")
	if source == "" || account == "" {
		return "", "", &store.InputError{Msg: "source and account are required"}
	}
	return source, account, nil
}

func (s *Server) getCursor(w http.ResponseWriter, r *http.Request, sender *store.Sender) {
	source, account, err := sourceAccount(r)
	if err == nil {
		var resp *api.CursorResponse
		if resp, err = s.st.Cursor(r.Context(), sender, source, account); err == nil {
			s.writeJSON(w, http.StatusOK, resp)
			return
		}
	}
	s.fail(w, scopeErr(err, sender, source, account))
}

func (s *Server) getOpen(w http.ResponseWriter, r *http.Request, sender *store.Sender) {
	source, account, err := sourceAccount(r)
	if err == nil {
		var resp *api.OpenResponse
		if resp, err = s.st.Open(r.Context(), sender, source, account); err == nil {
			s.writeJSON(w, http.StatusOK, resp)
			return
		}
	}
	s.fail(w, scopeErr(err, sender, source, account))
}

func scopeErr(err error, sender *store.Sender, source, account string) error {
	if errors.Is(err, store.ErrForbidden) {
		return fmt.Errorf("%w: sender %s may not use %s/%s", err, sender.Name, source, strings.ToLower(account))
	}
	return err
}

func (s *Server) pull(w http.ResponseWriter, r *http.Request, sender *store.Sender) {
	var req api.PullRequest
	if err := decodeOptional(w, r, &req); err != nil {
		s.fail(w, err)
		return
	}
	resp, err := s.st.Pull(r.Context(), sender.Name, req)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.log.Info("pull", "sender", sender.Name, "lease", resp.LeaseID, "items", len(resp.Items))
	s.writeJSON(w, http.StatusOK, resp)
}

// decodeOptional decodes a body that may be empty.
func decodeOptional(w http.ResponseWriter, r *http.Request, v any) error {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxJSONBytes))
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return nil
	}
	return decodeStrict(body, v)
}

func leaseErr(err error, id string) error {
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("lease %s: %w", id, err)
	}
	if errors.Is(err, store.ErrForbidden) {
		return fmt.Errorf("%w: lease %s belongs to another sender", err, id)
	}
	return err
}

func (s *Server) ack(w http.ResponseWriter, r *http.Request, sender *store.Sender) {
	var req api.AckRequest
	if err := decode(w, r, maxJSONBytes, &req); err != nil {
		s.fail(w, err)
		return
	}
	resp, err := s.st.Ack(r.Context(), sender.Name, req)
	if err != nil {
		s.fail(w, leaseErr(err, req.LeaseID))
		return
	}
	counts := map[string]int{}
	for _, res := range resp.Results {
		counts[res.Result]++
		if res.Result == api.AckRejected {
			s.log.Info("ack rejected", "lease", req.LeaseID, "key", res.Key, "reason", res.Reason)
		}
	}
	s.log.Info("ack", "sender", sender.Name, "lease", req.LeaseID, "acks", len(resp.Results),
		"ok", counts[api.AckOK], "duplicate", counts[api.AckDuplicate], "rejected", counts[api.AckRejected])
	s.writeJSON(w, http.StatusOK, resp)
}

func (s *Server) release(w http.ResponseWriter, r *http.Request, sender *store.Sender) {
	var req api.ReleaseRequest
	if err := decode(w, r, maxJSONBytes, &req); err != nil {
		s.fail(w, err)
		return
	}
	resp, err := s.st.Release(r.Context(), sender.Name, req)
	if err != nil {
		s.fail(w, leaseErr(err, req.LeaseID))
		return
	}
	s.log.Info("release", "sender", sender.Name, "lease", req.LeaseID, "released", resp.Released)
	s.writeJSON(w, http.StatusOK, resp)
}

func (s *Server) getLedger(w http.ResponseWriter, r *http.Request, _ *store.Sender) {
	q := r.URL.Query()
	resp, err := s.st.Ledger(r.Context(), store.LedgerFilter{
		State: q.Get("state"), Key: q.Get("key"), Marker: q.Get("marker"), UUID: q.Get("uuid"), Run: q.Get("run"),
	})
	if err != nil {
		s.fail(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, resp)
}

func (s *Server) putLedger(w http.ResponseWriter, r *http.Request, sender *store.Sender) {
	var req api.LedgerPutRequest
	if err := decode(w, r, maxJSONBytes, &req); err != nil {
		s.fail(w, err)
		return
	}
	resp, err := s.st.PutLedger(r.Context(), req)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.log.Info("ledger", "sender", sender.Name, "items", len(resp.Items), "applied_ops", resp.AppliedOps)
	s.writeJSON(w, http.StatusOK, resp)
}

func (s *Server) postAudit(w http.ResponseWriter, r *http.Request, sender *store.Sender) {
	var req api.AuditPostRequest
	if err := decode(w, r, maxJSONBytes, &req); err != nil {
		s.fail(w, err)
		return
	}
	resp, err := s.st.AppendAudit(r.Context(), req.Entries)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.log.Info("audit", "sender", sender.Name, "appended", resp.Appended, "duplicate", resp.Duplicate)
	s.writeJSON(w, http.StatusOK, resp)
}

func (s *Server) getAudit(w http.ResponseWriter, r *http.Request, _ *store.Sender) {
	resp, err := s.st.Audit(r.Context(), r.URL.Query().Get("run"))
	if err != nil {
		s.fail(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, resp)
}

func (s *Server) putSnapshot(w http.ResponseWriter, r *http.Request, sender *store.Sender) {
	run := r.PathValue("run")
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, api.MaxSnapshotBytes))
	if err == nil {
		err = s.st.PutSnapshot(r.Context(), run, body)
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	s.log.Info("snapshot", "sender", sender.Name, "run", run, "bytes", len(body))
	s.writeJSON(w, http.StatusOK, api.SnapshotPutResponse{RunID: run, Bytes: len(body)})
}

func (s *Server) getSnapshot(w http.ResponseWriter, r *http.Request, _ *store.Sender) {
	run := r.PathValue("run")
	body, err := s.st.Snapshot(r.Context(), run)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			err = fmt.Errorf("snapshot %s: %w", run, err)
		}
		s.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(body)
}

func (s *Server) status(w http.ResponseWriter, r *http.Request, _ *store.Sender) {
	resp, err := s.st.Status(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, resp)
}
