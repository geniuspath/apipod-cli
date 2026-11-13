package auth

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/zalando/go-keyring"
)

const (
	hostA = "https://api.apipod.ai"
	hostB = "https://evil.example.com"
)

func fileStore(t *testing.T) *Store {
	t.Helper()
	return &Store{Dir: t.TempDir(), KeychainAvailable: false}
}

func sampleCredential() Credential {
	return Credential{
		AccessToken:  "access-token-0123456789abcdef",
		RefreshToken: "refresh-token-0123456789abcdef",
		ExpiresAt:    time.Now().Add(time.Hour).UTC().Truncate(time.Second),
		ClientID:     ClientID,
		Scope:        Scopes,
		Email:        "dev@example.com",
	}
}

func TestKeychainStoreRoundTrip(t *testing.T) {
	keyring.MockInit()
	s := &Store{Dir: t.TempDir(), KeychainAvailable: true}
	cred := sampleCredential()
	if err := s.Save(hostA, cred); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(hostA)
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != cred.AccessToken || got.RefreshToken != cred.RefreshToken || !got.ExpiresAt.Equal(cred.ExpiresAt) {
		t.Errorf("round trip mismatch: %+v", got)
	}
	// With a working keychain nothing is written to disk.
	if _, err := os.Stat(s.Path()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("credentials file exists although the keychain is available (err=%v)", err)
	}
	if err := s.Delete(hostA); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(hostA); !errors.Is(err, ErrNoCredential) {
		t.Errorf("after Delete: err = %v, want ErrNoCredential", err)
	}
	if err := s.Delete(hostA); err != nil {
		t.Errorf("deleting a missing entry: %v", err)
	}
}

// A credential is only ever returned for the base URL it was stored under.
func TestCredentialIsBoundToItsHost(t *testing.T) {
	stores := map[string]*Store{"file": fileStore(t)}
	keyring.MockInit()
	stores["keychain"] = &Store{Dir: t.TempDir(), KeychainAvailable: true}

	for name, s := range stores {
		t.Run(name, func(t *testing.T) {
			if err := s.Save(hostA, sampleCredential()); err != nil {
				t.Fatal(err)
			}
			for _, other := range []string{hostB, "https://api.apipod.ai.evil.example.com", "http://127.0.0.1:8080", "https://API.APIPOD.AI:8443"} {
				if _, err := s.Get(other); !errors.Is(err, ErrNoCredential) {
					t.Errorf("Get(%q) err = %v, want ErrNoCredential", other, err)
				}
			}
			if _, err := s.Get(hostA); err != nil {
				t.Errorf("Get(own host): %v", err)
			}
		})
	}
}

func TestFileStoreWritesOwnerOnlyFile(t *testing.T) {
	s := fileStore(t)
	if err := s.Save(hostA, sampleCredential()); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Errorf("credentials file mode = %#o, want 0600", info.Mode().Perm())
	}
	// Saving a second host keeps the first and leaves no temp file behind.
	if err := s.Save(hostB, Credential{APIKey: "sk-another-host-key-123456"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(hostA); err != nil {
		t.Errorf("first host lost after saving a second: %v", err)
	}
	entries, _ := os.ReadDir(s.Dir)
	if len(entries) != 1 {
		t.Errorf("config dir holds %d entries, want only credentials.json", len(entries))
	}
}

func TestFileStoreRefusesLoosePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits do not exist on Windows")
	}
	for _, mode := range []os.FileMode{0o644, 0o640, 0o604, 0o660, 0o666, 0o777} {
		s := fileStore(t)
		if err := s.Save(hostA, sampleCredential()); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(s.Path(), mode); err != nil {
			t.Fatal(err)
		}
		_, err := s.Get(hostA)
		if err == nil {
			t.Errorf("mode %#o: credential was read from a file others can access", mode)
			continue
		}
		if errors.Is(err, ErrNoCredential) || !strings.Contains(err.Error(), "refusing to read") {
			t.Errorf("mode %#o: err = %v, want an explicit refusal", mode, err)
		}
	}
	// Stricter than 0600 is fine.
	s := fileStore(t)
	if err := s.Save(hostA, sampleCredential()); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(s.Path(), 0o400); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(hostA); err != nil {
		t.Errorf("mode 0400 rejected: %v", err)
	}
}

func TestFileStoreRefusesSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symlinks needs privileges on Windows")
	}
	s := fileStore(t)
	if err := s.Save(hostA, sampleCredential()); err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(t.TempDir(), "elsewhere.json")
	if err := os.Rename(s.Path(), real); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, s.Path()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(hostA); err == nil || errors.Is(err, ErrNoCredential) {
		t.Errorf("err = %v, want a refusal to read through a symlink", err)
	}
}

func TestFileStoreDelete(t *testing.T) {
	s := fileStore(t)
	if err := s.Delete(hostA); err != nil {
		t.Errorf("deleting from an empty store: %v", err)
	}
	if err := s.Save(hostA, sampleCredential()); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(hostB, sampleCredential()); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(hostA); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(hostA); !errors.Is(err, ErrNoCredential) {
		t.Errorf("deleted host still readable: %v", err)
	}
	if _, err := s.Get(hostB); err != nil {
		t.Errorf("Delete removed another host's credential: %v", err)
	}
}

func TestMaskNeverRevealsTheSecret(t *testing.T) {
	for _, secret := range []string{"", "short", "sk-0123456789abcdefghijklmnop", strings.Repeat("x", 64)} {
		masked := MaskToken(secret)
		if secret != "" && strings.Contains(masked, secret) {
			t.Errorf("MaskToken(%q) = %q reveals the secret", secret, masked)
		}
		if len(secret) >= 16 && len(masked) >= len(secret) {
			t.Errorf("MaskToken(%d chars) = %q is not shorter than the secret", len(secret), masked)
		}
	}
	if got := MaskToken("sk-0123456789abcdefghijklmnop"); got != "sk-01234…mnop" {
		t.Errorf("MaskToken = %q", got)
	}
}
