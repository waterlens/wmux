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
	"time"

	"github.com/creack/pty"
)

type localBackend struct {
	pty      *os.File
	cmd      *exec.Cmd
	done     chan error
	kind     Persistence
	toolPath string
	name     string

	closeOnce sync.Once
	closeErr  error
	inputMu   sync.Mutex
}

func (l launcher) startLocal(ctx context.Context, spec SessionSpec, requested Persistence) (backend, Persistence, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	resolved, tool, err := l.resolveLocal(requested)
	if err != nil {
		return nil, "", err
	}
	spec.Cwd, err = expandLocalHome(spec.Cwd)
	if err != nil {
		return nil, "", permanentStartError(err)
	}

	cols, rows := terminalSize(spec)
	name := backendName(spec.ID)
	var cmd *exec.Cmd
	switch resolved {
	case PersistenceTmux:
		if err := l.ensureLocalTmux(ctx, tool, name, spec, cols, rows); err != nil {
			return nil, "", err
		}
		cmd = exec.CommandContext(ctx, tool, l.tmuxArgs("attach-session", "-t", "="+name)...)
	case PersistenceScreen:
		screenConfig, screenEnv, err := l.screenRuntime(spec.Env)
		if err != nil {
			return nil, "", permanentStartError(err)
		}
		if err := l.ensureLocalScreen(ctx, tool, screenConfig, screenEnv, name, spec); err != nil {
			return nil, "", err
		}
		cmd = exec.CommandContext(ctx, tool, "-c", screenConfig, "-x", name)
		cmd.Env = screenEnv
	case PersistenceNone:
		shell := spec.Shell
		if shell == "" {
			shell = os.Getenv("SHELL")
			if shell == "" {
				shell = "/bin/sh"
			}
		}
		cmd = exec.CommandContext(ctx, shell, spec.Args...)
		if spec.Cwd != "" {
			cmd.Dir = spec.Cwd
		}
	default:
		return nil, "", fmt.Errorf("terminal: invalid resolved local backend %q", resolved)
	}
	if cmd.Env == nil {
		cmd.Env = localEnvironment(spec.Env)
	}

	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: cols, Rows: rows})
	if err != nil {
		if ctx.Err() != nil {
			return nil, "", ctx.Err()
		}
		return nil, "", permanentStartError(fmt.Errorf("terminal: start local %s: %w", resolved, err))
	}
	b := &localBackend{
		pty:      ptmx,
		cmd:      cmd,
		done:     make(chan error, 1),
		kind:     resolved,
		toolPath: tool,
		name:     name,
	}
	go func() {
		b.done <- cmd.Wait()
		close(b.done)
	}()
	return b, resolved, nil
}

func (l launcher) ensureLocalTmux(ctx context.Context, path, name string, spec SessionSpec, cols, rows uint16) error {
	if err := exec.CommandContext(ctx, path, l.tmuxArgs("has-session", "-t", "="+name)...).Run(); err == nil {
		return l.configureLocalTmux(ctx, path)
	}
	args := l.tmuxArgs("new-session", "-d", "-s", name, "-x", fmt.Sprint(cols), "-y", fmt.Sprint(rows))
	if spec.Cwd != "" {
		args = append(args, "-c", spec.Cwd)
	}
	if spec.Shell != "" {
		args = append(args, shellJoin(spec.Shell, spec.Args))
	}
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = localEnvironment(spec.Env)
	if output, err := cmd.CombinedOutput(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return permanentStartError(fmt.Errorf("terminal: create isolated tmux session: %w: %s", err, strings.TrimSpace(string(output))))
	}
	return l.configureLocalTmux(ctx, path)
}

func (l launcher) configureLocalTmux(ctx context.Context, path string) error {
	settings := [][]string{
		{"set-option", "-g", "status", "off"},
		{"set-option", "-g", "prefix", "None"},
		{"set-option", "-g", "prefix2", "None"},
		{"set-option", "-g", "mouse", "on"},
	}
	for _, setting := range settings {
		if output, err := exec.CommandContext(ctx, path, l.tmuxArgs(setting...)...).CombinedOutput(); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return permanentStartError(fmt.Errorf("terminal: configure isolated tmux server: %w: %s", err, strings.TrimSpace(string(output))))
		}
	}
	// tmux gained native OSC 8 support after terminal-features already
	// existed. Treat an unknown feature as an optional capability so older
	// tmux releases keep working; never enable allow-passthrough as a fallback.
	features, queryErr := exec.CommandContext(ctx, path, l.tmuxArgs("show-options", "-gqv", "terminal-features")...).CombinedOutput()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if queryErr == nil && !bytes.Contains(features, []byte("xterm*:hyperlinks")) {
		if _, err := exec.CommandContext(ctx, path, l.tmuxArgs("set-option", "-as", "terminal-features", tmuxHyperlinkFeatures)...).CombinedOutput(); err != nil && ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return nil
}

