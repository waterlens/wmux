package store

import "time"

const (
	HostAuthPassword = "password"
	HostAuthKey      = "privateKey"
	HostAuthAgent    = "agent"

	SessionKindLocal = "local"
	SessionKindSSH   = "ssh"

	SessionPersistenceAuto   = "auto"
	SessionPersistenceTmux   = "tmux"
	SessionPersistenceScreen = "screen"
	SessionPersistenceNone   = "none"

	SessionStatusConnecting   = "connecting"
	SessionStatusRunning      = "running"
	SessionStatusReconnecting = "reconnecting"
	SessionStatusDetached     = "detached"
	SessionStatusExited       = "exited"
	SessionStatusError        = "error"
)

// Credentials is the JSON payload encrypted into Host.EncryptedCredentials.
// Its tags are a real serialization contract; the other models here are
// persistence types only, and the API layer owns the wire shapes.
type Credentials struct {
	Password   string `json:"password,omitempty"`
	PrivateKey string `json:"privateKey,omitempty"`
	Passphrase string `json:"passphrase,omitempty"`
}

type User struct {
	Username     string
	PasswordHash string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// AuthSession stores only a SHA-256 token digest, never the bearer token.
type AuthSession struct {
	ID         string
	TokenHash  []byte
	CreatedAt  time.Time
	ExpiresAt  time.Time
	LastSeenAt time.Time
}

type Host struct {
	ID                   string
	Name                 string
	Address              string
	Port                 int
	Username             string
	AuthType             string
	EncryptedCredentials []byte
	Fingerprint          string
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

type Session struct {
	ID          string
	Name        string
	Kind        string
	HostID      *string
	HostName    *string
	Cwd         string
	Command     string
	Persistence string
	// Backend is the persistence kind the runtime actually resolved to
	// ("tmux", "screen" or "none"), not the name of a multiplexer session.
	Backend        string
	Status         string
	Generation     int
	Cols           int
	Rows           int
	CreatedAt      time.Time
	UpdatedAt      time.Time
	LastAttachedAt *time.Time
	Error          *string
}
