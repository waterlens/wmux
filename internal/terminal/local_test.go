package terminal

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
)

func TestSessionAbsentRecognizesMuxResponses(t *testing.T) {
	tests := []struct {
		name   string
		kind   Persistence
		output string
		want   bool
	}{
		{name: "tmux exact message", kind: PersistenceTmux, output: "can't find session: =wmux-demo", want: true},
		{name: "tmux no server", kind: PersistenceTmux, output: "no server running on /tmp/tmux-501/wmux", want: true},
		{name: "screen exact message", kind: PersistenceScreen, output: "No screen session found.", want: true},
		{name: "screen no sockets", kind: PersistenceScreen, output: "No Sockets found in /tmp/wmux.", want: true},
		{name: "real failure", kind: PersistenceTmux, output: "permission denied", want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := sessionAbsent(test.kind, []byte(test.output)); got != test.want {
				t.Fatalf("sessionAbsent(%q, %q) = %v, want %v", test.kind, test.output, got, test.want)
			}
		})
	}
}

func TestLocalBackendReconnectDecisionTreatsPTYClosureAsExit(t *testing.T) {
	for _, kind := range []Persistence{PersistenceNone, PersistenceTmux, PersistenceScreen} {
		backend := &localBackend{kind: kind}
		if backend.Reconnectable(io.EOF) || backend.Reconnectable(syscall.EIO) {
			t.Fatalf("%s backend reconnects after normal PTY closure", kind)
		}
	}
	if (&localBackend{kind: PersistenceNone}).Reconnectable(errors.New("transport failure")) {
		t.Fatal("direct PTY reconnects after process failure")
	}
	if !(&localBackend{kind: PersistenceTmux}).Reconnectable(errors.New("transport failure")) {
		t.Fatal("persistent tmux backend does not reconnect after transport failure")
	}
}

func TestLocalMuxConfigurationIsIsolated(t *testing.T) {
	runtimeDir := t.TempDir()
	l := newLauncher(Config{MuxName: "my wmux!", MuxRuntimeDir: runtimeDir})
	name := backendName("session/id")
	tmuxArgs := l.tmuxArgs("kill-session", "-t", "="+name)
	wantArgs := []string{"-L", "my-wmux", "-f", "/dev/null", "kill-session", "-t", "=" + name}
	if !slices.Equal(tmuxArgs, wantArgs) {
		t.Fatalf("tmux args = %#v, want %#v", tmuxArgs, wantArgs)
	}
	if slices.Contains(tmuxArgs, "kill-server") {
		t.Fatal("tmux command targets the whole server")
	}

	config, env, err := l.screenRuntime(map[string]string{"SCREENDIR": "/user/default"})
	if err != nil {
		t.Fatal(err)
	}
	wantDir := filepath.Join(runtimeDir, "screen-my-wmux")
	if config != filepath.Join(wantDir, "screenrc") {
		t.Fatalf("screen config = %q, want isolated config below %q", config, wantDir)
	}
	if !slices.Contains(env, "SCREENDIR="+wantDir) {
		t.Fatalf("screen environment does not override SCREENDIR: %#v", env)
	}
	contents, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	for _, setting := range []string{"startup_message off", "hardstatus off", "escape ^^^"} {
		if !strings.Contains(string(contents), setting+"\n") {
			t.Fatalf("screen config %q does not contain %q", contents, setting)
		}
	}
	info, err := os.Stat(config)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("screen config mode = %o, want 600", info.Mode().Perm())
	}
}

