package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

const authTokenHashSize = 32

func (s *Store) CreateAuthSession(ctx context.Context, tokenHash []byte, expiresAt time.Time) (AuthSession, error) {
	id, err := newID()
	if err != nil {
		return AuthSession{}, err
	}
	return s.PutAuthSession(ctx, AuthSession{ID: id, TokenHash: tokenHash, ExpiresAt: expiresAt})
}

// PutAuthSession persists a preconstructed session, generating missing ID and
// timestamps. It is primarily useful for imports and deterministic tests.
func (s *Store) PutAuthSession(ctx context.Context, auth AuthSession) (AuthSession, error) {
	if len(auth.TokenHash) != authTokenHashSize {
		return AuthSession{}, fmt.Errorf("%w: token hash must be 32 bytes", ErrInvalidInput)
	}
	if auth.ID == "" {
		id, err := newID()
		if err != nil {
			return AuthSession{}, err
		}
		auth.ID = id
	}
	now := s.utcNow()
	if auth.CreatedAt.IsZero() {
		auth.CreatedAt = now
	} else {
		auth.CreatedAt = auth.CreatedAt.UTC().Truncate(time.Millisecond)
	}
	if auth.LastSeenAt.IsZero() {
		auth.LastSeenAt = auth.CreatedAt
	} else {
		auth.LastSeenAt = auth.LastSeenAt.UTC().Truncate(time.Millisecond)
	}
	auth.ExpiresAt = auth.ExpiresAt.UTC().Truncate(time.Millisecond)
	if !auth.ExpiresAt.After(auth.CreatedAt) {
		return AuthSession{}, fmt.Errorf("%w: auth session expiry must follow creation", ErrInvalidInput)
	}
	auth.TokenHash = append([]byte(nil), auth.TokenHash...)
	_, err := s.db.ExecContext(ctx, `
INSERT INTO auth_sessions(id, token_hash, created_at, expires_at, last_seen_at)
VALUES (?, ?, ?, ?, ?)`,
		auth.ID,
		auth.TokenHash,
		unixMillis(auth.CreatedAt),
		unixMillis(auth.ExpiresAt),
		unixMillis(auth.LastSeenAt),
	)
	if err != nil {
		return AuthSession{}, fmt.Errorf("create auth session: %w", err)
	}
	return auth, nil
}

// GetAuthSession returns only a currently valid session. Expired sessions are
// indistinguishable from unknown tokens to callers.
func (s *Store) GetAuthSession(ctx context.Context, tokenHash []byte) (AuthSession, error) {
	if len(tokenHash) != authTokenHashSize {
		return AuthSession{}, ErrNotFound
	}
	var auth AuthSession
	var createdAt, expiresAt, lastSeenAt int64
	err := s.db.QueryRowContext(ctx, `
SELECT id, token_hash, created_at, expires_at, last_seen_at
FROM auth_sessions
WHERE token_hash = ? AND expires_at > ?`, tokenHash, unixMillis(s.utcNow())).Scan(
		&auth.ID,
		&auth.TokenHash,
		&createdAt,
		&expiresAt,
		&lastSeenAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return AuthSession{}, ErrNotFound
	}
	if err != nil {
		return AuthSession{}, fmt.Errorf("get auth session: %w", err)
	}
	auth.CreatedAt = fromUnixMillis(createdAt)
	auth.ExpiresAt = fromUnixMillis(expiresAt)
	auth.LastSeenAt = fromUnixMillis(lastSeenAt)
	return auth, nil
}

func (s *Store) TouchAuthSession(ctx context.Context, tokenHash []byte) error {
	if len(tokenHash) != authTokenHashSize {
		return ErrNotFound
	}
	now := unixMillis(s.utcNow())
	result, err := s.db.ExecContext(ctx, `
UPDATE auth_sessions
SET last_seen_at = ?
WHERE token_hash = ? AND expires_at > ?`, now, tokenHash, now)
	if err != nil {
		return fmt.Errorf("touch auth session: %w", err)
	}
	updated, err := rowsChanged(result)
	if err != nil {
		return fmt.Errorf("check auth session touch: %w", err)
	}
	if !updated {
		return ErrNotFound
	}
	return nil
}

func (s *Store) DeleteAuthSession(ctx context.Context, tokenHash []byte) error {
	result, err := s.db.ExecContext(ctx, "DELETE FROM auth_sessions WHERE token_hash = ?", tokenHash)
	if err != nil {
		return fmt.Errorf("delete auth session: %w", err)
	}
	deleted, err := rowsChanged(result)
	if err != nil {
		return fmt.Errorf("check auth session deletion: %w", err)
	}
	if !deleted {
		return ErrNotFound
	}
	return nil
}

func (s *Store) DeleteAllAuthSessions(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, "DELETE FROM auth_sessions"); err != nil {
		return fmt.Errorf("delete all auth sessions: %w", err)
	}
	return nil
}

func (s *Store) PurgeExpiredAuthSessions(ctx context.Context) (int64, error) {
	result, err := s.db.ExecContext(ctx, "DELETE FROM auth_sessions WHERE expires_at <= ?", unixMillis(s.utcNow()))
	if err != nil {
		return 0, fmt.Errorf("purge expired auth sessions: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count purged auth sessions: %w", err)
	}
	return count, nil
}
