package terminal

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/waterlens/wmux/internal/transcript"
)

type launcherStub struct {
	startFunc     func(context.Context, SessionSpec, Persistence, bool) (backend, Persistence, error)
	terminateFunc func(context.Context, SessionSpec, Persistence) error

	mu      sync.Mutex
	creates []bool
}

func (l *launcherStub) start(ctx context.Context, spec SessionSpec, persistence Persistence, create bool) (backend, Persistence, error) {
	l.mu.Lock()
	l.creates = append(l.creates, create)
	l.mu.Unlock()
	return l.startFunc(ctx, spec, persistence, create)
}

func (l *launcherStub) terminate(ctx context.Context, spec SessionSpec, persistence Persistence) error {
	if l.terminateFunc == nil {
		return nil
	}
	return l.terminateFunc(ctx, spec, persistence)
}

func (l *launcherStub) createFlags() []bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]bool(nil), l.creates...)
}

func staticLauncher(b backend, persistence Persistence) *launcherStub {
	return &launcherStub{startFunc: func(context.Context, SessionSpec, Persistence, bool) (backend, Persistence, error) {
		return b, persistence, nil
	}}
}

type backendStub struct {
	closed       chan struct{}
	closeOnce    sync.Once
	readErr      error
	waitErr      error
	reconnect    bool
	writeStarted chan struct{}
	blockWrite   bool
	// redrawErr makes Redraw fail; redraws and sizes record what was asked.
	redrawErr error
	redraws   chan struct{}
	sizes     chan [2]uint16
}

func newBackendStub() *backendStub {
	return &backendStub{closed: make(chan struct{}), redraws: make(chan struct{}, 8), sizes: make(chan [2]uint16, 8)}
}

func (b *backendStub) Read([]byte) (int, error) {
	<-b.closed
	if b.readErr != nil {
		return 0, b.readErr
	}
	return 0, io.EOF
}

func (b *backendStub) WriteContext(ctx context.Context, p []byte) (int, error) {
	if b.writeStarted != nil {
		select {
		case <-b.writeStarted:
		default:
			close(b.writeStarted)
		}
	}
	if b.blockWrite {
		select {
		case <-b.closed:
			return 0, io.ErrClosedPipe
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	return len(p), nil
}

func (b *backendStub) Resize(cols, rows uint16) error {
	select {
	case b.sizes <- [2]uint16{cols, rows}:
	default:
	}
	return nil
}

func (b *backendStub) Redraw(context.Context) error {
	select {
	case b.redraws <- struct{}{}:
	default:
	}
	return b.redrawErr
}

func (b *backendStub) Wait(ctx context.Context) error {
	select {
	case <-b.closed:
		return b.waitErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *backendStub) Close() error {
	b.closeOnce.Do(func() { close(b.closed) })
	return nil
}

func (b *backendStub) Terminate(context.Context) error { return b.Close() }
func (b *backendStub) Reconnectable(err error) bool    { return b.reconnect && err != nil }

func (b *backendStub) isClosed() bool {
	select {
	case <-b.closed:
		return true
	default:
		return false
	}
}

// scriptedBackend lets one test drive what Resize does.
type scriptedBackend struct {
	*backendStub
	onResize func(cols, rows uint16) error
}

func (b *scriptedBackend) Resize(cols, rows uint16) error {
	if b.onResize == nil {
		return nil
	}
	return b.onResize(cols, rows)
}

// finiteReadBackend replays a fixed byte slice and then exits cleanly.
type finiteReadBackend struct {
	*backendStub
	reader *bytes.Reader
}

func (b *finiteReadBackend) Read(p []byte) (int, error) { return b.reader.Read(p) }
func (*finiteReadBackend) Wait(context.Context) error   { return nil }

type repositoryStub struct {
	mu      sync.Mutex
	host    HostSpec
	loads   int
	records []SessionRecord
}

func (r *repositoryStub) ListSessions(context.Context) ([]SessionRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]SessionRecord(nil), r.records...), nil
}

func (r *repositoryStub) LoadHost(context.Context, string) (HostSpec, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.loads++
	return r.host, nil
}

func (r *repositoryStub) setHost(host HostSpec) {
	r.mu.Lock()
	r.host = host
	r.mu.Unlock()
}

func (r *repositoryStub) loadCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.loads
}

func TestTerminateFailureLeavesRunLoopAndAttachmentAlive(t *testing.T) {
	b := newBackendStub()
	var terminations atomic.Int32
	backends := staticLauncher(b, PersistenceTmux)
	backends.terminateFunc = func(context.Context, SessionSpec, Persistence) error {
		if terminations.Add(1) == 1 {
			return errors.New("control connection unavailable")
		}
		return nil
	}
	manager := managerWithLauncher(t, backends)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := manager.Create(SessionSpec{ID: "terminate-recovery", Persistence: PersistenceTmux}); err != nil {
		t.Fatal(err)
	}
	attachment, err := manager.Attach(ctx, "terminate-recovery", "browser", 0)
	if err != nil {
		t.Fatal(err)
	}
	waitState(ctx, t, manager, "terminate-recovery", StateRunning)
	if err := manager.Terminate(ctx, "terminate-recovery"); err == nil {
		t.Fatal("first termination unexpectedly succeeded")
	}
	status, err := manager.Status("terminate-recovery")
	if err != nil {
		t.Fatal(err)
	}
	if status.State != StateRunning || !attachment.IsWriter() {
		t.Fatalf("failed termination damaged live session: %+v, writer=%v", status, attachment.IsWriter())
	}
	select {
	case reason := <-attachment.Closed:
		t.Fatalf("attachment closed after failed termination: %s", reason)
	default:
	}
	if _, err := attachment.WriteContext(ctx, []byte("still alive")); err != nil {
		t.Fatalf("write after failed termination: %v", err)
	}
	if err := manager.Terminate(ctx, "terminate-recovery"); err != nil {
		t.Fatal(err)
	}
	if reason := awaitCloseReason(ctx, t, attachment); reason != AttachmentExited {
		t.Fatalf("close reason = %q, want exited", reason)
	}
}

