package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// ClientID is the first-party public OAuth client seeded on the server.
	ClientID = "apipod-cli"
	// Scopes requested at login. apipod:api is what lets a token call /v1.
	Scopes = "profile email apipod:api"
	// LoginTimeout bounds how long the loopback listener waits for the browser.
	LoginTimeout = 5 * time.Minute

	callbackPath = "/callback"
)

// PKCE is a verifier/challenge pair (RFC 7636, S256 only).
type PKCE struct {
	Verifier  string
	Challenge string
}

// NewPKCE generates a 32-byte verifier and its S256 challenge.
func NewPKCE() (PKCE, error) {
	verifier, err := randomURLSafe(32)
	if err != nil {
		return PKCE{}, err
	}
	sum := sha256.Sum256([]byte(verifier))
	return PKCE{Verifier: verifier, Challenge: base64.RawURLEncoding.EncodeToString(sum[:])}, nil
}

// NewState returns 32 random bytes, base64url encoded.
func NewState() (string, error) { return randomURLSafe(32) }

func randomURLSafe(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := io.ReadFull(rand.Reader, buf); err != nil {
		return "", fmt.Errorf("generate random bytes: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// StateMatches compares the callback state with the expected one in constant
// time, so the comparison leaks nothing about the expected value.
func StateMatches(expected, got string) bool {
	if expected == "" || got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(expected), []byte(got)) == 1
}

// AuthorizeURL builds the browser URL for the authorization request.
func AuthorizeURL(baseURL, redirectURI, state string, pkce PKCE) string {
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {ClientID},
		"redirect_uri":          {redirectURI},
		"scope":                 {Scopes},
		"state":                 {state},
		"code_challenge":        {pkce.Challenge},
		"code_challenge_method": {"S256"},
	}
	return baseURL + "/oauth/authorize?" + q.Encode()
}

// callbackResult is what the loopback handler hands back to the login flow.
type callbackResult struct {
	code string
	err  error
}

// Listener is a one-shot loopback callback server bound to 127.0.0.1 on an
// OS-assigned port (RFC 8252 §7.3). It accepts exactly one valid callback.
type Listener struct {
	ln       net.Listener
	srv      *http.Server
	state    string
	results  chan callbackResult
	consumed chan struct{}
}

// Listen binds 127.0.0.1:0. It never binds a wildcard or "localhost": the
// former would expose the code to the LAN, the latter can resolve elsewhere.
func Listen(state string) (*Listener, error) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("open loopback listener: %w", err)
	}
	l := &Listener{
		ln:       ln,
		state:    state,
		results:  make(chan callbackResult, 1),
		consumed: make(chan struct{}),
	}
	mux := http.NewServeMux()
	mux.HandleFunc(callbackPath, l.handle)
	l.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = l.srv.Serve(ln) }()
	return l, nil
}

// RedirectURI is the exact redirect_uri to send to /authorize and /token.
func (l *Listener) RedirectURI() string {
	return fmt.Sprintf("http://127.0.0.1:%d%s", l.ln.Addr().(*net.TCPAddr).Port, callbackPath)
}

// Close stops the listener.
func (l *Listener) Close() { _ = l.srv.Close() }

func (l *Listener) handle(w http.ResponseWriter, r *http.Request) {
	// Only top-level GET navigations carry the code.
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query()

	// Single use: the first request that presents the right state wins;
	// anything after it (or with a wrong state) gets a static error page and
	// never reaches the token exchange.
	if !StateMatches(l.state, q.Get("state")) {
		writePage(w, http.StatusBadRequest, pageFailed)
		return
	}
	select {
	case <-l.consumed:
		writePage(w, http.StatusBadRequest, pageFailed)
		return
	default:
		close(l.consumed)
	}

	res := callbackResult{}
	switch {
	case q.Get("error") != "":
		res.err = fmt.Errorf("authorization was not granted (%s)", sanitizeOAuthError(q.Get("error")))
		writePage(w, http.StatusOK, pageDenied)
	case q.Get("code") == "":
		res.err = errors.New("authorization callback carried no code")
		writePage(w, http.StatusBadRequest, pageFailed)
	default:
		res.code = q.Get("code")
		writePage(w, http.StatusOK, pageOK)
	}
	l.results <- res
}

// Wait blocks until the callback arrives, ctx is done, or LoginTimeout passes.
// The listener is closed before Wait returns.
func (l *Listener) Wait(ctx context.Context) (string, error) {
	defer l.Close()
	ctx, cancel := context.WithTimeout(ctx, LoginTimeout)
	defer cancel()
	select {
	case res := <-l.results:
		return res.code, res.err
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", errors.New("timed out waiting for the browser sign-in")
		}
		return "", ctx.Err()
	}
}

// sanitizeOAuthError keeps the RFC 6749 error token charset only, so a
// crafted callback cannot smuggle terminal escapes into our error message.
func sanitizeOAuthError(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			b.WriteRune(r)
		}
		if b.Len() >= 64 {
			break
		}
	}
	if b.Len() == 0 {
		return "unknown_error"
	}
	return b.String()
}

