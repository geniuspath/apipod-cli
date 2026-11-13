package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestPKCEIsS256(t *testing.T) {
	p, err := NewPKCE()
	if err != nil {
		t.Fatal(err)
	}
	// RFC 7636 §4.1: 43-128 characters from the unreserved set.
	if !regexp.MustCompile(`^[A-Za-z0-9\-._~]{43,128}$`).MatchString(p.Verifier) {
		t.Errorf("verifier %q is not a valid code_verifier", p.Verifier)
	}
	sum := sha256.Sum256([]byte(p.Verifier))
	if want := base64.RawURLEncoding.EncodeToString(sum[:]); p.Challenge != want {
		t.Errorf("challenge = %q, want BASE64URL(SHA256(verifier)) = %q", p.Challenge, want)
	}
	if p.Challenge == p.Verifier {
		t.Error("challenge equals the verifier (plain method)")
	}
	other, _ := NewPKCE()
	if other.Verifier == p.Verifier {
		t.Error("two PKCE pairs share a verifier")
	}
}

func TestStateIs32RandomBytes(t *testing.T) {
	a, err := NewState()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(a)
	if err != nil || len(raw) != 32 {
		t.Errorf("state decodes to %d bytes (err=%v), want 32", len(raw), err)
	}
	if b, _ := NewState(); a == b {
		t.Error("two states are equal")
	}
}

func TestStateMatches(t *testing.T) {
	const state = "c3RhdGUtdmFsdWUtZm9yLXRlc3Rpbmc"
	cases := []struct {
		got  string
		want bool
	}{
		{state, true},
		{"", false},
		{state + "x", false},
		{state[:len(state)-1], false},
		{strings.ToUpper(state), false},
		{"X" + state[1:], false},
	}
	for _, tc := range cases {
		if got := StateMatches(state, tc.got); got != tc.want {
			t.Errorf("StateMatches(state, %q) = %v, want %v", tc.got, got, tc.want)
		}
	}
	if StateMatches("", "") {
		t.Error("empty expected state matched an empty callback state")
	}
}

func TestAuthorizeURL(t *testing.T) {
	pkce := PKCE{Verifier: "verifier-must-not-appear", Challenge: "the-challenge"}
	raw := AuthorizeURL("https://api.apipod.ai", "http://127.0.0.1:53124/callback", "the-state", pkce)
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme+"://"+u.Host+u.Path != "https://api.apipod.ai/oauth/authorize" {
		t.Errorf("authorize endpoint = %s", raw)
	}
	q := u.Query()
	want := map[string]string{
		"response_type":         "code",
		"client_id":             "apipod-cli",
		"redirect_uri":          "http://127.0.0.1:53124/callback",
		"scope":                 "profile email apipod:api",
		"state":                 "the-state",
		"code_challenge":        "the-challenge",
		"code_challenge_method": "S256",
	}
	for k, v := range want {
		if q.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, q.Get(k), v)
		}
	}
	if strings.Contains(raw, pkce.Verifier) {
		t.Error("the code verifier is in the authorize URL")
	}
}

func startListener(t *testing.T, state string) *Listener {
	t.Helper()
	l, err := Listen(state)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.Close)
	return l
}

