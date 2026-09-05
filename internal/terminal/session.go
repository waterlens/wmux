package terminal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/waterlens/wmux/internal/transcript"
)

// runtimeSession owns one execution of a session: a run loop that keeps a
// backend connected, and the subscribers reading its output.
type runtimeSession struct {
	// Immutable after construction. ctx is the session lifetime, not a request,
	// so it outlives every caller and is cancelled only by teardown.
	manager *Manager
	log     transcript.Log
	created bool
	ctx     context.Context
	cancel  context.CancelFunc
	done    chan struct{}
	wake    chan struct{}
	retry   chan struct{}

	// operationMu serializes stop and discard; it guards no fields.
	operationMu sync.Mutex
	// sizeMu serializes Resize calls into the live backend; it guards no fields.
	sizeMu sync.Mutex

	// mu guards every field below.
	mu          sync.Mutex
	spec        SessionSpec
	state       SessionState
	lastErr     string
	resolved    Persistence
	backend     backend
	clients     map[string]*subscriber
	writerID    string
	joinCounter uint64
	cols        uint16
	rows        uint16
	// hasRunLoop reports that run() will close done, so teardown must wait for it.
	hasRunLoop  bool
	terminating bool
	closed      bool
}

func newRuntimeSession(manager *Manager, spec SessionSpec, resolved Persistence, log transcript.Log, created bool) *runtimeSession {
	ctx, cancel := context.WithCancel(context.Background())
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
		cols:     cols,
		rows:     rows,
		created:  created,
	}
}

func (s *runtimeSession) start() {
	s.mu.Lock()
	if s.hasRunLoop {
		s.mu.Unlock()
		return
	}
	s.hasRunLoop = true
	s.mu.Unlock()
	go s.run()
}

// markDormant registers a finished session that stays attachable for replay.
func (s *runtimeSession) markDormant() {
	s.mu.Lock()
	s.state = StateExited
	status := s.notifyLocked()
	s.mu.Unlock()
	s.manager.cfg.Callbacks.OnSessionState(status)
}

