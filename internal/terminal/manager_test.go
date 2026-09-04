package terminal

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/waterlens/wmux/internal/transcript"
)

func testManager(t *testing.T, clientBuffer int) *Manager {
	t.Helper()
	directory, err := transcript.NewDirectory(transcript.DirectoryConfig{
		Root:         t.TempDir(),
		SegmentBytes: 4 << 10,
		MaxBytes:     32 << 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(Config{
		Transcripts:  directory,
		ClientBuffer: clientBuffer,
		ReconnectMin: 10 * time.Millisecond,
		ReconnectMax: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	return manager
}

func directShell() string {
	if runtime.GOOS == "windows" {
		return "cmd.exe"
	}
	return "/bin/sh"
}

func TestDirectLocalPTYAndTranscriptReplay(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creack/pty direct test is Unix-only")
	}
	manager := testManager(t, 32)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := manager.Create(ctx, SessionSpec{
		ID:          "direct-pty",
		Persistence: PersistenceNone,
		Shell:       directShell(),
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
	waitRunning(t, ctx, manager, "direct-pty")
	if _, err := first.Write([]byte("printf 'wmux-direct-ok\\n'\n")); err != nil {
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
	if runtime.GOOS == "windows" {
		t.Skip("creack/pty direct test is Unix-only")
	}
	manager := testManager(t, 8)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := manager.Create(ctx, SessionSpec{ID: "lease", Persistence: PersistenceNone, Shell: directShell()}); err != nil {
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
	if _, err := second.Write([]byte("forbidden\n")); !errors.Is(err, ErrNotWriter) {
		t.Fatalf("second Write error = %v, want ErrNotWriter", err)
	}
	if err := second.TakeControl(); err != nil {
		t.Fatal(err)
	}
	if first.IsWriter() || !second.IsWriter() {
		t.Fatalf("after takeover: first writer = %v, second writer = %v", first.IsWriter(), second.IsWriter())
	}
	if _, err := first.Write([]byte("forbidden\n")); !errors.Is(err, ErrNotWriter) {
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
	if runtime.GOOS == "windows" {
		t.Skip("creack/pty direct test is Unix-only")
	}
	manager := testManager(t, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := manager.Create(ctx, SessionSpec{ID: "slow", Persistence: PersistenceNone, Shell: directShell()}); err != nil {
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
	if runtime.GOOS == "windows" {
		t.Skip("screen is Unix-only")
	}
	screenPath, err := exec.LookPath("screen")
	if err != nil {
		t.Skip("screen is not installed")
	}
	id := fmt.Sprintf("screen-close-%d-%d", os.Getpid(), time.Now().UnixNano())
	name := backendName(id)
	runtimeDir := t.TempDir()
	screenConfig, screenEnv, err := newLauncher(Config{MuxRuntimeDir: runtimeDir}).screenRuntime(nil)
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
			TmuxPath:      filepath.Join(t.TempDir(), "missing-tmux"),
			ScreenPath:    screenPath,
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
	waitRunning(t, ctx, firstManager, id)
	status, _ := firstManager.Status(id)
	if status.Persistence != PersistenceScreen {
		t.Fatalf("auto persistence = %q, want screen", status.Persistence)
	}
	if _, err := firstAttachment.Write([]byte("printf 'before-detach\\n'\n")); err != nil {
		t.Fatal(err)
	}
	waitForOutput(t, ctx, firstAttachment.Frames, "before-detach")
	_ = firstAttachment.Close()
	if err := firstManager.Close(); err != nil {
		t.Fatalf("close manager: %v", err)
	}
	if !screenSessionExists(screenPath, screenConfig, screenEnv, name) {
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
	waitRunning(t, secondCtx, secondManager, id)
	if !secondAttachment.IsWriter() {
		t.Fatal("reattached browser did not receive write lease")
	}
	terminateCtx, terminateCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer terminateCancel()
	if err := secondManager.Terminate(terminateCtx, id); err != nil {
		t.Fatal(err)
	}
	if screenSessionExists(screenPath, screenConfig, screenEnv, name) {
		t.Fatalf("screen session survived explicit Terminate: %s", screenSessionListing(screenPath, screenConfig, screenEnv, name))
	}
}

type memorySessionRepository struct {
	mu      sync.Mutex
	records map[string]SessionRecord
}

func newMemorySessionRepository() *memorySessionRepository {
	return &memorySessionRepository{records: make(map[string]SessionRecord)}
}

func (r *memorySessionRepository) SaveSession(_ context.Context, record SessionRecord) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	record.Args = append([]string(nil), record.Args...)
	record.Env = cloneMap(record.Env)
	r.records[record.ID] = record
	return nil
}

func (r *memorySessionRepository) ListSessions(context.Context) ([]SessionRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	records := make([]SessionRecord, 0, len(r.records))
	for _, record := range r.records {
		record.Args = append([]string(nil), record.Args...)
		record.Env = cloneMap(record.Env)
		records = append(records, record)
	}
	return records, nil
}

func (*memorySessionRepository) LoadHost(context.Context, string) (HostSpec, error) {
	return HostSpec{}, errors.New("unexpected host load for local test session")
}

func TestTmuxSessionSurvivesManagerRestoreAndTerminateKillsIt(t *testing.T) {
	if os.Getenv("WMUX_TMUX_INTEGRATION") != "1" {
		t.Skip("set WMUX_TMUX_INTEGRATION=1 to exercise the host tmux binary")
	}
	if runtime.GOOS == "windows" {
		t.Skip("tmux is Unix-only")
	}
	tmuxPath, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux is not installed")
	}

	id := fmt.Sprintf("tmux-restore-%d-%d", os.Getpid(), time.Now().UnixNano())
	name := backendName(id)
	tmuxName := fmt.Sprintf("wmux-test-%d-%d", os.Getpid(), time.Now().UnixNano())
	repository := newMemorySessionRepository()
	directory, err := transcript.NewDirectory(transcript.DirectoryConfig{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	cleanupLauncher := newLauncher(Config{TmuxPath: tmuxPath, MuxName: tmuxName})
	defer func() {
		cmd := exec.Command(tmuxPath, cleanupLauncher.tmuxArgs("kill-session", "-t", "="+name)...)
		_ = cmd.Run()
	}()

	newManager := func() *Manager {
		manager, err := NewManager(Config{
			Repository:  repository,
			Transcripts: directory,
			TmuxPath:    tmuxPath,
			ScreenPath:  filepath.Join(t.TempDir(), "missing-screen"),
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
	if _, err := firstManager.Create(ctx, SessionSpec{
		ID: id, Persistence: PersistenceAuto, Shell: "/bin/sh", Args: []string{"-i"}, Cols: 100, Rows: 30,
	}); err != nil {
		t.Fatal(err)
	}
	firstAttachment, err := firstManager.Attach(ctx, id, "first-browser", 0)
	if err != nil {
		t.Fatal(err)
	}
	waitRunning(t, ctx, firstManager, id)
	if status, err := firstManager.Status(id); err != nil || status.Persistence != PersistenceTmux {
		t.Fatalf("first manager status = %+v, %v; want tmux", status, err)
	}
	assertIsolatedTmuxInteractionOptions(t, tmuxPath, cleanupLauncher)
	if _, err := firstAttachment.Write([]byte("printf 'before-tmux-restore\\n'\n")); err != nil {
		t.Fatal(err)
	}
	waitForOutput(t, ctx, firstAttachment.Frames, "before-tmux-restore")
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
	waitRunning(t, ctx, secondManager, id)
	secondAttachment, err := secondManager.Attach(ctx, id, "second-browser", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !secondAttachment.IsWriter() {
		t.Fatal("restored attachment did not receive the write lease")
	}
	if _, err := secondAttachment.Write([]byte("printf 'after-tmux-restore\\n'\n")); err != nil {
		t.Fatal(err)
	}
	waitForOutput(t, ctx, secondAttachment.Frames, "after-tmux-restore")
	if err := secondManager.Terminate(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command(tmuxPath, cleanupLauncher.tmuxArgs("has-session", "-t", "="+name)...).Run(); err == nil {
		t.Fatal("tmux session survived explicit Terminate")
	}
}

func assertIsolatedTmuxInteractionOptions(t *testing.T, path string, l launcher) {
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
	if count := strings.Count(features, "xterm*:hyperlinks"); count != 1 {
		// Distinguish an old tmux that safely rejects the feature from a
		// compatible tmux where wmux failed to configure it.
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

func screenSessionExists(path, config string, env []string, name string) bool {
	output := screenSessionListing(path, config, env, name)
	return strings.Contains(output, "."+name+"\t") || strings.Contains(output, "."+name+" ")
}

func screenSessionListing(path, config string, env []string, name string) string {
	cmd := exec.Command(path, "-c", config, "-ls", name)
	cmd.Env = env
	output, _ := cmd.CombinedOutput()
	return string(output)
}

func waitForOutput(t *testing.T, ctx context.Context, frames <-chan OutputFrame, needle string) {
	t.Helper()
	var output []byte
	for !bytes.Contains(output, []byte(needle)) {
		select {
		case frame, ok := <-frames:
			if !ok {
				t.Fatalf("frames closed while waiting for %q; output %q", needle, output)
			}
			output = append(output, frame.Data...)
		case <-ctx.Done():
			t.Fatalf("waiting for %q: %v; output %q", needle, ctx.Err(), output)
		}
	}
}

func waitRunning(t *testing.T, ctx context.Context, manager *Manager, sessionID string) {
	t.Helper()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		status, err := manager.Status(sessionID)
		if err != nil {
			t.Fatal(err)
		}
		if status.State == StateRunning {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("session did not become running: %+v", status)
		case <-ticker.C:
		}
	}
}
