package terminal

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/waterlens/wmux/internal/transcript"
)

// TestMain skips the whole package on Windows: it needs a PTY, /bin/sh and
// tmux or screen, none of which exist there. CI runs on Linux.
func TestMain(m *testing.M) {
	if runtime.GOOS == "windows" {
		fmt.Fprintln(os.Stderr, "terminal tests require a Unix host")
		os.Exit(0)
	}
	os.Exit(m.Run())
}

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

func managerWithLauncher(t *testing.T, backends launcher, mutate ...func(*Config)) *Manager {
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
		launcher:        backends,
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

func waitState(ctx context.Context, t *testing.T, manager *Manager, id string, want SessionState) {
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

func waitCondition(ctx context.Context, t *testing.T, satisfied func() bool) {
	t.Helper()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for !satisfied() {
		select {
		case <-ctx.Done():
			t.Fatal("timed out waiting for condition")
		case <-ticker.C:
		}
	}
}

func waitForOutput(ctx context.Context, t *testing.T, frames <-chan OutputFrame, needle string) {
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

func waitForFileContent(ctx context.Context, t *testing.T, path, want string) {
	t.Helper()
	ticker := time.NewTicker(muxPollInterval)
	defer ticker.Stop()
	for {
		contents, err := os.ReadFile(path)
		if err == nil && string(contents) == want {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("file %s = %q, %v; want %q", path, contents, err, want)
		case <-ticker.C:
		}
	}
}

func awaitCloseReason(ctx context.Context, t *testing.T, attachment *Attachment) AttachmentCloseReason {
	t.Helper()
	select {
	case reason := <-attachment.Closed:
		return reason
	case <-ctx.Done():
		t.Fatal("timed out waiting for attachment close reason")
		return ""
	}
}

func awaitWriter(ctx context.Context, t *testing.T, attachment *Attachment) bool {
	t.Helper()
	select {
	case value := <-attachment.WriterChanges:
		return value
	case <-ctx.Done():
		t.Fatal("timed out waiting for writer change")
		return false
	}
}

func awaitState(ctx context.Context, t *testing.T, attachment *Attachment, accept func(SessionStatus) bool) SessionStatus {
	t.Helper()
	for {
		select {
		case status, ok := <-attachment.States:
			if !ok {
				t.Fatal("state channel closed before the expected status")
			}
			if accept(status) {
				return status
			}
		case <-ctx.Done():
			t.Fatal("timed out waiting for a session status")
			return SessionStatus{}
		}
	}
}
