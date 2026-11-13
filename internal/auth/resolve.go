package auth

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/geniuspath/apipod-cli/internal/exitcode"
)

// EnvAPIKey is the only way to hand the CLI a key non-interactively. There is
// deliberately no --api-key flag: argv is visible in `ps` and shell history.
const EnvAPIKey = "APIPOD_API_KEY"

// Credential sources, in priority order.
const (
	SourceEnv      = "env"
	SourceKeychain = "keychain"
	SourceFile     = "file"
)

// refreshSkew refreshes the access token slightly before it expires so a
// request never races the expiry.
const refreshSkew = 60 * time.Second

// ErrLoginRequired means no usable credential exists for the host.
var ErrLoginRequired = exitcode.New(exitcode.Auth,
	"not signed in. Run `apipod auth login`, or set %s", EnvAPIKey)

var errSessionExpired = exitcode.New(exitcode.Auth,
	"your sign-in has expired or was revoked. Run `apipod auth login` again")

// Session is the resolved credential for one base URL. It implements
// api.Transport and refreshes OAuth tokens on demand.
type Session struct {
	BaseURL string
	Source  string

	store *Store
	oauth *OAuthClient
	now   func() time.Time

	mu     sync.Mutex
	apiKey string
	cred   Credential
}

// Resolve picks the credential for baseURL: APIPOD_API_KEY first, then the
// credential stored for exactly this base URL. A credential stored for another
// host is never considered.
func Resolve(store *Store, baseURL string, oauth *OAuthClient) (*Session, error) {
	s := &Session{BaseURL: baseURL, store: store, oauth: oauth, now: time.Now}
	if key := strings.TrimSpace(os.Getenv(EnvAPIKey)); key != "" {
		if err := validateSecret(key); err != nil {
			return nil, exitcode.New(exitcode.Auth, "%s is not a valid key: %v", EnvAPIKey, err)
		}
		s.Source, s.apiKey = SourceEnv, key
		return s, nil
	}
	cred, err := store.Get(baseURL)
	if errors.Is(err, ErrNoCredential) {
		return nil, ErrLoginRequired
	}
	if err != nil {
		return nil, exitcode.Wrap(exitcode.Auth, err)
	}
	s.Source, s.cred = store.Backend(), cred
	return s, nil
}

// validateSecret rejects values that could not be a bearer credential, so a
// stray newline can never turn into a header-splitting attempt.
func validateSecret(v string) error {
	for _, r := range v {
		if r <= 0x20 || r == 0x7f || r > 0x7e {
			return errors.New("contains whitespace or non-ASCII characters")
		}
	}
	return nil
}

// ValidateAPIKey checks a key read from stdin or a prompt.
func ValidateAPIKey(v string) error {
	if v == "" {
		return errors.New("empty key")
	}
	return validateSecret(v)
}

// Credential returns the stored credential (zero for env keys).
func (s *Session) Credential() Credential {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cred
}

// MaskedSecret returns the redacted active secret for `auth status`.
func (s *Session) MaskedSecret() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.apiKey != "" {
		return MaskToken(s.apiKey)
	}
	return s.cred.Mask()
}

// IsAPIKey reports whether the session authenticates with an API key.
func (s *Session) IsAPIKey() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.apiKey != "" || s.cred.IsAPIKey()
}

// Token returns the Authorization header value, refreshing if needed.
func (s *Session) Token(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.apiKey != "":
		return "Bearer " + s.apiKey, nil
	case s.cred.IsAPIKey():
		return "Bearer " + s.cred.APIKey, nil
	}
	if s.cred.AccessToken != "" && s.now().Before(s.cred.ExpiresAt.Add(-refreshSkew)) {
		return "Bearer " + s.cred.AccessToken, nil
	}
	if err := s.refreshLocked(ctx); err != nil {
		return "", err
	}
	return "Bearer " + s.cred.AccessToken, nil
}

// refreshLocked rotates the token pair and persists the result.
func (s *Session) refreshLocked(ctx context.Context) error {
	if s.cred.RefreshToken == "" || s.oauth == nil {
		return ErrLoginRequired
	}
	used := s.cred.RefreshToken
	tr, err := s.oauth.Refresh(ctx, used)
	if err != nil {
		var te *TokenError
		if !errors.As(err, &te) {
			return exitcode.Wrap(exitcode.General, fmt.Errorf("refresh sign-in: %w", err))
		}
		// Refresh tokens rotate. If another apipod process refreshed first,
		// ours is already spent: pick up the pair it stored instead of
		// treating the session as dead.
		latest, gerr := s.store.Get(s.BaseURL)
		if gerr != nil || latest.RefreshToken == "" || latest.RefreshToken == used {
			return errSessionExpired
		}
		s.cred = latest
		if s.now().Before(latest.ExpiresAt.Add(-refreshSkew)) {
			return nil
		}
		used = latest.RefreshToken
		if tr, err = s.oauth.Refresh(ctx, used); err != nil {
			return errSessionExpired
		}
	}
	next := CredentialFromToken(tr, s.now())
	if next.RefreshToken == "" {
		next.RefreshToken = used
	}
	next.Email, next.Username = s.cred.Email, s.cred.Username
	s.cred = next
	if err := s.store.Save(s.BaseURL, next); err != nil {
		return exitcode.Wrap(exitcode.Auth, fmt.Errorf("store refreshed sign-in: %w", err))
	}
	return nil
}