// Static pages: nothing from the request is ever echoed back, so the callback
// endpoint cannot be used for reflected XSS or content injection.
const (
	pageOK     = `<!doctype html><meta charset="utf-8"><title>APIPod CLI</title><body style="font:16px system-ui;margin:4rem auto;max-width:32rem"><h1>Signed in</h1><p>You can close this tab and return to the terminal.</p></body>`
	pageDenied = `<!doctype html><meta charset="utf-8"><title>APIPod CLI</title><body style="font:16px system-ui;margin:4rem auto;max-width:32rem"><h1>Sign-in cancelled</h1><p>Access was not granted. You can close this tab.</p></body>`
	pageFailed = `<!doctype html><meta charset="utf-8"><title>APIPod CLI</title><body style="font:16px system-ui;margin:4rem auto;max-width:32rem"><h1>Sign-in failed</h1><p>This sign-in link is invalid or was already used. Run <code>apipod auth login</code> again.</p></body>`
)

func writePage(w http.ResponseWriter, status int, body string) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

// ---- token endpoint ----

// TokenResponse is the /oauth/token success body.
type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
}

// TokenError is an RFC 6749 §5.2 error from the token endpoint.
type TokenError struct {
	Status      int
	Code        string
	Description string
}

func (e *TokenError) Error() string {
	if e.Description != "" {
		return fmt.Sprintf("token endpoint: %s (%s)", e.Description, e.Code)
	}
	return fmt.Sprintf("token endpoint: HTTP %d %s", e.Status, e.Code)
}

// OAuthClient calls the OAuth endpoints of one base URL.
type OAuthClient struct {
	BaseURL   string
	HTTP      *http.Client
	UserAgent string
	// Debugf receives one line per request: method, path and status only.
	// Token endpoint bodies are never logged — they are nothing but secrets.
	Debugf func(format string, args ...any)
}

func (o *OAuthClient) debugf(format string, args ...any) {
	if o.Debugf != nil {
		o.Debugf(format, args...)
	}
}

// httpClient never follows redirects: a token request must not be replayed
// against whatever host a redirect names.
func (o *OAuthClient) httpClient() *http.Client {
	if o.HTTP != nil {
		return o.HTTP
	}
	return &http.Client{
		Timeout:       30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func (o *OAuthClient) send(req *http.Request) (*http.Response, error) {
	resp, err := o.httpClient().Do(req)
	if err != nil {
		o.debugf("> %s %s failed: %v", req.Method, req.URL.Path, err)
		return nil, err
	}
	o.debugf("> %s %s", req.Method, req.URL.Path)
	o.debugf("< %s", resp.Status)
	return resp, nil
}

func (o *OAuthClient) postForm(ctx context.Context, path string, form url.Values) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.BaseURL+path, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if o.UserAgent != "" {
		req.Header.Set("User-Agent", o.UserAgent)
	}
	return o.send(req)
}

func (o *OAuthClient) token(ctx context.Context, form url.Values) (TokenResponse, error) {
	resp, err := o.postForm(ctx, "/oauth/token", form)
	if err != nil {
		return TokenResponse{}, fmt.Errorf("token request failed: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return TokenResponse{}, err
	}
	if resp.StatusCode != http.StatusOK {
		te := &TokenError{Status: resp.StatusCode}
		var e struct {
			Error            string `json:"error"`
			ErrorDescription string `json:"error_description"`
		}
		if json.Unmarshal(body, &e) == nil {
			te.Code, te.Description = e.Error, e.ErrorDescription
		}
		return TokenResponse{}, te
	}
	var tr TokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return TokenResponse{}, fmt.Errorf("decode token response: %w", err)
	}
	if tr.AccessToken == "" {
		return TokenResponse{}, errors.New("token response carried no access_token")
	}
	return tr, nil
}

// Exchange trades an authorization code for tokens (with the PKCE verifier).
func (o *OAuthClient) Exchange(ctx context.Context, code, redirectURI, verifier string) (TokenResponse, error) {
	return o.token(ctx, url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {ClientID},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"code_verifier": {verifier},
	})
}

// Refresh rotates the token pair.
func (o *OAuthClient) Refresh(ctx context.Context, refreshToken string) (TokenResponse, error) {
	return o.token(ctx, url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {ClientID},
		"refresh_token": {refreshToken},
	})
}

// Revoke invalidates a token server-side (RFC 7009).
func (o *OAuthClient) Revoke(ctx context.Context, token string) error {
	resp, err := o.postForm(ctx, "/oauth/revoke", url.Values{"token": {token}, "client_id": {ClientID}})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("revoke: HTTP %d", resp.StatusCode)
	}
	return nil
}

// UserInfo fetches the signed-in identity for `auth status`.
func (o *OAuthClient) UserInfo(ctx context.Context, accessToken string) (email, username string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.BaseURL+"/oauth/userinfo", nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	if o.UserAgent != "" {
		req.Header.Set("User-Agent", o.UserAgent)
	}
	resp, err := o.send(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("userinfo: HTTP %d", resp.StatusCode)
	}
	var info struct {
		Email    string `json:"email"`
		Username string `json:"username"`
		Name     string `json:"name"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&info); err != nil {
		return "", "", err
	}
	if info.Username == "" {
		info.Username = info.Name
	}
	return info.Email, info.Username, nil
}

// CredentialFromToken converts a token response into a storable credential.
func CredentialFromToken(tr TokenResponse, now time.Time) Credential {
	return Credential{
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
		TokenType:    tr.TokenType,
		ExpiresAt:    now.Add(time.Duration(tr.ExpiresIn) * time.Second),
		ClientID:     ClientID,
		Scope:        tr.Scope,
	}
}
