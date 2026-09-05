package store

import (
	"context"
	"database/sql"
	"fmt"
)

const currentSchemaVersion = 3

var migrations = []string{
	1: `
CREATE TABLE users (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    username TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE TABLE auth_sessions (
    id TEXT PRIMARY KEY,
    token_hash BLOB NOT NULL UNIQUE CHECK (length(token_hash) = 32),
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL,
    last_seen_at INTEGER NOT NULL
);
CREATE INDEX auth_sessions_expires_at_idx ON auth_sessions(expires_at);

CREATE TABLE hosts (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    address TEXT NOT NULL,
    port INTEGER NOT NULL CHECK (port BETWEEN 1 AND 65535),
    username TEXT NOT NULL,
    auth_type TEXT NOT NULL CHECK (auth_type IN ('password', 'privateKey', 'agent')),
    encrypted_credentials BLOB,
    fingerprint TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);
CREATE INDEX hosts_name_idx ON hosts(name COLLATE NOCASE);

CREATE TABLE sessions (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('local', 'ssh')),
    host_id TEXT REFERENCES hosts(id) ON DELETE RESTRICT,
    cwd TEXT NOT NULL DEFAULT '',
    command TEXT NOT NULL DEFAULT '',
    persistence TEXT NOT NULL CHECK (persistence IN ('auto', 'tmux', 'screen', 'none')),
    backend TEXT NOT NULL DEFAULT '',
    backend_name TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL CHECK (status IN ('connecting', 'running', 'reconnecting', 'detached', 'exited', 'error')),
    cols INTEGER NOT NULL CHECK (cols > 0),
    rows INTEGER NOT NULL CHECK (rows > 0),
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    last_attached_at INTEGER,
    exit_code INTEGER,
    last_error TEXT
);
CREATE INDEX sessions_updated_at_idx ON sessions(updated_at DESC);
CREATE INDEX sessions_host_id_idx ON sessions(host_id);
`,
	// generation numbers one execution of a session; callbacks carry their own.
	2: `
ALTER TABLE sessions ADD COLUMN generation INTEGER NOT NULL DEFAULT 1;
`,
	// backend_name held the derived tmux/screen name and exit_code was never
	// written; neither is read and no index covers them.
	3: `
ALTER TABLE sessions DROP COLUMN backend_name;
ALTER TABLE sessions DROP COLUMN exit_code;
`,
}

func migrate(ctx context.Context, db *sql.DB) (returnErr error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("store: acquire migration connection: %w", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("store: begin database migration: %w", err)
	}
	defer func() {
		if returnErr != nil {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	var version int
	if err := conn.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("store: read schema version: %w", err)
	}
	if version > currentSchemaVersion {
		return fmt.Errorf("store: database schema version %d is newer than supported version %d", version, currentSchemaVersion)
	}
	for next := version + 1; next <= currentSchemaVersion; next++ {
		if _, err := conn.ExecContext(ctx, migrations[next]); err != nil {
			return fmt.Errorf("store: apply database migration %d: %w", next, err)
		}
		if _, err := conn.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", next)); err != nil {
			return fmt.Errorf("store: record database migration %d: %w", next, err)
		}
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("store: commit database migration: %w", err)
	}
	return nil
}
