package cli

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/geniuspath/apipod-cli/internal/auth"
	"github.com/geniuspath/apipod-cli/internal/exitcode"
)

func TestLoginThroughBrowser(t *testing.T) {
	h := newHarness(t)
	res := h.run("auth", "login").wantCode(t, exitcode.OK)

	// The authorization request: first-party client, PKCE S256, loopback.
	if len(h.api.authorize) != 1 {
		t.Fatalf("browser opened %d times, want 1", len(h.api.authorize))
	}
	q := h.api.authorize[0]
	if q.Get("client_id") != "apipod-cli" || q.Get("response_type") != "code" {
		t.Errorf("client_id=%q response_type=%q", q.Get("client_id"), q.Get("response_type"))
	}
	if q.Get("code_challenge_method") != "S256" || len(q.Get("code_challenge")) != 43 {
		t.Errorf("PKCE: method=%q challenge=%q", q.Get("code_challenge_method"), q.Get("code_challenge"))
	}
	if !strings.Contains(q.Get("scope"), "apipod:api") {
		t.Errorf("scope = %q, want apipod:api requested", q.Get("scope"))
	}
	redirect, err := url.Parse(q.Get("redirect_uri"))
	if err != nil || redirect.Scheme != "http" || redirect.Hostname() != "127.0.0.1" || redirect.Path != "/callback" || redirect.Port() == "" {
		t.Errorf("redirect_uri = %q, want http://127.0.0.1:<port>/callback", q.Get("redirect_uri"))
	}
	if len(q.Get("state")) < 43 {
		t.Errorf("state %q is shorter than 32 random bytes", q.Get("state"))
	}

	// The credential is stored for this host, and never printed.
	cred, err := h.store().Get(h.api.srv.URL)
	if err != nil {
		t.Fatalf("no stored credential after login: %v", err)
	}
	if cred.AccessToken == "" || cred.RefreshToken == "" || cred.Email != "dev@example.com" || cred.ClientID != "apipod-cli" {
		t.Errorf("stored credential = %+v", cred)
	}
	res.noSecret(t, cred.AccessToken, cred.RefreshToken)
	if !strings.Contains(res.stdout, "Signed in") || !strings.Contains(res.stdout, "dev@example.com") {
		t.Errorf("stdout = %s", res.stdout)
	}

	// The stored token works, refreshing nothing.
	h.run("balance").wantCode(t, exitcode.OK)
}

// A forged callback (wrong state) is ignored: only the genuine redirect's
// code is exchanged.
func TestLoginIgnoresForgedCallback(t *testing.T) {
	h := newHarness(t)
	h.browser = func(raw string) error {
		u, _ := url.Parse(raw)
		redirect := u.Query().Get("redirect_uri")
		for _, forged := range []string{"?code=attacker-code&state=guessed", "?code=attacker-code"} {
			resp, err := http.Get(redirect + forged)
			if err != nil {
				return err
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("forged callback %q answered %d, want 400", forged, resp.StatusCode)
			}
		}
		return h.api.approve(raw)
	}
	h.run("auth", "login").wantCode(t, exitcode.OK)
	for _, c := range h.api.to("/oauth/token") {
		if strings.Contains(string(c.Body), "attacker-code") {
			t.Error("the forged code was sent to the token endpoint")
		}
	}
	if _, err := h.store().Get(h.api.srv.URL); err != nil {
		t.Errorf("genuine login did not complete: %v", err)
	}
}

func TestLoginWithoutAPIScopeStoresNothing(t *testing.T) {
	h := newHarness(t)
	h.api.grantScope = "profile email"
	res := h.run("auth", "login").wantCode(t, exitcode.Auth)
	if _, err := h.store().Get(h.api.srv.URL); !errors.Is(err, auth.ErrNoCredential) {
		t.Errorf("a token without apipod:api was stored (err=%v)", err)
	}
	if !strings.Contains(res.stderr, "apipod:api") {
		t.Errorf("stderr = %s", res.stderr)
	}
	if len(h.api.revoked) == 0 {
		t.Error("the useless token was not revoked")
	}
}