func (l launcher) tmuxArgs(args ...string) []string {
	base := []string{"-L", l.muxName, "-f", "/dev/null"}
	return append(base, args...)
}

func (l launcher) ensureLocalScreen(ctx context.Context, path, config string, env []string, name string, spec SessionSpec) error {
	if localScreenExists(ctx, path, config, env, name) {
		return nil
	}
	args := []string{"-c", config, "-dmS", name}
	if spec.Shell != "" {
		args = append(args, spec.Shell)
		args = append(args, spec.Args...)
	}
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = env
	if spec.Cwd != "" {
		cmd.Dir = spec.Cwd
	}
	// Do not use CombinedOutput here. Some screen versions let the detached
	// daemon inherit os/exec's capture pipe, which makes CombinedOutput wait for
	// the lifetime of the session rather than the short-lived client process.
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return permanentStartError(fmt.Errorf("terminal: create isolated screen session: %w", err))
	}
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		if localScreenExists(ctx, path, config, env, name) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errors.New("terminal: screen session did not become ready")
		case <-ticker.C:
		}
	}
}

func localScreenExists(ctx context.Context, path, config string, env []string, name string) bool {
	cmd := exec.CommandContext(ctx, path, "-c", config, "-ls", name)
	cmd.Env = env
	out, _ := cmd.CombinedOutput()
	return bytes.Contains(out, []byte("."+name+"\t")) || bytes.Contains(out, []byte("."+name+" "))
}

func (l launcher) terminateLocal(ctx context.Context, spec SessionSpec, resolved Persistence) error {
	name := backendName(spec.ID)
	var path string
	var args []string
	var env []string
	switch resolved {
	case PersistenceTmux:
		_, path, _ = l.resolveLocal(PersistenceTmux)
		args = l.tmuxArgs("kill-session", "-t", "="+name)
	case PersistenceScreen:
		_, path, _ = l.resolveLocal(PersistenceScreen)
		config, screenEnv, err := l.screenRuntime(spec.Env)
		if err != nil {
			return err
		}
		env = screenEnv
		args = []string{"-c", config, "-S", name, "-X", "quit"}
	case PersistenceNone:
		return nil
	default:
		return fmt.Errorf("terminal: invalid local persistence %q", resolved)
	}
	if path == "" {
		return fmt.Errorf("terminal: %s is not installed", resolved)
	}
	cmd := exec.CommandContext(ctx, path, args...)
	if env != nil {
		cmd.Env = env
	}
	if output, err := cmd.CombinedOutput(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if sessionAbsent(resolved, output) {
			return nil
		}
		return fmt.Errorf("terminal: terminate %s session: %w: %s", resolved, err, strings.TrimSpace(string(output)))
	}
	if resolved == PersistenceScreen {
		return waitLocalScreenAbsent(ctx, path, configPath(args), env, name)
	}
	return nil
}

func configPath(args []string) string {
	if len(args) >= 2 && args[0] == "-c" {
		return args[1]
	}
	return ""
}

func waitLocalScreenAbsent(ctx context.Context, path, config string, env []string, name string) error {
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !localScreenExists(ctx, path, config, env, name) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return fmt.Errorf("terminal: screen session %s did not stop", name)
		case <-ticker.C:
			cmd := exec.CommandContext(ctx, path, "-c", config, "-S", name, "-X", "quit")
			cmd.Env = env
			if output, err := cmd.CombinedOutput(); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if sessionAbsent(PersistenceScreen, output) {
					return nil
				}
				return fmt.Errorf("terminal: terminate screen session: %w: %s", err, strings.TrimSpace(string(output)))
			}
		}
	}
}

func sessionAbsent(kind Persistence, output []byte) bool {
	text := strings.ToLower(string(output))
	common := strings.Contains(text, "no server running") || strings.Contains(text, "no sessions found") || strings.Contains(text, "no sockets found")
	if kind == PersistenceTmux {
		return common || strings.Contains(text, "can't find session") || strings.Contains(text, "session not found")
	}
	return common || strings.Contains(text, "no screen session found") || strings.Contains(text, "no screen session")
}

