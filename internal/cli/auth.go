package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/geniuspath/apipod-cli/internal/auth"
	"github.com/geniuspath/apipod-cli/internal/exitcode"
)

func (a *App) newAuthCmd(flags *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "auth",
		Short: "Sign in, sign out and show the current credential",
		Args:  positional(noArgs),
		RunE:  func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}

	var withAPIKey, noBrowser bool
	login := &cobra.Command{
		Use:   "login",
		Short: "Sign in through the browser (or store an API key)",
		Long: `Sign in to APIPod.

By default this opens the browser and completes an OAuth authorization
(PKCE, with a one-time callback on 127.0.0.1). The resulting tokens are kept
in the system keychain.

With --with-api-key the key is read from standard input, or from a hidden
prompt when standard input is a terminal. A key is never accepted as a
command-line argument: arguments are visible in shell history and to other
processes.

  apipod auth login
  printenv MY_KEY | apipod auth login --with-api-key`,
		Args: positional(noArgs),
		RunE: a.run(flags, func(rt *runtime, _ []string) error {
			if withAPIKey {
				return rt.loginWithAPIKey()
			}
			return rt.loginWithBrowser(noBrowser)
		}),
	}
	login.Flags().BoolVar(&withAPIKey, "with-api-key", false, "read an API key from stdin instead of signing in through the browser")
	login.Flags().BoolVar(&noBrowser, "no-browser", false, "print the sign-in URL instead of opening the browser")

	logout := &cobra.Command{
		Use:   "logout",
		Short: "Revoke the stored credential and remove it from this machine",
		Args:  positional(noArgs),
		RunE:  a.run(flags, func(rt *runtime, _ []string) error { return rt.logout() }),
	}

	status := &cobra.Command{
		Use:   "status",
		Short: "Show which credential the CLI would use",
		Args:  positional(noArgs),
		RunE:  a.run(flags, func(rt *runtime, _ []string) error { return rt.authStatus() }),
	}

	cmd.AddCommand(login, logout, status)
	return cmd
}

func (rt *runtime) loginWithBrowser(noBrowser bool) error {
	base, err := rt.baseURL()
	if err != nil {
		return err
	}
	state, err := auth.NewState()
	if err != nil {
		return err
	}
	pkce, err := auth.NewPKCE()
	if err != nil {
		return err
	}
	listener, err := auth.Listen(state)
	if err != nil {
		return err
	}
	defer listener.Close()

	redirectURI := listener.RedirectURI()
	authorizeURL := auth.AuthorizeURL(base, redirectURI, state, pkce)

	opened := false
	if !noBrowser && rt.app.OpenBrowser != nil {
		opened = rt.app.OpenBrowser(authorizeURL) == nil
	}
	// The URL is printed even when the browser opened: over SSH or in a
	// container the "open" may have succeeded on a display nobody is watching.
	if opened {
		fmt.Fprintf(rt.p.Err, "Opening your browser to sign in to %s.\nIf it does not open, visit:\n\n  %s\n\n", base, authorizeURL)
	} else {
		fmt.Fprintf(rt.p.Err, "Open this URL in your browser to sign in to %s:\n\n  %s\n\n", base, authorizeURL)
	}
	fmt.Fprintln(rt.p.Err, "Waiting for the sign-in to complete (5 minute limit)...")

	ctx := rt.cmd.Context()
	code, err := listener.Wait(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return err
		}
		return exitcode.Wrap(exitcode.Auth, err)
	}

	oauth := rt.oauthClient(base)
	tokens, err := oauth.Exchange(ctx, code, redirectURI, pkce.Verifier)
	if err != nil {
		return exitcode.Wrap(exitcode.Auth, fmt.Errorf("sign-in failed: %w", err))
	}
	rt.debug.AddSecret(tokens.AccessToken)
	rt.debug.AddSecret(tokens.RefreshToken)
	if !hasScope(tokens.Scope, "apipod:api") {
		// Without this scope the token cannot call /v1; storing it would only
		// produce confusing 401s later.
		_ = oauth.Revoke(ctx, tokens.AccessToken)
		return exitcode.New(exitcode.Auth, "sign-in did not grant API access (scope apipod:api); nothing was stored")
	}

	cred := auth.CredentialFromToken(tokens, time.Now())
	if email, username, err := oauth.UserInfo(ctx, tokens.AccessToken); err == nil {
		cred.Email, cred.Username = email, username
	}
	if err := rt.storeCredential(base, cred); err != nil {
		return err
	}
	return rt.reportLogin(base, cred)
}