func callback(t *testing.T, l *Listener, query string) (int, string) {
	t.Helper()
	resp, err := http.Get(l.RedirectURI() + "?" + query)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func TestListenerBindsLoopbackOnly(t *testing.T) {
	l := startListener(t, "s")
	u, err := url.Parse(l.RedirectURI())
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.Path != "/callback" || u.Port() == "" || u.Port() == "0" {
		t.Errorf("redirect URI = %s, want http://127.0.0.1:<port>/callback", l.RedirectURI())
	}
	addr := l.ln.Addr().(*net.TCPAddr)
	if !addr.IP.IsLoopback() {
		t.Errorf("listener bound to %s, want a loopback address", addr.IP)
	}
	other := startListener(t, "s")
	if other.RedirectURI() == l.RedirectURI() {
		t.Error("two listeners share a port")
	}
}

func TestListenerDeliversCode(t *testing.T) {
	l := startListener(t, "good-state")
	status, body := callback(t, l, "code=the-code&state=good-state")
	if status != http.StatusOK {
		t.Errorf("status = %d", status)
	}
	if strings.Contains(body, "the-code") || strings.Contains(body, "good-state") {
		t.Error("the callback page echoes request parameters")
	}
	code, err := l.Wait(context.Background())
	if err != nil || code != "the-code" {
		t.Errorf("Wait = %q, %v", code, err)
	}
}

func TestListenerRejectsWrongState(t *testing.T) {
	l := startListener(t, "good-state")
	for _, q := range []string{"code=stolen&state=wrong", "code=stolen", "code=stolen&state="} {
		if status, _ := callback(t, l, q); status != http.StatusBadRequest {
			t.Errorf("callback %q: status = %d, want 400", q, status)
		}
	}
	// A forged callback must not end the flow: the real one still works.
	if status, _ := callback(t, l, "code=real&state=good-state"); status != http.StatusOK {
		t.Fatalf("genuine callback after forged ones: status = %d", status)
	}
	code, err := l.Wait(context.Background())
	if err != nil || code != "real" {
		t.Errorf("Wait = %q, %v; want the genuine code", code, err)
	}
}

func TestListenerIsSingleUse(t *testing.T) {
	l := startListener(t, "good-state")
	if status, _ := callback(t, l, "code=first&state=good-state"); status != http.StatusOK {
		t.Fatalf("first callback: status = %d", status)
	}
	if status, _ := callback(t, l, "code=second&state=good-state"); status != http.StatusBadRequest {
		t.Errorf("second callback: status = %d, want 400", status)
	}
	code, err := l.Wait(context.Background())
	if err != nil || code != "first" {
		t.Errorf("Wait = %q, %v; want the first code", code, err)
	}
}

func TestListenerReportsDenial(t *testing.T) {
	l := startListener(t, "good-state")
	_, body := callback(t, l, "error=access_denied%1b%5b2J%3Cscript%3E&state=good-state")
	if strings.Contains(body, "access_denied") || strings.Contains(body, "script") {
		t.Error("the callback page echoes the error parameter")
	}
	_, err := l.Wait(context.Background())
	if err == nil {
		t.Fatal("Wait succeeded after a denial")
	}
	if strings.ContainsAny(err.Error(), "\x1b<>") {
		t.Errorf("error message carries unsanitized callback input: %q", err)
	}
	if !strings.Contains(err.Error(), "access_denied") {
		t.Errorf("error = %v, want it to name the OAuth error", err)
	}
}

func TestListenerClosesAfterWait(t *testing.T) {
	l := startListener(t, "good-state")
	callback(t, l, "code=c&state=good-state")
	if _, err := l.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := http.Get(l.RedirectURI() + "?code=late&state=good-state"); err == nil {
		t.Error("the listener still accepts connections after the flow finished")
	}
}

func TestListenerWaitHonoursContext(t *testing.T) {
	l := startListener(t, "s")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := l.Wait(ctx); err == nil {
		t.Fatal("Wait returned without a callback")
	}
}

func TestListenerRejectsNonGet(t *testing.T) {
	l := startListener(t, "good-state")
	resp, err := http.Post(l.RedirectURI()+"?code=c&state=good-state", "text/plain", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST status = %d, want 405", resp.StatusCode)
	}
}

func TestExchangeSendsVerifierAndClientID(t *testing.T) {
	var form url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		form = r.PostForm
		_, _ = io.WriteString(w, `{"access_token":"at","refresh_token":"rt","token_type":"Bearer","expires_in":3600,"scope":"profile email apipod:api"}`)
	}))
	defer srv.Close()

	o := &OAuthClient{BaseURL: srv.URL}
	tr, err := o.Exchange(context.Background(), "the-code", "http://127.0.0.1:5/callback", "the-verifier")
	if err != nil {
		t.Fatal(err)
	}
	if tr.AccessToken != "at" || tr.RefreshToken != "rt" {
		t.Errorf("tokens = %+v", tr)
	}
	want := map[string]string{
		"grant_type":    "authorization_code",
		"client_id":     "apipod-cli",
		"code":          "the-code",
		"redirect_uri":  "http://127.0.0.1:5/callback",
		"code_verifier": "the-verifier",
	}
	for k, v := range want {
		if form.Get(k) != v {
			t.Errorf("form %s = %q, want %q", k, form.Get(k), v)
		}
	}
	if form.Get("client_secret") != "" {
		t.Error("a public client sent a client_secret")
	}
}

func TestTokenEndpointErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"invalid_grant","error_description":"Authorization code is invalid"}`)
	}))
	defer srv.Close()
	_, err := (&OAuthClient{BaseURL: srv.URL}).Exchange(context.Background(), "c", "r", "v")
	var te *TokenError
	if err == nil || !asTokenError(err, &te) || te.Code != "invalid_grant" {
		t.Fatalf("err = %v, want a TokenError with code invalid_grant", err)
	}
}

func TestOAuthClientDoesNotFollowRedirects(t *testing.T) {
	hits := 0
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits++ }))
	defer target.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/oauth/token", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	if _, err := (&OAuthClient{BaseURL: srv.URL}).Refresh(context.Background(), "refresh-token"); err == nil {
		t.Error("a redirected token request reported success")
	}
	if hits != 0 {
		t.Errorf("the redirect target received %d requests carrying the refresh token, want 0", hits)
	}
}
