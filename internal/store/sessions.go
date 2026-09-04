package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const sessionSelect = `
SELECT s.id, s.name, s.kind, s.host_id, h.name,
       s.cwd, s.command, s.persistence, s.backend, s.backend_name,
       s.status, s.cols, s.rows, s.created_at, s.updated_at,
       s.last_attached_at, s.exit_code, s.last_error
FROM sessions s
LEFT JOIN hosts h ON h.id = s.host_id`

func (s *Store) CreateSession(ctx context.Context, session Session) (Session, error) {
	applySessionDefaults(&session)
	if err := validateSession(session); err != nil {
		return Session{}, err
	}
	if session.ID == "" {
		id, err := newID()
		if err != nil {
			return Session{}, err
		}
		session.ID = id
	}
	now := s.utcNow()
	session.CreatedAt = now
	session.UpdatedAt = now
	_, err := s.db.ExecContext(ctx, `
INSERT INTO sessions(
    id, name, kind, host_id, cwd, command, persistence, backend,
    backend_name, status, cols, rows, created_at, updated_at,
    last_attached_at, exit_code, last_error
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		session.ID,
		session.Name,
		session.Kind,
		nullableString(session.HostID),
		session.Cwd,
		session.Command,
		session.Persistence,
		session.Backend,
		session.BackendName,
		session.Status,
		session.Cols,
		session.Rows,
		unixMillis(session.CreatedAt),
		unixMillis(session.UpdatedAt),
		nullableMillis(session.LastAttachedAt),
		nullableInt(session.ExitCode),
		nullableString(session.Error),
	)
	if err != nil {
		return Session{}, fmt.Errorf("create session: %w", err)
	}
	return s.GetSession(ctx, session.ID)
}

func (s *Store) GetSession(ctx context.Context, id string) (Session, error) {
	if strings.TrimSpace(id) == "" {
		return Session{}, ErrNotFound
	}
	session, err := scanSession(s.db.QueryRowContext(ctx, sessionSelect+" WHERE s.id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	if err != nil {
		return Session{}, fmt.Errorf("get session: %w", err)
	}
	return session, nil
}

func (s *Store) ListSessions(ctx context.Context) ([]Session, error) {
	return s.listSessions(ctx, sessionSelect+" ORDER BY s.updated_at DESC, s.id", nil)
}

func (s *Store) listSessions(ctx context.Context, query string, args []any) ([]Session, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	defer rows.Close()
	sessions := make([]Session, 0)
	for rows.Next() {
		session, scanErr := scanSession(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("scan session: %w", scanErr)
		}
		sessions = append(sessions, session)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate sessions: %w", err)
	}
	return sessions, nil
}

// UpdateSession replaces product-owned session fields. Runtime-owned fields
// (backend, status, attachment, exit and error) are deliberately preserved so
// a concurrent terminal callback cannot be overwritten by a stale API model.
// Size-only updates do not change UpdatedAt, keeping sidebar ordering stable.
func (s *Store) UpdateSession(ctx context.Context, session Session) (Session, error) {
	if strings.TrimSpace(session.ID) == "" {
		return Session{}, fmt.Errorf("%w: session id is empty", ErrInvalidInput)
	}
	if err := validateSession(session); err != nil {
		return Session{}, err
	}
	result, err := s.db.ExecContext(ctx, `
UPDATE sessions
SET name = ?, kind = ?, host_id = ?, cwd = ?, command = ?,
    persistence = ?, cols = ?, rows = ?,
    updated_at = CASE
        WHEN name IS NOT ? OR kind IS NOT ? OR host_id IS NOT ?
          OR cwd IS NOT ? OR command IS NOT ? OR persistence IS NOT ?
        THEN ? ELSE updated_at END
WHERE id = ?`,
		session.Name,
		session.Kind,
		nullableString(session.HostID),
		session.Cwd,
		session.Command,
		session.Persistence,
		session.Cols,
		session.Rows,
		session.Name,
		session.Kind,
		nullableString(session.HostID),
		session.Cwd,
		session.Command,
		session.Persistence,
		unixMillis(s.utcNow()),
		session.ID,
	)
	if err != nil {
		return Session{}, fmt.Errorf("update session: %w", err)
	}
	updated, err := rowsChanged(result)
	if err != nil {
		return Session{}, fmt.Errorf("check session update: %w", err)
	}
	if !updated {
		return Session{}, ErrNotFound
	}
	return s.GetSession(ctx, session.ID)
}

func (s *Store) DeleteSession(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, "DELETE FROM sessions WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	deleted, err := rowsChanged(result)
	if err != nil {
		return fmt.Errorf("check session deletion: %w", err)
	}
	if !deleted {
		return ErrNotFound
	}
	return nil
}

// UpdateSessionName updates the only frequently edited product metadata field
// and returns the joined representation used by the API.
func (s *Store) UpdateSessionName(ctx context.Context, id, name string) (Session, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Session{}, fmt.Errorf("%w: session name is empty", ErrInvalidInput)
	}
	result, err := s.db.ExecContext(ctx, `
UPDATE sessions SET name = ?, updated_at = ?
WHERE id = ? AND name IS NOT ?`, name, unixMillis(s.utcNow()), id, name)
	if err != nil {
		return Session{}, fmt.Errorf("update session name: %w", err)
	}
	if err := s.requireSession(ctx, id, result); err != nil {
		return Session{}, err
	}
	return s.GetSession(ctx, id)
}

func (s *Store) UpdateSessionStatus(ctx context.Context, id, status string, exitCode *int, sessionError *string) error {
	if !validSessionStatus(status) {
		return fmt.Errorf("%w: unsupported session status %q", ErrInvalidInput, status)
	}
	result, err := s.db.ExecContext(ctx, `
UPDATE sessions
SET status = ?, exit_code = ?, last_error = ?
WHERE id = ?
  AND (status IS NOT ? OR exit_code IS NOT ? OR last_error IS NOT ?)`,
		status, nullableInt(exitCode), nullableString(sessionError),
		id, status, nullableInt(exitCode), nullableString(sessionError))
	if err != nil {
		return fmt.Errorf("update session status: %w", err)
	}
	return s.requireSession(ctx, id, result)
}

func (s *Store) UpdateSessionSize(ctx context.Context, id string, cols, rows int) error {
	if cols <= 0 || rows <= 0 {
		return fmt.Errorf("%w: session dimensions must be positive", ErrInvalidInput)
	}
	result, err := s.db.ExecContext(ctx, `
UPDATE sessions SET cols = ?, rows = ?
WHERE id = ? AND (cols IS NOT ? OR rows IS NOT ?)`,
		cols, rows, id, cols, rows)
	if err != nil {
		return fmt.Errorf("update session size: %w", err)
	}
	return s.requireSession(ctx, id, result)
}

func (s *Store) UpdateSessionBackend(ctx context.Context, id, backend, backendName string) error {
	result, err := s.db.ExecContext(ctx, `
UPDATE sessions SET backend = ?, backend_name = ?
WHERE id = ? AND (backend IS NOT ? OR backend_name IS NOT ?)`,
		backend, backendName, id, backend, backendName)
	if err != nil {
		return fmt.Errorf("update session backend: %w", err)
	}
	return s.requireSession(ctx, id, result)
}

func (s *Store) TouchSession(ctx context.Context, id string, attachedAt time.Time) error {
	if attachedAt.IsZero() {
		attachedAt = s.utcNow()
	} else {
		attachedAt = attachedAt.UTC().Truncate(time.Millisecond)
	}
	attachedMillis := unixMillis(attachedAt)
	result, err := s.db.ExecContext(ctx, `
UPDATE sessions SET last_attached_at = ?
WHERE id = ? AND last_attached_at IS NOT ?`,
		attachedMillis, id, attachedMillis)
	if err != nil {
		return fmt.Errorf("touch session: %w", err)
	}
	return s.requireSession(ctx, id, result)
}

// UpdateSessionRuntime atomically applies a terminal callback. An empty
// backend leaves the resolved backend untouched (connecting callbacks occur
// before backend resolution). Repeated identical callbacks perform no write.
// Runtime activity intentionally never changes UpdatedAt.
func (s *Store) UpdateSessionRuntime(ctx context.Context, id, status, backend, backendName string, sessionError *string) error {
	if !validSessionStatus(status) {
		return fmt.Errorf("%w: unsupported session status %q", ErrInvalidInput, status)
	}
	result, err := s.db.ExecContext(ctx, `
UPDATE sessions
SET status = ?,
    backend = CASE WHEN ? = '' THEN backend ELSE ? END,
    backend_name = CASE WHEN ? = '' THEN backend_name ELSE ? END,
    last_error = ?
WHERE id = ? AND (
    status IS NOT ?
    OR (? <> '' AND backend IS NOT ?)
    OR (? <> '' AND backend_name IS NOT ?)
    OR last_error IS NOT ?
)`,
		status,
		backend, backend,
		backend, backendName,
		nullableString(sessionError),
		id,
		status,
		backend, backend,
		backend, backendName,
		nullableString(sessionError),
	)
	if err != nil {
		return fmt.Errorf("update session runtime: %w", err)
	}
	return s.requireSession(ctx, id, result)
}

// SaveRuntimeSession atomically creates terminal-owned metadata or refreshes
// only runtime-owned fields of an existing product session. This is the
// persistence boundary used by terminal.Repository.SaveSession.
func (s *Store) SaveRuntimeSession(ctx context.Context, session Session) error {
	applySessionDefaults(&session)
	if strings.TrimSpace(session.ID) == "" {
		return fmt.Errorf("%w: session id is empty", ErrInvalidInput)
	}
	if err := validateSession(session); err != nil {
		return err
	}
	now := unixMillis(s.utcNow())
	_, err := s.db.ExecContext(ctx, `
INSERT INTO sessions(
    id, name, kind, host_id, cwd, command, persistence, backend,
    backend_name, status, cols, rows, created_at, updated_at,
    last_attached_at, exit_code, last_error
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
    backend = excluded.backend,
    backend_name = excluded.backend_name,
    cols = excluded.cols,
    rows = excluded.rows,
    status = CASE
        WHEN excluded.status = 'exited' THEN excluded.status
        ELSE sessions.status END`,
		session.ID,
		session.Name,
		session.Kind,
		nullableString(session.HostID),
		session.Cwd,
		session.Command,
		session.Persistence,
		session.Backend,
		session.BackendName,
		session.Status,
		session.Cols,
		session.Rows,
		now,
		now,
		nullableMillis(session.LastAttachedAt),
		nullableInt(session.ExitCode),
		nullableString(session.Error),
	)
	if err != nil {
		return fmt.Errorf("save runtime session: %w", err)
	}
	return nil
}

func (s *Store) requireSession(ctx context.Context, id string, result sql.Result) error {
	changed, err := rowsChanged(result)
	if err != nil {
		return fmt.Errorf("check session update: %w", err)
	}
	if changed {
		return nil
	}
	var exists int
	err = s.db.QueryRowContext(ctx, "SELECT 1 FROM sessions WHERE id = ?", id).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("check session existence: %w", err)
	}
	return nil
}

func applySessionDefaults(session *Session) {
	if session.Persistence == "" {
		session.Persistence = SessionPersistenceAuto
	}
	if session.Status == "" {
		session.Status = SessionStatusConnecting
	}
	if session.Cols == 0 {
		session.Cols = 120
	}
	if session.Rows == 0 {
		session.Rows = 36
	}
}

func validateSession(session Session) error {
	if strings.TrimSpace(session.Name) == "" {
		return fmt.Errorf("%w: session name is empty", ErrInvalidInput)
	}
	switch session.Kind {
	case SessionKindLocal:
		if session.HostID != nil {
			return fmt.Errorf("%w: local session cannot reference a host", ErrInvalidInput)
		}
	case SessionKindSSH:
		if session.HostID == nil || strings.TrimSpace(*session.HostID) == "" {
			return fmt.Errorf("%w: SSH session requires a host", ErrInvalidInput)
		}
	default:
		return fmt.Errorf("%w: unsupported session kind %q", ErrInvalidInput, session.Kind)
	}
	switch session.Persistence {
	case SessionPersistenceAuto, SessionPersistenceTmux, SessionPersistenceScreen, SessionPersistenceNone:
	default:
		return fmt.Errorf("%w: unsupported session persistence %q", ErrInvalidInput, session.Persistence)
	}
	if !validSessionStatus(session.Status) {
		return fmt.Errorf("%w: unsupported session status %q", ErrInvalidInput, session.Status)
	}
	if session.Cols <= 0 || session.Rows <= 0 {
		return fmt.Errorf("%w: session dimensions must be positive", ErrInvalidInput)
	}
	return nil
}

func validSessionStatus(status string) bool {
	switch status {
	case SessionStatusConnecting,
		SessionStatusRunning,
		SessionStatusReconnecting,
		SessionStatusDetached,
		SessionStatusExited,
		SessionStatusError:
		return true
	default:
		return false
	}
}

func scanSession(row scanner) (Session, error) {
	var session Session
	var hostID, hostName, lastError sql.NullString
	var lastAttachedAt, exitCode sql.NullInt64
	var createdAt, updatedAt int64
	err := row.Scan(
		&session.ID,
		&session.Name,
		&session.Kind,
		&hostID,
		&hostName,
		&session.Cwd,
		&session.Command,
		&session.Persistence,
		&session.Backend,
		&session.BackendName,
		&session.Status,
		&session.Cols,
		&session.Rows,
		&createdAt,
		&updatedAt,
		&lastAttachedAt,
		&exitCode,
		&lastError,
	)
	if err != nil {
		return Session{}, err
	}
	if hostID.Valid {
		value := hostID.String
		session.HostID = &value
	}
	if hostName.Valid {
		value := hostName.String
		session.HostName = &value
	}
	if lastAttachedAt.Valid {
		value := fromUnixMillis(lastAttachedAt.Int64)
		session.LastAttachedAt = &value
	}
	if exitCode.Valid {
		value := int(exitCode.Int64)
		session.ExitCode = &value
	}
	if lastError.Valid {
		value := lastError.String
		session.Error = &value
	}
	session.CreatedAt = fromUnixMillis(createdAt)
	session.UpdatedAt = fromUnixMillis(updatedAt)
	return session, nil
}