func TestTerminateExitedSessionNeverContactsTheBackend(t *testing.T) {
	b := newBackendStub()
	var terminations atomic.Int32
	backends := staticLauncher(b, PersistenceTmux)
	backends.terminateFunc = func(context.Context, SessionSpec, Persistence) error {
		terminations.Add(1)
		return errors.New("host is unreachable")
	}
	manager := managerWithLauncher(t, backends)
	defer manager.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := manager.Create(SessionSpec{ID: "exited-terminate", Persistence: PersistenceTmux}); err != nil {
		t.Fatal(err)
	}
	waitState(ctx, t, manager, "exited-terminate", StateRunning)
	_ = b.Close()
	waitState(ctx, t, manager, "exited-terminate", StateExited)

	if err := manager.Terminate(ctx, "exited-terminate"); err != nil {
		t.Fatalf("Terminate on an exited session = %v, want success without contacting the host", err)
	}
	if terminations.Load() != 0 {
		t.Fatalf("exited session contacted the backend %d time(s)", terminations.Load())
	}
	if _, err := manager.Status("exited-terminate"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("Status after Terminate = %v, want ErrSessionNotFound", err)
	}
}

func TestStopForRestartClosesClientsWithRestartedReason(t *testing.T) {
	b := newBackendStub()
	manager := managerWithLauncher(t, staticLauncher(b, PersistenceTmux))
	defer manager.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := manager.Create(SessionSpec{ID: "restart-stop", Persistence: PersistenceTmux}); err != nil {
		t.Fatal(err)
	}
	a, err := manager.Attach(ctx, "restart-stop", "browser", 0)
	if err != nil {
		t.Fatal(err)
	}
	waitState(ctx, t, manager, "restart-stop", StateRunning)
	if err := manager.StopForRestart(ctx, "restart-stop"); err != nil {
		t.Fatal(err)
	}
	if reason := awaitCloseReason(ctx, t, a); reason != AttachmentRestarted {
		t.Fatalf("close reason = %q, want restarted", reason)
	}
	if _, err := manager.Status("restart-stop"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("Status after StopForRestart = %v, want ErrSessionNotFound", err)
	}
	// The replacement execution registers under the same ID.
	if err := manager.Create(SessionSpec{ID: "restart-stop", Persistence: PersistenceTmux, Generation: 2}); err != nil {
		t.Fatalf("re-create after StopForRestart: %v", err)
	}
}

func TestBlockedWriteDoesNotBlockTerminate(t *testing.T) {
	b := newBackendStub()
	b.blockWrite = true
	b.writeStarted = make(chan struct{})
	manager := managerWithLauncher(t, staticLauncher(b, PersistenceTmux))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := manager.Create(SessionSpec{ID: "blocked-write", Persistence: PersistenceTmux}); err != nil {
		t.Fatal(err)
	}
	attachment, err := manager.Attach(ctx, "blocked-write", "browser", 0)
	if err != nil {
		t.Fatal(err)
	}
	waitState(ctx, t, manager, "blocked-write", StateRunning)
	writeDone := make(chan error, 1)
	go func() {
		_, err := attachment.WriteContext(ctx, []byte("blocked"))
		writeDone <- err
	}()
	select {
	case <-b.writeStarted:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := manager.Terminate(ctx, "blocked-write"); err != nil {
		t.Fatalf("Terminate waited behind Write: %v", err)
	}
	select {
	case <-writeDone:
	case <-ctx.Done():
		t.Fatal("blocked Write did not unblock when backend closed")
	}
}

func TestWriteTimeoutReturnsContextErrorAndKeepsBackendOpen(t *testing.T) {
	b := newBackendStub()
	b.blockWrite = true
	b.writeStarted = make(chan struct{})
	manager := managerWithLauncher(t, staticLauncher(b, PersistenceTmux))
	defer manager.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := manager.Create(SessionSpec{ID: "write-context", Persistence: PersistenceTmux}); err != nil {
		t.Fatal(err)
	}
	a, err := manager.Attach(ctx, "write-context", "browser", 0)
	if err != nil {
		t.Fatal(err)
	}
	waitState(ctx, t, manager, "write-context", StateRunning)
	writeCtx, writeCancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer writeCancel()
	started := time.Now()
	_, err = a.WriteContext(writeCtx, []byte("blocked"))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WriteContext error = %v, want deadline", err)
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("WriteContext cancellation took %s", elapsed)
	}
	// A stuck client does not end the shared session.
	if b.isClosed() {
		t.Fatal("input timeout closed the shared backend")
	}
	status, err := manager.Status("write-context")
	if err != nil || status.State != StateRunning {
		t.Fatalf("status after input timeout = %+v, %v", status, err)
	}
}