func (l launcher) screenRuntime(extra map[string]string) (string, []string, error) {
	if l.screenMu != nil {
		l.screenMu.Lock()
		defer l.screenMu.Unlock()
	}
	root := l.runtimeDir
	if root == "" {
		cache, err := os.UserCacheDir()
		if err != nil {
			return "", nil, fmt.Errorf("terminal: locate screen runtime directory: %w", err)
		}
		root = filepath.Join(cache, "wmux", "mux")
	}
	dir := filepath.Join(root, "screen-"+l.muxName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", nil, fmt.Errorf("terminal: create isolated screen directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", nil, fmt.Errorf("terminal: secure isolated screen directory: %w", err)
	}
	config := filepath.Join(dir, "screenrc")
	contents := []byte("startup_message off\nhardstatus off\ncaption splitonly\nvbell off\nescape ^^^\n")
	if err := os.WriteFile(config, contents, 0o600); err != nil {
		return "", nil, fmt.Errorf("terminal: write isolated screen config: %w", err)
	}
	if err := os.Chmod(config, 0o600); err != nil {
		return "", nil, fmt.Errorf("terminal: secure isolated screen config: %w", err)
	}
	env := localEnvironment(extra)
	env = setEnvironment(env, "SCREENDIR", dir)
	return config, env, nil
}

func setEnvironment(env []string, key, value string) []string {
	prefix := key + "="
	for index, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			env[index] = prefix + value
			return env
		}
	}
	return append(env, prefix+value)
}

func expandLocalHome(path string) (string, error) {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("terminal: expand home directory: %w", err)
	}
	if path == "~" {
		return home, nil
	}
	return filepath.Join(home, strings.TrimPrefix(path, "~/")), nil
}

func localEnvironment(extra map[string]string) []string {
	values := make(map[string]string)
	for _, entry := range os.Environ() {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			values[key] = value
		}
	}
	if _, explicitlySet := extra["TERM"]; !explicitlySet {
		values["TERM"] = "xterm-256color"
	}
	for key, value := range extra {
		values[key] = value
	}
	return sortedEnv(values)
}

func terminalSize(spec SessionSpec) (cols, rows uint16) {
	cols, rows = spec.Cols, spec.Rows
	if cols == 0 {
		cols = 80
	}
	if rows == 0 {
		rows = 24
	}
	return cols, rows
}

func (b *localBackend) Read(p []byte) (int, error) { return b.pty.Read(p) }
func (b *localBackend) Write(p []byte) (int, error) {
	b.inputMu.Lock()
	defer b.inputMu.Unlock()
	return b.pty.Write(p)
}

func (b *localBackend) WriteContext(ctx context.Context, p []byte) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	stopCancellation := context.AfterFunc(ctx, func() { _ = b.Close() })
	n, err := b.Write(p)
	stopCancellation()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return n, ctxErr
	}
	return n, err
}

func (b *localBackend) Resize(cols, rows uint16) error {
	return pty.Setsize(b.pty, &pty.Winsize{Cols: cols, Rows: rows})
}

func (b *localBackend) Wait(ctx context.Context) error {
	select {
	case err, ok := <-b.done:
		if !ok {
			return nil
		}
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *localBackend) Close() error {
	b.closeOnce.Do(func() {
		// Close is the detach operation for tmux/screen and the shutdown
		// operation for a direct PTY. Killing this client process cannot kill a
		// named tmux/screen server, and guarantees Manager.Close does not wait
		// forever on a process that ignores terminal hangup.
		var processErr error
		if b.cmd.Process != nil {
			processErr = b.cmd.Process.Kill()
			if errors.Is(processErr, os.ErrProcessDone) {
				processErr = nil
			}
		}
		b.closeErr = errors.Join(processErr, b.pty.Close())
	})
	return b.closeErr
}

func (b *localBackend) Terminate(ctx context.Context) error {
	if b.kind != PersistenceNone {
		return errors.New("terminal: persistent local backend must be terminated through its isolated launcher")
	}
	if b.cmd.Process != nil {
		if err := b.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return err
		}
	}
	_ = b.Close()
	return nil
}

func (b *localBackend) Reconnectable(err error) bool {
	return b.kind != PersistenceNone && err != nil && !isTerminalEOF(err)
}
