package terminal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/waterlens/wmux/internal/transcript"
)

type Manager struct {
	cfg       Config
	launcher  backendLauncher
	callbacks Callbacks

	mu       sync.RWMutex
	sessions map[string]*runtimeSession
	closed   bool
}

func NewManager(cfg Config) (*Manager, error) {
	if cfg.Transcripts == nil {
		return nil, errors.New("terminal: transcript factory is required")
	}
	if cfg.Callbacks == nil {
		cfg.Callbacks = NopCallbacks{}
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
	selectedLauncher := cfg.launcher
	if selectedLauncher == nil {
		selectedLauncher = newLauncher(cfg)
	}
	return &Manager{
		cfg:       cfg,
		launcher:  selectedLauncher,
		callbacks: cfg.Callbacks,
		sessions:  make(map[string]*runtimeSession),
	}, nil
}

// Create starts the runtime for a persisted session row; only this first launch
// may create the tmux/screen session.
func (m *Manager) Create(ctx context.Context, spec SessionSpec) (SessionStatus, error) {
	spec = cloneSpec(spec)
	if err := validateSpec(spec); err != nil {
		return SessionStatus{}, err
	}
	m.mu.RLock()
	closed := m.closed
	_, exists := m.sessions[spec.ID]
	m.mu.RUnlock()
	if closed {
		return SessionStatus{}, ErrClosed
	}
	if exists {
		return SessionStatus{}, ErrSessionExists
	}

	log, err := m.cfg.Transcripts.Open(spec.ID)
	if err != nil {
		return SessionStatus{}, fmt.Errorf("terminal: open transcript: %w", err)
	}
	resolved := Persistence("")
	if spec.Persistence != PersistenceAuto {
		resolved = spec.Persistence
	}
	s := newRuntimeSession(m, spec, resolved, log, true)

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		_ = log.Close()
		return SessionStatus{}, ErrClosed
	}
	if _, ok := m.sessions[spec.ID]; ok {
		m.mu.Unlock()
		_ = log.Close()
		return SessionStatus{}, ErrSessionExists
	}
	m.sessions[spec.ID] = s
	m.mu.Unlock()
	s.start()
	return s.status(), nil
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
			restoreErrors = append(restoreErrors, fmt.Errorf("session %s: %w", record.ID, err))
		}
	}
	return errors.Join(restoreErrors...)
}

func (m *Manager) restoreOne(ctx context.Context, record SessionRecord) error {
	spec := SessionSpec{
		ID:          record.ID,
		Name:        record.Name,
		Persistence: record.Persistence,
		Generation:  record.Generation,
		Shell:       record.Shell,
		Args:        append([]string(nil), record.Args...),
		Cwd:         record.Cwd,
		Env:         cloneMap(record.Env),
		Cols:        record.Cols,
		Rows:        record.Rows,
	}
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
	if err := s.discard(ctx); err != nil {
		return err
	}
	m.forget(sessionID, s)
	return nil
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
		go func(index int, session *runtimeSession) {
			defer wg.Done()
			errs[index] = session.shutdown(ctx)
		}(i, s)
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

type subscriber struct {
	id      string
	joined  uint64
	ch      chan OutputFrame
	writers chan bool
	states  chan SessionStatus
	closed  chan AttachmentCloseReason
}

type runtimeSession struct {
	manager *Manager
	spec    SessionSpec
	log     transcript.Log

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	wake   chan struct{}
	retry  chan struct{}

	mu          sync.Mutex
	operationMu sync.Mutex
	sizeMu      sync.Mutex
	state       SessionState
	lastErr     string
	resolved    Persistence
	backend     backend
	clients     map[string]*subscriber
	writerID    string
	joinCounter uint64
	lastSeq     uint64
	cols        uint16
	rows        uint16
	created     bool
	started     bool
	terminating bool
	closed      bool
}

func newRuntimeSession(manager *Manager, spec SessionSpec, resolved Persistence, log transcript.Log, created bool) *runtimeSession {
	ctx, cancel := context.WithCancel(context.Background())
	_, newest := log.Bounds()
	cols, rows := terminalSize(spec)
	return &runtimeSession{
		manager:  manager,
		spec:     spec,
		log:      log,
		ctx:      ctx,
		cancel:   cancel,
		done:     make(chan struct{}),
		wake:     make(chan struct{}, 1),
		retry:    make(chan struct{}, 1),
		state:    StateConnecting,
		resolved: resolved,
		clients:  make(map[string]*subscriber),
		lastSeq:  newest,
		cols:     cols,
		rows:     rows,
		created:  created,
	}
}

func (s *runtimeSession) start() {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return
	}
	s.started = true
	s.mu.Unlock()
	go s.run()
}

