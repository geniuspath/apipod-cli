package auth

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/geniuspath/apipod-cli/internal/exitcode"
)

func asTokenError(err error, target **TokenError) bool { return errors.As(err, target) }

func TestResolvePrefersEnvKey(t *testing.T) {
	s := fileStore(t)
	if err := s.Save(hostA, sampleCredential()); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvAPIKey, "sk-env-key-0123456789abcdef")
	sess, err := Resolve(s, hostA, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sess.Source != SourceEnv || !sess.IsAPIKey() {
		t.Errorf("source = %s, apiKey = %v; want the env key to win", sess.Source, sess.IsAPIKey())
	}
	header, err := sess.Token(context.Background())
	if err != nil || header != "Bearer sk-env-key-0123456789abcdef" {
		t.Errorf("Token = %q, %v", header, err)
	}
}

func TestResolveRejectsMalformedEnvKey(t *testing.T) {
	for _, bad := range []string{"sk-abc\r\nX-Injected: 1", "sk abc", "sk-ab\tc", "sk-ключ"} {
		t.Setenv(EnvAPIKey, bad)
		if _, err := Resolve(fileStore(t), hostA, nil); exitcode.From(err) != exitcode.Auth {
			t.Errorf("key %q: err = %v, want an auth error", bad, err)
		}
	}
}

// The stored credential of one host is never used for another, whatever
// APIPOD_BASE_URL says.
func TestResolveNeverUsesAnotherHostsCredential(t *testing.T) {
	t.Setenv(EnvAPIKey, "")
	s := fileStore(t)
	if err := s.Save(hostA, sampleCredential()); err != nil {
		t.Fatal(err)
	}
	_, err := Resolve(s, hostB, nil)
	if !errors.Is(err, ErrLoginRequired) {
		t.Fatalf("Resolve(other host) err = %v, want ErrLoginRequired", err)
	}
	if exitcode.From(err) != exitcode.Auth {
		t.Errorf("exit code = %d, want %d", exitcode.From(err), exitcode.Auth)
	}
	sess, err := Resolve(s, hostA, nil)
	if err != nil || sess.Source != SourceFile {
		t.Errorf("Resolve(own host) = %+v, %v", sess, err)
	}
}

func TestStoredAPIKeyIsUsedAsBearer(t *testing.T) {
	t.Setenv(EnvAPIKey, "")
	s := fileStore(t)
	if err := s.Save(hostA, Credential{APIKey: "sk-stored-key-0123456789"}); err != nil {
		t.Fatal(err)
	}
	sess, err := Resolve(s, hostA, nil)
	if err != nil {
		t.Fatal(err)
	}
	if header, _ := sess.Token(context.Background()); header != "Bearer sk-stored-key-0123456789" {
		t.Errorf("Token = %q", header)
	}
}

// tokenServer is a fake /oauth/token that rotates refresh tokens.
type tokenServer struct {
	*httptest.Server
	calls   atomic.Int32
	current atomic.Value // the refresh token that is valid right now
}