func hasScope(granted, want string) bool {
	for _, s := range strings.Fields(granted) {
		if s == want {
			return true
		}
	}
	return false
}

func (rt *runtime) loginWithAPIKey() error {
	base, err := rt.baseURL()
	if err != nil {
		return err
	}
	key, err := readSecret(rt.cmd.Context(), rt.app.In, rt.p.Err, "Paste your APIPod API key: ")
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return err
		}
		return exitcode.Wrap(exitcode.Usage, err)
	}
	if err := auth.ValidateAPIKey(key); err != nil {
		return exitcode.New(exitcode.Usage, "invalid API key: %v", err)
	}
	rt.debug.AddSecret(key)

	// Check the key before storing it, so a typo fails here and not later.
	client := rt.newClient(base, staticToken("Bearer "+key))
	if err := client.CheckCredential(rt.cmd.Context()); err != nil {
		return fmt.Errorf("the key was rejected, nothing was stored: %w", err)
	}
	cred := auth.Credential{APIKey: key}
	if err := rt.storeCredential(base, cred); err != nil {
		return err
	}
	return rt.reportLogin(base, cred)
}

// staticToken is a fixed Authorization header value.
type staticToken string

func (s staticToken) Token(context.Context) (string, error) { return string(s), nil }

// readSecret reads one line without echo from a terminal, or the first line
// of piped input.
func readSecret(ctx context.Context, in io.Reader, prompt io.Writer, label string) (string, error) {
	f, ok := in.(*os.File)
	if !ok || !term.IsTerminal(int(f.Fd())) {
		line, err := bufio.NewReader(io.LimitReader(in, 8192)).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", fmt.Errorf("read key from stdin: %w", err)
		}
		line = strings.TrimSpace(line)
		if line == "" {
			return "", errors.New("no API key on stdin")
		}
		return line, nil
	}

	fd := int(f.Fd())
	// Remember the terminal state: if the wait is interrupted, echo must be
	// switched back on before the process exits.
	state, _ := term.GetState(fd)
	fmt.Fprint(prompt, label)
	type read struct {
		raw []byte
		err error
	}
	done := make(chan read, 1)
	go func() {
		raw, err := term.ReadPassword(fd)
		done <- read{raw, err}
	}()
	select {
	case <-ctx.Done():
		if state != nil {
			_ = term.Restore(fd, state)
		}
		fmt.Fprintln(prompt)
		return "", ctx.Err()
	case r := <-done:
		fmt.Fprintln(prompt)
		if r.err != nil {
			return "", fmt.Errorf("read key: %w", r.err)
		}
		return strings.TrimSpace(string(r.raw)), nil
	}
}

func (rt *runtime) storeCredential(base string, cred auth.Credential) error {
	store := rt.credentialStore()
	if err := store.Save(base, cred); err != nil {
		return exitcode.Wrap(exitcode.General, fmt.Errorf("store credential: %w", err))
	}
	if store.Backend() == auth.SourceFile {
		rt.p.Warnf("no system keychain is available; the credential was written to %s (mode 0600)", store.Path())
	}
	if os.Getenv(auth.EnvAPIKey) != "" {
		rt.p.Warnf("%s is set and takes precedence over the stored credential", auth.EnvAPIKey)
	}
	return nil
}

// authInfo is the JSON shape of `auth status` and `auth login`.
type authInfo struct {
	Authenticated bool   `json:"authenticated"`
	BaseURL       string `json:"base_url"`
	Source        string `json:"source,omitempty"` // env, keychain or file
	Method        string `json:"method,omitempty"` // oauth or api_key
	ClientID      string `json:"client_id,omitempty"`
	Email         string `json:"email,omitempty"`
	Scope         string `json:"scope,omitempty"`
	ExpiresAt     string `json:"access_token_expires_at,omitempty"`
	Secret        string `json:"credential,omitempty"` // always masked
}

func describeCredential(base, source string, cred auth.Credential, masked string) authInfo {
	info := authInfo{Authenticated: true, BaseURL: base, Source: source, Secret: masked}
	if source == auth.SourceEnv || cred.IsAPIKey() {
		info.Method = "api_key"
		return info
	}
	info.Method = "oauth"
	info.ClientID = cred.ClientID
	info.Email = cred.Email
	info.Scope = cred.Scope
	if !cred.ExpiresAt.IsZero() {
		info.ExpiresAt = cred.ExpiresAt.UTC().Format(time.RFC3339)
	}
	return info
}