func TestNewerSizeIsNeverOverwrittenByAnOlderOne(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{})
	var mu sync.Mutex
	var sizes [][2]uint16
	b := &scriptedBackend{backendStub: newBackendStub()}
	b.onResize = func(cols, rows uint16) error {
		mu.Lock()
		sizes = append(sizes, [2]uint16{cols, rows})
		first := len(sizes) == 1
		mu.Unlock()
		if first {
			close(entered)
			<-release
		}
		return nil
	}
	applied := func() [][2]uint16 {
		mu.Lock()
		defer mu.Unlock()
		return append([][2]uint16(nil), sizes...)
	}

	manager := managerWithLauncher(t, staticLauncher(b, PersistenceTmux))
	defer manager.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := manager.Create(SessionSpec{ID: "resize-order", Persistence: PersistenceTmux, Cols: 120, Rows: 36}); err != nil {
		t.Fatal(err)
	}
	a, err := manager.Attach(ctx, "resize-order", "browser", 0)
	if err != nil {
		t.Fatal(err)
	}
	session, err := manager.session("resize-order")
	if err != nil {
		t.Fatal(err)
	}
	// Both requests queue behind the blocked activation resize.
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("backend activation never applied the launch size")
	}
	older := make(chan error, 1)
	go func() { older <- a.Resize(200, 50) }()
	waitCondition(ctx, t, func() bool { return requestedSize(session) == [2]uint16{200, 50} })
	newer := make(chan error, 1)
	go func() { newer <- a.Resize(220, 60) }()
	waitCondition(ctx, t, func() bool { return requestedSize(session) == [2]uint16{220, 60} })
	close(release)
	if err := <-older; err != nil {
		t.Fatalf("older resize: %v", err)
	}
	if err := <-newer; err != nil {
		t.Fatalf("newer resize: %v", err)
	}
	sizeHistory := applied()
	if len(sizeHistory) < 2 || sizeHistory[0] != [2]uint16{120, 36} {
		t.Fatalf("applied sizes %v do not start with the launch size", sizeHistory)
	}
	for _, size := range sizeHistory[1:] {
		if size != [2]uint16{220, 60} {
			t.Fatalf("applied sizes %v include a superseded size", sizeHistory)
		}
	}
}

func requestedSize(s *runtimeSession) [2]uint16 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return [2]uint16{s.cols, s.rows}
}

