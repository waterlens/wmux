package terminal

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/waterlens/wmux/internal/transcript"
)

type launcherStub struct {
	startFunc     func(context.Context, SessionSpec, Persistence) (backend, Persistence, error)
	terminateFunc func(context.Context, SessionSpec, Persistence) error
}

func (l *launcherStub) start(ctx context.Context, spec SessionSpec, persistence Persistence) (backend, Persistence, error) {
	return l.startFunc(ctx, spec, persistence)
}

func (l *launcherStub) terminate(ctx context.Context, spec SessionSpec, persistence Persistence) error {
	if l.terminateFunc == nil {
		return nil
	}
	return l.terminateFunc(ctx, spec, persistence)
}

type backendStub struct {
	closed       chan struct{}
	closeOnce    sync.Once
	readErr      error
	waitErr      error
	reconnect    bool
	writeStarted chan struct{}
	blockWrite   bool
}

func newBackendStub() *backendStub {
	return &backendStub{closed: make(chan struct{})}
}

func (b *backendStub) Read([]byte) (int, error) {
	<-b.closed
	if b.readErr != nil {
		return 0, b.readErr
	}
	return 0, io.EOF
}

func (b *backendStub) Write(p []byte) (int, error) {
	return b.WriteContext(context.Background(), p)
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

func (b *backendStub) Resize(uint16, uint16) error { return nil }
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

func managerWithLauncher(t *testing.T, launcher backendLauncher, mutate ...func(*Config)) *Manager {
	t.Helper()
	directory, err := transcript.NewDirectory(transcript.DirectoryConfig{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		Transcripts:     directory,
		ReconnectMin:    5 * time.Millisecond,
		ReconnectMax:    10 * time.Millisecond,
		ShutdownTimeout: time.Second,
		launcher:        launcher,
	}
	for _, change := range mutate {
		change(&cfg)
	}
	manager, err := NewManager(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return manager
}

func TestTerminateFailureLeavesRunLoopAndAttachmentAlive(t *testing.T) {
	b := newBackendStub()
	var terminations atomic.Int32
	launcher := &launcherStub{
		startFunc: func(context.Context, SessionSpec, Persistence) (backend, Persistence, error) {
			return b, PersistenceTmux, nil
		},
		terminateFunc: func(context.Context, SessionSpec, Persistence) error {
			if terminations.Add(1) == 1 {
				return errors.New("control connection unavailable")
			}
			return nil
		},
	}
	manager := managerWithLauncher(t, launcher)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := manager.Create(ctx, SessionSpec{ID: "terminate-recovery", Persistence: PersistenceTmux}); err != nil {
		t.Fatal(err)
	}
	attachment, err := manager.Attach(ctx, "terminate-recovery", "browser", 0)
	if err != nil {
		t.Fatal(err)
	}
	waitRunning(t, ctx, manager, "terminate-recovery")
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
	if _, err := attachment.Write([]byte("still alive")); err != nil {
		t.Fatalf("write after failed termination: %v", err)
	}
	if err := manager.Terminate(ctx, "terminate-recovery"); err != nil {
		t.Fatal(err)
	}
	if reason := awaitCloseReason(t, ctx, attachment); reason != AttachmentExited {
		t.Fatalf("close reason = %q, want exited", reason)
	}
}

func TestBlockedWriteDoesNotBlockTerminate(t *testing.T) {
	b := newBackendStub()
	b.blockWrite = true
	b.writeStarted = make(chan struct{})
	launcher := &launcherStub{
		startFunc: func(context.Context, SessionSpec, Persistence) (backend, Persistence, error) {
			return b, PersistenceTmux, nil
		},
	}
	manager := managerWithLauncher(t, launcher)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := manager.Create(ctx, SessionSpec{ID: "blocked-write", Persistence: PersistenceTmux}); err != nil {
		t.Fatal(err)
	}
	attachment, err := manager.Attach(ctx, "blocked-write", "browser", 0)
	if err != nil {
		t.Fatal(err)
	}
	waitRunning(t, ctx, manager, "blocked-write")
	writeDone := make(chan error, 1)
	go func() {
		_, err := attachment.Write([]byte("blocked"))
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

func TestWriteContextCancelsBlockedBackendWrite(t *testing.T) {
	b := newBackendStub()
	b.blockWrite = true
	b.writeStarted = make(chan struct{})
	manager := managerWithLauncher(t, &launcherStub{startFunc: func(context.Context, SessionSpec, Persistence) (backend, Persistence, error) {
		return b, PersistenceTmux, nil
	}})
	defer manager.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _ = manager.Create(ctx, SessionSpec{ID: "write-context", Persistence: PersistenceTmux})
	a, err := manager.Attach(ctx, "write-context", "browser", 0)
	if err != nil {
		t.Fatal(err)
	}
	waitRunning(t, ctx, manager, "write-context")
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
}

type sizeRecordingBackend struct {
	*backendStub
	mu      sync.Mutex
	sizes   [][2]uint16
	resized chan [2]uint16
}

type closeBlockedResizeBackend struct {
	*backendStub
	entered chan struct{}
	once    sync.Once
}

func (b *closeBlockedResizeBackend) Resize(uint16, uint16) error {
	b.once.Do(func() { close(b.entered) })
	<-b.closed
	return io.ErrClosedPipe
}

func (b *sizeRecordingBackend) Resize(cols, rows uint16) error {
	size := [2]uint16{cols, rows}
	b.mu.Lock()
	b.sizes = append(b.sizes, size)
	b.mu.Unlock()
	select {
	case b.resized <- size:
	default:
	}
	return nil
}

func TestResizeDuringConnectingIsAcceptedReconciledAndRetained(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	recorder := &sizeRecordingBackend{
		backendStub: newBackendStub(),
		resized:     make(chan [2]uint16, 1),
	}
	var launchedCols, launchedRows atomic.Int32
	launcher := &launcherStub{startFunc: func(_ context.Context, spec SessionSpec, _ Persistence) (backend, Persistence, error) {
		launchedCols.Store(int32(spec.Cols))
		launchedRows.Store(int32(spec.Rows))
		close(entered)
		<-release
		return recorder, PersistenceTmux, nil
	}}
	manager := managerWithLauncher(t, launcher)
	defer manager.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := manager.Create(ctx, SessionSpec{ID: "connecting-resize", Persistence: PersistenceTmux, Cols: 120, Rows: 36}); err != nil {
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
	s, err := manager.session("connecting-resize")
	if err != nil {
		t.Fatal(err)
	}
	record := s.record(true)
	if record.Cols != 200 || record.Rows != 50 {
		t.Fatalf("runtime persistence snapshot size = %dx%d, want 200x50", record.Cols, record.Rows)
	}
	close(release)
	waitRunning(t, ctx, manager, "connecting-resize")
	select {
	case size := <-recorder.resized:
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
	b := &closeBlockedResizeBackend{backendStub: newBackendStub(), entered: make(chan struct{})}
	manager := managerWithLauncher(t, &launcherStub{startFunc: func(context.Context, SessionSpec, Persistence) (backend, Persistence, error) {
		close(launchEntered)
		<-launchRelease
		return b, PersistenceTmux, nil
	}})
	defer manager.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := manager.Create(ctx, SessionSpec{ID: "resize-terminate", Persistence: PersistenceTmux, Cols: 120, Rows: 36}); err != nil {
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
	case <-b.entered:
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

func TestResizeWaitsOutOlderRepositorySnapshot(t *testing.T) {
	repository := &orderedSaveRepository{secondStart: make(chan struct{}), release: make(chan struct{})}
	b := newBackendStub()
	manager := managerWithLauncher(t, &launcherStub{startFunc: func(context.Context, SessionSpec, Persistence) (backend, Persistence, error) {
		return b, PersistenceTmux, nil
	}}, func(cfg *Config) { cfg.Repository = repository })
	defer manager.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := manager.Create(ctx, SessionSpec{ID: "resize-save-order", Persistence: PersistenceAuto, Cols: 120, Rows: 36}); err != nil {
		t.Fatal(err)
	}
	a, err := manager.Attach(ctx, "resize-save-order", "browser", 0)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-repository.secondStart:
	case <-ctx.Done():
		t.Fatal("resolved-backend save did not start")
	}
	resized := make(chan error, 1)
	go func() { resized <- a.Resize(200, 50) }()
	select {
	case err := <-resized:
		t.Fatalf("Resize returned before the older repository snapshot completed: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(repository.release)
	if err := <-resized; err != nil {
		t.Fatal(err)
	}
	s, err := manager.session("resize-save-order")
	if err != nil {
		t.Fatal(err)
	}
	record := s.record(true)
	if record.Cols != 200 || record.Rows != 50 {
		t.Fatalf("runtime size after save barrier = %dx%d, want 200x50", record.Cols, record.Rows)
	}
}

type finiteReadBackend struct {
	*backendStub
	reader *bytes.Reader
}

func (b *finiteReadBackend) Read(p []byte) (int, error) { return b.reader.Read(p) }
func (*finiteReadBackend) Wait(context.Context) error   { return nil }

func TestConsumeBoundsLargeOutputFrames(t *testing.T) {
	data := bytes.Repeat([]byte("wmux-output-"), 10<<10)
	b := &finiteReadBackend{backendStub: newBackendStub(), reader: bytes.NewReader(data)}
	manager := managerWithLauncher(t, &launcherStub{startFunc: func(context.Context, SessionSpec, Persistence) (backend, Persistence, error) {
		return b, PersistenceNone, nil
	}})
	defer manager.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := manager.Create(ctx, SessionSpec{ID: "bounded-output", Persistence: PersistenceNone}); err != nil {
		t.Fatal(err)
	}
	waitState(t, ctx, manager, "bounded-output", StateExited)
	a, err := manager.Attach(ctx, "bounded-output", "replay", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Initial) < 2 {
		t.Fatalf("large output produced %d frame(s), want multiple bounded frames", len(a.Initial))
	}
	joined := make([]byte, 0, len(data))
	for _, frame := range a.Initial {
		if len(frame.Data) > 32<<10 {
			t.Fatalf("output frame has %d bytes, want at most 32 KiB", len(frame.Data))
		}
		joined = append(joined, frame.Data...)
	}
	if !bytes.Equal(joined, data) {
		t.Fatalf("bounded output replay has %d bytes, want %d", len(joined), len(data))
	}
}

type orderedSaveRepository struct {
	mu          sync.Mutex
	calls       int
	records     []SessionRecord
	secondStart chan struct{}
	release     chan struct{}
}

func (r *orderedSaveRepository) SaveSession(_ context.Context, record SessionRecord) error {
	r.mu.Lock()
	r.calls++
	call := r.calls
	r.mu.Unlock()
	if call == 2 {
		close(r.secondStart)
		<-r.release
	}
	r.mu.Lock()
	r.records = append(r.records, record)
	r.mu.Unlock()
	return nil
}

func (*orderedSaveRepository) ListSessions(context.Context) ([]SessionRecord, error) {
	return nil, nil
}

func (*orderedSaveRepository) LoadHost(context.Context, string) (HostSpec, error) {
	return HostSpec{}, errors.New("unexpected host load")
}

func TestTerminationCannotBeOverwrittenByOlderActiveSave(t *testing.T) {
	repository := &orderedSaveRepository{secondStart: make(chan struct{}), release: make(chan struct{})}
	b := newBackendStub()
	killStarted := make(chan struct{})
	manager := managerWithLauncher(t, &launcherStub{
		startFunc: func(context.Context, SessionSpec, Persistence) (backend, Persistence, error) {
			return b, PersistenceTmux, nil
		},
		terminateFunc: func(context.Context, SessionSpec, Persistence) error {
			close(killStarted)
			return nil
		},
	}, func(cfg *Config) { cfg.Repository = repository })
	defer manager.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := manager.Create(ctx, SessionSpec{ID: "save-order", Persistence: PersistenceAuto}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-repository.secondStart:
	case <-ctx.Done():
		t.Fatal("resolved-backend active save did not start")
	}
	terminated := make(chan error, 1)
	go func() { terminated <- manager.Terminate(ctx, "save-order") }()
	select {
	case <-killStarted:
	case <-ctx.Done():
		t.Fatal("Terminate did not reach the exact backend kill")
	}
	close(repository.release)
	if err := <-terminated; err != nil {
		t.Fatal(err)
	}
	repository.mu.Lock()
	records := append([]SessionRecord(nil), repository.records...)
	repository.mu.Unlock()
	if len(records) < 3 || records[len(records)-1].Active {
		t.Fatalf("save order ended with an active record: %+v", records)
	}
}

type repositoryStub struct {
	mu    sync.Mutex
	host  HostSpec
	loads int
}

func (r *repositoryStub) SaveSession(context.Context, SessionRecord) error { return nil }
func (r *repositoryStub) ListSessions(context.Context) ([]SessionRecord, error) {
	return nil, nil
}
func (r *repositoryStub) LoadHost(context.Context, string) (HostSpec, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.loads++
	return cloneHost(r.host), nil
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

func cloneHost(host HostSpec) HostSpec {
	copy := host
	return copy
}

func TestPermanentHostErrorWaitsForRefreshAndReloadsHost(t *testing.T) {
	repository := &repositoryStub{host: HostSpec{ID: "host", Address: "old", User: "user", Fingerprint: "SHA256:test", Credential: PasswordCredential{Password: "old"}}}
	var starts atomic.Int32
	launcher := &launcherStub{
		startFunc: func(_ context.Context, spec SessionSpec, _ Persistence) (backend, Persistence, error) {
			starts.Add(1)
			if spec.Host.Address == "old" {
				return nil, "", permanentStartError(errors.New("bad credentials"))
			}
			return newBackendStub(), PersistenceTmux, nil
		},
	}
	manager := managerWithLauncher(t, launcher, func(cfg *Config) { cfg.Repository = repository })
	defer manager.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := manager.Create(ctx, SessionSpec{
		ID:          "host-refresh",
		Persistence: PersistenceAuto,
		Host:        &HostSpec{ID: "host", Address: "captured", User: "user", Fingerprint: "SHA256:test", Credential: PasswordCredential{Password: "captured"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, ctx, manager, "host-refresh", StateError)
	attachment, err := manager.Attach(ctx, "host-refresh", "browser", 0)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)
	if got := starts.Load(); got != 1 {
		t.Fatalf("permanent error retried after attach: starts=%d", got)
	}
	repository.setHost(HostSpec{ID: "host", Address: "new", User: "user", Fingerprint: "SHA256:test", Credential: PasswordCredential{Password: "new"}})
	if woken := manager.RefreshHost("host"); woken != 1 {
		t.Fatalf("RefreshHost woke %d sessions, want 1", woken)
	}
	waitRunning(t, ctx, manager, "host-refresh")
	if starts.Load() != 2 || repository.loadCount() < 2 {
		t.Fatalf("starts=%d host loads=%d, want fresh load per attempt", starts.Load(), repository.loadCount())
	}
	_ = attachment.Close()
}

func TestDiscardRemovesOnlyInactiveRuntimeWithoutBackendTermination(t *testing.T) {
	var terminations atomic.Int32
	manager := managerWithLauncher(t, &launcherStub{
		startFunc: func(context.Context, SessionSpec, Persistence) (backend, Persistence, error) {
			return nil, "", permanentStartError(errors.New("host unavailable"))
		},
		terminateFunc: func(context.Context, SessionSpec, Persistence) error {
			terminations.Add(1)
			return nil
		},
	})
	defer manager.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _ = manager.Create(ctx, SessionSpec{ID: "discard-error", Persistence: PersistenceTmux})
	waitState(t, ctx, manager, "discard-error", StateError)
	a, err := manager.Attach(ctx, "discard-error", "browser", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.DiscardContext(ctx, "discard-error"); err != nil {
		t.Fatal(err)
	}
	if terminations.Load() != 0 {
		t.Fatal("Discard invoked destructive backend termination")
	}
	if _, err := manager.Status("discard-error"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("Status after Discard = %v, want ErrSessionNotFound", err)
	}
	if reason := awaitCloseReason(t, ctx, a); reason != AttachmentExited {
		t.Fatalf("discard close reason = %q, want exited", reason)
	}

	runningBackend := newBackendStub()
	manager.launcher = &launcherStub{startFunc: func(context.Context, SessionSpec, Persistence) (backend, Persistence, error) {
		return runningBackend, PersistenceTmux, nil
	}}
	_, _ = manager.Create(ctx, SessionSpec{ID: "discard-running", Persistence: PersistenceTmux})
	waitRunning(t, ctx, manager, "discard-running")
	if err := manager.DiscardContext(ctx, "discard-running"); !errors.Is(err, ErrSessionActive) {
		t.Fatalf("Discard running session error = %v, want ErrSessionActive", err)
	}
}

func TestAttachmentCloseReasonsAndWriterNotifications(t *testing.T) {
	t.Run("server shutdown", func(t *testing.T) {
		b := newBackendStub()
		manager := managerWithLauncher(t, &launcherStub{startFunc: func(context.Context, SessionSpec, Persistence) (backend, Persistence, error) {
			return b, PersistenceTmux, nil
		}})
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, _ = manager.Create(ctx, SessionSpec{ID: "shutdown-reason", Persistence: PersistenceTmux})
		a, err := manager.Attach(ctx, "shutdown-reason", "browser", 0)
		if err != nil {
			t.Fatal(err)
		}
		if err := manager.CloseContext(ctx); err != nil {
			t.Fatal(err)
		}
		if reason := awaitCloseReason(t, ctx, a); reason != AttachmentServerShutdown {
			t.Fatalf("reason = %q", reason)
		}
	})

	t.Run("natural exit", func(t *testing.T) {
		b := newBackendStub()
		manager := managerWithLauncher(t, &launcherStub{startFunc: func(context.Context, SessionSpec, Persistence) (backend, Persistence, error) {
			return b, PersistenceNone, nil
		}})
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, _ = manager.Create(ctx, SessionSpec{ID: "exit-reason", Persistence: PersistenceNone})
		a, err := manager.Attach(ctx, "exit-reason", "browser", 0)
		if err != nil {
			t.Fatal(err)
		}
		waitRunning(t, ctx, manager, "exit-reason")
		_ = b.Close()
		if reason := awaitCloseReason(t, ctx, a); reason != AttachmentExited {
			t.Fatalf("reason = %q", reason)
		}
		_ = manager.Close()
	})

	t.Run("writer takeover", func(t *testing.T) {
		b := newBackendStub()
		manager := managerWithLauncher(t, &launcherStub{startFunc: func(context.Context, SessionSpec, Persistence) (backend, Persistence, error) {
			return b, PersistenceTmux, nil
		}})
		defer manager.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, _ = manager.Create(ctx, SessionSpec{ID: "writer-events", Persistence: PersistenceTmux})
		first, _ := manager.Attach(ctx, "writer-events", "first", 0)
		second, _ := manager.Attach(ctx, "writer-events", "second", 0)
		if got := awaitWriter(t, ctx, first); !got {
			t.Fatal("first attachment initial writer event was false")
		}
		if got := awaitWriter(t, ctx, second); got {
			t.Fatal("second attachment initial writer event was true")
		}
		if err := second.TakeControl(); err != nil {
			t.Fatal(err)
		}
		if got := awaitWriter(t, ctx, first); got {
			t.Fatal("old writer did not receive immediate false event")
		}
		if got := awaitWriter(t, ctx, second); !got {
			t.Fatal("new writer did not receive immediate true event")
		}
	})
}

func TestLinuxPTYEIOIsTreatedAsCleanExit(t *testing.T) {
	var starts atomic.Int32
	b := newBackendStub()
	b.readErr = syscall.EIO
	launcher := &launcherStub{startFunc: func(context.Context, SessionSpec, Persistence) (backend, Persistence, error) {
		starts.Add(1)
		return b, PersistenceTmux, nil
	}}
	manager := managerWithLauncher(t, launcher)
	defer manager.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _ = manager.Create(ctx, SessionSpec{ID: "pty-eio", Persistence: PersistenceTmux})
	waitRunning(t, ctx, manager, "pty-eio")
	_ = b.Close()
	waitState(t, ctx, manager, "pty-eio", StateExited)
	if starts.Load() != 1 {
		t.Fatalf("EIO caused backend recreation: starts=%d", starts.Load())
	}
}

func TestCloseContextReturnsAtDeadlineWhenLauncherIgnoresCancellation(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	launcher := &launcherStub{startFunc: func(ctx context.Context, _ SessionSpec, _ Persistence) (backend, Persistence, error) {
		close(entered)
		<-release
		return nil, "", ctx.Err()
	}}
	manager := managerWithLauncher(t, launcher)
	_, err := manager.Create(context.Background(), SessionSpec{ID: "close-deadline", Persistence: PersistenceTmux})
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

func TestAttachmentExposesReplayBoundsAndSinceZeroTruncation(t *testing.T) {
	log := &fixedLog{oldest: 5, newest: 7}
	manager, err := NewManager(Config{
		Transcripts: fixedFactory{log: log},
		launcher: &launcherStub{startFunc: func(context.Context, SessionSpec, Persistence) (backend, Persistence, error) {
			return newBackendStub(), PersistenceTmux, nil
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, _ = manager.Create(ctx, SessionSpec{ID: "replay-bounds", Persistence: PersistenceTmux})
	a, err := manager.Attach(ctx, "replay-bounds", "browser", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !a.Truncated || a.OldestSequence != 5 || a.LatestSequence != 7 {
		t.Fatalf("attachment replay metadata = truncated:%v oldest:%d latest:%d", a.Truncated, a.OldestSequence, a.LatestSequence)
	}
}

func TestTranscriptAppendFailureDoesNotPublishOrAdvanceSequence(t *testing.T) {
	log := &toggleLog{newest: 3, fail: true}
	manager, err := NewManager(Config{
		Transcripts: fixedFactory{log: log},
		launcher: &launcherStub{startFunc: func(context.Context, SessionSpec, Persistence) (backend, Persistence, error) {
			return newBackendStub(), PersistenceTmux, nil
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, _ = manager.Create(ctx, SessionSpec{ID: "append-failure", Persistence: PersistenceTmux})
	waitState(t, ctx, manager, "append-failure", StateRunning)
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
	if status.LastSequence != 3 || status.LastError == "" {
		t.Fatalf("status after append failure = %+v", status)
	}

	log.setFail(false)
	s.publish([]byte("persisted"))
	select {
	case frame := <-a.Frames:
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

func waitState(t *testing.T, ctx context.Context, manager *Manager, id string, want SessionState) {
	t.Helper()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		status, err := manager.Status(id)
		if err != nil {
			t.Fatal(err)
		}
		if status.State == want {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("state = %s, want %s", status.State, want)
		case <-ticker.C:
		}
	}
}

func awaitCloseReason(t *testing.T, ctx context.Context, attachment *Attachment) AttachmentCloseReason {
	t.Helper()
	select {
	case reason := <-attachment.Closed:
		return reason
	case <-ctx.Done():
		t.Fatal("timed out waiting for attachment close reason")
		return ""
	}
}

func awaitWriter(t *testing.T, ctx context.Context, attachment *Attachment) bool {
	t.Helper()
	select {
	case value := <-attachment.WriterChanges:
		return value
	case <-ctx.Done():
		t.Fatal("timed out waiting for writer change")
		return false
	}
}