func newTokenServer(t *testing.T, validRefresh string) *tokenServer {
	t.Helper()
	ts := &tokenServer{}
	ts.current.Store(validRefresh)
	ts.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		n := ts.calls.Add(1)
		if r.PostForm.Get("grant_type") != "refresh_token" || r.PostForm.Get("client_id") != ClientID ||
			r.PostForm.Get("refresh_token") != ts.current.Load().(string) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"invalid_grant"}`)
			return
		}
		next := url.QueryEscape("rotated-refresh-" + string(rune('0'+n)))
		ts.current.Store(next)
		_, _ = io.WriteString(w, `{"access_token":"new-access-`+string(rune('0'+n))+`","refresh_token":"`+next+`","token_type":"Bearer","expires_in":3600,"scope":"profile email apipod:api"}`)
	}))
	t.Cleanup(ts.Close)
	return ts
}

func expiredCredential(refresh string) Credential {
	c := sampleCredential()
	c.AccessToken = "expired-access"
	c.RefreshToken = refresh
	c.ExpiresAt = time.Now().Add(-time.Minute)
	return c
}

func TestSessionUsesValidAccessTokenWithoutRefreshing(t *testing.T) {
	t.Setenv(EnvAPIKey, "")
	ts := newTokenServer(t, "r0")
	s := fileStore(t)
	if err := s.Save(ts.URL, sampleCredential()); err != nil {
		t.Fatal(err)
	}
	sess, err := Resolve(s, ts.URL, &OAuthClient{BaseURL: ts.URL})
	if err != nil {
		t.Fatal(err)
	}
	header, err := sess.Token(context.Background())
	if err != nil || header != "Bearer "+sampleCredential().AccessToken {
		t.Errorf("Token = %q, %v", header, err)
	}
	if ts.calls.Load() != 0 {
		t.Errorf("token endpoint called %d times for a valid access token", ts.calls.Load())
	}
}

func TestSessionRefreshesAndPersistsRotatedTokens(t *testing.T) {
	t.Setenv(EnvAPIKey, "")
	ts := newTokenServer(t, "r0")
	s := fileStore(t)
	if err := s.Save(ts.URL, expiredCredential("r0")); err != nil {
		t.Fatal(err)
	}
	sess, err := Resolve(s, ts.URL, &OAuthClient{BaseURL: ts.URL})
	if err != nil {
		t.Fatal(err)
	}
	header, err := sess.Token(context.Background())
	if err != nil || header != "Bearer new-access-1" {
		t.Fatalf("Token = %q, %v", header, err)
	}
	// The rotated pair is on disk: the old refresh token is single-use, so
	// losing the new one would end the session.
	stored, err := s.Get(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	if stored.AccessToken != "new-access-1" || stored.RefreshToken != "rotated-refresh-1" {
		t.Errorf("stored credential = %+v, want the rotated pair", stored)
	}
	if stored.Email != "dev@example.com" {
		t.Errorf("account identity lost on refresh: %q", stored.Email)
	}
	// A second call reuses the fresh access token.
	if _, err := sess.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ts.calls.Load() != 1 {
		t.Errorf("token endpoint called %d times, want 1", ts.calls.Load())
	}
}

func TestSessionWithRevokedRefreshTokenRequiresLogin(t *testing.T) {
	t.Setenv(EnvAPIKey, "")
	ts := newTokenServer(t, "the-valid-one")
	s := fileStore(t)
	if err := s.Save(ts.URL, expiredCredential("revoked")); err != nil {
		t.Fatal(err)
	}
	sess, err := Resolve(s, ts.URL, &OAuthClient{BaseURL: ts.URL})
	if err != nil {
		t.Fatal(err)
	}
	_, err = sess.Token(context.Background())
	if exitcode.From(err) != exitcode.Auth {
		t.Fatalf("err = %v (exit %d), want an auth error", err, exitcode.From(err))
	}
}

// Two apipod processes share one refresh token. The loser of the race finds
// its token already rotated and must adopt the winner's pair, not sign out.
func TestSessionAdoptsTokensRotatedByAnotherProcess(t *testing.T) {
	t.Setenv(EnvAPIKey, "")
	ts := newTokenServer(t, "r0")
	s := fileStore(t)
	if err := s.Save(ts.URL, expiredCredential("r0")); err != nil {
		t.Fatal(err)
	}
	loser, err := Resolve(s, ts.URL, &OAuthClient{BaseURL: ts.URL})
	if err != nil {
		t.Fatal(err)
	}
	winner, err := Resolve(s, ts.URL, &OAuthClient{BaseURL: ts.URL})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := winner.Token(context.Background()); err != nil {
		t.Fatal(err)
	}

	header, err := loser.Token(context.Background())
	if err != nil {
		t.Fatalf("the losing process was signed out: %v", err)
	}
	if header != "Bearer new-access-1" {
		t.Errorf("Token = %q, want the pair stored by the winner", header)
	}
}

func TestSessionWithoutRefreshTokenRequiresLogin(t *testing.T) {
	t.Setenv(EnvAPIKey, "")
	s := fileStore(t)
	cred := expiredCredential("")
	if err := s.Save(hostA, cred); err != nil {
		t.Fatal(err)
	}
	sess, err := Resolve(s, hostA, &OAuthClient{BaseURL: hostA})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sess.Token(context.Background()); !errors.Is(err, ErrLoginRequired) {
		t.Errorf("err = %v, want ErrLoginRequired", err)
	}
}
