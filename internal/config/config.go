// Package config reads and writes the CLI's non-secret settings.
//
// config.toml never holds credentials; those live in the keychain (see
// internal/auth). Everything here is safe to commit to a dotfiles repo.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// DefaultConfirmAbove is the spend (USD) above which a generation needs
// confirmation, --yes or --max-cost.
const DefaultConfirmAbove = 1.0

// Config is the on-disk settings file.
type Config struct {
	Defaults Defaults `toml:"defaults"`
	Spend    Spend    `toml:"spend"`
	Notices  Notices  `toml:"notices"`
}

// Notices records one-time notices the user has already seen.
type Notices struct {
	UploadPublic bool `toml:"upload_public,omitempty"`
}

// Defaults are fallbacks for flags the user did not pass.
type Defaults struct {
	ImageModel string `toml:"image_model,omitempty"`
	VideoModel string `toml:"video_model,omitempty"`
	Output     string `toml:"output,omitempty"` // "human" or "json"
}

// Spend holds the spending guard settings.
type Spend struct {
	ConfirmAbove *float64 `toml:"confirm_above,omitempty"`
}

// ConfirmThreshold returns the effective confirmation threshold.
func (c Config) ConfirmThreshold() float64 {
	if c.Spend.ConfirmAbove != nil {
		return *c.Spend.ConfirmAbove
	}
	return DefaultConfirmAbove
}

// Dir returns the CLI's config directory. APIPOD_CONFIG_DIR overrides it.
func Dir() (string, error) {
	if d := strings.TrimSpace(os.Getenv("APIPOD_CONFIG_DIR")); d != "" {
		return d, nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate config directory: %w", err)
	}
	return filepath.Join(base, "apipod"), nil
}

// Load reads config.toml from dir; a missing file yields the zero Config.
func Load(dir string) (Config, error) {
	var cfg Config
	data, err := os.ReadFile(filepath.Join(dir, "config.toml"))
	if errors.Is(err, fs.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, fmt.Errorf("read config: %w", err)
	}
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parse %s: %w", filepath.Join(dir, "config.toml"), err)
	}
	return cfg, nil
}

// Save writes cfg to dir/config.toml atomically.
func Save(dir string, cfg Config) error {
	data, err := toml.Marshal(cfg)
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(dir, "config.toml"), data, 0o600)
}

// writeFileAtomic writes data to a temp file in the same directory and renames
// it over path, so readers never see a half-written file.
func writeFileAtomic(path string, data []byte, perm fs.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if err := tmp.Chmod(perm); err != nil {
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
	return os.Rename(tmpName, path)
}

// Keys lists the settable keys in display order.
func Keys() []string {
	return []string{"defaults.image_model", "defaults.video_model", "defaults.output", "spend.confirm_above"}
}

// Get returns the string value of key ("" when unset).
func (c Config) Get(key string) (string, error) {
	switch key {
	case "defaults.image_model":
		return c.Defaults.ImageModel, nil
	case "defaults.video_model":
		return c.Defaults.VideoModel, nil
	case "defaults.output":
		return c.Defaults.Output, nil
	case "spend.confirm_above":
		return strconv.FormatFloat(c.ConfirmThreshold(), 'f', -1, 64), nil
	}
	return "", fmt.Errorf("unknown config key (valid: %s)", strings.Join(Keys(), ", "))
}

// Set returns a copy of c with key set to value.
func (c Config) Set(key, value string) (Config, error) {
	value = strings.TrimSpace(value)
	out := c
	switch key {
	case "defaults.image_model":
		out.Defaults.ImageModel = value
	case "defaults.video_model":
		out.Defaults.VideoModel = value
	case "defaults.output":
		if value != "" && value != "human" && value != "json" {
			return c, fmt.Errorf("defaults.output must be human or json")
		}
		out.Defaults.Output = value
	case "spend.confirm_above":
		f, err := strconv.ParseFloat(value, 64)
		if err != nil || f < 0 {
			return c, fmt.Errorf("spend.confirm_above must be a non-negative number of USD")
		}
		out.Spend.ConfirmAbove = &f
	default:
		return c, fmt.Errorf("unknown config key (valid: %s)", strings.Join(Keys(), ", "))
	}
	return out, nil
}
