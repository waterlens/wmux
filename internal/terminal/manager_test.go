package terminal

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/waterlens/wmux/internal/transcript"
)

// memorySessionRepository stands in for the application's session table.
type memorySessionRepository struct {
	mu      sync.Mutex
	records map[string]SessionRecord
}

func newMemorySessionRepository() *memorySessionRepository {
	return &memorySessionRepository{records: make(map[string]SessionRecord)}
}

func (r *memorySessionRepository) put(record SessionRecord) {
	r.mu.Lock()
	defer r.mu.Unlock()
	record.Spec = cloneSpec(record.Spec)
	r.records[record.Spec.ID] = record
}

func (r *memorySessionRepository) ListSessions(context.Context) ([]SessionRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	records := make([]SessionRecord, 0, len(r.records))
	for _, record := range r.records {
		record.Spec = cloneSpec(record.Spec)
		records = append(records, record)
	}
	return records, nil
}

func (*memorySessionRepository) LoadHost(context.Context, string) (HostSpec, error) {
	return HostSpec{}, errors.New("unexpected host load for local test session")
}

func (r *memorySessionRepository) OnSessionState(status SessionStatus) {
	r.mu.Lock()
	defer r.mu.Unlock()
	record, exists := r.records[status.ID]
	if !exists {
		return
	}
	record.ResolvedPersistence = status.Persistence
	record.Active = status.State != StateExited && status.State != StateTerminated
	r.records[status.ID] = record
}

func (*memorySessionRepository) OnClientDropped(string, string, string) {}

