package terminal

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

type Manager struct {
	cfg      Config
	launcher launcher

	mu       sync.RWMutex
	sessions map[string]*runtimeSession
	closed   bool
}

func NewManager(cfg Config) (*Manager, error) {
	if cfg.Transcripts == nil {
		return nil, errors.New("terminal: transcript factory is required")
	}
	if cfg.Callbacks == nil {
		cfg.Callbacks = nopCallbacks{}
	}
	if cfg.ClientBuffer <= 0 {
		cfg.ClientBuffer = 256
	}
	if cfg.ReplayLimit <= 0 {
		cfg.ReplayLimit = 4096
	}
	if cfg.ReconnectMin <= 0 {
		cfg.ReconnectMin = 250 * time.Millisecond
	}
	if cfg.ReconnectMax <= 0 {
		cfg.ReconnectMax = 10 * time.Second
	}
	if cfg.ShutdownTimeout <= 0 {
		cfg.ShutdownTimeout = 10 * time.Second
	}
	selected := cfg.launcher
	if selected == nil {
		selected = newExecLauncher(cfg)
	}
	return &Manager{
		cfg:      cfg,
		launcher: selected,
		sessions: make(map[string]*runtimeSession),
	}, nil
}

// Create starts the runtime for a persisted session row; only this first launch
// may create the tmux/screen session. It takes no context because the runtime
// outlives the request that asked for it and owns its own cancellation; callers
// observe the session afterwards through Status.
func (m *Manager) Create(spec SessionSpec) error {
	spec = cloneSpec(spec)
	if err := validateSpec(spec); err != nil {
		return err
	}
	m.mu.RLock()
	closed := m.closed
	_, exists := m.sessions[spec.ID]
	m.mu.RUnlock()
	if closed {
		return ErrClosed
	}
	if exists {
		return ErrSessionExists
	}

	log, err := m.cfg.Transcripts.Open(spec.ID)
	if err != nil {
		return fmt.Errorf("terminal: open transcript: %w", err)
	}
	var resolved Persistence
	if spec.Persistence != PersistenceAuto {
		resolved = spec.Persistence
	}
	s := newRuntimeSession(m, spec, resolved, log, true)

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		_ = log.Close()
		return ErrClosed
	}
	if _, ok := m.sessions[spec.ID]; ok {
		m.mu.Unlock()
		_ = log.Close()
		return ErrSessionExists
	}
	m.sessions[spec.ID] = s
	m.mu.Unlock()
	s.start()
	return nil
}

// Restore attaches to the backends of active repository records; a session whose
// backend is gone becomes exited.
func (m *Manager) Restore(ctx context.Context) error {
	if m.cfg.Repository == nil {
		return nil
	}
	records, err := m.cfg.Repository.ListSessions(ctx)
	if err != nil {
		return fmt.Errorf("terminal: list sessions: %w", err)
	}
	var restoreErrors []error
	for _, record := range records {
		if err := m.restoreOne(ctx, record); err != nil {
			restoreErrors = append(restoreErrors, fmt.Errorf("session %s: %w", record.Spec.ID, err))
		}
	}
	return errors.Join(restoreErrors...)
}

func (m *Manager) restoreOne(ctx context.Context, record SessionRecord) error {
	spec := cloneSpec(record.Spec)
	if record.HostID != "" {
		host, err := m.cfg.Repository.LoadHost(ctx, record.HostID)
		if err != nil {
			return fmt.Errorf("load host: %w", err)
		}
		spec.Host = &host
	}
	if err := validateSpec(spec); err != nil {
		return err
	}
	log, err := m.cfg.Transcripts.Open(spec.ID)
	if err != nil {
		return fmt.Errorf("open transcript: %w", err)
	}
	active := record.Active
	if spec.Persistence == PersistenceNone || record.ResolvedPersistence == PersistenceNone {
		active = false
	}
	s := newRuntimeSession(m, spec, record.ResolvedPersistence, log, false)

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		_ = log.Close()
		return ErrClosed
	}
	if _, exists := m.sessions[spec.ID]; exists {
		m.mu.Unlock()
		_ = log.Close()
		return ErrSessionExists
	}
	m.sessions[spec.ID] = s
	m.mu.Unlock()
	if active {
		s.start()
	} else {
		s.markDormant()
	}
	return nil
}

func (m *Manager) Attach(ctx context.Context, sessionID, clientID string, after uint64) (*Attachment, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s, err := m.session(sessionID)
	if err != nil {
		return nil, err
	}
	return s.attach(clientID, after)
}

