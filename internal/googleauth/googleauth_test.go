package googleauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

const clientJSON = `{
  "installed": {
    "client_id": "test-client-id.apps.googleusercontent.com",
    "client_secret": "test-secret",
    "auth_uri": "https://accounts.google.com/o/oauth2/auth",
    "token_uri": "https://oauth2.googleapis.com/token",
    "redirect_uris": ["http://localhost"]
  }
}`

func writeClientFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "google-client.json")
	if err := os.WriteFile(p, []byte(clientJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestEnvelopeRoundTripAndPerms(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "sub", "google-token.json")
	in := &envelope{
		Token: &oauth2.Token{
			AccessToken:  "at-1",
			RefreshToken: "rt-1",
			TokenType:    "Bearer",
			Expiry:       time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC),
		},
		Scopes: []string{"scope.a", "scope.b"},
	}
	if err := writeEnvelope(tokenFile, in); err != nil {
		t.Fatalf("writeEnvelope: %v", err)
	}

	fi, err := os.Stat(tokenFile)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("token file perms = %o, want 0600", got)
	}

	out, err := readEnvelope(tokenFile)
	if err != nil {
		t.Fatalf("readEnvelope: %v", err)
	}
	if out.Token.AccessToken != in.Token.AccessToken ||
		out.Token.RefreshToken != in.Token.RefreshToken ||
		out.Token.TokenType != in.Token.TokenType ||
		!out.Token.Expiry.Equal(in.Token.Expiry) {
		t.Errorf("token round-trip mismatch: got %+v want %+v", out.Token, in.Token)
	}
	if len(out.Scopes) != 2 || out.Scopes[0] != "scope.a" || out.Scopes[1] != "scope.b" {
		t.Errorf("scopes round-trip mismatch: %v", out.Scopes)
	}

	// Overwrite must stay atomic and keep 0600 even with a permissive umask.
	in.Token.AccessToken = "at-2"
	if err := writeEnvelope(tokenFile, in); err != nil {
		t.Fatalf("writeEnvelope overwrite: %v", err)
	}
	fi, err = os.Stat(tokenFile)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("token file perms after overwrite = %o, want 0600", got)
	}
}

