// Package auth stores and resolves CLI credentials.
//
// A credential is always bound to the base URL of the server that issued it:
// keychain entries and file entries are keyed by normalized origin, so a token
// for api.apipod.ai can never be sent to a different host (e.g. one injected
// through APIPOD_BASE_URL).
package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/zalando/go-keyring"
)

// Credential is what `auth login` stores: an OAuth token pair, or an API key
// when the user chose `auth login --with-api-key`.
type Credential struct {
	APIKey       string    `json:"api_key,omitempty"`
	AccessToken  string    `json:"access_token,omitempty"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	TokenType    string    `json:"token_type,omitempty"`
	ExpiresAt    time.Time `json:"expires_at,omitzero"` // access-token expiry
	ClientID     string    `json:"client_id,omitempty"`
	Scope        string    `json:"scope,omitempty"`
	Email        string    `json:"email,omitempty"`
	Username     string    `json:"username,omitempty"`
}

// IsAPIKey reports whether the credential is a stored API key.
func (c Credential) IsAPIKey() bool { return c.APIKey != "" }

// Mask returns a redacted form of the secret, safe to print.
func (c Credential) Mask() string {
	if c.IsAPIKey() {
		return MaskToken(c.APIKey)
	}
	return MaskToken(c.AccessToken)
}

// MaskToken redacts a secret down to a recognisable prefix and suffix.
func MaskToken(t string) string {
	switch {
	case t == "":
		return "(none)"
	case len(t) < 16:
		return "****"
	default:
		return t[:8] + "…" + t[len(t)-4:]
	}
}

// Store persists credentials. The primary backend is the OS keychain
// (macOS Keychain / Windows Credential Manager / Secret Service); the JSON
// file is a fallback for headless Linux boxes, never written when the keychain
// worked.
type Store struct {
	// Dir is the CLI config directory (credentials.json lives beside config.toml).
	Dir string
	// KeychainAvailable marks the system keychain as usable.
	KeychainAvailable bool
}

// keychain service name; the account is the normalized base URL.
const keyringService = "apipod-cli"

// NewStore probes the keychain once and returns the store.
func NewStore(dir string) *Store {
	return &Store{Dir: dir, KeychainAvailable: keyringAvailable()}
}

func keyringAvailable() bool {
	// keyring.Get on macOS/Windows never returns ErrKeychainNotFound for a
	// bogus service; a round trip with a sentinel is the portable probe.
	_, err := keyring.Get("apipod-cli-availability-probe", "probe")
	return err == nil || errors.Is(err, keyring.ErrNotFound)
}

// Path is the location of the fallback credentials file.
func (s *Store) Path() string {
	return filepath.Join(s.Dir, "credentials.json")
}

// Backend names the active storage backend: "keychain" or "file".
func (s *Store) Backend() string {
	if s.KeychainAvailable {
		return "keychain"
	}
	return "file"
}

// Save stores cred under baseURL.
func (s *Store) Save(baseURL string, cred Credential) error {
	if s.KeychainAvailable {
		data, err := json.Marshal(cred)
		if err != nil {
			return err
		}
		return keyring.Set(keyringService, baseURL, string(data))
	}
	creds, err := s.readAll()
	if err != nil {
		return err
	}
	// Build a new map instead of editing the one that was read.
	next := make(map[string]Credential, len(creds)+1)
	for host, c := range creds {
		next[host] = c
	}
	next[baseURL] = cred
	data, err := json.Marshal(next)
	if err != nil {
		return err
	}
	return atomicWrite0600(s.Path(), data)
}

// Get returns the credential for baseURL, or ErrNoCredential.
func (s *Store) Get(baseURL string) (Credential, error) {
	if s.KeychainAvailable {
		raw, err := keyring.Get(keyringService, baseURL)
		if errors.Is(err, keyring.ErrNotFound) {
			return Credential{}, ErrNoCredential
		}
		if err != nil {
			return Credential{}, fmt.Errorf("read keychain: %w", err)
		}
		var cred Credential
		if err := json.Unmarshal([]byte(raw), &cred); err != nil {
			return Credential{}, fmt.Errorf("decode keychain entry: %w", err)
		}
		return cred, nil
	}
	creds, err := s.readAll()
	if err != nil {
		return Credential{}, err
	}
	cred, ok := creds[baseURL]
	if !ok {
		return Credential{}, ErrNoCredential
	}
	return cred, nil
}

// Delete removes the credential for baseURL; a missing entry is not an error.
func (s *Store) Delete(baseURL string) error {
	if s.KeychainAvailable {
		err := keyring.Delete(keyringService, baseURL)
		if errors.Is(err, keyring.ErrNotFound) {
			return nil
		}
		return err
	}
	creds, err := s.readAll()
	if err != nil {
		return err
	}
	if _, ok := creds[baseURL]; !ok {
		return nil
	}
	next := make(map[string]Credential, len(creds))
	for host, c := range creds {
		if host != baseURL {
			next[host] = c
		}
	}
	data, err := json.Marshal(next)
	if err != nil {
		return err
	}
	return atomicWrite0600(s.Path(), data)
}

// ErrNoCredential means nothing is stored for a host.
var ErrNoCredential = errors.New("no stored credential for this host")

// readAll loads the fallback file; a missing file is an empty store. A file
// that group or others can access is refused, the way ssh refuses a loose
// private key.
func (s *Store) readAll() (map[string]Credential, error) {
	path := s.Path()
	// Lstat: a symlink in place of the credentials file is refused, not followed.
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]Credential{}, nil
	}
	if err != nil {
		return nil, err
	}
	if !checkFilePerms(info) {
		return nil, fmt.Errorf("refusing to read %s: it must be a regular file readable only by you (mode 0600, found %s). Run: chmod 600 %s", path, info.Mode(), path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var creds map[string]Credential
	if err := json.Unmarshal(data, &creds); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return creds, nil
}

// checkFilePerms enforces owner-only access. Windows has no POSIX mode bits
// (Go reports 0666 for every file there) and uses the Credential Manager, so
// the check only applies elsewhere.
func checkFilePerms(info os.FileInfo) bool {
	if !info.Mode().IsRegular() {
		return false
	}
	if runtime.GOOS == "windows" {
		return true
	}
	return info.Mode().Perm()&0o077 == 0
}

func atomicWrite0600(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".credentials-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
