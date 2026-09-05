// Package store provides wmux's concurrency-safe SQLite persistence layer.
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

var (
	ErrNotFound     = errors.New("store: not found")
	ErrAlreadySetup = errors.New("store: already set up")
	ErrInvalidInput = errors.New("store: invalid input")
	ErrInUse        = errors.New("store: resource is in use")
	openMu          sync.Mutex
)

const maxOpenConns = 8

// Store wraps a database/sql pool. All methods are safe for concurrent use.
type Store struct {
	db  *sql.DB
	now func() time.Time
}

// Open opens a SQLite database, applies connection safety pragmas and runs all
// schema migrations before returning.
func Open(ctx context.Context, path string) (*Store, error) {
	return open(ctx, path, time.Now)
}

// open takes the clock as a parameter so tests can drive timestamps.
func open(ctx context.Context, path string, now func() time.Time) (*Store, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("%w: database path is empty", ErrInvalidInput)
	}
	absolute, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return nil, fmt.Errorf("store: resolve database path: %w", err)
	}
	dir := filepath.Dir(absolute)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("store: create database directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("store: secure database directory: %w", err)
	}
	if info, statErr := os.Lstat(absolute); statErr == nil && info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("store: refusing symlink database path %q", absolute)
	} else if statErr != nil && !os.IsNotExist(statErr) {
		return nil, fmt.Errorf("store: inspect database path: %w", statErr)
	}

	query := url.Values{}
	query.Add("_pragma", "busy_timeout(5000)")
	query.Add("_pragma", "foreign_keys(1)")
	dsn := (&url.URL{Scheme: "file", Path: absolute, RawQuery: query.Encode()}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open sqlite database: %w", err)
	}
	db.SetMaxOpenConns(maxOpenConns)
	db.SetMaxIdleConns(maxOpenConns)
	db.SetConnMaxLifetime(0)

	s := &Store{db: db, now: now}
	// Initialization changes database-wide state. Serialize it within this
	// process; the busy timeout protects other SQLite clients. The application
	// separately holds config.DataDirLock for its full runtime lifetime.
	openMu.Lock()
	defer openMu.Unlock()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: connect sqlite database: %w", err)
	}
	var journalMode string
	if err := db.QueryRowContext(ctx, "PRAGMA journal_mode = WAL").Scan(&journalMode); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: enable sqlite WAL: %w", err)
	}
	if strings.ToLower(journalMode) != "wal" {
		_ = db.Close()
		return nil, fmt.Errorf("store: enable sqlite WAL: database selected %q", journalMode)
	}
	if err := migrate(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := os.Chmod(absolute, 0o600); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: secure sqlite database: %w", err)
	}
	return s, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) Ping(ctx context.Context) error {
	if err := s.db.PingContext(ctx); err != nil {
		return fmt.Errorf("store: ping: %w", err)
	}
	return nil
}

func (s *Store) utcNow() time.Time {
	return s.now().UTC().Truncate(time.Millisecond)
}

// NewID returns a random 128-bit identifier. A non-empty prefix is joined to
// it with an underscore; an empty prefix yields bare hex.
func NewID(prefix string) (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("store: generate id: %w", err)
	}
	id := hex.EncodeToString(raw)
	if prefix == "" {
		return id, nil
	}
	return prefix + "_" + id, nil
}

func unixMillis(t time.Time) int64 {
	return t.UTC().UnixMilli()
}

func fromUnixMillis(value int64) time.Time {
	return time.UnixMilli(value).UTC()
}

func nullableMillis(value *time.Time) any {
	if value == nil {
		return nil
	}
	return unixMillis(*value)
}

func nullableString(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

func nullableInt(value *int) any {
	if value == nil {
		return nil
	}
	return *value
}

// execAffecting runs a statement that must hit exactly one row and reports
// ErrNotFound when it hits none. RowsAffected cannot fail after a successful
// Exec on modernc.org/sqlite, so its error is not reported separately.
func (s *Store) execAffecting(ctx context.Context, action, query string, args ...any) error {
	result, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("store: %s: %w", action, err)
	}
	if count, _ := result.RowsAffected(); count == 0 {
		return ErrNotFound
	}
	return nil
}

func rowsChanged(result sql.Result) bool {
	count, _ := result.RowsAffected()
	return count > 0
}