func TestLoginDeniedInBrowser(t *testing.T) {
	h := newHarness(t)
	h.browser = func(raw string) error {
		u, _ := url.Parse(raw)
		q := u.Query()
		resp, err := http.Get(q.Get("redirect_uri") + "?" + url.Values{"error": {"access_denied"}, "state": {q.Get("state")}}.Encode())
		if err == nil {
			resp.Body.Close()
		}
		return err
	}
	h.run("auth", "login").wantCode(t, exitcode.Auth)
	if len(h.api.to("/oauth/token")) != 0 {
		t.Error("the token endpoint was called after a denial")
	}
}

func TestLoginPrintsURLWhenBrowserCannotOpen(t *testing.T) {
	h := newHarness(t)
	h.browser = func(raw string) error {
		// The "user" opens the printed URL by hand a moment later.
		go func() { time.Sleep(20 * time.Millisecond); _ = h.api.approve(raw) }()
		return errors.New("no display")
	}
	res := h.run("auth", "login").wantCode(t, exitcode.OK)
	if !strings.Contains(res.stderr, h.api.srv.URL+"/oauth/authorize?") {
		t.Errorf("the sign-in URL was not printed:\n%s", res.stderr)
	}
}

func TestLoginWithAPIKeyFromStdin(t *testing.T) {
	h := newHarness(t)
	res := h.runIn(testKey+"\n", "auth", "login", "--with-api-key").wantCode(t, exitcode.OK)
	res.noSecret(t, testKey)

	cred, err := h.store().Get(h.api.srv.URL)
	if err != nil || cred.APIKey != testKey {
		t.Fatalf("stored credential = %+v, %v", cred, err)
	}
	if len(h.api.to("/v1/account/status")) != 1 {
		t.Error("the key was not verified before being stored")
	}
	h.run("balance").wantCode(t, exitcode.OK)

	status := h.run("auth", "status").wantCode(t, exitcode.OK)
	status.noSecret(t, testKey)
	if !strings.Contains(status.stdout, "api_key") || !strings.Contains(status.stdout, "sk-test-…wxyz") {
		t.Errorf("status = %s", status.stdout)
	}
}

func TestLoginWithRejectedAPIKeyStoresNothing(t *testing.T) {
	h := newHarness(t)
	const bad = "sk-wrong-0123456789abcdefghijklmnop"
	res := h.runIn(bad+"\n", "auth", "login", "--with-api-key").wantCode(t, exitcode.Auth)
	res.noSecret(t, bad)
	if _, err := h.store().Get(h.api.srv.URL); !errors.Is(err, auth.ErrNoCredential) {
		t.Errorf("a rejected key was stored (err=%v)", err)
	}
	h.runIn("\n", "auth", "login", "--with-api-key").wantCode(t, exitcode.Usage)
}

// A secret can never be passed on the command line: argv is visible in shell
// history and in `ps`.
func TestNoFlagAcceptsASecret(t *testing.T) {
	root := (&App{}).newRoot(&globalFlags{})
	var walk func(cmd *cobra.Command)
	walk = func(cmd *cobra.Command) {
		check := func(f *pflag.Flag) {
			name := strings.ToLower(f.Name)
			for _, word := range []string{"key", "token", "secret", "password", "credential"} {
				if strings.Contains(name, word) && f.Value.Type() != "bool" {
					t.Errorf("%s --%s takes a value: secrets must not be passed as arguments", cmd.CommandPath(), f.Name)
				}
			}
		}
		cmd.Flags().VisitAll(check)
		cmd.PersistentFlags().VisitAll(check)
		for _, sub := range cmd.Commands() {
			walk(sub)
		}
	}
	walk(root)

	h := newHarness(t)
	for _, args := range [][]string{
		{"image", "a fox", "-m", "m", "--api-key", testKey},
		{"balance", "--api-key=" + testKey},
		{"balance", "--token", testKey},
		{"auth", "login", "--api-key", testKey},
		{"auth", "login", "--with-api-key", testKey}, // the key as an argument
	} {
		res := h.run(args...).wantCode(t, exitcode.Usage)
		res.noSecret(t, testKey)
	}
	if n := len(h.api.all()); n != 0 {
		t.Errorf("%d requests were sent by commands that should have been rejected", n)
	}
}

