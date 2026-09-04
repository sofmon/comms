// Package googleauth implements the shared Google desktop OAuth flow used
// by the gmail and gchat connectors: loopback-redirect + PKCE interactive
// authorization, an atomically persisted token file (0600), silent refresh
// with write-back, and scope-drift detection so the CLI can demand re-auth
// when the configured scope set grows (e.g. enabling mirror_drive_files).
//
// The token file is a small JSON envelope {token, scopes} — the granted
// scopes are recorded alongside the oauth2 token so ScopeDrift can compare
// them against what the current config wants. Token values are never logged.
package googleauth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/renameio/v2"
	"golang.org/x/oauth2"
)

// ErrNeedsAuth is returned (wrapped) when no usable cached token exists;
// the CLI reacts by running the interactive Authenticate flow.
var ErrNeedsAuth = errors.New("google authorization required (run `comms auth google`)")

// Test seams. openBrowser launches the system browser (macOS `open`);
// authOut receives the human-readable auth URL prompt; testEndpoint, when
// non-nil, replaces the Google endpoint so tests can point Exchange at a
// local httptest server.
var (
	openBrowser = func(url string) error {
		return exec.Command("open", url).Start()
	}
	authOut      io.Writer = os.Stdout
	testEndpoint *oauth2.Endpoint
)

// envelope is the persisted token-file format: the oauth2 token plus the
// scope set that was actually granted when it was issued.
type envelope struct {
	Token  *oauth2.Token `json:"token"`
	Scopes []string      `json:"scopes"`
}

// LoadClient parses the OAuth "Desktop app" client file downloaded from the
// Google Cloud Console and returns a config requesting the given scopes.
//
// This mirrors golang.org/x/oauth2/google.ConfigFromJSON exactly (the
// "installed"/"web" credential JSON). We cannot import that package here:
// its default-credentials code pulls in cloud.google.com/go/compute/metadata,
// which is not in this module's dependency set.
func LoadClient(clientFile string, scopes ...string) (*oauth2.Config, error) {
	b, err := os.ReadFile(clientFile)
	if err != nil {
		return nil, fmt.Errorf("googleauth: read client file: %w", err)
	}
	type cred struct {
		ClientID     string   `json:"client_id"`
		ClientSecret string   `json:"client_secret"`
		RedirectURIs []string `json:"redirect_uris"`
		AuthURI      string   `json:"auth_uri"`
		TokenURI     string   `json:"token_uri"`
	}
	var j struct {
		Web       *cred `json:"web"`
		Installed *cred `json:"installed"`
	}
	if err := json.Unmarshal(b, &j); err != nil {
		return nil, fmt.Errorf("googleauth: parse client file %s: %w", clientFile, err)
	}
	var c *cred
	switch {
	case j.Installed != nil:
		c = j.Installed
	case j.Web != nil:
		c = j.Web
	default:
		return nil, fmt.Errorf("googleauth: client file %s: no credentials found (expected a Desktop-app client JSON with an %q key)", clientFile, "installed")
	}
	if len(c.RedirectURIs) < 1 {
		return nil, fmt.Errorf("googleauth: client file %s: missing redirect URL", clientFile)
	}
	return &oauth2.Config{
		ClientID:     c.ClientID,
		ClientSecret: c.ClientSecret,
		RedirectURL:  c.RedirectURIs[0],
		Scopes:       scopes,
		Endpoint: oauth2.Endpoint{
			AuthURL:  c.AuthURI,
			TokenURL: c.TokenURI,
		},
	}, nil
}

// TokenSource returns a token source backed by the cached token file. If no
// usable token is cached the error wraps ErrNeedsAuth. Tokens refreshed by
// the underlying source are persisted back to tokenFile atomically (0600)
// so refresh-token rotation survives restarts.
func TokenSource(ctx context.Context, clientFile, tokenFile string, scopes []string) (oauth2.TokenSource, error) {
	cfg, err := LoadClient(clientFile, scopes...)
	if err != nil {
		return nil, err
	}
	env, err := readEnvelope(tokenFile)
	if err != nil {
		return nil, err
	}
	return &persistingSource{
		base:   cfg.TokenSource(ctx, env.Token),
		file:   tokenFile,
		scopes: env.Scopes, // refresh never changes the grant; keep recorded scopes
		last:   env.Token,
	}, nil
}

// persistingSource wraps an oauth2.TokenSource and writes every changed
// token back to the envelope file, so silent refreshes (and refresh-token
// rotation) are durable.
type persistingSource struct {
	mu     sync.Mutex
	base   oauth2.TokenSource
	file   string
	scopes []string
	last   *oauth2.Token
}

func (p *persistingSource) Token() (*oauth2.Token, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	tok, err := p.base.Token()
	if err != nil {
		return nil, err
	}
	if !sameToken(p.last, tok) {
		if err := writeEnvelope(p.file, &envelope{Token: tok, Scopes: p.scopes}); err != nil {
			// Losing a rotated refresh token silently would force a future
			// re-auth; fail loudly instead.
			return nil, fmt.Errorf("googleauth: persist refreshed token: %w", err)
		}
		p.last = tok
	}
	return tok, nil
}

func sameToken(a, b *oauth2.Token) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.AccessToken == b.AccessToken &&
		a.RefreshToken == b.RefreshToken &&
		a.Expiry.Equal(b.Expiry)
}