func (s *runtimeSession) run() {
	defer close(s.done)
	// Only a freshly created session may create its backend, and only once.
	create := s.created
	attempt := 0
	for {
		if !s.keepRunning() {
			return
		}
		s.setState(StateConnecting, nil)
		spec, err := s.launchSpec(s.ctx)
		if err != nil {
			next, retry := s.retryAfterStartError(err, attempt)
			if !retry {
				return
			}
			attempt = next
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
			next, retry := s.retryAfterStartError(err, attempt)
			if !retry {
				return
			}
			attempt = next
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
			if !s.keepRunning() {
				return
			}
			next, retry := s.retryAfterBackendError(b, resizeErr, attempt)
			if !retry {
				return
			}
			attempt = next
			continue
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
		if !s.keepRunning() {
			return
		}
		// Wait is authoritative for exit; a clean SSH command also closes its pipe.
		backendErr := waitErr
		if backendErr == nil && readErr != nil && !isTerminalEOF(readErr) {
			backendErr = readErr
		}
		next, retry := s.retryAfterBackendError(b, backendErr, attempt)
		if !retry {
			return
		}
		attempt = next
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

// retryAfterStartError reports whether the run loop should try to start again,
// and the attempt count to continue from. A permanent failure waits for an
// explicit Reconnect or RefreshHost instead of retrying on a timer.
func (s *runtimeSession) retryAfterStartError(err error, attempt int) (int, bool) {
	if s.ctx.Err() != nil || !s.keepRunning() {
		return attempt, false
	}
	if errors.Is(err, ErrMuxSessionMissing) {
		s.finishExited(err)
		return attempt, false
	}
	if isPermanentStartError(err) {
		s.setState(StateError, err)
		select {
		case <-s.ctx.Done():
			return attempt, false
		case <-s.retry:
		}
		return 0, true
	}
	s.setState(StateDisconnected, err)
	if !s.awaitRetry(attempt) {
		return attempt, false
	}
	return attempt + 1, true
}

// retryAfterBackendError reports whether the run loop should reconnect after a
// live backend failed, and the attempt count to continue from.
func (s *runtimeSession) retryAfterBackendError(b backend, err error, attempt int) (int, bool) {
	if errors.Is(err, ErrMuxSessionMissing) || !b.Reconnectable(err) {
		s.finishExited(err)
		return attempt, false
	}
	s.setState(StateDisconnected, err)
	if !s.awaitRetry(attempt) {
		return attempt, false
	}
	return attempt + 1, true
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

// keepRunning blocks while a stop is in flight and reports whether the run loop
// may continue.
func (s *runtimeSession) keepRunning() bool {
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
	buffer := make([]byte, backendReadBuffer)
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

// awaitRetry blocks until the next connection attempt should run, and reports
// whether one should. Without clients it waits indefinitely for an explicit
// Reconnect rather than reconnecting on a timer.
func (s *runtimeSession) awaitRetry(attempt int) bool {
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
			}
			continue
		}
		// Since Go 1.23 a timer needs no draining and is collected once unused.
		timer := time.NewTimer(reconnectDelay(s.manager.cfg.ReconnectMin, s.manager.cfg.ReconnectMax, attempt))
		select {
		case <-s.ctx.Done():
			timer.Stop()
			return false
		case <-s.retry:
			timer.Stop()
			return true
		case <-s.wake:
			timer.Stop()
		case <-timer.C:
			return true
		}
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
	s.manager.cfg.Callbacks.OnSessionState(status)
}

func (s *runtimeSession) status() SessionStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.statusLocked()
}

// signal wakes keepRunning and awaitRetry after a client-count or terminating
// flag change. Cancellation is covered by the run loop's own ctx, so teardown
// does not need it.
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
		s.manager.cfg.Callbacks.OnSessionState(status)
		if err := s.killBackend(ctx, b, resolved); err != nil {
			s.abortStop(previous, err)
			return err
		}
	}
	teardownCtx, cancel := context.WithTimeout(context.Background(), s.manager.cfg.ShutdownTimeout)
	defer cancel()
	// The kill already succeeded, so cleanup must not fail the caller.
	_ = s.teardown(teardownCtx, reason, StateTerminated)
	return nil
}

// discard forgets a runtime without killing its backend.
func (s *runtimeSession) discard(ctx context.Context) error {
	s.operationMu.Lock()
	defer s.operationMu.Unlock()
	return s.teardown(ctx, AttachmentExited, "")
}

// shutdown detaches a runtime on server shutdown, leaving the backend running.
func (s *runtimeSession) shutdown(ctx context.Context) error {
	return s.teardown(ctx, AttachmentServerShutdown, "")
}

// teardown ends one execution: it cancels the run loop, closes every client with
// reason, releases the transcript and, when finalState is set, publishes it as
// the session's last status.
func (s *runtimeSession) teardown(ctx context.Context, reason AttachmentCloseReason, finalState SessionState) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	b, hasRunLoop := s.backend, s.hasRunLoop
	s.backend = nil
	s.cancel()
	s.mu.Unlock()

	s.closeClients(reason)
	var err error
	if b != nil {
		err = b.Close()
	}
	if hasRunLoop {
		select {
		case <-s.done:
		case <-ctx.Done():
			// The run loop still writes to the transcript; close it once it stops.
			go func() {
				<-s.done
				_ = s.log.Close()
			}()
			return errors.Join(err, ctx.Err())
		}
	}
	if finalState != "" {
		s.mu.Lock()
		s.state = finalState
		status := s.statusLocked()
		s.mu.Unlock()
		s.manager.cfg.Callbacks.OnSessionState(status)
	}
	return errors.Join(err, s.log.Close())
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
	s.manager.cfg.Callbacks.OnSessionState(status)
}