func TestReadEnvelopeNeedsAuth(t *testing.T) {
	dir := t.TempDir()

	corrupt := filepath.Join(dir, "corrupt.json")
	if err := os.WriteFile(corrupt, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(dir, "empty.json")
	if err := os.WriteFile(empty, []byte(`{"token":{},"scopes":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}

	for name, path := range map[string]string{
		"missing":  filepath.Join(dir, "nope.json"),
		"corrupt":  corrupt,
		"no token": empty,
	} {
		if _, err := readEnvelope(path); !errors.Is(err, ErrNeedsAuth) {
			t.Errorf("%s: err = %v, want ErrNeedsAuth", name, err)
		}
	}
}

func TestTokenSourceMissingFileNeedsAuth(t *testing.T) {
	clientFile := writeClientFile(t)
	_, err := TokenSource(context.Background(), clientFile,
		filepath.Join(t.TempDir(), "absent.json"), []string{"scope.a"})
	if !errors.Is(err, ErrNeedsAuth) {
		t.Fatalf("err = %v, want ErrNeedsAuth", err)
	}
}

// fakeSource returns queued tokens in order, repeating the last one.
type fakeSource struct {
	toks []*oauth2.Token
	i    int
}

func (f *fakeSource) Token() (*oauth2.Token, error) {
	if len(f.toks) == 0 {
		return nil, errors.New("no tokens")
	}
	t := f.toks[f.i]
	if f.i < len(f.toks)-1 {
		f.i++
	}
	return t, nil
}

func TestPersistingSourceWritesOnChangeOnly(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "google-token.json")
	tok1 := &oauth2.Token{AccessToken: "at-1", RefreshToken: "rt-1", TokenType: "Bearer",
		Expiry: time.Now().Add(time.Hour).Truncate(time.Second)}
	tok2 := &oauth2.Token{AccessToken: "at-2", RefreshToken: "rt-2", TokenType: "Bearer",
		Expiry: time.Now().Add(2 * time.Hour).Truncate(time.Second)}

	if err := writeEnvelope(tokenFile, &envelope{Token: tok1, Scopes: []string{"scope.a"}}); err != nil {
		t.Fatal(err)
	}
	ps := &persistingSource{
		base:   &fakeSource{toks: []*oauth2.Token{tok1, tok1, tok2}},
		file:   tokenFile,
		scopes: []string{"scope.a"},
		last:   tok1,
	}

	// Unchanged token: no write. Prove it by removing the file — the skip
	// path must not recreate it.
	if _, err := ps.Token(); err != nil {
		t.Fatalf("Token #1: %v", err)
	}
	if err := os.Remove(tokenFile); err != nil {
		t.Fatal(err)
	}
	if _, err := ps.Token(); err != nil {
		t.Fatalf("Token #2: %v", err)
	}
	if _, err := os.Stat(tokenFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unchanged token was re-persisted (stat err = %v)", err)
	}

	// Changed token: persisted, with the recorded scopes intact.
	got, err := ps.Token()
	if err != nil {
		t.Fatalf("Token #3: %v", err)
	}
	if got.AccessToken != "at-2" {
		t.Fatalf("Token #3 = %q, want at-2", got.AccessToken)
	}
	env, err := readEnvelope(tokenFile)
	if err != nil {
		t.Fatalf("readEnvelope after refresh: %v", err)
	}
	if env.Token.AccessToken != "at-2" || env.Token.RefreshToken != "rt-2" {
		t.Errorf("persisted token = %q/%q, want at-2/rt-2", env.Token.AccessToken, env.Token.RefreshToken)
	}
	if len(env.Scopes) != 1 || env.Scopes[0] != "scope.a" {
		t.Errorf("persisted scopes = %v, want [scope.a]", env.Scopes)
	}
	fi, err := os.Stat(tokenFile)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("refreshed token file perms = %o, want 0600", got)
	}
}

func TestScopeDrift(t *testing.T) {
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "google-token.json")
	err := writeEnvelope(tokenFile, &envelope{
		Token:  &oauth2.Token{AccessToken: "at", RefreshToken: "rt"},
		Scopes: []string{"gmail.readonly", "chat.messages.readonly"},
	})
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		file    string
		want    []string
		missing []string
		errIs   error
	}{
		{name: "all present", file: tokenFile,
			want: []string{"gmail.readonly", "chat.messages.readonly"}},
		{name: "subset present", file: tokenFile,
			want: []string{"gmail.readonly"}},
		{name: "one missing", file: tokenFile,
			want:    []string{"gmail.readonly", "drive.readonly"},
			missing: []string{"drive.readonly"}},
		{name: "all missing preserves order", file: tokenFile,
			want:    []string{"z.scope", "a.scope"},
			missing: []string{"z.scope", "a.scope"}},
		{name: "empty want", file: tokenFile, want: nil},
		{name: "missing file", file: filepath.Join(dir, "absent.json"),
			want: []string{"gmail.readonly"}, errIs: ErrNeedsAuth},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			missing, err := ScopeDrift(tt.file, tt.want)
			if tt.errIs != nil {
				if !errors.Is(err, tt.errIs) {
					t.Fatalf("err = %v, want %v", err, tt.errIs)
				}
				return
			}
			if err != nil {
				t.Fatalf("ScopeDrift: %v", err)
			}
			if len(missing) != len(tt.missing) {
				t.Fatalf("missing = %v, want %v", missing, tt.missing)
			}
			for i := range missing {
				if missing[i] != tt.missing[i] {
					t.Fatalf("missing = %v, want %v", missing, tt.missing)
				}
			}
		})
	}
}

func TestAuthenticateExchange(t *testing.T) {
	// Fake token endpoint: verifies the PKCE verifier and code arrive, then
	// issues a token with a granted-scope string.
	var gotCode, gotVerifier string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("token endpoint parse form: %v", err)
		}
		gotCode = r.Form.Get("code")
		gotVerifier = r.Form.Get("code_verifier")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
			"access_token":  "granted-access",
			"refresh_token": "granted-refresh",
			"token_type":    "Bearer",
			"expires_in":    3600,
			"scope":         "scope.granted.a scope.granted.b",
		})
	}))
	defer ts.Close()

	testEndpoint = &oauth2.Endpoint{
		AuthURL:  ts.URL + "/auth",
		TokenURL: ts.URL + "/token",
	}
	origOpen, origOut := openBrowser, authOut
	t.Cleanup(func() {
		testEndpoint = nil
		openBrowser, authOut = origOpen, origOut
	})
	authOut = io.Discard

	// Stand-in for the browser: parse the consent URL, then hit the local
	// redirect endpoint the way Google would after user approval.
	browserErr := make(chan error, 1)
	openBrowser = func(authURL string) error {
		go func() {
			browserErr <- func() error {
				u, err := url.Parse(authURL)
				if err != nil {
					return err
				}
				q := u.Query()
				redirect := q.Get("redirect_uri")
				state := q.Get("state")
				if q.Get("code_challenge_method") != "S256" {
					return fmt.Errorf("code_challenge_method = %q, want S256", q.Get("code_challenge_method"))
				}
				if q.Get("code_challenge") == "" {
					return errors.New("missing code_challenge")
				}
				if q.Get("access_type") != "offline" {
					return fmt.Errorf("access_type = %q, want offline", q.Get("access_type"))
				}
				resp, err := http.Get(redirect + "?state=" + url.QueryEscape(state) + "&code=test-auth-code")
				if err != nil {
					return err
				}
				defer resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					return fmt.Errorf("redirect hit status %d", resp.StatusCode)
				}
				return nil
			}()
		}()
		return nil
	}

	clientFile := writeClientFile(t)
	tokenFile := filepath.Join(t.TempDir(), "google-token.json")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	scopes := []string{"scope.requested"}
	if err := Authenticate(ctx, clientFile, tokenFile, scopes); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if err := <-browserErr; err != nil {
		t.Fatalf("browser stand-in: %v", err)
	}

	if gotCode != "test-auth-code" {
		t.Errorf("token endpoint saw code %q, want test-auth-code", gotCode)
	}
	if gotVerifier == "" {
		t.Error("token endpoint saw no code_verifier (PKCE missing)")
	}

	env, err := readEnvelope(tokenFile)
	if err != nil {
		t.Fatalf("readEnvelope: %v", err)
	}
	if env.Token.AccessToken != "granted-access" || env.Token.RefreshToken != "granted-refresh" {
		t.Errorf("persisted token = %q/%q", env.Token.AccessToken, env.Token.RefreshToken)
	}
	// Granted scopes from the token response win over the requested set.
	if len(env.Scopes) != 2 || env.Scopes[0] != "scope.granted.a" || env.Scopes[1] != "scope.granted.b" {
		t.Errorf("recorded scopes = %v, want the granted pair", env.Scopes)
	}
	fi, err := os.Stat(tokenFile)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("token file perms = %o, want 0600", got)
	}

	// The persisted envelope must satisfy TokenSource without re-auth.
	src, err := TokenSource(context.Background(), clientFile, tokenFile, scopes)
	if err != nil {
		t.Fatalf("TokenSource after Authenticate: %v", err)
	}
	tok, err := src.Token()
	if err != nil {
		t.Fatalf("Token after Authenticate: %v", err)
	}
	if tok.AccessToken != "granted-access" {
		t.Errorf("TokenSource token = %q, want granted-access", tok.AccessToken)
	}
}

func TestAuthenticateDenied(t *testing.T) {
	testEndpoint = &oauth2.Endpoint{AuthURL: "http://127.0.0.1:1/auth", TokenURL: "http://127.0.0.1:1/token"}
	origOpen, origOut := openBrowser, authOut
	t.Cleanup(func() {
		testEndpoint = nil
		openBrowser, authOut = origOpen, origOut
	})
	authOut = io.Discard
	openBrowser = func(authURL string) error {
		go func() {
			u, _ := url.Parse(authURL)
			redirect := u.Query().Get("redirect_uri")
			resp, err := http.Get(redirect + "?error=access_denied")
			if err == nil {
				resp.Body.Close()
			}
		}()
		return nil
	}

	clientFile := writeClientFile(t)
	tokenFile := filepath.Join(t.TempDir(), "google-token.json")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := Authenticate(ctx, clientFile, tokenFile, []string{"scope.a"})
	if err == nil || !strings.Contains(err.Error(), "access_denied") {
		t.Fatalf("err = %v, want access_denied", err)
	}
	if _, statErr := os.Stat(tokenFile); !errors.Is(statErr, os.ErrNotExist) {
		t.Error("token file written despite denial")
	}
}
