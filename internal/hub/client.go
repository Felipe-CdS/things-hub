// Package hub is the Mac tool's client for thub-server's HTTP API.
package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Felipe-CdS/things-hub/api"
)

// Client calls thub-server with the Mac's app token.
type Client struct {
	BaseURL string
	Token   string
	// CFAccessID and CFAccessSecret are sent once the inbox sits behind
	// Cloudflare Access; empty in phase 1.
	CFAccessID     string
	CFAccessSecret string
	HTTP           *http.Client
}

// New returns a client for baseURL with token.
func New(baseURL, token string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Token:   token,
		HTTP:    &http.Client{Timeout: 60 * time.Second},
	}
}

// Error is a non-2xx response.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("thub-server: %d %s", e.Status, e.Message)
}

// IsNotFound reports whether err is a 404 from the server.
func IsNotFound(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == http.StatusNotFound
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case []byte:
		rd = bytes.NewReader(b)
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, rd)
	if err != nil {
		return err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	if c.CFAccessID != "" {
		req.Header.Set("CF-Access-Client-Id", c.CFAccessID)
		req.Header.Set("CF-Access-Client-Secret", c.CFAccessSecret)
	}
	if rd != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		var e api.ErrorResponse
		if json.Unmarshal(raw, &e) != nil || e.Error == "" {
			e.Error = strings.TrimSpace(string(raw))
		}
		return &Error{Status: resp.StatusCode, Message: e.Error}
	}
	if out == nil {
		return nil
	}
	if p, ok := out.(*[]byte); ok {
		*p = raw
		return nil
	}
	return json.Unmarshal(raw, out)
}

// Health calls /healthz.
func (c *Client) Health(ctx context.Context) error {
	return c.do(ctx, http.MethodGet, "/healthz", nil, nil)
}

// PostEvents sends a batch.
func (c *Client) PostEvents(ctx context.Context, b api.Batch) (api.BatchResponse, error) {
	var out api.BatchResponse
	err := c.do(ctx, http.MethodPost, "/v1/events", b, &out)
	return out, err
}

// Open lists open items for a source and account.
func (c *Client) Open(ctx context.Context, source, account string) (api.OpenResponse, error) {
	var out api.OpenResponse
	q := url.Values{"source": {source}, "account": {account}}
	err := c.do(ctx, http.MethodGet, "/v1/open?"+q.Encode(), nil, &out)
	return out, err
}

// Pull leases ready items.
func (c *Client) Pull(ctx context.Context, req api.PullRequest) (api.PullResponse, error) {
	var out api.PullResponse
	err := c.do(ctx, http.MethodPost, "/v1/pull", req, &out)
	return out, err
}

// Ack records outcomes for a lease.
func (c *Client) Ack(ctx context.Context, req api.AckRequest) (api.AckResponse, error) {
	var out api.AckResponse
	err := c.do(ctx, http.MethodPost, "/v1/ack", req, &out)
	return out, err
}

// Release returns leased items to the queue.
func (c *Client) Release(ctx context.Context, req api.ReleaseRequest) (api.ReleaseResponse, error) {
	var out api.ReleaseResponse
	err := c.do(ctx, http.MethodPost, "/v1/release", req, &out)
	return out, err
}

// LedgerQuery narrows GET /v1/ledger.
type LedgerQuery struct {
	State, Key, Marker, UUID, Run string
}

// Ledger reads ledger rows, and with Run, that run's applied ops.
func (c *Client) Ledger(ctx context.Context, q LedgerQuery) (api.LedgerResponse, error) {
	v := url.Values{}
	for k, s := range map[string]string{"state": q.State, "key": q.Key, "marker": q.Marker, "uuid": q.UUID, "run": q.Run} {
		if s != "" {
			v.Set(k, s)
		}
	}
	path := "/v1/ledger"
	if len(v) > 0 {
		path += "?" + v.Encode()
	}
	var out api.LedgerResponse
	err := c.do(ctx, http.MethodGet, path, nil, &out)
	return out, err
}

// PutLedger upserts ledger rows and applied ops atomically.
func (c *Client) PutLedger(ctx context.Context, req api.LedgerPutRequest) (api.LedgerPutResponse, error) {
	var out api.LedgerPutResponse
	err := c.do(ctx, http.MethodPut, "/v1/ledger/items", req, &out)
	return out, err
}

// PostAudit appends audit entries.
func (c *Client) PostAudit(ctx context.Context, entries []api.AuditEntry) (api.AuditPostResponse, error) {
	var out api.AuditPostResponse
	err := c.do(ctx, http.MethodPost, "/v1/audit", api.AuditPostRequest{Entries: entries}, &out)
	return out, err
}

// Audit reads a run's audit log in recorded order.
func (c *Client) Audit(ctx context.Context, run string) (api.AuditResponse, error) {
	var out api.AuditResponse
	err := c.do(ctx, http.MethodGet, "/v1/audit?"+url.Values{"run": {run}}.Encode(), nil, &out)
	return out, err
}

// PutSnapshot stores a run's Things snapshot.
func (c *Client) PutSnapshot(ctx context.Context, run string, body []byte) error {
	return c.do(ctx, http.MethodPut, "/v1/snapshots/"+url.PathEscape(run), body, nil)
}

// Snapshot fetches a run's stored snapshot.
func (c *Client) Snapshot(ctx context.Context, run string) ([]byte, error) {
	var out []byte
	err := c.do(ctx, http.MethodGet, "/v1/snapshots/"+url.PathEscape(run), nil, &out)
	return out, err
}

// Status reads the queue status.
func (c *Client) Status(ctx context.Context) (api.StatusResponse, error) {
	var out api.StatusResponse
	err := c.do(ctx, http.MethodGet, "/v1/status", nil, &out)
	return out, err
}

// Cursor reads the stored cursor for a source and account.
func (c *Client) Cursor(ctx context.Context, source, account string) (api.CursorResponse, error) {
	var out api.CursorResponse
	q := url.Values{"source": {source}, "account": {account}}
	err := c.do(ctx, http.MethodGet, "/v1/cursor?"+q.Encode(), nil, &out)
	return out, err
}
