// Package app wires product storage to the terminal runtime.
package app

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/waterlens/wmux/internal/security"
	"github.com/waterlens/wmux/internal/store"
	"github.com/waterlens/wmux/internal/terminal"
)

// RuntimeRepository adapts SQLite sessions and encrypted hosts to terminal's
// secret-free persistence interface.
type RuntimeRepository struct {
	Store     *store.Store
	MasterKey []byte
	Logger    *slog.Logger

	stateMu sync.Mutex
	states  map[string]terminal.SessionState
}

// ListSessions returns every product session, active or exited.
func (r *RuntimeRepository) ListSessions(ctx context.Context) ([]terminal.SessionRecord, error) {
	sessions, err := r.Store.ListSessions(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]terminal.SessionRecord, 0, len(sessions))
	for _, session := range sessions {
		record := terminal.SessionRecord{
			Spec:                launchSpec(session),
			ResolvedPersistence: terminal.Persistence(session.Backend),
			Active:              session.Status == store.SessionStatusConnecting || session.Status == store.SessionStatusRunning || session.Status == store.SessionStatusReconnecting || session.Status == store.SessionStatusDetached,
		}
		if session.HostID != nil {
			record.HostID = *session.HostID
		}
		result = append(result, record)
	}
	return result, nil
}

// launchSpec maps a persisted session row to a host-free launch spec.
func launchSpec(session store.Session) terminal.SessionSpec {
	spec := terminal.SessionSpec{
		ID:          session.ID,
		Persistence: terminal.Persistence(session.Persistence),
		Generation:  session.Generation,
		Cwd:         session.Cwd,
		Env:         sessionEnvironment(session.ID),
		Cols:        uint16(session.Cols),
		Rows:        uint16(session.Rows),
	}
	if session.Command != "" {
		spec.Shell = "/bin/sh"
		spec.Args = []string{"-lc", session.Command}
	}
	return spec
}

// LoadHost decrypts credentials only when terminal needs an SSH connection.
func (r *RuntimeRepository) LoadHost(ctx context.Context, id string) (terminal.HostSpec, error) {
	host, err := r.Store.GetHost(ctx, id)
	if err != nil {
		return terminal.HostSpec{}, err
	}
	var credentials store.Credentials
	if len(host.EncryptedCredentials) != 0 {
		if err := security.DecryptJSON(r.MasterKey, host.EncryptedCredentials, &credentials); err != nil {
			return terminal.HostSpec{}, err
		}
	}
	credential, err := TerminalCredential(host.AuthType, credentials)
	if err != nil {
		return terminal.HostSpec{}, err
	}
	return terminal.HostSpec{
		ID:                host.ID,
		Address:           SSHAddress(host),
		User:              host.Username,
		Fingerprint:       host.Fingerprint,
		Credential:        credential,
		ConnectTimeout:    10 * time.Second,
		KeepAliveInterval: 15 * time.Second,
	}, nil
}

// OnSessionState persists lifecycle changes emitted by terminal.Manager.
func (r *RuntimeRepository) OnSessionState(status terminal.SessionStatus) {
	state := store.SessionStatusError
	switch status.State {
	case terminal.StateConnecting:
		state = store.SessionStatusConnecting
	case terminal.StateRunning:
		state = store.SessionStatusRunning
	case terminal.StateDisconnected:
		state = store.SessionStatusReconnecting
	case terminal.StateExited, terminal.StateTerminated:
		state = store.SessionStatusExited
	case terminal.StateTerminating:
		state = store.SessionStatusDetached
	}
	var sessionError *string
	if status.LastError != "" {
		value := status.LastError
		sessionError = &value
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	backend := ""
	backendName := ""
	if status.Persistence != "" && status.Persistence != terminal.PersistenceAuto {
		backend = string(status.Persistence)
		backendName = terminal.MuxSessionName(status.ID)
	}
	if err := r.Store.UpdateSessionRuntime(ctx, status.ID, status.Generation, state, backend, backendName, sessionError); err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			r.logError("persist terminal runtime", err, "session", status.ID)
		}
		return
	}
	r.logStateChange(status)
}

// OnClientDropped logs slow-consumer diagnostics without terminal output.
func (r *RuntimeRepository) OnClientDropped(sessionID, clientID, reason string) {
	if r.Logger != nil {
		r.Logger.Warn("terminal client dropped", "session", sessionID, "client", clientID, "reason", reason)
	}
}

// SessionSpec builds a launch spec from a persisted product session.
func (r *RuntimeRepository) SessionSpec(ctx context.Context, session store.Session) (terminal.SessionSpec, error) {
	spec := launchSpec(session)
	if session.HostID != nil {
		host, err := r.LoadHost(ctx, *session.HostID)
		if err != nil {
			return terminal.SessionSpec{}, err
		}
		spec.Host = &host
	}
	return spec, nil
}

// sessionEnvironment is the per-session environment every launch shares.
func sessionEnvironment(id string) map[string]string {
	return map[string]string{"WMUX_SESSION_ID": id, "COLORTERM": "truecolor"}
}

// SSHAddress is the single place that turns a stored host into a dial address.
func SSHAddress(host store.Host) string {
	return net.JoinHostPort(strings.Trim(host.Address, "[]"), strconv.Itoa(host.Port))
}

// TerminalCredential maps a host's stored authentication type and decrypted
// secrets onto the credential the SSH layers accept.
func TerminalCredential(authType string, credentials store.Credentials) (terminal.Credential, error) {
	switch authType {
	case store.HostAuthPassword:
		return terminal.PasswordCredential{Password: credentials.Password}, nil
	case store.HostAuthKey:
		return terminal.PrivateKeyCredential{PEM: []byte(credentials.PrivateKey), Passphrase: []byte(credentials.Passphrase)}, nil
	case store.HostAuthAgent:
		return terminal.AgentCredential{}, nil
	default:
		return nil, errors.New("unsupported SSH authentication type")
	}
}

func (r *RuntimeRepository) logError(message string, err error, args ...any) {
	if r.Logger == nil {
		return
	}
	args = append(args, "error", err)
	r.Logger.Error(message, args...)
}

func (r *RuntimeRepository) logStateChange(status terminal.SessionStatus) {
	if r.Logger == nil {
		return
	}
	r.stateMu.Lock()
	if r.states == nil {
		r.states = make(map[string]terminal.SessionState)
	}
	previous := r.states[status.ID]
	if previous == status.State {
		r.stateMu.Unlock()
		return
	}
	r.states[status.ID] = status.State
	r.stateMu.Unlock()

	args := []any{
		"session", status.ID,
		"state", status.State,
		"backend", status.Persistence,
		"clients", status.Clients,
	}
	if previous != "" {
		args = append(args, "previous_state", previous)
	}
	if status.LastError != "" {
		args = append(args, "error", status.LastError)
	}
	if status.State == terminal.StateDisconnected || status.State == terminal.StateError {
		r.Logger.Warn("terminal session state changed", args...)
		return
	}
	r.Logger.Info("terminal session state changed", args...)
}