func TestLogoutRevokesAndRemoves(t *testing.T) {
	h := newHarness(t)
	h.run("auth", "login").wantCode(t, exitcode.OK)
	cred, _ := h.store().Get(h.api.srv.URL)

	res := h.run("auth", "logout").wantCode(t, exitcode.OK)
	res.noSecret(t, cred.AccessToken, cred.RefreshToken)
	revoked := strings.Join(h.api.revoked, " ")
	if !strings.Contains(revoked, cred.RefreshToken) || !strings.Contains(revoked, cred.AccessToken) {
		t.Errorf("revoked = %v, want both the refresh and the access token", h.api.revoked)
	}
	if _, err := h.store().Get(h.api.srv.URL); !errors.Is(err, auth.ErrNoCredential) {
		t.Errorf("the credential is still stored after logout (err=%v)", err)
	}
	h.run("balance").wantCode(t, exitcode.Auth)
	h.run("auth", "logout").wantCode(t, exitcode.OK) // idempotent
}

// Even when the server cannot be reached, the local credential is removed
// and the user is told to revoke it in the console.
func TestLogoutRemovesLocalCredentialWhenRevocationFails(t *testing.T) {
	h := newHarness(t)
	h.run("auth", "login").wantCode(t, exitcode.OK)
	h.api.revokeStatus = 500

	res := h.run("auth", "logout").wantCode(t, exitcode.OK)
	if _, err := h.store().Get(h.api.srv.URL); !errors.Is(err, auth.ErrNoCredential) {
		t.Errorf("the credential survived a failed revocation (err=%v)", err)
	}
	if !strings.Contains(res.stderr, "could not be revoked") || !strings.Contains(res.stderr, "/console") {
		t.Errorf("stderr lacks the manual revocation hint:\n%s", res.stderr)
	}
}

func TestAuthStatus(t *testing.T) {
	h := newHarness(t)
	res := h.run("auth", "status", "--json").wantCode(t, exitcode.Auth)
	var info authInfo
	res.decode(t, &info)
	if info.Authenticated {
		t.Error("reported as authenticated without a credential")
	}

	h.useKey()
	res = h.run("auth", "status", "--json").wantCode(t, exitcode.OK)
	res.noSecret(t, testKey)
	res.decode(t, &info)
	if !info.Authenticated || info.Source != "env" || info.Method != "api_key" || info.BaseURL != h.api.srv.URL {
		t.Errorf("status = %+v", info)
	}
	if info.Secret != "sk-test-…wxyz" {
		t.Errorf("masked credential = %q", info.Secret)
	}
}

// A token stored for one host is never sent to another, even when
// APIPOD_BASE_URL is pointed there.
func TestStoredCredentialIsNotSentToAnotherHost(t *testing.T) {
	h := newHarness(t)
	h.run("auth", "login").wantCode(t, exitcode.OK)
	cred, _ := h.store().Get(h.api.srv.URL)

	other := newFakeAPI(t)
	t.Setenv(EnvBaseURL, other.srv.URL)
	for _, args := range [][]string{{"balance"}, {"image", "x", "-m", "m", "--yes"}, {"assets", "list"}, {"job", "get", "img_1"}} {
		h.run(args...).wantCode(t, exitcode.Auth)
	}
	for _, c := range other.all() {
		if c.Header.Get("Authorization") != "" {
			t.Errorf("%s %s carried a credential issued for another host", c.Method, c.Path)
		}
		if strings.Contains(string(c.Body), cred.AccessToken) || strings.Contains(string(c.Body), cred.RefreshToken) {
			t.Errorf("%s %s carried a token in its body", c.Method, c.Path)
		}
	}
	if n := len(other.all()); n != 0 {
		t.Errorf("the other host received %d requests, want 0", n)
	}
	// The credential is untouched and still works for its own host.
	t.Setenv(EnvBaseURL, h.api.srv.URL)
	h.run("balance").wantCode(t, exitcode.OK)
}

