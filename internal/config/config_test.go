package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestNormalizeBaseURL(t *testing.T) {
	ok := map[string]string{
		"":                             DefaultBaseURL,
		"  ":                           DefaultBaseURL,
		"https://api.apipod.ai":        "https://api.apipod.ai",
		"https://api.apipod.ai/":       "https://api.apipod.ai",
		"HTTPS://API.Example.COM:8443": "https://api.example.com:8443",
		"http://127.0.0.1:8080":        "http://127.0.0.1:8080",
		"http://localhost:8080/":       "http://localhost:8080",
		"http://[::1]:8080":            "http://[::1]:8080",
	}
	for in, want := range ok {
		got, err := NormalizeBaseURL(in)
		if err != nil || got != want {
			t.Errorf("NormalizeBaseURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

// Plain http would put the bearer token on the wire in clear text, so it is
// accepted for loopback hosts only.
func TestNormalizeBaseURLRejectsUnsafeURLs(t *testing.T) {
	bad := []string{
		"http://api.apipod.ai",
		"http://evil.example.com",
		"http://127.0.0.1.evil.example.com",
		"http://localhost.evil.example.com",
		"http://192.168.1.10:8080",
		"http://10.0.0.1",
		"http://0.0.0.0:8080",
		"ftp://api.apipod.ai",
		"file:///etc/passwd",
		"api.apipod.ai",
		"//api.apipod.ai",
		"https://",
		"https://user:pass@api.apipod.ai",
		"https://api.apipod.ai/v1",
		"https://api.apipod.ai?x=1",
		"https://api.apipod.ai#frag",
		"javascript:alert(1)",
	}
	for _, in := range bad {
		if got, err := NormalizeBaseURL(in); err == nil {
			t.Errorf("NormalizeBaseURL(%q) = %q, want an error", in, got)
		}
	}
}

func TestConsoleURL(t *testing.T) {
	cases := []struct{ base, override, want string }{
		{DefaultBaseURL, "", "https://www.apipod.ai"},
		{"http://127.0.0.1:8080", "", "http://127.0.0.1:8080"},
		{"http://127.0.0.1:8080", "http://localhost:3000", "http://localhost:3000"},
		{DefaultBaseURL, "https://console.example.com/", "https://console.example.com"},
	}
	for _, tc := range cases {
		got, err := ConsoleURL(tc.base, tc.override)
		if err != nil || got != tc.want {
			t.Errorf("ConsoleURL(%q, %q) = %q, %v; want %q", tc.base, tc.override, got, err, tc.want)
		}
	}
	if _, err := ConsoleURL(DefaultBaseURL, "http://evil.example.com"); err == nil {
		t.Error("an insecure console override was accepted")
	}
}

func TestLoadMissingFileGivesDefaults(t *testing.T) {
	cfg, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ConfirmThreshold() != DefaultConfirmAbove {
		t.Errorf("threshold = %v, want the default %v", cfg.ConfirmThreshold(), DefaultConfirmAbove)
	}
}

func TestSetGetSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{}
	var err error
	for key, value := range map[string]string{
		"defaults.image_model": "gpt-image-2",
		"defaults.video_model": "kling-3.0-turbo-t2v",
		"defaults.output":      "json",
		"spend.confirm_above":  "0.25",
	} {
		if cfg, err = cfg.Set(key, value); err != nil {
			t.Fatalf("Set(%s): %v", key, err)
		}
	}
	if err := Save(dir, cfg); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range Keys() {
		want, _ := cfg.Get(key)
		got, _ := loaded.Get(key)
		if got != want {
			t.Errorf("%s = %q after reload, want %q", key, got, want)
		}
	}
	if loaded.ConfirmThreshold() != 0.25 {
		t.Errorf("threshold = %v", loaded.ConfirmThreshold())
	}
	if runtime.GOOS != "windows" {
		info, _ := os.Stat(filepath.Join(dir, "config.toml"))
		if info.Mode().Perm() != 0o600 {
			t.Errorf("config.toml mode = %#o, want 0600", info.Mode().Perm())
		}
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("config dir holds %d entries, want only config.toml", len(entries))
	}
}

// A threshold of zero is a real setting ("always ask"), distinct from unset.
func TestZeroThresholdIsKept(t *testing.T) {
	dir := t.TempDir()
	cfg, err := Config{}.Set("spend.confirm_above", "0")
	if err != nil {
		t.Fatal(err)
	}
	if err := Save(dir, cfg); err != nil {
		t.Fatal(err)
	}
	loaded, _ := Load(dir)
	if loaded.ConfirmThreshold() != 0 {
		t.Errorf("threshold = %v, want 0", loaded.ConfirmThreshold())
	}
}

func TestSetDoesNotMutateTheReceiver(t *testing.T) {
	orig := Config{}
	next, err := orig.Set("defaults.image_model", "m")
	if err != nil {
		t.Fatal(err)
	}
	if orig.Defaults.ImageModel != "" || next.Defaults.ImageModel != "m" {
		t.Errorf("orig = %+v, next = %+v", orig.Defaults, next.Defaults)
	}
}

func TestSetRejectsBadInput(t *testing.T) {
	bad := [][2]string{
		{"spend.confirm_above", "-1"},
		{"spend.confirm_above", "lots"},
		{"defaults.output", "yaml"},
		{"nope.key", "x"},
		{"api_key", "sk-123"},
	}
	for _, kv := range bad {
		if _, err := (Config{}).Set(kv[0], kv[1]); err == nil {
			t.Errorf("Set(%q, %q) accepted", kv[0], kv[1])
		}
	}
	if _, err := (Config{}).Get("api_key"); err == nil {
		t.Error("Get(api_key) accepted: credentials are not config")
	}
}

// config.toml has no place for a secret: no settable key can hold one.
func TestNoConfigKeyHoldsCredentials(t *testing.T) {
	for _, key := range Keys() {
		lower := strings.ToLower(key)
		for _, word := range []string{"key", "token", "secret", "password", "credential"} {
			if strings.Contains(lower, word) {
				t.Errorf("config key %q looks like a credential slot", key)
			}
		}
	}
}

func TestLoadReportsMalformedFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte("this is = = not toml"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil {
		t.Error("malformed config loaded without error")
	}
}

func TestDirHonoursOverride(t *testing.T) {
	t.Setenv("APIPOD_CONFIG_DIR", "/custom/dir")
	if got, err := Dir(); err != nil || got != "/custom/dir" {
		t.Errorf("Dir = %q, %v", got, err)
	}
}