// markDormant registers a finished session that stays attachable for replay.
func (s *runtimeSession) markDormant() {
	s.mu.Lock()
	if !s.started {
		s.started = true
		close(s.done)
	}
	s.state = StateExited
	status := s.notifyLocked()
	s.mu.Unlock()
	s.manager.callbacks.OnSessionState(status)
}

func (s *runtimeSession) run() {
	defer close(s.done)
	// Only a freshly created session may create its backend, and only once.
	create := s.created
	attempt := 0
	for {
		if !s.waitIfTerminating() {
			return
		}
		s.setState(StateConnecting, nil)
		spec, err := s.launchSpec(s.ctx)
		if err != nil {
			if !s.handleStartError(err, &attempt) {
				return
			}
			continue
		}
		s.mu.Lock()
		requested := s.resolved
		if requested == "" {
			requested = spec.Persistence
		}
		s.mu.Unlock()
		b, resolved, err := s.manager.launcher.start(s.ctx, spec, requested, create)
		if err != nil {
			if !s.handleStartError(err, &attempt) {
				return
			}
			continue
		}
		if s.ctx.Err() != nil {
			_ = b.Close()
			return
		}
		create = false
		attempt = 0
		if resizeErr := s.activateBackend(b, resolved); resizeErr != nil {
			_ = b.Close()
			if !s.waitIfTerminating() {
				return
			}
			if b.Reconnectable(resizeErr) {
				s.setState(StateDisconnected, resizeErr)
				if s.waitForRetry(attempt) {
					attempt++
					continue
				}
				return
			}
			s.finishExited(resizeErr)
			return
		}
		s.setState(StateRunning, nil)

		readErr := s.consume(b)
		waitErr := b.Wait(s.ctx)
		_ = b.Close()
		s.mu.Lock()
		if s.backend == b {
			s.backend = nil
		}
		s.mu.Unlock()
		if !s.waitIfTerminating() {
			return
		}
		// Wait is authoritative for exit; a clean SSH command also closes its pipe.
		backendErr := waitErr
		if backendErr == nil && readErr != nil && !isTerminalEOF(readErr) {
			backendErr = readErr
		}
		if !errors.Is(backendErr, ErrBackendMissing) && b.Reconnectable(backendErr) {
			s.setState(StateDisconnected, backendErr)
			if s.waitForRetry(attempt) {
				attempt++
				continue
			}
			return
		}
		s.finishExited(backendErr)
		return
	}
}

// activateBackend publishes a freshly launched backend and applies the current size.
func (s *runtimeSession) activateBackend(b backend, resolved Persistence) error {
	s.mu.Lock()
	if err := s.ctx.Err(); err != nil {
		s.mu.Unlock()
		return err
	}
	if s.closed {
		s.mu.Unlock()
		return ErrClosed
	}
	s.resolved = resolved
	s.backend = b
	s.lastErr = ""
	s.mu.Unlock()
	if err := s.applySize(); err != nil {
		s.mu.Lock()
		if s.backend == b {
			s.backend = nil
		}
		s.mu.Unlock()
		return err
	}
	return nil
}

// applySize pushes the newest requested size to the live backend under sizeMu.
func (s *runtimeSession) applySize() error {
	s.sizeMu.Lock()
	defer s.sizeMu.Unlock()
	s.mu.Lock()
	unavailable := s.closed || s.terminating
	b := s.backend
	cols, rows := s.cols, s.rows
	s.mu.Unlock()
	if unavailable {
		return ErrUnavailable
	}
	if b == nil {
		return nil
	}
	if err := b.Resize(cols, rows); err != nil {
		return fmt.Errorf("terminal: apply current size %dx%d: %w", cols, rows, err)
	}
	return nil
}

