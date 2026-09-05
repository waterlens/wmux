package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFromLookupEnv(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "private")
	sshConfigPath := filepath.Join(t.TempDir(), "ssh", "config")
	values := map[string]string{
		"WMUX_HOST":        "::1",
		"WMUX_PORT":        "9999",
		"WMUX_DATA_DIR":    dataDir,
		"WMUX_SSH_CONFIG":  sshConfigPath,
		"WMUX_PUBLIC_URL":  "https://terminal.example.test/",
		"WMUX_TRUST_PROXY": "true",
		"WMUX_SESSION_TTL": "12h",
		"WMUX_LOG_LEVEL":   " WARNING ",
	}
	cfg, err := FromLookupEnv(func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	})
	if err != nil {
		t.Fatalf("FromLookupEnv: %v", err)
	}
	if cfg.ListenAddr != "[::1]:9999" {
		t.Fatalf("ListenAddr = %q", cfg.ListenAddr)
	}
	if cfg.PublicURL != "https://terminal.example.test" || !cfg.CookieSecure || !cfg.TrustProxy {
		t.Fatalf("unexpected proxy config: %+v", cfg)
	}
	if cfg.SessionTTL != 12*time.Hour {
		t.Fatalf("SessionTTL = %v", cfg.SessionTTL)
	}
	if cfg.LogLevel != slog.LevelWarn {
		t.Fatalf("LogLevel = %v", cfg.LogLevel)
	}
	if cfg.DatabasePath != filepath.Join(dataDir, "wmux.db") {
		t.Fatalf("DatabasePath = %q", cfg.DatabasePath)
	}
	if cfg.SSHConfigPath != sshConfigPath {
		t.Fatalf("SSHConfigPath = %q", cfg.SSHConfigPath)
	}
	if err := cfg.Ensure(); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	for _, path := range []string{cfg.DataDir, cfg.RecordingsDir} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if info.Mode().Perm() != 0o700 {
			t.Fatalf("mode for %s = %o", path, info.Mode().Perm())
		}
	}
}

func TestFromLookupEnvRejectsInvalidValues(t *testing.T) {
	tests := []map[string]string{
		{"WMUX_PORT": "0"},
		{"WMUX_PORT": "nope"},
		{"WMUX_PUBLIC_URL": "/relative"},
		{"WMUX_COOKIE_SECURE": "sometimes"},
		{"WMUX_SESSION_TTL": "0s"},
		{"WMUX_LOG_LEVEL": "verbose"},
	}
	for _, values := range tests {
		_, err := FromLookupEnv(func(key string) (string, bool) {
			value, ok := values[key]
			return value, ok
		})
		if err == nil {
			t.Fatalf("expected error for %#v", values)
		}
	}
}

func TestEnsureRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	cfg := Config{DataDir: link, RecordingsDir: filepath.Join(link, "recordings")}
	if err := cfg.Ensure(); err == nil {
		t.Fatal("expected symlink data directory to be rejected")
	}
}

func TestDataDirLockAcrossProcesses(t *testing.T) {
	if helperDir := os.Getenv("WMUX_LOCK_HELPER_DIR"); helperDir != "" {
		lock, err := AcquireDataDirLock(helperDir)
		wantBlocked := os.Getenv("WMUX_LOCK_HELPER_BLOCKED") == "1"
		if wantBlocked {
			if !errors.Is(err, ErrDataDirLocked) {
				t.Fatalf("helper lock error = %v, want ErrDataDirLocked", err)
			}
			return
		}
		if err != nil {
			t.Fatalf("helper acquire after release: %v", err)
		}
		if err := lock.Close(); err != nil {
			t.Fatalf("helper close: %v", err)
		}
		return
	}

	dir := filepath.Join(t.TempDir(), "data")
	lock, err := AcquireDataDirLock(dir)
	if err != nil {
		t.Fatalf("AcquireDataDirLock: %v", err)
	}
	if err := runLockHelper(dir, true); err != nil {
		_ = lock.Close()
		t.Fatal(err)
	}
	if err := lock.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if err := runLockHelper(dir, false); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, "wmux.lock"))
	if err == nil && info.Mode().Perm() != 0o600 {
		t.Fatalf("lock file mode = %o", info.Mode().Perm())
	}
}

func runLockHelper(dir string, blocked bool) error {
	command := exec.Command(os.Args[0], "-test.run=^TestDataDirLockAcrossProcesses$")
	wantBlocked := "0"
	if blocked {
		wantBlocked = "1"
	}
	command.Env = append(os.Environ(),
		"WMUX_LOCK_HELPER_DIR="+dir,
		"WMUX_LOCK_HELPER_BLOCKED="+wantBlocked,
	)
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("lock helper failed: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}
