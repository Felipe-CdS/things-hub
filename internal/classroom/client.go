// Package classroom is a minimal read-only Google Classroom client for the
// Mac-side Classroom routine: the installed-app OAuth login, the token
// refresh, and the course, coursework and submission reads it needs.
package classroom

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/oauth2"
)

// Google endpoints. Tests point Config at a fake server instead.
const (
	GoogleAuthURL  = "https://accounts.google.com/o/oauth2/v2/auth"
	GoogleTokenURL = "https://oauth2.googleapis.com/token"
	ClassroomURL   = "https://classroom.googleapis.com"
)

// Scopes are the read-only scopes the login asks for.
var Scopes = []string{
	"https://www.googleapis.com/auth/classroom.courses.readonly",
	"https://www.googleapis.com/auth/classroom.coursework.me.readonly",
	"https://www.googleapis.com/auth/classroom.student-submissions.me.readonly",
}

// AnnouncementsScope is added to the login only with --announcements.
const AnnouncementsScope = "https://www.googleapis.com/auth/classroom.announcements.readonly"

// ErrLoginExpired means Google refused the refresh token.
var ErrLoginExpired = errors.New("Google refused the refresh token (revoked, expired, or the app is in Testing mode); run `thub-classroom login` again")

// Config holds the OAuth client and the endpoints.
type Config struct {
	ClientID     string
	ClientSecret string
	AuthURL      string // default GoogleAuthURL
	TokenURL     string // default GoogleTokenURL
	APIURL       string // default ClassroomURL
	HTTPClient   *http.Client
}

func (c Config) oauth(scopes []string) *oauth2.Config {
	auth, token := c.AuthURL, c.TokenURL
	if auth == "" {
		auth = GoogleAuthURL
	}
	if token == "" {
		token = GoogleTokenURL
	}
	return &oauth2.Config{
		ClientID:     c.ClientID,
		ClientSecret: c.ClientSecret,
		Endpoint:     oauth2.Endpoint{AuthURL: auth, TokenURL: token, AuthStyle: oauth2.AuthStyleInParams},
		Scopes:       scopes,
	}
}

func (c Config) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (c Config) oauthCtx(ctx context.Context) context.Context {
	return context.WithValue(ctx, oauth2.HTTPClient, c.httpClient())
}

// Client calls the Classroom API as the logged-in student.
type Client struct {
	http *http.Client
	base string
}

// NewClient returns a client that refreshes its access token from
// refreshToken as needed.
func NewClient(ctx context.Context, cfg Config, refreshToken string) *Client {
	ctx = cfg.oauthCtx(ctx)
	ts := cfg.oauth(Scopes).TokenSource(ctx, &oauth2.Token{RefreshToken: refreshToken})
	return newClient(ctx, cfg, ts)
}

func newClient(ctx context.Context, cfg Config, ts oauth2.TokenSource) *Client {
	base := cfg.APIURL
	if base == "" {
		base = ClassroomURL
	}
	hc := oauth2.NewClient(ctx, ts)
	hc.Timeout = cfg.httpClient().Timeout
	return &Client{http: hc, base: strings.TrimSuffix(base, "/")}
}

// apiError is Google's error body.
type apiError struct {
	Error struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error"`
}

// get fetches path with query and decodes the JSON body into v.
func (c *Client) get(ctx context.Context, path string, q url.Values, v any) error {
	u := c.base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		var re *oauth2.RetrieveError
		if errors.As(err, &re) && re.ErrorCode == "invalid_grant" {
			return ErrLoginExpired
		}
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		var ae apiError
		if json.Unmarshal(body, &ae) == nil && ae.Error.Message != "" {
			return fmt.Errorf("classroom GET %s: HTTP %d %s: %s", path, resp.StatusCode, ae.Error.Status, ae.Error.Message)
		}
		return fmt.Errorf("classroom GET %s: HTTP %d", path, resp.StatusCode)
	}
	return json.Unmarshal(body, v)
}

// list follows nextPageToken and returns every element of the field named
// field across pages.
func (c *Client) list(ctx context.Context, path string, q url.Values, field string) ([]json.RawMessage, error) {
	var out []json.RawMessage
	q.Set("pageSize", "100")
	for page := 0; ; page++ {
		if page >= 100 {
			return nil, fmt.Errorf("classroom GET %s: more than 100 pages", path)
		}
		var body map[string]json.RawMessage
		if err := c.get(ctx, path, q, &body); err != nil {
			return nil, err
		}
		if raw, ok := body[field]; ok {
			var items []json.RawMessage
			if err := json.Unmarshal(raw, &items); err != nil {
				return nil, fmt.Errorf("classroom GET %s: %s: %w", path, field, err)
			}
			out = append(out, items...)
		}
		var next string
		if raw, ok := body["nextPageToken"]; ok {
			if err := json.Unmarshal(raw, &next); err != nil {
				return nil, err
			}
		}
		if next == "" {
			if out == nil {
				out = []json.RawMessage{}
			}
			return out, nil
		}
		q.Set("pageToken", next)
	}
}

// Courses returns the student's active courses, as the API returns them.
func (c *Client) Courses(ctx context.Context) ([]json.RawMessage, error) {
	return c.list(ctx, "/v1/courses", url.Values{"studentId": {"me"}, "courseStates": {"ACTIVE"}}, "courses")
}

// CourseWork returns a course's published coursework.
func (c *Client) CourseWork(ctx context.Context, courseID string) ([]json.RawMessage, error) {
	return c.list(ctx, "/v1/courses/"+url.PathEscape(courseID)+"/courseWork",
		url.Values{"courseWorkStates": {"PUBLISHED"}}, "courseWork")
}

// Submissions returns the student's own submissions for every coursework in
// a course.
func (c *Client) Submissions(ctx context.Context, courseID string) ([]json.RawMessage, error) {
	return c.list(ctx, "/v1/courses/"+url.PathEscape(courseID)+"/courseWork/-/studentSubmissions",
		url.Values{"userId": {"me"}}, "studentSubmissions")
}