// handleStartError reports whether the run loop should try again.
func (s *runtimeSession) handleStartError(err error, attempt *int) bool {
	if s.ctx.Err() != nil {
		return false
	}
	if !s.waitIfTerminating() {
		return false
	}
	if errors.Is(err, ErrBackendMissing) {
		s.finishExited(err)
		return false
	}
	if isPermanentStartError(err) {
		// A permanent failure repeats, so wait for Reconnect or RefreshHost.
		s.setState(StateError, err)
		select {
		case <-s.ctx.Done():
			return false
		case <-s.retry:
		}
		*attempt = 0
		return true
	}
	s.setState(StateDisconnected, err)
	if !s.waitForRetry(*attempt) {
		return false
	}
	*attempt++
	return true
}

func (s *runtimeSession) launchSpec(ctx context.Context) (SessionSpec, error) {
	s.mu.Lock()
	spec := cloneSpec(s.spec)
	spec.Cols, spec.Rows = s.cols, s.rows
	hostID := ""
	if spec.Host != nil {
		hostID = spec.Host.ID
	}
	s.mu.Unlock()
	if hostID == "" || s.manager.cfg.Repository == nil {
		return spec, nil
	}
	host, err := s.manager.cfg.Repository.LoadHost(ctx, hostID)
	if err != nil {
		return SessionSpec{}, permanentStartError(fmt.Errorf("terminal: reload SSH host %s: %w", hostID, err))
	}
	if host.ID == "" {
		host.ID = hostID
	}
	spec.Host = &host
	spec = cloneSpec(spec)
	if err := validateSpec(spec); err != nil {
		return SessionSpec{}, permanentStartError(err)
	}
	s.mu.Lock()
	fresh := *spec.Host
	s.spec.Host = &fresh
	s.mu.Unlock()
	return spec, nil
}

func (s *runtimeSession) waitIfTerminating() bool {
	for {
		if s.ctx.Err() != nil {
			return false
		}
		s.mu.Lock()
		terminating := s.terminating
		s.mu.Unlock()
		if !terminating {
			return true
		}
		select {
		case <-s.ctx.Done():
			return false
		case <-s.wake:
		}
	}
}

func (s *runtimeSession) consume(b backend) error {
	buffer := make([]byte, 32<<10)
	for {
		n, err := b.Read(buffer)
		if n > 0 {
			s.publish(buffer[:n])
		}
		if err != nil {
			return err
		}
	}
}

func (s *runtimeSession) publish(data []byte) {
	now := time.Now().UTC()
	copyOfData := append([]byte(nil), data...)
	var dropped []string
	writerChanged := false
	s.mu.Lock()
	sequence, err := s.log.Append(copyOfData)
	if err != nil {
		s.lastErr = err.Error()
		status := s.notifyLocked()
		s.mu.Unlock()
		s.manager.callbacks.OnSessionState(status)
		s.signal()
		return
	}
	if sequence > s.lastSeq {
		s.lastSeq = sequence
	}
	frame := OutputFrame{Sequence: sequence, Time: now, Data: copyOfData}
	for id, client := range s.clients {
		select {
		case client.ch <- frame:
		default:
			delete(s.clients, id)
			s.closeSubscriberLocked(client, AttachmentEvicted)
			dropped = append(dropped, id)
			if s.writerID == id {
				s.writerID = ""
				writerChanged = true
			}
		}
	}
	if writerChanged {
		s.assignWriterLocked()
		s.notifyWritersLocked()
	}
	writer := s.writerID
	var status SessionStatus
	if len(dropped) != 0 {
		status = s.notifyLocked()
	}
	s.mu.Unlock()
	for _, id := range dropped {
		s.manager.callbacks.OnClientDropped(s.spec.ID, id, "output buffer is full")
	}
	if writerChanged {
		s.manager.callbacks.OnWriterChanged(s.spec.ID, writer)
	}
	if len(dropped) != 0 {
		s.manager.callbacks.OnSessionState(status)
	}
	s.signal()
}

