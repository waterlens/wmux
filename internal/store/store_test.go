package store

import (
	"context"
	"database/sql"
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

func TestOpenMigratesAVersion2DatabaseAndKeepsItsRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wmux.db")
	legacy, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	for version := 1; version <= 2; version++ {
		if _, err := legacy.Exec(migrations[version]); err != nil {
			t.Fatalf("apply legacy migration %d: %v", version, err)
		}
	}
	if _, err := legacy.Exec("PRAGMA user_version = 2"); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`
INSERT INTO sessions(
    id, name, kind, cwd, command, persistence, backend, backend_name,
    status, cols, rows, created_at, updated_at, exit_code, last_error
) VALUES ('ses_v2', 'Legacy', 'local', '/srv', '', 'tmux', 'tmux', 'wmux-ses_v2-dead',
          'running', 180, 50, 1000, 2000, 7, 'boom')`); err != nil {
		t.Fatalf("insert legacy session: %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open a version 2 database: %v", err)
	}
	defer s.Close()

	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != currentSchemaVersion {
		t.Fatalf("schema version after migration = %d, want %d", version, currentSchemaVersion)
	}
	session, err := s.GetSession(context.Background(), "ses_v2")
	if err != nil {
		t.Fatalf("GetSession after migration: %v", err)
	}
	if session.Name != "Legacy" || session.Cwd != "/srv" || session.Backend != "tmux" ||
		session.Status != SessionStatusRunning || session.Cols != 180 || session.Rows != 50 ||
		session.Generation != 1 || session.Error == nil || *session.Error != "boom" {
		t.Fatalf("migration lost session data: %+v", session)
	}
	var dropped int
	if err := s.db.QueryRow(`
SELECT count(*) FROM pragma_table_info('sessions')
WHERE name IN ('backend_name', 'exit_code')`).Scan(&dropped); err != nil {
		t.Fatal(err)
	}
	if dropped != 0 {
		t.Fatalf("%d dead session columns survived the migration", dropped)
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