// Authenticate runs the interactive desktop OAuth flow: it starts a loopback
// listener on 127.0.0.1, opens the consent URL in the browser (and prints
// it, for headless/SSH use), exchanges the redirected code with PKCE, and
// persists the token envelope (with the granted scopes) to tokenFile.
func Authenticate(ctx context.Context, clientFile, tokenFile string, scopes []string) error {
	cfg, err := LoadClient(clientFile, scopes...)
	if err != nil {
		return err
	}
	if testEndpoint != nil {
		cfg.Endpoint = *testEndpoint
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("googleauth: loopback listener: %w", err)
	}
	defer ln.Close()
	cfg.RedirectURL = fmt.Sprintf("http://%s/callback", ln.Addr().String())

	stateBytes := make([]byte, 16)
	if _, err := rand.Read(stateBytes); err != nil {
		return fmt.Errorf("googleauth: random state: %w", err)
	}
	state := hex.EncodeToString(stateBytes)
	verifier := oauth2.GenerateVerifier()

	type result struct {
		code string
		err  error
	}
	resCh := make(chan result, 1)
	deliver := func(r result) {
		select {
		case resCh <- r:
		default: // a second redirect hit; first one wins
		}
	}
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()
			switch {
			case q.Get("error") != "":
				http.Error(w, "Authorization failed. You can close this window.", http.StatusBadRequest)
				deliver(result{err: fmt.Errorf("googleauth: authorization denied: %s", q.Get("error"))})
			case q.Get("code") != "":
				if q.Get("state") != state {
					http.Error(w, "State mismatch. You can close this window.", http.StatusBadRequest)
					deliver(result{err: errors.New("googleauth: oauth state mismatch")})
					return
				}
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				fmt.Fprint(w, "<html><body><p>Authorization complete — you can close this window and return to <code>comms</code>.</p></body></html>")
				deliver(result{code: q.Get("code")})
			default:
				// favicon.ico and other stray browser requests
				http.NotFound(w, r)
			}
		}),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go srv.Serve(ln) //nolint:errcheck // returns ErrServerClosed on shutdown
	defer func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(shutCtx) //nolint:errcheck
	}()

	authURL := cfg.AuthCodeURL(state, oauth2.AccessTypeOffline, oauth2.S256ChallengeOption(verifier))
	fmt.Fprintf(authOut, "Open this URL in your browser to authorize comms:\n\n  %s\n\n", authURL)
	if err := openBrowser(authURL); err != nil {
		fmt.Fprintf(authOut, "(could not open browser automatically: %v)\n", err)
	}

	var code string
	select {
	case <-ctx.Done():
		return fmt.Errorf("googleauth: waiting for authorization: %w", ctx.Err())
	case r := <-resCh:
		if r.err != nil {
			return r.err
		}
		code = r.code
	}

	tok, err := cfg.Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return fmt.Errorf("googleauth: code exchange: %w", err)
	}
	granted := grantedScopes(tok, scopes)
	if err := writeEnvelope(tokenFile, &envelope{Token: tok, Scopes: granted}); err != nil {
		return fmt.Errorf("googleauth: persist token: %w", err)
	}
	return nil
}

// grantedScopes extracts the scope set actually granted from the token
// response ("scope" is space-separated per RFC 6749); when the server omits
// it, the requested set is assumed.
func grantedScopes(tok *oauth2.Token, requested []string) []string {
	if s, ok := tok.Extra("scope").(string); ok {
		if fields := strings.Fields(s); len(fields) > 0 {
			return fields
		}
	}
	out := make([]string, len(requested))
	copy(out, requested)
	return out
}

// ScopeDrift reports which of the wanted scopes are absent from the token
// file's recorded grant. A missing or unreadable token file wraps
// ErrNeedsAuth. A non-empty missing list means the CLI must re-run
// Authenticate with the full wanted set.
func ScopeDrift(tokenFile string, want []string) (missing []string, err error) {
	env, err := readEnvelope(tokenFile)
	if err != nil {
		return nil, err
	}
	have := make(map[string]bool, len(env.Scopes))
	for _, s := range env.Scopes {
		have[s] = true
	}
	for _, w := range want {
		if !have[w] {
			missing = append(missing, w)
		}
	}
	return missing, nil
}

func readEnvelope(tokenFile string) (*envelope, error) {
	b, err := os.ReadFile(tokenFile)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("googleauth: no token file at %s: %w", tokenFile, ErrNeedsAuth)
	}
	if err != nil {
		return nil, fmt.Errorf("googleauth: read token file: %w", err)
	}
	var env envelope
	if err := json.Unmarshal(b, &env); err != nil {
		return nil, fmt.Errorf("googleauth: token file %s is corrupt (%v): %w", tokenFile, err, ErrNeedsAuth)
	}
	if env.Token == nil || (env.Token.AccessToken == "" && env.Token.RefreshToken == "") {
		return nil, fmt.Errorf("googleauth: token file %s holds no token: %w", tokenFile, ErrNeedsAuth)
	}
	return &env, nil
}

// writeEnvelope persists the envelope atomically with static 0600
// permissions (renameio: same-directory temp file + rename, explicit chmod
// so the umask cannot widen access).
func writeEnvelope(tokenFile string, env *envelope) error {
	b, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(tokenFile), 0o700); err != nil {
		return err
	}
	pf, err := renameio.NewPendingFile(tokenFile, renameio.WithStaticPermissions(0o600))
	if err != nil {
		return err
	}
	defer pf.Cleanup() //nolint:errcheck // no-op after successful replace
	if _, err := pf.Write(b); err != nil {
		return err
	}
	return pf.CloseAtomicallyReplace()
}