// waitForRetry blocks until the next connection attempt should run.
func (s *runtimeSession) waitForRetry(attempt int) bool {
	for {
		if s.ctx.Err() != nil {
			return false
		}
		s.mu.Lock()
		hasClients := len(s.clients) != 0
		s.mu.Unlock()
		if !hasClients {
			select {
			case <-s.ctx.Done():
				return false
			case <-s.retry:
				return true
			case <-s.wake:
				continue
			}
		}
		timer := time.NewTimer(reconnectDelay(s.manager.cfg.ReconnectMin, s.manager.cfg.ReconnectMax, attempt))
		select {
		case <-s.ctx.Done():
			stopTimer(timer)
			return false
		case <-s.retry:
			stopTimer(timer)
			return true
		case <-s.wake:
			stopTimer(timer)
			continue
		case <-timer.C:
			return true
		}
	}
}

func stopTimer(timer *time.Timer) {
	if !timer.Stop() {
		<-timer.C
	}
}

func (s *runtimeSession) finishExited(err error) {
	s.setState(StateExited, err)
	s.closeClients(AttachmentExited)
}

func (s *runtimeSession) setState(state SessionState, err error) {
	s.mu.Lock()
	if s.terminating && state != StateTerminating {
		s.mu.Unlock()
		return
	}
	s.state = state
	if err != nil && !errors.Is(err, io.EOF) {
		s.lastErr = err.Error()
	} else if err == nil {
		s.lastErr = ""
	}
	status := s.notifyLocked()
	s.mu.Unlock()
	s.manager.callbacks.OnSessionState(status)
}

func (s *runtimeSession) status() SessionStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.statusLocked()
}

func (s *runtimeSession) statusLocked() SessionStatus {
	persistence := s.resolved
	if persistence == "" {
		persistence = s.spec.Persistence
	}
	return SessionStatus{
		ID:           s.spec.ID,
		Generation:   s.spec.Generation,
		State:        s.state,
		Persistence:  persistence,
		WriterID:     s.writerID,
		Clients:      len(s.clients),
		LastError:    s.lastErr,
		LastSequence: s.lastSeq,
	}
}

// notifyLocked hands the current status to every client, newest status only.
func (s *runtimeSession) notifyLocked() SessionStatus {
	status := s.statusLocked()
	for _, client := range s.clients {
		select {
		case client.states <- status:
		default:
			select {
			case <-client.states:
			default:
			}
			client.states <- status
		}
	}
	return status
}

func (s *runtimeSession) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *runtimeSession) requestRetry() {
	select {
	case s.retry <- struct{}{}:
	default:
	}
}

func (s *runtimeSession) hostID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.spec.Host == nil {
		return ""
	}
	return s.spec.Host.ID
}

func (s *runtimeSession) assignWriterLocked() {
	if s.writerID != "" {
		return
	}
	var selected *subscriber
	for _, client := range s.clients {
		if selected == nil || client.joined < selected.joined {
			selected = client
		}
	}
	if selected != nil {
		s.writerID = selected.id
	}
}

func (s *runtimeSession) notifyWritersLocked() {
	for id, client := range s.clients {
		value := id == s.writerID
		select {
		case client.writers <- value:
		default:
			select {
			case <-client.writers:
			default:
			}
			client.writers <- value
		}
	}
}

func (s *runtimeSession) closeSubscriberLocked(client *subscriber, reason AttachmentCloseReason) {
	select {
	case client.closed <- reason:
	default:
	}
	close(client.closed)
	close(client.writers)
	close(client.states)
	close(client.ch)
}

func (s *runtimeSession) closeClients(reason AttachmentCloseReason) {
	s.mu.Lock()
	hadWriter := s.writerID != ""
	clients := make([]string, 0, len(s.clients))
	for id, client := range s.clients {
		clients = append(clients, id)
		s.closeSubscriberLocked(client, reason)
	}
	s.clients = make(map[string]*subscriber)
	s.writerID = ""
	status := s.statusLocked()
	s.mu.Unlock()
	if hadWriter {
		s.manager.callbacks.OnWriterChanged(s.spec.ID, "")
	}
	if len(clients) != 0 {
		s.manager.callbacks.OnSessionState(status)
	}
}

