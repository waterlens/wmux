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
		id, err := newID()
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
		return Host{}, fmt.Errorf("create host: %w", err)
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
		return Host{}, fmt.Errorf("get host: %w", err)
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
		return nil, fmt.Errorf("list hosts: %w", err)
	}
	defer rows.Close()
	hosts := make([]Host, 0)
	for rows.Next() {
		host, scanErr := scanHost(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("scan host: %w", scanErr)
		}
		hosts = append(hosts, host)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate hosts: %w", err)
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
	result, err := s.db.ExecContext(ctx, `
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
		return Host{}, fmt.Errorf("update host: %w", err)
	}
	updated, err := rowsChanged(result)
	if err != nil {
		return Host{}, fmt.Errorf("check host update: %w", err)
	}
	if !updated {
		return Host{}, ErrNotFound
	}
	return s.GetHost(ctx, host.ID)
}

// UpdateHostFingerprint records a confirmed SSH host key and nothing else.
func (s *Store) UpdateHostFingerprint(ctx context.Context, id, fingerprint string) error {
	result, err := s.db.ExecContext(ctx, `
UPDATE hosts SET fingerprint = ?, updated_at = ? WHERE id = ?`,
		fingerprint, unixMillis(s.utcNow()), id)
	if err != nil {
		return fmt.Errorf("update host fingerprint: %w", err)
	}
	updated, err := rowsChanged(result)
	if err != nil {
		return fmt.Errorf("check host fingerprint update: %w", err)
	}
	if !updated {
		return ErrNotFound
	}
	return nil
}

func (s *Store) DeleteHost(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, "DELETE FROM hosts WHERE id = ?", id)
	if err != nil {
		var sqliteErr *sqliteDriver.Error
		if errors.As(err, &sqliteErr) && sqliteErr.Code()&0xff == sqliteConstraint {
			return fmt.Errorf("%w: host still has sessions", ErrInUse)
		}
		return fmt.Errorf("delete host: %w", err)
	}
	deleted, err := rowsChanged(result)
	if err != nil {
		return fmt.Errorf("check host deletion: %w", err)
	}
	if !deleted {
		return ErrNotFound
	}
	return nil
}

const sqliteConstraint = 19

func validateHost(host Host) error {
	if strings.TrimSpace(host.Name) == "" {
		return fmt.Errorf("%w: host name is empty", ErrInvalidInput)
	}
	if strings.TrimSpace(host.Address) == "" {
		return fmt.Errorf("%w: host address is empty", ErrInvalidInput)
	}
	if host.Port < 1 || host.Port > 65535 {
		return fmt.Errorf("%w: host port must be between 1 and 65535", ErrInvalidInput)
	}
	if strings.TrimSpace(host.Username) == "" {
		return fmt.Errorf("%w: host username is empty", ErrInvalidInput)
	}
	switch host.AuthType {
	case HostAuthPassword, HostAuthKey, HostAuthAgent:
	default:
		return fmt.Errorf("%w: unsupported host auth type %q", ErrInvalidInput, host.AuthType)
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