func (m *Manager) Status(sessionID string) (SessionStatus, error) {
	s, err := m.session(sessionID)
	if err != nil {
		return SessionStatus{}, err
	}
	return s.status(), nil
}

// RefreshHost retries every session of one host with a freshly loaded HostSpec.
func (m *Manager) RefreshHost(hostID string) int {
	m.mu.RLock()
	sessions := make([]*runtimeSession, 0, len(m.sessions))
	for _, session := range m.sessions {
		sessions = append(sessions, session)
	}
	m.mu.RUnlock()
	woken := 0
	for _, session := range sessions {
		if session.hostID() == hostID {
			session.requestRetry()
			woken++
		}
	}
	return woken
}

// Reconnect retries one session's backend connection immediately.
func (m *Manager) Reconnect(sessionID string) error {
	s, err := m.session(sessionID)
	if err != nil {
		return err
	}
	s.requestRetry()
	return nil
}

// Terminate kills the backend and removes the runtime; a failed kill keeps the
// runtime attachable and returns the error.
func (m *Manager) Terminate(ctx context.Context, sessionID string) error {
	return m.stop(ctx, sessionID, AttachmentExited)
}

// StopForRestart is Terminate with a close reason that asks browsers to reconnect.
func (m *Manager) StopForRestart(ctx context.Context, sessionID string) error {
	return m.stop(ctx, sessionID, AttachmentRestarted)
}

func (m *Manager) stop(ctx context.Context, sessionID string, reason AttachmentCloseReason) error {
	s, err := m.session(sessionID)
	if err != nil {
		return err
	}
	if err := s.stop(ctx, reason); err != nil {
		return err
	}
	m.forget(sessionID, s)
	return nil
}

// Discard forgets a runtime without contacting or killing its backend.
func (m *Manager) Discard(ctx context.Context, sessionID string) error {
	s, err := m.session(sessionID)
	if err != nil {
		return err
	}
	// Teardown leaves the runtime unusable, so drop it from the registry even
	// when releasing the backend or transcript reported an error.
	defer m.forget(sessionID, s)
	return s.discard(ctx)
}

// Close detaches all backend clients without killing tmux/screen sessions.
func (m *Manager) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), m.cfg.ShutdownTimeout)
	defer cancel()
	return m.CloseContext(ctx)
}

// CloseContext is Close bounded by ctx.
func (m *Manager) CloseContext(ctx context.Context) error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	sessions := make([]*runtimeSession, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, s)
	}
	m.mu.Unlock()

	errs := make([]error, len(sessions))
	var wg sync.WaitGroup
	for i, s := range sessions {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = s.shutdown(ctx)
		}()
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return errors.Join(errs...)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *Manager) session(id string) (*runtimeSession, error) {
	m.mu.RLock()
	s := m.sessions[id]
	closed := m.closed
	m.mu.RUnlock()
	if closed {
		return nil, ErrClosed
	}
	if s == nil {
		return nil, ErrSessionNotFound
	}
	return s, nil
}

func (m *Manager) forget(id string, s *runtimeSession) {
	m.mu.Lock()
	if m.sessions[id] == s {
		delete(m.sessions, id)
	}
	m.mu.Unlock()
}

func validateSpec(spec SessionSpec) error {
	if strings.TrimSpace(spec.ID) == "" {
		return errors.New("terminal: session ID is required")
	}
	switch spec.Persistence {
	case "", PersistenceAuto, PersistenceTmux, PersistenceScreen, PersistenceNone:
	default:
		return fmt.Errorf("terminal: unsupported persistence %q", spec.Persistence)
	}
	if spec.Host != nil && strings.TrimSpace(spec.Host.ID) == "" {
		return errors.New("terminal: remote host ID is required")
	}
	for key := range spec.Env {
		if !validEnvName.MatchString(key) {
			return fmt.Errorf("terminal: invalid environment name %q", key)
		}
	}
	return nil
}

func cloneSpec(spec SessionSpec) SessionSpec {
	spec.Args = append([]string(nil), spec.Args...)
	spec.Env = cloneMap(spec.Env)
	if spec.Persistence == "" {
		spec.Persistence = PersistenceAuto
	}
	if spec.Host != nil {
		host := *spec.Host
		if credential, ok := host.Credential.(PrivateKeyCredential); ok {
			credential.PEM = append([]byte(nil), credential.PEM...)
			credential.Passphrase = append([]byte(nil), credential.Passphrase...)
			host.Credential = credential
		}
		spec.Host = &host
	}
	return spec
}

func cloneMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	copyOfValues := make(map[string]string, len(values))
	for key, value := range values {
		copyOfValues[key] = value
	}
	return copyOfValues
}