func TestCustomBaseURLMustBeHTTPS(t *testing.T) {
	h := newHarness(t)
	h.useKey()
	for _, bad := range []string{"http://api.example.com", "http://192.168.1.5:8080", "ftp://api.example.com", "api.example.com", "https://user:pw@api.example.com"} {
		t.Setenv(EnvBaseURL, bad)
		for _, args := range [][]string{{"balance"}, {"auth", "login"}, {"models", "list"}} {
			res := h.run(args...).wantCode(t, exitcode.Usage)
			res.noSecret(t, testKey)
			if !strings.Contains(res.stderr, "APIPOD_BASE_URL") {
				t.Errorf("%q: stderr = %s", bad, res.stderr)
			}
		}
	}
	if n := len(h.api.all()); n != 0 {
		t.Errorf("%d requests were sent with an invalid base URL", n)
	}
}

func TestExpiredAccessTokenIsRefreshed(t *testing.T) {
	h := newHarness(t)
	const access, refresh = "stale-access-token-0123456789", "seed-refresh-token-0123456789"
	h.api.refresh[refresh] = true
	err := h.store().Save(h.api.srv.URL, auth.Credential{
		AccessToken: access, RefreshToken: refresh, ExpiresAt: time.Now().Add(-time.Hour), ClientID: auth.ClientID,
	})
	if err != nil {
		t.Fatal(err)
	}

	res := h.run("balance", "--debug").wantCode(t, exitcode.OK)
	next, _ := h.store().Get(h.api.srv.URL)
	if next.AccessToken == access || next.RefreshToken == refresh || next.RefreshToken == "" {
		t.Errorf("the rotated pair was not stored: %+v", next)
	}
	res.noSecret(t, access, refresh, next.AccessToken, next.RefreshToken)
	if got := h.api.to("/v1/account/balance")[0].Header.Get("Authorization"); got != "Bearer "+next.AccessToken {
		t.Errorf("the request used %q, want the refreshed token", got)
	}

	// Once the refresh token is gone too, the user is asked to sign in again.
	_ = h.store().Save(h.api.srv.URL, auth.Credential{AccessToken: access, RefreshToken: "revoked-refresh-token-000", ExpiresAt: time.Now().Add(-time.Hour)})
	res = h.run("balance").wantCode(t, exitcode.Auth)
	if !strings.Contains(res.stderr, "apipod auth login") {
		t.Errorf("stderr = %s", res.stderr)
	}
}

func TestDebugOutputNeverContainsCredentials(t *testing.T) {
	h := newHarness(t)

	// OAuth: the code, verifier and both tokens all cross the wire here.
	login := h.run("auth", "login", "--debug").wantCode(t, exitcode.OK)
	cred, _ := h.store().Get(h.api.srv.URL)
	secrets := []string{cred.AccessToken, cred.RefreshToken}
	for _, c := range h.api.to("/oauth/token") {
		form, _ := url.ParseQuery(string(c.Body))
		secrets = append(secrets, form.Get("code"), form.Get("code_verifier"))
	}
	login.noSecret(t, secrets...)
	if !strings.Contains(login.stderr, "debug: > POST /oauth/token") {
		t.Errorf("--debug printed no request trace:\n%s", login.stderr)
	}

	img := h.file("ref.png", pngFixture)
	for _, args := range [][]string{
		{"balance", "--debug"},
		{"image", "a fox", "-m", "m", "--image", img, "--debug", "-o", t.TempDir()},
		{"upload", img, "--debug"},
		{"auth", "status", "--debug"},
		{"auth", "logout", "--debug"},
	} {
		res := h.run(args...).wantCode(t, exitcode.OK)
		res.noSecret(t, secrets...)
		if strings.Contains(res.stderr, "Bearer ") && !strings.Contains(res.stderr, "Bearer [REDACTED]") {
			t.Errorf("%v: an unredacted bearer value was logged:\n%s", args, res.stderr)
		}
	}

	// API key from the environment.
	h.useKey()
	res := h.run("balance", "--debug").wantCode(t, exitcode.OK)
	res.noSecret(t, testKey)
	if !strings.Contains(res.stderr, "Authorization: Bearer [REDACTED]") {
		t.Errorf("stderr = %s", res.stderr)
	}
}
