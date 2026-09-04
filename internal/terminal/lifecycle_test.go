package terminal

import (
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
