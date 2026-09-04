package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// IsSetup reports whether the single wmux owner has been created.
func (s *Store) IsSetup(ctx context.Context) (bool, error) {
	var exists int
	err := s.db.QueryRowContext(ctx, "SELECT 1 FROM users WHERE id = 1").Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check setup state: %w", err)
	}
	return true, nil
}

// Setup creates the single owner account. Exactly one concurrent caller can
// succeed; all later calls receive ErrAlreadySetup.
func (s *Store) Setup(ctx context.Context, username, passwordHash string) error {
	username = strings.TrimSpace(username)
	if username == "" {
		return fmt.Errorf("%w: username is empty", ErrInvalidInput)
	}
	if strings.TrimSpace(passwordHash) == "" {
		return fmt.Errorf("%w: password hash is empty", ErrInvalidInput)
	}
	now := unixMillis(s.utcNow())
	result, err := s.db.ExecContext(ctx, `
INSERT INTO users(id, username, password_hash, created_at, updated_at)
VALUES (1, ?, ?, ?, ?)
ON CONFLICT(id) DO NOTHING`, username, passwordHash, now, now)
	if err != nil {
		return fmt.Errorf("set up owner: %w", err)
	}
	created, err := rowsChanged(result)
	if err != nil {
		return fmt.Errorf("check setup result: %w", err)
	}
	if !created {
		return ErrAlreadySetup
	}
	return nil
}

func (s *Store) GetUser(ctx context.Context) (User, error) {
	var user User
	var createdAt, updatedAt int64
	err := s.db.QueryRowContext(ctx, `
SELECT username, password_hash, created_at, updated_at
FROM users WHERE id = 1`).Scan(&user.Username, &user.PasswordHash, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("get owner: %w", err)
	}
	user.CreatedAt = fromUnixMillis(createdAt)
	user.UpdatedAt = fromUnixMillis(updatedAt)
	return user, nil
}

func (s *Store) GetUserByUsername(ctx context.Context, username string) (User, error) {
	var user User
	var createdAt, updatedAt int64
	err := s.db.QueryRowContext(ctx, `
SELECT username, password_hash, created_at, updated_at
FROM users WHERE id = 1 AND username = ?`, strings.TrimSpace(username)).Scan(
		&user.Username,
		&user.PasswordHash,
		&createdAt,
		&updatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("get owner by username: %w", err)
	}
	user.CreatedAt = fromUnixMillis(createdAt)
	user.UpdatedAt = fromUnixMillis(updatedAt)
	return user, nil
}

func (s *Store) UpdatePassword(ctx context.Context, passwordHash string) error {
	if strings.TrimSpace(passwordHash) == "" {
		return fmt.Errorf("%w: password hash is empty", ErrInvalidInput)
	}
	result, err := s.db.ExecContext(ctx, `
UPDATE users SET password_hash = ?, updated_at = ? WHERE id = 1`,
		passwordHash, unixMillis(s.utcNow()))
	if err != nil {
		return fmt.Errorf("update owner password: %w", err)
	}
	updated, err := rowsChanged(result)
	if err != nil {
		return fmt.Errorf("check password update: %w", err)
	}
	if !updated {
		return ErrNotFound
	}
	return nil
}
