package store

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func openTestStore(t *testing.T, now *time.Time) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "data", "wmux.db")
	s, err := open(context.Background(), path, func() time.Time { return *now })
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return s
}

func TestOpenMigratesAndConfiguresSQLite(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "private", "wmux.db")
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	var version, foreignKeys, busyTimeout int
	var journalMode string
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow("PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow("PRAGMA busy_timeout").Scan(&busyTimeout); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow("PRAGMA journal_mode").Scan(&journalMode); err != nil {
		t.Fatal(err)
	}
	if version != currentSchemaVersion || foreignKeys != 1 || busyTimeout != 5000 || journalMode != "wal" {
		t.Fatalf("pragmas: version=%d foreign_keys=%d busy_timeout=%d journal=%q", version, foreignKeys, busyTimeout, journalMode)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("database mode = %o", info.Mode().Perm())
	}
}

func TestOpenIsSafeConcurrently(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wmux.db")
	const workers = 4
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	stores := make(chan *Store, workers)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := Open(context.Background(), path)
			stores <- s
			errs <- err
		}()
	}
	wg.Wait()
	close(stores)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent Open: %v", err)
		}
	}
	for s := range stores {
		if s != nil {
			_ = s.Close()
		}
	}
}

func TestNewIDPrefixIsOptional(t *testing.T) {
	bare, err := NewID("")
	if err != nil {
		t.Fatal(err)
	}
	if len(bare) != 32 {
		t.Fatalf("bare id = %q", bare)
	}
	prefixed, err := NewID("ses")
	if err != nil {
		t.Fatal(err)
	}
	if len(prefixed) != len("ses_")+32 || prefixed[:4] != "ses_" {
		t.Fatalf("prefixed id = %q", prefixed)
	}
}
