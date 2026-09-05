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
       s.status, s.generation, s.cols, s.rows, s.created_at, s.updated_at,
       s.last_attached_at, s.exit_code, s.last_error
FROM sessions s
LEFT JOIN hosts h ON h.id = s.host_id`

func (s *Store) CreateSession(ctx context.Context, session Session) (Session, error) {
	applySessionDefaults(&session)
	if err := validateSession(session); err != nil {
		return Session{}, err
	}
	if session.ID == "" {
		id, err := NewID("")
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
    backend_name, status, generation, cols, rows, created_at, updated_at,
    last_attached_at, exit_code, last_error
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
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
		session.Generation,
		session.Cols,
		session.Rows,
		unixMillis(session.CreatedAt),
		unixMillis(session.UpdatedAt),
		nullableMillis(session.LastAttachedAt),
		nullableInt(session.ExitCode),
		nullableString(session.Error),
	)
	if err != nil {
		return Session{}, fmt.Errorf("store: create session: %w", err)
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
		return Session{}, fmt.Errorf("store: get session: %w", err)
	}
	return session, nil
}

func (s *Store) ListSessions(ctx context.Context) ([]Session, error) {
	return s.listSessions(ctx, sessionSelect+" ORDER BY s.updated_at DESC, s.id", nil)
}

func (s *Store) listSessions(ctx context.Context, query string, args []any) ([]Session, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list sessions: %w", err)
	}
	defer rows.Close()
	sessions := make([]Session, 0)
	for rows.Next() {
		session, scanErr := scanSession(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("store: scan session: %w", scanErr)
		}
		sessions = append(sessions, session)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate sessions: %w", err)
	}
	return sessions, nil
}

func (s *Store) DeleteSession(ctx context.Context, id string) error {
	return s.execAffecting(ctx, "delete session", "DELETE FROM sessions WHERE id = ?", id)
}

// UpdateSessionName renames a session and returns the joined row.
func (s *Store) UpdateSessionName(ctx context.Context, id, name string) (Session, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Session{}, fmt.Errorf("%w: session name is empty", ErrInvalidInput)
	}
	result, err := s.db.ExecContext(ctx, `
UPDATE sessions SET name = ?, updated_at = ?
WHERE id = ? AND name IS NOT ?`, name, unixMillis(s.utcNow()), id, name)
	if err != nil {
		return Session{}, fmt.Errorf("store: update session name: %w", err)
	}
	if err := s.requireSession(ctx, id, result); err != nil {
		return Session{}, err
	}
	return s.GetSession(ctx, id)
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
		return fmt.Errorf("store: update session size: %w", err)
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
		return fmt.Errorf("store: touch session: %w", err)
	}
	return s.requireSession(ctx, id, result)
}

// UpdateSessionRuntime applies a terminal callback, ignoring an empty backend
// field and any superseded generation.
func (s *Store) UpdateSessionRuntime(ctx context.Context, id string, generation int, status, backend, backendName string, sessionError *string) error {
	if !validSessionStatus(status) {
		return fmt.Errorf("%w: unsupported session status %q", ErrInvalidInput, status)
	}
	result, err := s.db.ExecContext(ctx, `
UPDATE sessions
SET status = ?,
    backend = CASE WHEN ? = '' THEN backend ELSE ? END,
    backend_name = CASE WHEN ? = '' THEN backend_name ELSE ? END,
    last_error = ?
WHERE id = ? AND generation = ? AND (
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
		generation,
		status,
		backend, backend,
		backend, backendName,
		nullableString(sessionError),
	)
	if err != nil {
		return fmt.Errorf("store: update session runtime: %w", err)
	}
	return s.requireSession(ctx, id, result)
}

// BeginSessionRestart increments the generation, clears the previous exit and
// returns the row to connecting.
func (s *Store) BeginSessionRestart(ctx context.Context, id string) (int, error) {
	var generation int
	err := s.db.QueryRowContext(ctx, `
UPDATE sessions
SET generation = generation + 1, status = ?, exit_code = NULL, last_error = NULL
WHERE id = ?
RETURNING generation`, SessionStatusConnecting, id).Scan(&generation)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("store: begin session restart: %w", err)
	}
	return generation, nil
}

// requireSession accepts a statement that changed nothing as long as the row
// still exists: an update that is already applied, or one superseded by a newer
// generation, is not an error.
func (s *Store) requireSession(ctx context.Context, id string, result sql.Result) error {
	if rowsChanged(result) {
		return nil
	}
	var exists int
	err := s.db.QueryRowContext(ctx, "SELECT 1 FROM sessions WHERE id = ?", id).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("store: check session existence: %w", err)
	}
	return nil
}

func applySessionDefaults(session *Session) {
	if session.Persistence == "" {
		session.Persistence = SessionPersistenceAuto
	}
	if session.Generation <= 0 {
		session.Generation = 1
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

// validateSession covers only the host/kind agreement, which no single-column
// CHECK can express. The sessions table already constrains kind, persistence,
// status and the terminal dimensions, and the API layer owns the user-facing
// messages.
func validateSession(session Session) error {
	switch session.Kind {
	case SessionKindLocal:
		if session.HostID != nil {
			return fmt.Errorf("%w: local session cannot reference a host", ErrInvalidInput)
		}
	case SessionKindSSH:
		if session.HostID == nil || strings.TrimSpace(*session.HostID) == "" {
			return fmt.Errorf("%w: SSH session requires a host", ErrInvalidInput)
		}
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
		&session.Generation,
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
