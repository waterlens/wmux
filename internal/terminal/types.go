// Package terminal owns long-lived PTY and SSH terminal sessions. It has no
// transport concerns: HTTP/WebSocket adapters attach clients and forward the
// OutputFrame stream exposed here.
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
)

// Credential deliberately exposes no secret serialization contract. Session
// repositories persist a HostID and resolve the HostSpec (and credential) when
// restoring, so private key material never needs to enter a session record.
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

	Shell string
	Args  []string
	Cwd   string
	Env   map[string]string
	Cols  uint16
	Rows  uint16
}

// SessionRecord is safe to persist: it references a remote host but contains
// no Credential. Active indicates that wmux should reconnect to a persistent
// backend after a service restart.
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
	CreatedAt           time.Time
}

// Repository is implemented by the application's storage layer. LoadHost is
// intentionally separate from ListSessions so credential handling stays in the
// host repository instead of being serialized into SessionRecord.
type Repository interface {
	SaveSession(ctx context.Context, record SessionRecord) error
	ListSessions(ctx context.Context) ([]SessionRecord, error)
	LoadHost(ctx context.Context, hostID string) (HostSpec, error)
}

type SessionStatus struct {
	ID           string
	State        SessionState
	Persistence  Persistence
	WriterID     string
	Clients      int
	LastError    string
	LastSequence uint64
}

// Callbacks run after runtime locks are released. Implementations should return
// promptly; durable state belongs in Repository.
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
