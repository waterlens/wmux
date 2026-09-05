package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	sqliteDriver "modernc.org/sqlite"
)

func (s *Store) CreateHost(ctx context.Context, host Host) (Host, error) {
	if err := validateHost(host); err != nil {
		return Host{}, err
	}
	if host.ID == "" {
		id, err := NewID("")
		if err != nil {
			return Host{}, err
		}
		host.ID = id
	}
	now := s.utcNow()
	host.CreatedAt = now
	host.UpdatedAt = now
	host.EncryptedCredentials = append([]byte(nil), host.EncryptedCredentials...)
	_, err := s.db.ExecContext(ctx, `
INSERT INTO hosts(
    id, name, address, port, username, auth_type,
    encrypted_credentials, fingerprint, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		host.ID,
		host.Name,
		host.Address,
		host.Port,
		host.Username,
		host.AuthType,
		nullableBytes(host.EncryptedCredentials),
		host.Fingerprint,
		unixMillis(host.CreatedAt),
		unixMillis(host.UpdatedAt),
	)
	if err != nil {
		return Host{}, fmt.Errorf("store: create host: %w", err)
	}
	return host, nil
}

func (s *Store) GetHost(ctx context.Context, id string) (Host, error) {
	if strings.TrimSpace(id) == "" {
		return Host{}, ErrNotFound
	}
	host, err := scanHost(s.db.QueryRowContext(ctx, `
SELECT id, name, address, port, username, auth_type,
       encrypted_credentials, fingerprint, created_at, updated_at
FROM hosts WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Host{}, ErrNotFound
	}
	if err != nil {
		return Host{}, fmt.Errorf("store: get host: %w", err)
	}
	return host, nil
}

func (s *Store) ListHosts(ctx context.Context) ([]Host, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, name, address, port, username, auth_type,
       encrypted_credentials, fingerprint, created_at, updated_at
FROM hosts
ORDER BY name COLLATE NOCASE, id`)
	if err != nil {
		return nil, fmt.Errorf("store: list hosts: %w", err)
	}
	defer rows.Close()
	hosts := make([]Host, 0)
	for rows.Next() {
		host, scanErr := scanHost(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("store: scan host: %w", scanErr)
		}
		hosts = append(hosts, host)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate hosts: %w", err)
	}
	return hosts, nil
}

// UpdateHost replaces every mutable host field and ignores CreatedAt.
func (s *Store) UpdateHost(ctx context.Context, host Host) (Host, error) {
	if strings.TrimSpace(host.ID) == "" {
		return Host{}, fmt.Errorf("%w: host id is empty", ErrInvalidInput)
	}
	if err := validateHost(host); err != nil {
		return Host{}, err
	}
	err := s.execAffecting(ctx, "update host", `
UPDATE hosts
SET name = ?, address = ?, port = ?, username = ?, auth_type = ?,
    encrypted_credentials = ?, fingerprint = ?, updated_at = ?
WHERE id = ?`,
		host.Name,
		host.Address,
		host.Port,
		host.Username,
		host.AuthType,
		nullableBytes(host.EncryptedCredentials),
		host.Fingerprint,
		unixMillis(s.utcNow()),
		host.ID,
	)
	if err != nil {
		return Host{}, err
	}
	return s.GetHost(ctx, host.ID)
}

// UpdateHostFingerprint records a confirmed SSH host key and nothing else.
func (s *Store) UpdateHostFingerprint(ctx context.Context, id, fingerprint string) error {
	return s.execAffecting(ctx, "update host fingerprint", `
UPDATE hosts SET fingerprint = ?, updated_at = ? WHERE id = ?`,
		fingerprint, unixMillis(s.utcNow()), id)
}

func (s *Store) DeleteHost(ctx context.Context, id string) error {
	err := s.execAffecting(ctx, "delete host", "DELETE FROM hosts WHERE id = ?", id)
	var sqliteErr *sqliteDriver.Error
	if errors.As(err, &sqliteErr) && sqliteErr.Code()&0xff == sqliteConstraint {
		return fmt.Errorf("%w: host still has sessions", ErrInUse)
	}
	return err
}

const sqliteConstraint = 19

// validateHost covers only what the hosts table cannot: its CHECK constraints
// already reject an out-of-range port and an unknown auth type, and the API
// layer owns the user-facing messages.
func validateHost(host Host) error {
	if strings.TrimSpace(host.Name) == "" {
		return fmt.Errorf("%w: host name is empty", ErrInvalidInput)
	}
	if strings.TrimSpace(host.Address) == "" {
		return fmt.Errorf("%w: host address is empty", ErrInvalidInput)
	}
	if strings.TrimSpace(host.Username) == "" {
		return fmt.Errorf("%w: host username is empty", ErrInvalidInput)
	}
	return nil
}

type scanner interface {
	Scan(dest ...any) error
}

func scanHost(row scanner) (Host, error) {
	var host Host
	var credentials []byte
	var createdAt, updatedAt int64
	err := row.Scan(
		&host.ID,
		&host.Name,
		&host.Address,
		&host.Port,
		&host.Username,
		&host.AuthType,
		&credentials,
		&host.Fingerprint,
		&createdAt,
		&updatedAt,
	)
	if err != nil {
		return Host{}, err
	}
	host.EncryptedCredentials = append([]byte(nil), credentials...)
	host.CreatedAt = fromUnixMillis(createdAt)
	host.UpdatedAt = fromUnixMillis(updatedAt)
	return host, nil
}

func nullableBytes(value []byte) any {
	if len(value) == 0 {
		return nil
	}
	return value
}