func TestDirectLocalPTYAndTranscriptReplay(t *testing.T) {
	manager := testManager(t, 32)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := manager.Create(ctx, SessionSpec{
		ID:          "direct-pty",
		Persistence: PersistenceNone,
		Shell:       "/bin/sh",
		Cols:        100,
		Rows:        30,
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := manager.Attach(ctx, "direct-pty", "browser-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	waitState(ctx, t, manager, "direct-pty", StateRunning)
	if _, err := first.WriteContext(ctx, []byte("printf 'wmux-direct-ok\\n'\n")); err != nil {
		t.Fatal(err)
	}

	var output []byte
	var last uint64
	for !bytes.Contains(output, []byte("wmux-direct-ok")) {
		select {
		case frame, ok := <-first.Frames:
			if !ok {
				t.Fatalf("output closed early: %q", output)
			}
			if frame.Sequence <= last {
				t.Fatalf("non-monotonic sequence %d after %d", frame.Sequence, last)
			}
			last = frame.Sequence
			output = append(output, frame.Data...)
		case <-ctx.Done():
			t.Fatalf("waiting for PTY output: %v; output %q", ctx.Err(), output)
		}
	}

	second, err := manager.Attach(ctx, "direct-pty", "browser-2", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	var replay []byte
	for _, frame := range second.Initial {
		replay = append(replay, frame.Data...)
	}
	if !bytes.Contains(replay, []byte("wmux-direct-ok")) {
		t.Fatalf("replay does not contain terminal output: %q", replay)
	}
	if err := manager.Terminate(ctx, "direct-pty"); err != nil {
		t.Fatal(err)
	}
}

func TestWriterLeaseAndTakeControl(t *testing.T) {
	manager := testManager(t, 8)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := manager.Create(ctx, SessionSpec{ID: "lease", Persistence: PersistenceNone, Shell: "/bin/sh"}); err != nil {
		t.Fatal(err)
	}
	first, err := manager.Attach(ctx, "lease", "first", 0)
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.Attach(ctx, "lease", "second", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !first.IsWriter() || second.IsWriter() {
		t.Fatalf("first writer = %v, second writer = %v", first.IsWriter(), second.IsWriter())
	}
	if _, err := second.WriteContext(ctx, []byte("forbidden\n")); !errors.Is(err, ErrNotWriter) {
		t.Fatalf("second Write error = %v, want ErrNotWriter", err)
	}
	if err := second.TakeControl(); err != nil {
		t.Fatal(err)
	}
	if first.IsWriter() || !second.IsWriter() {
		t.Fatalf("after takeover: first writer = %v, second writer = %v", first.IsWriter(), second.IsWriter())
	}
	if _, err := first.WriteContext(ctx, []byte("forbidden\n")); !errors.Is(err, ErrNotWriter) {
		t.Fatalf("old writer Write error = %v, want ErrNotWriter", err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	if !first.IsWriter() {
		t.Fatal("write lease was not reassigned to oldest remaining attachment")
	}
	if err := manager.Terminate(ctx, "lease"); err != nil {
		t.Fatal(err)
	}
}

func TestSlowClientIsDroppedWithoutBlockingPublisher(t *testing.T) {
	manager := testManager(t, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := manager.Create(ctx, SessionSpec{ID: "slow", Persistence: PersistenceNone, Shell: "/bin/sh"}); err != nil {
		t.Fatal(err)
	}
	attachment, err := manager.Attach(ctx, "slow", "slow-browser", 0)
	if err != nil {
		t.Fatal(err)
	}
	s, err := manager.session("slow")
	if err != nil {
		t.Fatal(err)
	}
	s.publish([]byte("one"))
	s.publish([]byte("two"))
	status, err := manager.Status("slow")
	if err != nil {
		t.Fatal(err)
	}
	if status.Clients != 0 || attachment.IsWriter() {
		t.Fatalf("slow client was retained: %+v", status)
	}
	select {
	case reason := <-attachment.Closed:
		if reason != AttachmentEvicted {
			t.Fatalf("slow client close reason = %q, want %q", reason, AttachmentEvicted)
		}
	case <-ctx.Done():
		t.Fatal("slow client did not receive an eviction reason")
	}
	if err := manager.Terminate(ctx, "slow"); err != nil {
		t.Fatal(err)
	}
}

func TestScreenSessionSurvivesManagerCloseAndTerminateKillsIt(t *testing.T) {
	if os.Getenv("WMUX_SCREEN_INTEGRATION") != "1" {
		t.Skip("set WMUX_SCREEN_INTEGRATION=1 to exercise the host screen binary")
	}
	screenPath, err := exec.LookPath("screen")
	if err != nil {
		t.Skip("screen is not installed")
	}
	id := fmt.Sprintf("screen-close-%d-%d", os.Getpid(), time.Now().UnixNano())
	name := BackendName(id)
	runtimeDir := t.TempDir()
	screenConfig, screenEnv, err := newExecLauncher(Config{MuxRuntimeDir: runtimeDir}).screenRuntime(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		cmd := exec.Command(screenPath, "-c", screenConfig, "-S", name, "-X", "quit")
		cmd.Env = screenEnv
		_ = cmd.Run()
	}()

	directory, err := transcript.NewDirectory(transcript.DirectoryConfig{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	newManager := func() *Manager {
		manager, err := NewManager(Config{
			Transcripts:   directory,
			tmuxPath:      filepath.Join(t.TempDir(), "missing-tmux"),
			screenPath:    screenPath,
			MuxRuntimeDir: runtimeDir,
		})
		if err != nil {
			t.Fatal(err)
		}
		return manager
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	firstManager := newManager()
	if _, err := firstManager.Create(ctx, SessionSpec{ID: id, Persistence: PersistenceAuto, Shell: "/bin/sh", Args: []string{"-i"}}); err != nil {
		t.Fatal(err)
	}
	firstAttachment, err := firstManager.Attach(ctx, id, "first-browser", 0)
	if err != nil {
		t.Fatal(err)
	}
	waitState(ctx, t, firstManager, id, StateRunning)
	status, _ := firstManager.Status(id)
	if status.Persistence != PersistenceScreen {
		t.Fatalf("auto persistence = %q, want screen", status.Persistence)
	}
	if _, err := firstAttachment.WriteContext(ctx, []byte("printf 'before-detach\\n'\n")); err != nil {
		t.Fatal(err)
	}
	waitForOutput(ctx, t, firstAttachment.Frames, "before-detach")
	_ = firstAttachment.Close()
	if err := firstManager.Close(); err != nil {
		t.Fatalf("close manager: %v", err)
	}
	if !localScreenExists(ctx, screenPath, screenConfig, screenEnv, name) {
		t.Fatal("screen session did not survive Manager.Close")
	}

	secondManager := newManager()
	defer secondManager.Close()
	secondCtx, secondCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer secondCancel()
	if _, err := secondManager.Create(secondCtx, SessionSpec{ID: id, Persistence: PersistenceAuto, Shell: "/bin/sh", Args: []string{"-i"}}); err != nil {
		t.Fatal(err)
	}
	secondAttachment, err := secondManager.Attach(secondCtx, id, "second-browser", 0)
	if err != nil {
		t.Fatal(err)
	}
	waitState(secondCtx, t, secondManager, id, StateRunning)
	if !secondAttachment.IsWriter() {
		t.Fatal("reattached browser did not receive write lease")
	}
	terminateCtx, terminateCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer terminateCancel()
	if err := secondManager.Terminate(terminateCtx, id); err != nil {
		t.Fatal(err)
	}
	if localScreenExists(terminateCtx, screenPath, screenConfig, screenEnv, name) {
		t.Fatal("screen session survived explicit Terminate")
	}
}

func TestTmuxSessionSurvivesManagerRestoreAndTerminateKillsIt(t *testing.T) {
	if os.Getenv("WMUX_TMUX_INTEGRATION") != "1" {
		t.Skip("set WMUX_TMUX_INTEGRATION=1 to exercise the host tmux binary")
	}
	tmuxPath, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux is not installed")
	}

	id := fmt.Sprintf("tmux-restore-%d-%d", os.Getpid(), time.Now().UnixNano())
	name := BackendName(id)
	tmuxName := fmt.Sprintf("wmux-test-%d-%d", os.Getpid(), time.Now().UnixNano())
	repository := newMemorySessionRepository()
	directory, err := transcript.NewDirectory(transcript.DirectoryConfig{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	cleanupLauncher := newExecLauncher(Config{tmuxPath: tmuxPath, MuxName: tmuxName})
	defer func() {
		cmd := exec.Command(tmuxPath, cleanupLauncher.tmuxArgs("kill-session", "-t", "="+name)...)
		_ = cmd.Run()
	}()

	newManager := func() *Manager {
		manager, err := NewManager(Config{
			Repository:  repository,
			Callbacks:   repository,
			Transcripts: directory,
			tmuxPath:    tmuxPath,
			screenPath:  filepath.Join(t.TempDir(), "missing-screen"),
			MuxName:     tmuxName,
		})
		if err != nil {
			t.Fatal(err)
		}
		return manager
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	firstManager := newManager()
	spec := SessionSpec{
		ID: id, Persistence: PersistenceAuto, Shell: "/bin/sh", Args: []string{"-i"}, Cols: 100, Rows: 30, Generation: 1,
	}
	repository.put(SessionRecord{Spec: spec, Active: true})
	if _, err := firstManager.Create(ctx, spec); err != nil {
		t.Fatal(err)
	}
	firstAttachment, err := firstManager.Attach(ctx, id, "first-browser", 0)
	if err != nil {
		t.Fatal(err)
	}
	waitState(ctx, t, firstManager, id, StateRunning)
	if status, err := firstManager.Status(id); err != nil || status.Persistence != PersistenceTmux {
		t.Fatalf("first manager status = %+v, %v; want tmux", status, err)
	}
	assertIsolatedTmuxInteractionOptions(t, tmuxPath, cleanupLauncher)
	if _, err := firstAttachment.WriteContext(ctx, []byte("printf 'before-tmux-restore\\n'\n")); err != nil {
		t.Fatal(err)
	}
	waitForOutput(ctx, t, firstAttachment.Frames, "before-tmux-restore")
	_ = firstAttachment.Close()
	if err := firstManager.Close(); err != nil {
		t.Fatalf("close first manager: %v", err)
	}
	if err := exec.Command(tmuxPath, cleanupLauncher.tmuxArgs("has-session", "-t", "="+name)...).Run(); err != nil {
		t.Fatalf("tmux session did not survive Manager.Close: %v", err)
	}

	secondManager := newManager()
	defer secondManager.Close()
	if err := secondManager.Restore(ctx); err != nil {
		t.Fatalf("restore second manager: %v", err)
	}
	waitState(ctx, t, secondManager, id, StateRunning)
	secondAttachment, err := secondManager.Attach(ctx, id, "second-browser", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !secondAttachment.IsWriter() {
		t.Fatal("restored attachment did not receive the write lease")
	}
	if _, err := secondAttachment.WriteContext(ctx, []byte("printf 'after-tmux-restore\\n'\n")); err != nil {
		t.Fatal(err)
	}
	waitForOutput(ctx, t, secondAttachment.Frames, "after-tmux-restore")
	if err := secondManager.Terminate(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command(tmuxPath, cleanupLauncher.tmuxArgs("has-session", "-t", "="+name)...).Run(); err == nil {
		t.Fatal("tmux session survived explicit Terminate")
	}
}

func assertIsolatedTmuxInteractionOptions(t *testing.T, path string, l *execLauncher) {
	t.Helper()
	global := func(option string) (string, error) {
		output, err := exec.Command(path, l.tmuxArgs("show-options", "-gv", option)...).CombinedOutput()
		return strings.TrimSpace(string(output)), err
	}
	for option, want := range map[string]string{"mouse": "on", "status": "off", "prefix": "None"} {
		got, err := global(option)
		if err != nil || got != want {
			t.Fatalf("isolated tmux option %s = %q, %v; want %q", option, got, err, want)
		}
	}
	wheelBinding, err := exec.Command(path, l.tmuxArgs("list-keys", "-T", "root")...).CombinedOutput()
	if err != nil || !strings.Contains(string(wheelBinding), "WheelUpPane") {
		t.Fatalf("isolated tmux wheel binding unavailable: %q, %v", wheelBinding, err)
	}
	features, err := global("terminal-features")
	if err != nil {
		t.Fatalf("query isolated tmux terminal-features: %v", err)
	}
	overrides, err := global("terminal-overrides")
	if err != nil || !strings.Contains(overrides, "xterm*:Tc") {
		t.Fatalf("isolated tmux terminal-overrides = %q, %v; want truecolor support", overrides, err)
	}
	if count := strings.Count(features, "xterm*:hyperlinks"); count != 1 {
		// An old tmux rejects the feature; a compatible one must have it set.
		probe := exec.Command(path, l.tmuxArgs("set-option", "-as", "terminal-features", tmuxHyperlinkFeatures)...)
		if probeErr := probe.Run(); probeErr == nil {
			t.Fatalf("compatible tmux did not configure hyperlinks exactly once; value %q", features)
		}
	}
	output, err := exec.Command(path, l.tmuxArgs("show-options", "-Apgv", "allow-passthrough")...).CombinedOutput()
	if err == nil && strings.TrimSpace(string(output)) != "off" {
		t.Fatalf("isolated tmux allow-passthrough = %q, want off", output)
	}
}