// stop kills the backend, removes the runtime and closes every client with reason.
func (s *runtimeSession) stop(ctx context.Context, reason AttachmentCloseReason) error {
	s.operationMu.Lock()
	defer s.operationMu.Unlock()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	previous := s.state
	// An exited or terminated runtime has no backend left to contact.
	kill := previous != StateExited && previous != StateTerminated
	var b backend
	var resolved Persistence
	var status SessionStatus
	if kill {
		s.terminating = true
		s.state = StateTerminating
		b, resolved = s.backend, s.resolved
		status = s.notifyLocked()
	}
	s.mu.Unlock()
	if kill {
		s.manager.callbacks.OnSessionState(status)
		if err := s.killBackend(ctx, b, resolved); err != nil {
			s.abortStop(previous, err)
			return err
		}
	}
	s.finishStop(reason)
	return nil
}

func (s *runtimeSession) killBackend(ctx context.Context, b backend, resolved Persistence) error {
	spec, err := s.launchSpec(ctx)
	if err != nil {
		return err
	}
	if resolved != "" && resolved != PersistenceNone && resolved != PersistenceAuto {
		// A separate control connection leaves the live backend intact on failure.
		return s.manager.launcher.terminate(ctx, spec, resolved)
	}
	if b != nil {
		return b.Terminate(ctx)
	}
	return nil
}

// abortStop restores an attachable runtime after a failed kill.
func (s *runtimeSession) abortStop(previous SessionState, err error) {
	s.mu.Lock()
	s.terminating = false
	switch {
	case s.backend != nil:
		s.state = StateRunning
	case previous == StateError, previous == StateExited:
		s.state = previous
	default:
		s.state = StateDisconnected
	}
	s.lastErr = err.Error()
	status := s.notifyLocked()
	s.mu.Unlock()
	s.signal()
	s.manager.callbacks.OnSessionState(status)
}

// finishStop ends the run loop, closes clients and releases the transcript.
func (s *runtimeSession) finishStop(reason AttachmentCloseReason) {
	s.mu.Lock()
	b := s.backend
	started := s.started
	s.cancel()
	s.mu.Unlock()
	if b != nil {
		_ = b.Close()
	}
	s.closeClients(reason)
	s.signal()
	if started {
		ctx, cancel := context.WithTimeout(context.Background(), s.manager.cfg.ShutdownTimeout)
		defer cancel()
		select {
		case <-s.done:
		case <-ctx.Done():
			// The kill already succeeded, so cleanup may not block the caller.
		}
	}
	s.mu.Lock()
	s.state = StateTerminated
	s.closed = true
	s.backend = nil
	status := s.statusLocked()
	s.mu.Unlock()
	s.manager.callbacks.OnSessionState(status)
	_ = s.log.Close()
}

// discard forgets a runtime without killing its backend.
func (s *runtimeSession) discard(ctx context.Context) error {
	s.operationMu.Lock()
	defer s.operationMu.Unlock()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	b := s.backend
	started := s.started
	s.cancel()
	s.mu.Unlock()
	s.closeClients(AttachmentExited)
	if b != nil {
		_ = b.Close()
	}
	s.signal()
	if started {
		select {
		case <-s.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return s.log.Close()
}

func (s *runtimeSession) shutdown(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	b := s.backend
	s.cancel()
	s.mu.Unlock()
	s.closeClients(AttachmentServerShutdown)
	var err error
	if b != nil {
		err = b.Close()
	}
	s.signal()
	s.mu.Lock()
	started := s.started
	s.mu.Unlock()
	if started {
		select {
		case <-s.done:
		case <-ctx.Done():
			go func() {
				<-s.done
				_ = s.log.Close()
			}()
			return errors.Join(err, ctx.Err())
		}
	}
	return errors.Join(err, s.log.Close())
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
		switch credential := host.Credential.(type) {
		case PrivateKeyCredential:
			credential.PEM = append([]byte(nil), credential.PEM...)
			credential.Passphrase = append([]byte(nil), credential.Passphrase...)
			host.Credential = credential
		case *PrivateKeyCredential:
			if credential != nil {
				copyOfCredential := *credential
				copyOfCredential.PEM = append([]byte(nil), credential.PEM...)
				copyOfCredential.Passphrase = append([]byte(nil), credential.Passphrase...)
				host.Credential = &copyOfCredential
			}
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