func TestConfigureLocalTmuxEnablesMouseAndSafelyProbesHyperlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("capture helper is a POSIX shell script")
	}
	dir := t.TempDir()
	capture := filepath.Join(dir, "commands")
	tool := filepath.Join(dir, "tmux")
	script := []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$WMUX_CAPTURE_PATH\"\ncase \"$*\" in *'set-option -as terminal-features'*) exit 1;; esac\n")
	if err := os.WriteFile(tool, script, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WMUX_CAPTURE_PATH", capture)
	l := newLauncher(Config{MuxName: "isolated"})
	if err := l.configureLocalTmux(context.Background(), tool); err != nil {
		t.Fatalf("optional hyperlink support broke an older tmux: %v", err)
	}
	contents, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	commands := string(contents)
	for _, wanted := range []string{
		"-L isolated -f /dev/null set-option -g status off",
		"-L isolated -f /dev/null set-option -g prefix None",
		"-L isolated -f /dev/null set-option -g mouse on",
		"-L isolated -f /dev/null show-options -gqv terminal-features",
		"-L isolated -f /dev/null set-option -as terminal-features " + tmuxHyperlinkFeatures,
	} {
		if !strings.Contains(commands, wanted) {
			t.Fatalf("tmux configuration commands %q do not contain %q", commands, wanted)
		}
	}
	if strings.Contains(commands, "allow-passthrough") {
		t.Fatalf("tmux configuration enabled passthrough: %q", commands)
	}
}

func TestExpandLocalHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	tests := map[string]string{
		"~":           home,
		"~/work/wmux": filepath.Join(home, "work", "wmux"),
		"~someone/x":  "~someone/x",
		"/tmp/plain":  "/tmp/plain",
	}
	for input, want := range tests {
		got, err := expandLocalHome(input)
		if err != nil {
			t.Fatalf("expandLocalHome(%q): %v", input, err)
		}
		if got != want {
			t.Fatalf("expandLocalHome(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestTerminateLocalTargetsOnlyNamedIsolatedSession(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("capture helper is a POSIX shell script")
	}
	dir := t.TempDir()
	capture := filepath.Join(dir, "args")
	environmentCapture := filepath.Join(dir, "screen-dir")
	tool := filepath.Join(dir, "mux-tool")
	script := []byte("#!/bin/sh\nif [ \"$3\" = '-ls' ]; then exit 1; fi\nprintf '%s\\n' \"$@\" > \"$WMUX_CAPTURE_PATH\"\nprintf '%s\\n' \"$SCREENDIR\" > \"$WMUX_ENV_CAPTURE_PATH\"\n")
	if err := os.WriteFile(tool, script, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WMUX_CAPTURE_PATH", capture)
	t.Setenv("WMUX_ENV_CAPTURE_PATH", environmentCapture)

	spec := SessionSpec{ID: "one/session"}
	name := backendName(spec.ID)
	tmuxLauncher := newLauncher(Config{TmuxPath: tool, MuxName: "isolated"})
	if err := tmuxLauncher.terminateLocal(context.Background(), spec, PersistenceTmux); err != nil {
		t.Fatal(err)
	}
	assertCapturedArgs(t, capture, []string{"-L", "isolated", "-f", "/dev/null", "kill-session", "-t", "=" + name})

	screenRoot := filepath.Join(dir, "runtime")
	screenLauncher := newLauncher(Config{ScreenPath: tool, MuxName: "isolated", MuxRuntimeDir: screenRoot})
	if err := screenLauncher.terminateLocal(context.Background(), spec, PersistenceScreen); err != nil {
		t.Fatal(err)
	}
	wantScreenDir := filepath.Join(screenRoot, "screen-isolated")
	assertCapturedArgs(t, capture, []string{"-c", filepath.Join(wantScreenDir, "screenrc"), "-S", name, "-X", "quit"})
	contents, err := os.ReadFile(environmentCapture)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(contents)); got != wantScreenDir {
		t.Fatalf("screen SCREENDIR = %q, want %q", got, wantScreenDir)
	}
}

func assertCapturedArgs(t *testing.T, path string, want []string) {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSuffix(string(contents), "\n"), "\n")
	if !slices.Equal(got, want) {
		t.Fatalf("captured args = %#v, want %#v", got, want)
	}
}
