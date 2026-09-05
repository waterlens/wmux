// Package config loads wmux's process configuration and prepares its private
// on-disk data directories.
package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	defaultHost       = "127.0.0.1"
	defaultPort       = 8787
	defaultDataDir    = ".wmux"
	defaultSessionTTL = 7 * 24 * time.Hour
)

// Config is the complete server configuration. Derived paths are made
// absolute by Load and FromLookupEnv so callers do not depend on a later
// working-directory change.
type Config struct {
	Host          string
	Port          int
	ListenAddr    string
	DataDir       string
	DatabasePath  string
	MasterKeyPath string
	RecordingsDir string
	SSHConfigPath string
	PublicURL     string
	CookieSecure  bool
	TrustProxy    bool
	SessionTTL    time.Duration
}

// Load reads configuration from the current process environment.
func Load() (Config, error) {
	return FromLookupEnv(os.LookupEnv)
}

// FromLookupEnv loads configuration using lookup. It exists to make config
// loading deterministic in tests and embedded deployments.
func FromLookupEnv(lookup func(string) (string, bool)) (Config, error) {
	if lookup == nil {
		return Config{}, errors.New("config: lookup function is nil")
	}

	host := valueOr(lookup, "WMUX_HOST", defaultHost)
	if strings.TrimSpace(host) == "" {
		return Config{}, errors.New("config: WMUX_HOST must not be empty")
	}

	port, err := intValue(lookup, "WMUX_PORT", defaultPort)
	if err != nil {
		return Config{}, err
	}
	if port < 1 || port > 65535 {
		return Config{}, errors.New("config: WMUX_PORT must be between 1 and 65535")
	}

	dataDir := valueOr(lookup, "WMUX_DATA_DIR", defaultDataDir)
	dataDir, err = filepath.Abs(filepath.Clean(dataDir))
	if err != nil {
		return Config{}, fmt.Errorf("config: resolve WMUX_DATA_DIR: %w", err)
	}
	sshConfigPath := strings.TrimSpace(valueOr(lookup, "WMUX_SSH_CONFIG", ""))
	if sshConfigPath != "" {
		sshConfigPath, err = filepath.Abs(filepath.Clean(sshConfigPath))
		if err != nil {
			return Config{}, fmt.Errorf("config: resolve WMUX_SSH_CONFIG: %w", err)
		}
	}

	publicURL := strings.TrimSpace(valueOr(lookup, "WMUX_PUBLIC_URL", ""))
	if publicURL != "" {
		parsed, parseErr := url.Parse(publicURL)
		if parseErr != nil || parsed.Scheme == "" || parsed.Host == "" {
			return Config{}, errors.New("config: WMUX_PUBLIC_URL must be an absolute URL")
		}
		if parsed.Scheme != "http" && parsed.Scheme != "https" {
			return Config{}, errors.New("config: WMUX_PUBLIC_URL must use http or https")
		}
		publicURL = strings.TrimRight(publicURL, "/")
	}

	cookieSecureDefault := strings.HasPrefix(strings.ToLower(publicURL), "https://")
	cookieSecure, err := boolValue(lookup, "WMUX_COOKIE_SECURE", cookieSecureDefault)
	if err != nil {
		return Config{}, err
	}
	trustProxy, err := boolValue(lookup, "WMUX_TRUST_PROXY", false)
	if err != nil {
		return Config{}, err
	}
	ttl, err := durationValue(lookup, "WMUX_SESSION_TTL", defaultSessionTTL)
	if err != nil {
		return Config{}, err
	}
	if ttl <= 0 {
		return Config{}, errors.New("config: WMUX_SESSION_TTL must be positive")
	}

	cfg := Config{
		Host:          host,
		Port:          port,
		ListenAddr:    net.JoinHostPort(host, strconv.Itoa(port)),
		DataDir:       dataDir,
		DatabasePath:  filepath.Join(dataDir, "wmux.db"),
		MasterKeyPath: filepath.Join(dataDir, "master.key"),
		RecordingsDir: filepath.Join(dataDir, "recordings"),
		SSHConfigPath: sshConfigPath,
		PublicURL:     publicURL,
		CookieSecure:  cookieSecure,
		TrustProxy:    trustProxy,
		SessionTTL:    ttl,
	}
	return cfg, nil
}

// Ensure creates all private directories needed by wmux. Existing directory
// permissions are tightened as well, because credentials and terminal output
// can both contain secrets.
func (c Config) Ensure() error {
	for _, path := range []string{c.DataDir, c.RecordingsDir} {
		if strings.TrimSpace(path) == "" {
			return errors.New("config: configuration contains an empty data path")
		}
		if err := ensurePrivateDir(path); err != nil {
			return err
		}
	}
	return nil
}

func ensurePrivateDir(path string) error {
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("config: refusing symlink data directory %q", path)
	} else if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("config: inspect data directory %q: %w", path, err)
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("config: create data directory %q: %w", path, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("config: inspect data directory %q: %w", path, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("config: data path %q is not a directory", path)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return fmt.Errorf("config: secure data directory %q: %w", path, err)
	}
	return nil
}

func valueOr(lookup func(string) (string, bool), key, fallback string) string {
	if value, ok := lookup(key); ok {
		return strings.TrimSpace(value)
	}
	return fallback
}

func intValue(lookup func(string) (string, bool), key string, fallback int) (int, error) {
	value, ok := lookup(key)
	if !ok || strings.TrimSpace(value) == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return 0, fmt.Errorf("config: %s must be an integer: %w", key, err)
	}
	return parsed, nil
}

func boolValue(lookup func(string) (string, bool), key string, fallback bool) (bool, error) {
	value, ok := lookup(key)
	if !ok || strings.TrimSpace(value) == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseBool(strings.TrimSpace(value))
	if err != nil {
		return false, fmt.Errorf("config: %s must be a boolean: %w", key, err)
	}
	return parsed, nil
}

func durationValue(lookup func(string) (string, bool), key string, fallback time.Duration) (time.Duration, error) {
	value, ok := lookup(key)
	if !ok || strings.TrimSpace(value) == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(strings.TrimSpace(value))
	if err != nil {
		return 0, fmt.Errorf("config: %s must be a duration: %w", key, err)
	}
	return parsed, nil
}