func TestResizeDuringConnectingIsAcceptedAndReconciled(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	resized := make(chan [2]uint16, 1)
	recorder := &scriptedBackend{backendStub: newBackendStub()}
	recorder.onResize = func(cols, rows uint16) error {
		select {
		case resized <- [2]uint16{cols, rows}:
		default:
		}
		return nil
	}
	var launchedCols, launchedRows atomic.Int32
	backends := &launcherStub{startFunc: func(_ context.Context, spec SessionSpec, _ Persistence, _ bool) (backend, Persistence, error) {
		launchedCols.Store(int32(spec.Cols))
		launchedRows.Store(int32(spec.Rows))
		close(entered)
		<-release
		return recorder, PersistenceTmux, nil
	}}
	manager := managerWithLauncher(t, backends)
	defer manager.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := manager.Create(SessionSpec{ID: "connecting-resize", Persistence: PersistenceTmux, Cols: 120, Rows: 36}); err != nil {
		t.Fatal(err)
	}
	<-entered
	a, err := manager.Attach(ctx, "connecting-resize", "browser", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Resize(200, 50); err != nil {
		t.Fatalf("Resize while connecting: %v", err)
	}
	close(release)
	waitState(ctx, t, manager, "connecting-resize", StateRunning)
	select {
	case size := <-resized:
		if size != [2]uint16{200, 50} {
			t.Fatalf("reconciled backend size = %v, want [200 50]", size)
		}
	case <-ctx.Done():
		t.Fatal("new backend never received the connecting-time resize")
	}
	if launchedCols.Load() != 120 || launchedRows.Load() != 36 {
		t.Fatalf("probe did not exercise a stale launch snapshot: %dx%d", launchedCols.Load(), launchedRows.Load())
	}
}

func TestTerminateUnblocksConcurrentActivationAndResize(t *testing.T) {
	launchEntered := make(chan struct{})
	launchRelease := make(chan struct{})
	resizeEntered := make(chan struct{})
	stub := newBackendStub()
	b := &scriptedBackend{backendStub: stub}
	var once sync.Once
	b.onResize = func(uint16, uint16) error {
		once.Do(func() { close(resizeEntered) })
		<-stub.closed
		return io.ErrClosedPipe
	}
	manager := managerWithLauncher(t, &launcherStub{startFunc: func(context.Context, SessionSpec, Persistence, bool) (backend, Persistence, error) {
		close(launchEntered)
		<-launchRelease
		return b, PersistenceTmux, nil
	}})
	defer manager.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := manager.Create(SessionSpec{ID: "resize-terminate", Persistence: PersistenceTmux, Cols: 120, Rows: 36}); err != nil {
		t.Fatal(err)
	}
	<-launchEntered
	a, err := manager.Attach(ctx, "resize-terminate", "browser", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Resize(200, 50); err != nil {
		t.Fatal(err)
	}
	close(launchRelease)
	select {
	case <-resizeEntered:
	case <-ctx.Done():
		t.Fatal("backend activation never began size reconciliation")
	}
	resizeStarted := make(chan struct{})
	resizeDone := make(chan error, 1)
	go func() {
		close(resizeStarted)
		resizeDone <- a.Resize(220, 60)
	}()
	<-resizeStarted
	terminateDone := make(chan error, 1)
	go func() { terminateDone <- manager.Terminate(ctx, "resize-terminate") }()
	if err := <-terminateDone; err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-resizeDone:
		if !errors.Is(err, ErrUnavailable) && !errors.Is(err, ErrAttachmentClosed) {
			t.Fatalf("concurrent Resize error = %v, want closed/unavailable", err)
		}
	case <-ctx.Done():
		t.Fatal("concurrent Resize remained blocked after Terminate")
	}
}

func TestConsumeBoundsLargeOutputFrames(t *testing.T) {
	data := bytes.Repeat([]byte("wmux-output-"), 10<<10)
	b := &finiteReadBackend{backendStub: newBackendStub(), reader: bytes.NewReader(data)}
	manager := managerWithLauncher(t, staticLauncher(b, PersistenceNone))
	defer manager.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := manager.Create(SessionSpec{ID: "bounded-output", Persistence: PersistenceNone}); err != nil {
		t.Fatal(err)
	}
	waitState(ctx, t, manager, "bounded-output", StateExited)
	a, err := manager.Attach(ctx, "bounded-output", "replay", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Initial) < 2 {
		t.Fatalf("large output produced %d frame(s), want multiple bounded frames", len(a.Initial))
	}
	joined := make([]byte, 0, len(data))
	for _, frame := range a.Initial {
		if len(frame.Data) > backendReadBuffer {
			t.Fatalf("output frame has %d bytes, want at most 32 KiB", len(frame.Data))
		}
		joined = append(joined, frame.Data...)
	}
	if !bytes.Equal(joined, data) {
		t.Fatalf("bounded output replay has %d bytes, want %d", len(joined), len(data))
	}
}

func TestRestoreOnlyAttachesAndExitsWhenTheBackendIsGone(t *testing.T) {
	repository := &repositoryStub{records: []SessionRecord{{
		Spec: SessionSpec{
			ID:          "restored",
			Persistence: PersistenceTmux,
			Shell:       "/bin/sh",
			Args:        []string{"-lc", "make release"},
			Generation:  4,
		},
		ResolvedPersistence: PersistenceTmux,
		Active:              true,
	}}}
	backends := &launcherStub{startFunc: func(context.Context, SessionSpec, Persistence, bool) (backend, Persistence, error) {
		return nil, "", ErrMuxSessionMissing
	}}
	manager := managerWithLauncher(t, backends, func(cfg *Config) { cfg.Repository = repository })
	defer manager.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := manager.Restore(ctx); err != nil {
		t.Fatal(err)
	}
	waitState(ctx, t, manager, "restored", StateExited)
	// Negative assertion: a stray retry needs time to appear, so a shorter sleep
	// loses sensitivity rather than making the test flaky.
	time.Sleep(40 * time.Millisecond)
	flags := backends.createFlags()
	if len(flags) != 1 || flags[0] {
		t.Fatalf("restore launch flags = %v, want exactly one attach-only launch", flags)
	}
	status, err := manager.Status("restored")
	if err != nil {
		t.Fatal(err)
	}
	if status.Generation != 4 {
		t.Fatalf("restored status generation = %d, want 4", status.Generation)
	}
	if status.LastError == "" {
		t.Fatalf("restored status hides the missing backend: %+v", status)
	}
}

func TestFirstLaunchCreatesAndEveryReconnectOnlyAttaches(t *testing.T) {
	first := newBackendStub()
	first.reconnect = true
	first.waitErr = errors.New("connection reset")
	second := newBackendStub()
	var starts atomic.Int32
	backends := &launcherStub{startFunc: func(context.Context, SessionSpec, Persistence, bool) (backend, Persistence, error) {
		if starts.Add(1) == 1 {
			return first, PersistenceTmux, nil
		}
		return second, PersistenceTmux, nil
	}}
	manager := managerWithLauncher(t, backends)
	defer manager.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := manager.Create(SessionSpec{ID: "reconnect-attach", Persistence: PersistenceTmux}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Attach(ctx, "reconnect-attach", "browser", 0); err != nil {
		t.Fatal(err)
	}
	waitState(ctx, t, manager, "reconnect-attach", StateRunning)
	_ = first.Close()
	waitCondition(ctx, t, func() bool { return starts.Load() >= 2 })
	waitState(ctx, t, manager, "reconnect-attach", StateRunning)
	flags := backends.createFlags()
	if len(flags) < 2 || !flags[0] {
		t.Fatalf("first launch must create: %v", flags)
	}
	for index, create := range flags[1:] {
		if create {
			t.Fatalf("reconnect %d created a new backend: %v", index+1, flags)
		}
	}
}

func TestPermanentHostErrorWaitsForReconnectAndReloadsHost(t *testing.T) {
	repository := &repositoryStub{host: HostSpec{ID: "host", Address: "old", User: "user", Fingerprint: "SHA256:test", Credential: PasswordCredential{Password: "old"}}}
	var starts atomic.Int32
	backends := &launcherStub{
		startFunc: func(_ context.Context, spec SessionSpec, _ Persistence, _ bool) (backend, Persistence, error) {
			starts.Add(1)
			if spec.Host.Address == "old" {
				return nil, "", permanentStartError(errors.New("bad credentials"))
			}
			return newBackendStub(), PersistenceTmux, nil
		},
	}
	manager := managerWithLauncher(t, backends, func(cfg *Config) { cfg.Repository = repository })
	defer manager.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := manager.Create(SessionSpec{
		ID:          "host-refresh",
		Persistence: PersistenceAuto,
		Host:        &HostSpec{ID: "host", Address: "captured", User: "user", Fingerprint: "SHA256:test", Credential: PasswordCredential{Password: "captured"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitState(ctx, t, manager, "host-refresh", StateError)
	attachment, err := manager.Attach(ctx, "host-refresh", "browser", 0)
	if err != nil {
		t.Fatal(err)
	}
	// Negative assertion: a retry after the attach needs time to appear, so a
	// shorter sleep loses sensitivity rather than making the test flaky.
	time.Sleep(30 * time.Millisecond)
	if got := starts.Load(); got != 1 {
		t.Fatalf("permanent error retried after attach: starts=%d", got)
	}
	repository.setHost(HostSpec{ID: "host", Address: "new", User: "user", Fingerprint: "SHA256:test", Credential: PasswordCredential{Password: "new"}})
	if woken := manager.RefreshHost("host"); woken != 1 {
		t.Fatalf("RefreshHost woke %d sessions, want 1", woken)
	}
	waitState(ctx, t, manager, "host-refresh", StateRunning)
	if starts.Load() != 2 || repository.loadCount() < 2 {
		t.Fatalf("starts=%d host loads=%d, want fresh load per attempt", starts.Load(), repository.loadCount())
	}
	_ = attachment.Close()
}

func TestReconnectWakesPermanentErrorAndTimedBackoff(t *testing.T) {
	t.Run("permanent error", func(t *testing.T) {
		var reachable atomic.Bool
		backends := &launcherStub{startFunc: func(context.Context, SessionSpec, Persistence, bool) (backend, Persistence, error) {
			if !reachable.Load() {
				return nil, "", permanentStartError(errors.New("host key mismatch"))
			}
			return newBackendStub(), PersistenceTmux, nil
		}}
		manager := managerWithLauncher(t, backends)
		defer manager.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := manager.Create(SessionSpec{ID: "reconnect-permanent", Persistence: PersistenceTmux}); err != nil {
			t.Fatal(err)
		}
		waitState(ctx, t, manager, "reconnect-permanent", StateError)
		reachable.Store(true)
		if err := manager.Reconnect("reconnect-permanent"); err != nil {
			t.Fatal(err)
		}
		waitState(ctx, t, manager, "reconnect-permanent", StateRunning)
	})

	t.Run("timed backoff", func(t *testing.T) {
		var reachable atomic.Bool
		backends := &launcherStub{startFunc: func(context.Context, SessionSpec, Persistence, bool) (backend, Persistence, error) {
			if !reachable.Load() {
				return nil, "", errors.New("dial tcp: connection refused")
			}
			return newBackendStub(), PersistenceTmux, nil
		}}
		// An hour of backoff: only Reconnect can make this session run.
		manager := managerWithLauncher(t, backends, func(cfg *Config) {
			cfg.ReconnectMin = time.Hour
			cfg.ReconnectMax = time.Hour
		})
		defer manager.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := manager.Create(SessionSpec{ID: "reconnect-backoff", Persistence: PersistenceTmux}); err != nil {
			t.Fatal(err)
		}
		waitState(ctx, t, manager, "reconnect-backoff", StateDisconnected)
		if _, err := manager.Attach(ctx, "reconnect-backoff", "browser", 0); err != nil {
			t.Fatal(err)
		}
		reachable.Store(true)
		if err := manager.Reconnect("reconnect-backoff"); err != nil {
			t.Fatal(err)
		}
		waitState(ctx, t, manager, "reconnect-backoff", StateRunning)
	})

	t.Run("unknown session", func(t *testing.T) {
		manager := managerWithLauncher(t, staticLauncher(newBackendStub(), PersistenceTmux))
		defer manager.Close()
		if err := manager.Reconnect("missing"); !errors.Is(err, ErrSessionNotFound) {
			t.Fatalf("Reconnect on a missing session = %v, want ErrSessionNotFound", err)
		}
	})
}

func TestDiscardWorksInAnyStateAndNeverKillsTheBackend(t *testing.T) {
	countingTerminations := func(start func(context.Context, SessionSpec, Persistence, bool) (backend, Persistence, error), terminations *atomic.Int32) *launcherStub {
		return &launcherStub{
			startFunc: start,
			terminateFunc: func(context.Context, SessionSpec, Persistence) error {
				terminations.Add(1)
				return nil
			},
		}
	}

	t.Run("permanent start error", func(t *testing.T) {
		var terminations atomic.Int32
		manager := managerWithLauncher(t, countingTerminations(func(context.Context, SessionSpec, Persistence, bool) (backend, Persistence, error) {
			return nil, "", permanentStartError(errors.New("host unavailable"))
		}, &terminations))
		defer manager.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := manager.Create(SessionSpec{ID: "discard-error", Persistence: PersistenceTmux}); err != nil {
			t.Fatal(err)
		}
		waitState(ctx, t, manager, "discard-error", StateError)
		a, err := manager.Attach(ctx, "discard-error", "browser", 0)
		if err != nil {
			t.Fatal(err)
		}
		if err := manager.Discard(ctx, "discard-error"); err != nil {
			t.Fatal(err)
		}
		if terminations.Load() != 0 {
			t.Fatal("Discard invoked destructive backend termination")
		}
		if _, err := manager.Status("discard-error"); !errors.Is(err, ErrSessionNotFound) {
			t.Fatalf("Status after Discard = %v, want ErrSessionNotFound", err)
		}
		if reason := awaitCloseReason(ctx, t, a); reason != AttachmentExited {
			t.Fatalf("discard close reason = %q, want exited", reason)
		}
	})

	t.Run("running backend", func(t *testing.T) {
		var terminations atomic.Int32
		b := newBackendStub()
		manager := managerWithLauncher(t, countingTerminations(func(context.Context, SessionSpec, Persistence, bool) (backend, Persistence, error) {
			return b, PersistenceTmux, nil
		}, &terminations))
		defer manager.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := manager.Create(SessionSpec{ID: "discard-running", Persistence: PersistenceTmux}); err != nil {
			t.Fatal(err)
		}
		waitState(ctx, t, manager, "discard-running", StateRunning)
		if err := manager.Discard(ctx, "discard-running"); err != nil {
			t.Fatalf("Discard on a running session = %v, want success", err)
		}
		if terminations.Load() != 0 {
			t.Fatal("Discard killed a running persistent backend")
		}
		if !b.isClosed() {
			t.Fatal("Discard left the data connection attached")
		}
		if _, err := manager.Status("discard-running"); !errors.Is(err, ErrSessionNotFound) {
			t.Fatalf("Status after Discard = %v, want ErrSessionNotFound", err)
		}
	})
}

func TestAttachmentStatesDeliverTheNewestStatus(t *testing.T) {
	b := newBackendStub()
	manager := managerWithLauncher(t, staticLauncher(b, PersistenceTmux))
	defer manager.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := manager.Create(SessionSpec{ID: "states", Persistence: PersistenceTmux, Generation: 7}); err != nil {
		t.Fatal(err)
	}
	a, err := manager.Attach(ctx, "states", "first", 0)
	if err != nil {
		t.Fatal(err)
	}
	status := awaitState(ctx, t, a, func(status SessionStatus) bool { return status.State == StateRunning })
	if status.Generation != 7 || status.Clients != 1 {
		t.Fatalf("state event = %+v, want generation 7 and one client", status)
	}
	if _, err := manager.Attach(ctx, "states", "second", 0); err != nil {
		t.Fatal(err)
	}
	if status := awaitState(ctx, t, a, func(status SessionStatus) bool { return status.Clients == 2 }); status.State != StateRunning {
		t.Fatalf("client-count event = %+v", status)
	}
	_ = b.Close()
	if status := awaitState(ctx, t, a, func(status SessionStatus) bool { return status.State == StateExited }); status.State != StateExited {
		t.Fatalf("exit event = %+v", status)
	}
}

func TestAttachmentCloseReasonsAndWriterNotifications(t *testing.T) {
	t.Run("server shutdown", func(t *testing.T) {
		b := newBackendStub()
		manager := managerWithLauncher(t, staticLauncher(b, PersistenceTmux))
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := manager.Create(SessionSpec{ID: "shutdown-reason", Persistence: PersistenceTmux}); err != nil {
			t.Fatal(err)
		}
		a, err := manager.Attach(ctx, "shutdown-reason", "browser", 0)
		if err != nil {
			t.Fatal(err)
		}
		if err := manager.CloseContext(ctx); err != nil {
			t.Fatal(err)
		}
		if reason := awaitCloseReason(ctx, t, a); reason != AttachmentServerShutdown {
			t.Fatalf("reason = %q", reason)
		}
	})

	t.Run("natural exit", func(t *testing.T) {
		b := newBackendStub()
		manager := managerWithLauncher(t, staticLauncher(b, PersistenceNone))
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := manager.Create(SessionSpec{ID: "exit-reason", Persistence: PersistenceNone}); err != nil {
			t.Fatal(err)
		}
		a, err := manager.Attach(ctx, "exit-reason", "browser", 0)
		if err != nil {
			t.Fatal(err)
		}
		waitState(ctx, t, manager, "exit-reason", StateRunning)
		_ = b.Close()
		if reason := awaitCloseReason(ctx, t, a); reason != AttachmentExited {
			t.Fatalf("reason = %q", reason)
		}
		_ = manager.Close()
	})

	t.Run("writer takeover", func(t *testing.T) {
		b := newBackendStub()
		manager := managerWithLauncher(t, staticLauncher(b, PersistenceTmux))
		defer manager.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := manager.Create(SessionSpec{ID: "writer-events", Persistence: PersistenceTmux}); err != nil {
			t.Fatal(err)
		}
		first, err := manager.Attach(ctx, "writer-events", "first", 0)
		if err != nil {
			t.Fatal(err)
		}
		second, err := manager.Attach(ctx, "writer-events", "second", 0)
		if err != nil {
			t.Fatal(err)
		}
		if got := awaitWriter(ctx, t, first); !got {
			t.Fatal("first attachment initial writer event was false")
		}
		if got := awaitWriter(ctx, t, second); got {
			t.Fatal("second attachment initial writer event was true")
		}
		if err := second.TakeControl(); err != nil {
			t.Fatal(err)
		}
		if got := awaitWriter(ctx, t, first); got {
			t.Fatal("old writer did not receive immediate false event")
		}
		if got := awaitWriter(ctx, t, second); !got {
			t.Fatal("new writer did not receive immediate true event")
		}
	})
}

func TestLinuxPTYEIOIsTreatedAsCleanExit(t *testing.T) {
	var starts atomic.Int32
	b := newBackendStub()
	b.readErr = syscall.EIO
	backends := &launcherStub{startFunc: func(context.Context, SessionSpec, Persistence, bool) (backend, Persistence, error) {
		starts.Add(1)
		return b, PersistenceTmux, nil
	}}
	manager := managerWithLauncher(t, backends)
	defer manager.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := manager.Create(SessionSpec{ID: "pty-eio", Persistence: PersistenceTmux}); err != nil {
		t.Fatal(err)
	}
	waitState(ctx, t, manager, "pty-eio", StateRunning)
	_ = b.Close()
	waitState(ctx, t, manager, "pty-eio", StateExited)
	if starts.Load() != 1 {
		t.Fatalf("EIO caused backend recreation: starts=%d", starts.Load())
	}
}

func TestCloseContextReturnsAtDeadlineWhenLauncherIgnoresCancellation(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	backends := &launcherStub{startFunc: func(ctx context.Context, _ SessionSpec, _ Persistence, _ bool) (backend, Persistence, error) {
		close(entered)
		<-release
		return nil, "", ctx.Err()
	}}
	manager := managerWithLauncher(t, backends)
	err := manager.Create(SessionSpec{ID: "close-deadline", Persistence: PersistenceTmux})
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	started := time.Now()
	err = manager.CloseContext(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("CloseContext error = %v", err)
	}
	if time.Since(started) > 300*time.Millisecond {
		t.Fatalf("CloseContext ignored deadline: %s", time.Since(started))
	}
	close(release)
}

func TestDirectAttachReplaysTranscriptAndNudgesWhenTruncated(t *testing.T) {
	log := &fixedLog{oldest: 5, newest: 7}
	b := newBackendStub()
	// A direct PTY backend has no multiplexer command to offer.
	b.redrawErr = errRedrawUnsupported
	manager, err := NewManager(Config{
		Transcripts: fixedFactory{log: log},
		launcher:    staticLauncher(b, PersistenceNone),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := manager.Create(SessionSpec{ID: "replay-bounds", Persistence: PersistenceNone, Cols: 80, Rows: 24}); err != nil {
		t.Fatal(err)
	}
	waitState(ctx, t, manager, "replay-bounds", StateRunning)
	drainSizes(b)

	a, err := manager.Attach(ctx, "replay-bounds", "browser", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !a.Truncated || a.OldestSequence != 5 || a.LatestSequence != 7 || replaySequences(a) != "5,6,7" {
		t.Fatalf("attachment replay = truncated:%v oldest:%d latest:%d frames:%s", a.Truncated, a.OldestSequence, a.LatestSequence, replaySequences(a))
	}
	// The cut-short replay is followed by a one-row nudge that makes
	// full-screen programs repaint.
	if got := awaitSizes(ctx, t, b, 2); !slices.Equal(got, [][2]uint16{{80, 23}, {80, 24}}) {
		t.Fatalf("nudge sizes = %v, want 80x23 then 80x24", got)
	}

	// A client that is caught up replays exactly the gap and is left alone.
	caughtUp, err := manager.Attach(ctx, "replay-bounds", "browser-2", 6)
	if err != nil {
		t.Fatal(err)
	}
	if caughtUp.Truncated || replaySequences(caughtUp) != "7" {
		t.Fatalf("gap replay = truncated:%v frames:%s, want frame 7 only", caughtUp.Truncated, replaySequences(caughtUp))
	}
	expectNoSizeChange(t, b)
}

func TestMultiplexerAttachSkipsTranscriptAndRepaints(t *testing.T) {
	log := &fixedLog{oldest: 1, newest: 7}
	b := newBackendStub()
	manager, err := NewManager(Config{
		Transcripts: fixedFactory{log: log},
		launcher:    staticLauncher(b, PersistenceTmux),
		// Frames 6 and 7 are two bytes; a one-byte cap forces the repaint path.
		ReplayGapBytes: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := manager.Create(SessionSpec{ID: "mux-replay", Persistence: PersistenceTmux, Cols: 80, Rows: 24}); err != nil {
		t.Fatal(err)
	}
	waitState(ctx, t, manager, "mux-replay", StateRunning)
	drainSizes(b)

	// From scratch: nothing to replay, the multiplexer paints the screen.
	fresh, err := manager.Attach(ctx, "mux-replay", "fresh", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(fresh.Initial) != 0 || fresh.Truncated || fresh.LatestSequence != 7 {
		t.Fatalf("fresh multiplexer attach = %d frame(s), truncated:%v latest:%d; want no frames", len(fresh.Initial), fresh.Truncated, fresh.LatestSequence)
	}
	awaitRedraw(ctx, t, b)

	// A short gap is cheaper to replay than to repaint.
	gap, err := manager.Attach(ctx, "mux-replay", "gap", 6)
	if err != nil {
		t.Fatal(err)
	}
	if replaySequences(gap) != "7" || gap.Truncated {
		t.Fatalf("one-frame gap replay = %s, truncated:%v; want frame 7", replaySequences(gap), gap.Truncated)
	}
	expectNoRedraw(t, b)

	// A gap over the byte cap is skipped in favour of a repaint.
	wide, err := manager.Attach(ctx, "mux-replay", "wide", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(wide.Initial) != 0 || wide.Truncated {
		t.Fatalf("over-cap gap replayed %d frame(s), truncated:%v; want a repaint instead", len(wide.Initial), wide.Truncated)
	}
	awaitRedraw(ctx, t, b)

	// A client that is already current needs neither.
	current, err := manager.Attach(ctx, "mux-replay", "current", 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(current.Initial) != 0 {
		t.Fatalf("current client replayed %d frame(s)", len(current.Initial))
	}
	expectNoRedraw(t, b)
	expectNoSizeChange(t, b)
}

// burstBackend prints a start-up burst once and then stays silent until closed,
// like a multiplexer client that has painted the screen.
type burstBackend struct {
	*backendStub
	burst []byte
}

func (b *burstBackend) Read(p []byte) (int, error) {
	if len(b.burst) != 0 {
		n := copy(p, b.burst)
		b.burst = b.burst[n:]
		return n, nil
	}
	return b.backendStub.Read(p)
}

func TestMultiplexerFreshAttachGetsAttachPreludeOnly(t *testing.T) {
	setup := []byte("\x1b[?1049h\x1b[?1h\x1b=\x1b[?2004h\x1b[H\x1b[2Jprompt$ ")
	b := &burstBackend{backendStub: newBackendStub(), burst: append([]byte(nil), setup...)}
	// The backend starts only once the first client is subscribed, so that
	// client sees the burst live rather than as a prelude.
	release := make(chan struct{})
	launcher := &launcherStub{startFunc: func(ctx context.Context, _ SessionSpec, _ Persistence, _ bool) (backend, Persistence, error) {
		select {
		case <-release:
			return b, PersistenceTmux, nil
		case <-ctx.Done():
			return nil, "", ctx.Err()
		}
	}}
	manager := managerWithLauncher(t, launcher)
	defer manager.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := manager.Create(SessionSpec{ID: "prelude", Persistence: PersistenceTmux}); err != nil {
		t.Fatal(err)
	}
	live, err := manager.Attach(ctx, "prelude", "live", 0)
	if err != nil {
		t.Fatal(err)
	}
	close(release)
	waitState(ctx, t, manager, "prelude", StateRunning)
	waitForOutput(ctx, t, live.Frames, "prompt$ ")

	fresh, err := manager.Attach(ctx, "prelude", "fresh", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(fresh.Prelude, setup) || len(fresh.Initial) != 0 {
		t.Fatalf("fresh attach prelude = %q with %d transcript frame(s), want the start-up burst alone", fresh.Prelude, len(fresh.Initial))
	}
	// A reconnecting browser still has its terminal state; only the gap matters.
	back, err := manager.Attach(ctx, "prelude", "back", fresh.LatestSequence)
	if err != nil {
		t.Fatal(err)
	}
	if len(back.Prelude) != 0 || len(back.Initial) != 0 {
		t.Fatalf("caught-up reconnect got prelude %q and %d frame(s)", back.Prelude, len(back.Initial))
	}
}

func TestMultiplexerRepaintFallsBackToSizeNudge(t *testing.T) {
	log := &fixedLog{oldest: 1, newest: 3}
	b := newBackendStub()
	b.redrawErr = errors.New("tmux client is gone")
	manager, err := NewManager(Config{
		Transcripts: fixedFactory{log: log},
		launcher:    staticLauncher(b, PersistenceScreen),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := manager.Create(SessionSpec{ID: "mux-nudge", Persistence: PersistenceScreen, Cols: 120, Rows: 1}); err != nil {
		t.Fatal(err)
	}
	waitState(ctx, t, manager, "mux-nudge", StateRunning)
	drainSizes(b)
	if _, err := manager.Attach(ctx, "mux-nudge", "browser", 0); err != nil {
		t.Fatal(err)
	}
	awaitRedraw(ctx, t, b)
	// A one-row terminal cannot shrink, so the nudge grows it instead.
	if got := awaitSizes(ctx, t, b, 2); !slices.Equal(got, [][2]uint16{{120, 2}, {120, 1}}) {
		t.Fatalf("fallback nudge sizes = %v, want 120x2 then 120x1", got)
	}
}

func replaySequences(a *Attachment) string {
	parts := make([]string, 0, len(a.Initial))
	for _, frame := range a.Initial {
		parts = append(parts, fmt.Sprint(frame.Sequence))
	}
	return strings.Join(parts, ",")
}

func drainSizes(b *backendStub) {
	for {
		select {
		case <-b.sizes:
		default:
			return
		}
	}
}

func awaitSizes(ctx context.Context, t *testing.T, b *backendStub, count int) [][2]uint16 {
	t.Helper()
	var sizes [][2]uint16
	for len(sizes) < count {
		select {
		case size := <-b.sizes:
			sizes = append(sizes, size)
		case <-ctx.Done():
			t.Fatalf("saw %d of %d size changes: %v", len(sizes), count, sizes)
		}
	}
	return sizes
}

func awaitRedraw(ctx context.Context, t *testing.T, b *backendStub) {
	t.Helper()
	select {
	case <-b.redraws:
	case <-ctx.Done():
		t.Fatal("backend was not asked to repaint")
	}
}

// expectNoRedraw and expectNoSizeChange wait long enough for the attach
// goroutine to have run; the repaint is asynchronous by design.
func expectNoRedraw(t *testing.T, b *backendStub) {
	t.Helper()
	select {
	case <-b.redraws:
		t.Fatal("backend was asked to repaint")
	case <-time.After(3 * nudgeSettle):
	}
}

func expectNoSizeChange(t *testing.T, b *backendStub) {
	t.Helper()
	select {
	case size := <-b.sizes:
		t.Fatalf("backend was resized to %v", size)
	case <-time.After(3 * nudgeSettle):
	}
}

func TestTranscriptAppendFailureDoesNotPublishOrAdvanceSequence(t *testing.T) {
	log := &toggleLog{newest: 3, fail: true}
	manager, err := NewManager(Config{
		Transcripts: fixedFactory{log: log},
		launcher:    staticLauncher(newBackendStub(), PersistenceTmux),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := manager.Create(SessionSpec{ID: "append-failure", Persistence: PersistenceTmux}); err != nil {
		t.Fatal(err)
	}
	waitState(ctx, t, manager, "append-failure", StateRunning)
	a, err := manager.Attach(ctx, "append-failure", "browser", 3)
	if err != nil {
		t.Fatal(err)
	}
	s, err := manager.session("append-failure")
	if err != nil {
		t.Fatal(err)
	}
	s.publish([]byte("dropped"))
	select {
	case frame := <-a.Frames:
		t.Fatalf("frame was published despite transcript failure: %+v", frame)
	default:
	}
	status, err := manager.Status("append-failure")
	if err != nil {
		t.Fatal(err)
	}
	if status.LastError == "" {
		t.Fatalf("status after append failure = %+v", status)
	}

	log.setFail(false)
	s.publish([]byte("persisted"))
	select {
	case frame := <-a.Frames:
		// Sequence 4 proves the failed append did not consume a sequence number.
		if frame.Sequence != 4 || string(frame.Data) != "persisted" {
			t.Fatalf("frame after append recovery = %+v", frame)
		}
	case <-ctx.Done():
		t.Fatal("successful append was not published")
	}
}

type fixedFactory struct{ log transcript.Log }

func (f fixedFactory) Open(string) (transcript.Log, error) { return f.log, nil }

type fixedLog struct {
	oldest uint64
	newest uint64
}

func (l *fixedLog) Append([]byte) (uint64, error) { l.newest++; return l.newest, nil }

func (l *fixedLog) Replay(after uint64, limit int, yield func(uint64, time.Time, []byte) error) error {
	count := 0
	for sequence := l.oldest; sequence <= l.newest; sequence++ {
		if sequence <= after {
			continue
		}
		if limit > 0 && count == limit {
			break
		}
		if err := yield(sequence, time.Now(), []byte{byte(sequence)}); err != nil {
			return err
		}
		count++
	}
	return nil
}

func (l *fixedLog) Bounds() (uint64, uint64) { return l.oldest, l.newest }
func (*fixedLog) Close() error               { return nil }

type toggleLog struct {
	mu     sync.Mutex
	newest uint64
	fail   bool
}

func (l *toggleLog) Append([]byte) (uint64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.fail {
		return 0, errors.New("transcript unavailable")
	}
	l.newest++
	return l.newest, nil
}

func (l *toggleLog) Replay(uint64, int, func(uint64, time.Time, []byte) error) error {
	return nil
}

func (l *toggleLog) Bounds() (uint64, uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.newest == 0 {
		return 0, 0
	}
	return 1, l.newest
}

func (*toggleLog) Close() error { return nil }

func (l *toggleLog) setFail(fail bool) {
	l.mu.Lock()
	l.fail = fail
	l.mu.Unlock()
}
