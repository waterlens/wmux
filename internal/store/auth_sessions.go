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
	if len(tokenHash) != authTokenHashSize {
		return AuthSession{}, fmt.Errorf("%w: token hash must be 32 bytes", ErrInvalidInput)
	}
	id, err := NewID("")
	if err != nil {
		return AuthSession{}, err
	}
	now := s.utcNow()
	auth := AuthSession{
		ID:         id,
		TokenHash:  append([]byte(nil), tokenHash...),
		CreatedAt:  now,
		ExpiresAt:  expiresAt.UTC().Truncate(time.Millisecond),
		LastSeenAt: now,
	}
	if !auth.ExpiresAt.After(auth.CreatedAt) {
		return AuthSession{}, fmt.Errorf("%w: auth session expiry must follow creation", ErrInvalidInput)
	}
	_, err = s.db.ExecContext(ctx, `
INSERT INTO auth_sessions(id, token_hash, created_at, expires_at, last_seen_at)
VALUES (?, ?, ?, ?, ?)`,
		auth.ID,
		auth.TokenHash,
		unixMillis(auth.CreatedAt),
		unixMillis(auth.ExpiresAt),
		unixMillis(auth.LastSeenAt),
	)
	if err != nil {
		return AuthSession{}, fmt.Errorf("store: create auth session: %w", err)
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
		return AuthSession{}, fmt.Errorf("store: get auth session: %w", err)
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
	return s.execAffecting(ctx, "touch auth session", `
UPDATE auth_sessions
SET last_seen_at = ?
WHERE token_hash = ? AND expires_at > ?`, now, tokenHash, now)
}

func (s *Store) DeleteAuthSession(ctx context.Context, tokenHash []byte) error {
	return s.execAffecting(ctx, "delete auth session",
		"DELETE FROM auth_sessions WHERE token_hash = ?", tokenHash)
}

func (s *Store) DeleteAllAuthSessions(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, "DELETE FROM auth_sessions"); err != nil {
		return fmt.Errorf("store: delete all auth sessions: %w", err)
	}
	return nil
}

func (s *Store) PurgeExpiredAuthSessions(ctx context.Context) (int64, error) {
	result, err := s.db.ExecContext(ctx, "DELETE FROM auth_sessions WHERE expires_at <= ?", unixMillis(s.utcNow()))
	if err != nil {
		return 0, fmt.Errorf("store: purge expired auth sessions: %w", err)
	}
	count, _ := result.RowsAffected()
	return count, nil
}