func (rt *runtime) printAuthInfo(w io.Writer, info authInfo) {
	pairs := []string{"Host", info.BaseURL, "Source", info.Source, "Method", info.Method}
	if info.Email != "" {
		pairs = append(pairs, "Account", info.Email)
	}
	if info.ClientID != "" {
		pairs = append(pairs, "Client", info.ClientID)
	}
	if info.Scope != "" {
		pairs = append(pairs, "Scopes", info.Scope)
	}
	if info.ExpiresAt != "" {
		pairs = append(pairs, "Access token expires", info.ExpiresAt+" (renewed automatically)")
	}
	pairs = append(pairs, "Credential", info.Secret)
	rt.p.KV(w, pairs...)
}

func (rt *runtime) reportLogin(base string, cred auth.Credential) error {
	info := describeCredential(base, rt.credentialStore().Backend(), cred, cred.Mask())
	return rt.p.Result(info, func(w io.Writer) {
		fmt.Fprintln(w, "Signed in.")
		rt.printAuthInfo(w, info)
	})
}

func (rt *runtime) authStatus() error {
	base, err := rt.baseURL()
	if err != nil {
		return err
	}
	sess, err := rt.session()
	if err != nil {
		if !errors.Is(err, auth.ErrLoginRequired) {
			return err
		}
		if rt.p.JSON {
			_ = rt.p.EmitJSON(authInfo{Authenticated: false, BaseURL: base})
		}
		return exitcode.New(exitcode.Auth, "not signed in to %s. Run `apipod auth login`, or set %s", base, auth.EnvAPIKey)
	}
	info := describeCredential(base, sess.Source, sess.Credential(), sess.MaskedSecret())
	return rt.p.Result(info, func(w io.Writer) { rt.printAuthInfo(w, info) })
}

func (rt *runtime) logout() error {
	base, err := rt.baseURL()
	if err != nil {
		return err
	}
	store := rt.credentialStore()
	cred, err := store.Get(base)
	if errors.Is(err, auth.ErrNoCredential) {
		return rt.reportLogout(base, false, false)
	}
	// A stored entry that cannot be decoded is still removed below; there is
	// just nothing to revoke.
	readable := err == nil

	revoked := readable && !cred.IsAPIKey() && rt.revoke(base, cred)

	// Local removal happens whether or not the server could be reached.
	if err := store.Delete(base); err != nil {
		return exitcode.Wrap(exitcode.General, fmt.Errorf("remove stored credential: %w", err))
	}
	if readable && !cred.IsAPIKey() && !revoked {
		console, cerr := rt.consoleURL()
		if cerr != nil {
			console = base
		}
		rt.p.Warnf("the credential was removed from this machine, but it could not be revoked on the server. Remove the \"APIPod CLI\" authorization in the console: %s/console", console)
	}
	if readable && cred.IsAPIKey() {
		rt.p.Infof("The stored API key was removed from this machine. It remains valid until you delete it in the console.")
	}
	return rt.reportLogout(base, true, revoked)
}

// revoke invalidates both tokens server-side and reports whether every
// revocation succeeded. The refresh token goes first: it is the long-lived
// one, so it matters most if the second request never happens.
func (rt *runtime) revoke(base string, cred auth.Credential) bool {
	oauth := rt.oauthClient(base)
	ctx, cancel := context.WithTimeout(rt.cmd.Context(), 15*time.Second)
	defer cancel()
	ok := true
	for _, token := range []string{cred.RefreshToken, cred.AccessToken} {
		if token == "" {
			continue
		}
		if err := oauth.Revoke(ctx, token); err != nil {
			ok = false
		}
	}
	return ok
}

func (rt *runtime) reportLogout(base string, removed, revoked bool) error {
	if os.Getenv(auth.EnvAPIKey) != "" {
		rt.p.Warnf("%s is still set; commands will keep using it", auth.EnvAPIKey)
	}
	result := map[string]any{"base_url": base, "removed": removed, "revoked": revoked}
	return rt.p.Result(result, func(w io.Writer) {
		if !removed {
			fmt.Fprintf(w, "No stored credential for %s.\n", base)
			return
		}
		fmt.Fprintf(w, "Signed out of %s.\n", base)
	})
}
