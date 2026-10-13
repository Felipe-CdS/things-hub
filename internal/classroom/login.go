package classroom

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"golang.org/x/oauth2"
)

// LoginOptions tunes Login.
type LoginOptions struct {
	// Port is the loopback port for the redirect; 0 picks a free one.
	Port int
	// Announcements adds the read-only announcements scope.
	Announcements bool
	// OpenBrowser opens the consent page; nil only prints its URL.
	OpenBrowser func(url string) error
	// Timeout bounds the wait for the browser (default 10 minutes).
	Timeout time.Duration
	Out     io.Writer
}

type callback struct {
	code string
	err  error
}

// Login runs the installed-app flow: a loopback redirect on 127.0.0.1 with
// PKCE. Google asks which account to use and shows the consent screen every
// time, so it always returns a refresh token.
func Login(ctx context.Context, cfg Config, opts LoginOptions) (*oauth2.Token, error) {
	if cfg.ClientID == "" || cfg.ClientSecret == "" {
		return nil, errors.New("THUB_GOOGLE_CLIENT_ID and THUB_GOOGLE_CLIENT_SECRET must be set")
	}
	if opts.Timeout == 0 {
		opts.Timeout = 10 * time.Minute
	}
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(opts.Port)))
	if err != nil {
		return nil, fmt.Errorf("listen for the login redirect: %w", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	scopes := Scopes
	if opts.Announcements {
		scopes = append(append([]string{}, Scopes...), AnnouncementsScope)
	}
	oc := cfg.oauth(scopes)
	oc.RedirectURL = fmt.Sprintf("http://127.0.0.1:%d/", port)
	state, err := randomState()
	if err != nil {
		return nil, err
	}
	verifier := oauth2.GenerateVerifier()
	authURL := oc.AuthCodeURL(state, oauth2.AccessTypeOffline, oauth2.S256ChallengeOption(verifier),
		oauth2.SetAuthURLParam("prompt", "select_account consent"))

	got := make(chan callback, 1)
	srv := &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query()
		var cb callback
		switch {
		case q.Get("state") != state:
			cb.err = errors.New("the login redirect carried the wrong state; start the login again")
		case q.Get("error") != "":
			cb.err = fmt.Errorf("Google refused the login: %s", q.Get("error"))
		case q.Get("code") == "":
			cb.err = errors.New("the login redirect carried no code")
		default:
			cb.code = q.Get("code")
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if cb.err != nil {
			fmt.Fprintf(w, "<p>things-hub login failed: %s</p>", html.EscapeString(cb.err.Error()))
		} else {
			fmt.Fprint(w, "<p>things-hub is logged in to Google Classroom. You can close this tab.</p>")
		}
		select {
		case got <- cb:
		default:
		}
	})}
	go srv.Serve(ln)
	defer srv.Close()

	fmt.Fprintf(opts.Out, "Open this page, pick the Google account that holds Classroom, and allow read-only access:\n\n%s\n\n", authURL)
	if opts.OpenBrowser != nil {
		if err := opts.OpenBrowser(authURL); err != nil {
			fmt.Fprintf(opts.Out, "Could not open the browser (%v); open the page above by hand.\n", err)
		}
	}
	fmt.Fprintf(opts.Out, "Waiting for the browser on http://127.0.0.1:%d/ ...\n", port)

	var cb callback
	select {
	case cb = <-got:
	case <-time.After(opts.Timeout):
		return nil, errors.New("timed out waiting for the browser login")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if cb.err != nil {
		return nil, cb.err
	}
	tok, err := oc.Exchange(cfg.oauthCtx(ctx), cb.code, oauth2.VerifierOption(verifier))
	if err != nil {
		return nil, fmt.Errorf("exchange the login code: %w", err)
	}
	if tok.RefreshToken == "" {
		return nil, errors.New("Google returned no refresh token; remove the app's access at myaccount.google.com/permissions and log in again")
	}
	return tok, nil
}

func randomState() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// ClientFromToken returns a client using a token from Login.
func ClientFromToken(ctx context.Context, cfg Config, tok *oauth2.Token) *Client {
	ctx = cfg.oauthCtx(ctx)
	return newClient(ctx, cfg, cfg.oauth(Scopes).TokenSource(ctx, tok))
}
