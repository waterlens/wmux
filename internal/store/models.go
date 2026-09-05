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
// It supports password, private-key and encrypted-private-key SSH auth. Empty
// fields are omitted from the plaintext JSON before encryption.
type Credentials struct {
	Password   string `json:"password,omitempty"`
	PrivateKey string `json:"privateKey,omitempty"`
	Passphrase string `json:"passphrase,omitempty"`
}

type User struct {
	Username     string    `json:"username"`
	PasswordHash string    `json:"-"`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

// AuthSession stores only a SHA-256 token digest. The bearer token must never
// be assigned to this type or persisted.
type AuthSession struct {
	ID         string    `json:"id"`
	TokenHash  []byte    `json:"-"`
	CreatedAt  time.Time `json:"createdAt"`
	ExpiresAt  time.Time `json:"expiresAt"`
	LastSeenAt time.Time `json:"lastSeenAt"`
}

type Host struct {
	ID                   string    `json:"id"`
	Name                 string    `json:"name"`
	Address              string    `json:"address"`
	Port                 int       `json:"port"`
	Username             string    `json:"username"`
	AuthType             string    `json:"authType"`
	EncryptedCredentials []byte    `json:"-"`
	Fingerprint          string    `json:"fingerprint,omitempty"`
	CreatedAt            time.Time `json:"createdAt"`
	UpdatedAt            time.Time `json:"updatedAt"`
}

type Session struct {
	ID             string     `json:"id"`
	Name           string     `json:"name"`
	Kind           string     `json:"kind"`
	HostID         *string    `json:"hostId,omitempty"`
	HostName       *string    `json:"hostName,omitempty"`
	Cwd            string     `json:"cwd,omitempty"`
	Command        string     `json:"command,omitempty"`
	Persistence    string     `json:"persistence"`
	Backend        string     `json:"backend,omitempty"`
	BackendName    string     `json:"backendName,omitempty"`
	Status         string     `json:"status"`
	Generation     int        `json:"generation"`
	Cols           int        `json:"cols"`
	Rows           int        `json:"rows"`
	CreatedAt      time.Time  `json:"createdAt"`
	UpdatedAt      time.Time  `json:"updatedAt"`
	LastAttachedAt *time.Time `json:"lastAttachedAt,omitempty"`
	ExitCode       *int       `json:"exitCode,omitempty"`
	Error          *string    `json:"error,omitempty"`
}
