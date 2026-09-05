// Package terminal owns long-lived PTY and SSH terminal sessions.
package terminal

import (
	"context"
	"errors"
	"time"

	"github.com/waterlens/wmux/internal/transcript"
)

type Persistence string

const (
	PersistenceAuto   Persistence = "auto"
	PersistenceTmux   Persistence = "tmux"
	PersistenceScreen Persistence = "screen"
	PersistenceNone   Persistence = "none"
)

type SessionState string

const (
	StateConnecting   SessionState = "connecting"
	StateRunning      SessionState = "running"
	StateDisconnected SessionState = "disconnected"
	StateError        SessionState = "error"
	StateExited       SessionState = "exited"
	StateTerminating  SessionState = "terminating"
	StateTerminated   SessionState = "terminated"
)

type AttachmentCloseReason string

const (
	AttachmentExited         AttachmentCloseReason = "exited"
	AttachmentRestarted      AttachmentCloseReason = "restarted"
	AttachmentServerShutdown AttachmentCloseReason = "server_shutdown"
	AttachmentEvicted        AttachmentCloseReason = "evicted"
	AttachmentClientClosed   AttachmentCloseReason = "client_closed"
)

var (
	ErrClosed           = errors.New("terminal: manager is closed")
	ErrSessionNotFound  = errors.New("terminal: session not found")
	ErrSessionExists    = errors.New("terminal: session already exists")
	ErrClientExists     = errors.New("terminal: client is already attached")
	ErrNotWriter        = errors.New("terminal: client does not hold the write lease")
	ErrAttachmentClosed = errors.New("terminal: attachment is closed")
	ErrUnavailable      = errors.New("terminal: backend is unavailable")

	// ErrBackendMissing reports that the tmux/screen session is gone.
	ErrBackendMissing = errors.New("terminal: backend session no longer exists")
)

// Credential exposes no serialization contract, so secrets stay out of records.
type Credential interface {
	isCredential()
}

type PasswordCredential struct {
	Password string
}

func (PasswordCredential) isCredential() {}

type PrivateKeyCredential struct {
	PEM        []byte
	Passphrase []byte
}

func (PrivateKeyCredential) isCredential() {}

type AgentCredential struct {
	// Socket defaults to SSH_AUTH_SOCK when empty.
	Socket string
}

func (AgentCredential) isCredential() {}

type HostSpec struct {
	ID          string
	Address     string
	User        string
	Fingerprint string
	Credential  Credential

	ConnectTimeout    time.Duration
	KeepAliveInterval time.Duration
}

type SessionSpec struct {
	ID          string
	Name        string
	Host        *HostSpec // nil means a PTY on the wmux host.
	Persistence Persistence

	// Generation is the execution number owned by the application's session row.
	Generation int

	Shell string
	Args  []string
	Cwd   string
	Env   map[string]string
	Cols  uint16
	Rows  uint16
}

// SessionRecord is safe to persist: it references a host but holds no Credential.
type SessionRecord struct {
	ID                  string
	Name                string
	HostID              string
	Persistence         Persistence
	ResolvedPersistence Persistence
	Shell               string
	Args                []string
	Cwd                 string
	Env                 map[string]string
	Cols                uint16
	Rows                uint16
	Active              bool
	Generation          int
}

// Repository is implemented by the application's storage layer.
type Repository interface {
	ListSessions(ctx context.Context) ([]SessionRecord, error)
	LoadHost(ctx context.Context, hostID string) (HostSpec, error)
}

type SessionStatus struct {
	ID           string
	Generation   int
	State        SessionState
	Persistence  Persistence
	WriterID     string
	Clients      int
	LastError    string
	LastSequence uint64
}

// Callbacks run after runtime locks are released and should return promptly.
type Callbacks interface {
	OnSessionState(status SessionStatus)
	OnWriterChanged(sessionID, clientID string)
	OnClientDropped(sessionID, clientID, reason string)
}

type NopCallbacks struct{}

func (NopCallbacks) OnSessionState(SessionStatus)           {}
func (NopCallbacks) OnWriterChanged(string, string)         {}
func (NopCallbacks) OnClientDropped(string, string, string) {}

type OutputFrame struct {
	Sequence uint64    `json:"sequence"`
	Time     time.Time `json:"time"`
	Data     []byte    `json:"data"`
}

type Config struct {
	Repository  Repository
	Callbacks   Callbacks
	Transcripts transcript.Factory

	ClientBuffer    int
	ReplayLimit     int
	ReconnectMin    time.Duration
	ReconnectMax    time.Duration
	ShutdownTimeout time.Duration

	TmuxPath      string
	ScreenPath    string
	MuxName       string
	MuxRuntimeDir string

	launcher backendLauncher
}
