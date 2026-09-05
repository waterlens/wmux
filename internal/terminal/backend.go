package terminal

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"maps"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
)

type backend interface {
	io.Reader
	WriteContext(context.Context, []byte) (int, error)
	Resize(cols, rows uint16) error
	Wait(context.Context) error
	Close() error
	Terminate(context.Context) error
	Reconnectable(error) bool
}

// launcher creates or attaches a backend connection; create is set only for a
// session's first launch.
type launcher interface {
	start(ctx context.Context, spec SessionSpec, resolved Persistence, create bool) (backend, Persistence, error)
	terminate(ctx context.Context, spec SessionSpec, resolved Persistence) error
}

// execLauncher drives the local tmux/screen binaries and remote shell scripts.
type execLauncher struct {
	tmuxPath   string
	screenPath string
	muxName    string
	runtimeDir string
	// screenMu serializes writes to the shared isolated screen runtime directory.
	screenMu sync.Mutex
}

const (
	tmuxHyperlinkFeatures = ",xterm*:hyperlinks"
	tmuxTrueColorOverride = ",xterm*:Tc"

	// remoteMissingExitStatus is the attach script's exit status for a missing session.
	remoteMissingExitStatus = 3

	maxMuxNameLen = 32

	// muxSettleTimeout and muxPollInterval bound the wait for a local or remote
	// multiplexer session to appear or disappear.
	muxSettleTimeout = 2 * time.Second
	muxPollInterval  = 50 * time.Millisecond

	backendReadBuffer = 32 << 10
)

// DefaultCols and DefaultRows are the terminal dimensions a new session starts
// with, before a browser reports the size it actually rendered.
const (
	DefaultCols uint16 = 120
	DefaultRows uint16 = 36
)

// tmuxBaseEnvironment is tmux's documented update-environment default.
var tmuxBaseEnvironment = []string{
	"DISPLAY", "KRB5CCNAME", "SSH_ASKPASS", "SSH_AUTH_SOCK", "SSH_AGENT_PID",
	"SSH_CONNECTION", "WINDOWID", "XAUTHORITY",
}

// screenConfigLines is the isolated screen session's fixed screenrc; local and
// remote launches render the same lines through different mechanisms.
var screenConfigLines = []string{
	"startup_message off", "hardstatus off", "caption splitonly", "vbell off", "escape ^^^",
}

// tmuxServerOptions are the isolated tmux server's fixed settings.
var tmuxServerOptions = [][2]string{
	{"status", "off"}, {"prefix", "None"}, {"prefix2", "None"}, {"mouse", "on"},
}

func newExecLauncher(cfg Config) *execLauncher {
	return &execLauncher{
		tmuxPath:   cfg.tmuxPath,
		screenPath: cfg.screenPath,
		muxName:    safeMuxName(cfg.MuxName, "wmux", maxMuxNameLen),
		runtimeDir: cfg.MuxRuntimeDir,
	}
}

func (l *execLauncher) start(ctx context.Context, spec SessionSpec, resolved Persistence, create bool) (backend, Persistence, error) {
	if spec.Host != nil {
		return l.startSSH(ctx, spec, resolved, create)
	}
	return l.startLocal(ctx, spec, resolved, create)
}

// terminate kills the multiplexer session itself; killBackend decides whether a
// session is persistent enough to need it.
func (l *execLauncher) terminate(ctx context.Context, spec SessionSpec, resolved Persistence) error {
	if spec.Host != nil {
		return l.terminateSSH(ctx, spec, resolved)
	}
	return l.terminateLocal(ctx, spec, resolved)
}

// resolveLocal reports the persistence to use and the absolute path of its binary.
func (l *execLauncher) resolveLocal(requested Persistence) (Persistence, string, error) {
	if requested == "" {
		requested = PersistenceAuto
	}
	find := func(explicit, fallback string) string {
		name := explicit
		if name == "" {
			name = fallback
		}
		path, err := exec.LookPath(name)
		if err != nil {
			return ""
		}
		return path
	}

	switch requested {
	case PersistenceAuto:
		if path := find(l.tmuxPath, "tmux"); path != "" {
			return PersistenceTmux, path, nil
		}
		if path := find(l.screenPath, "screen"); path != "" {
			return PersistenceScreen, path, nil
		}
		return PersistenceNone, "", nil
	case PersistenceTmux:
		if path := find(l.tmuxPath, "tmux"); path != "" {
			return PersistenceTmux, path, nil
		}
		return "", "", permanentStartError(errors.New("terminal: tmux was requested but is not installed"))
	case PersistenceScreen:
		if path := find(l.screenPath, "screen"); path != "" {
			return PersistenceScreen, path, nil
		}
		return "", "", permanentStartError(errors.New("terminal: screen was requested but is not installed"))
	case PersistenceNone:
		return PersistenceNone, "", nil
	default:
		return "", "", permanentStartError(fmt.Errorf("terminal: unsupported persistence %q", requested))
	}
}

type permanentError struct{ error }

func permanentStartError(err error) error {
	if err == nil || isPermanentStartError(err) {
		return err
	}
	return permanentError{error: err}
}

func (e permanentError) Unwrap() error { return e.error }

func isPermanentStartError(err error) bool {
	var target permanentError
	return errors.As(err, &target)
}

func isTerminalEOF(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, syscall.EIO)
}

var unsafeSessionName = regexp.MustCompile(`[^A-Za-z0-9_-]+`)

// safeMuxName reduces value to the characters tmux and screen accept in a
// socket or session name, falling back to fallback when nothing is left.
func safeMuxName(value, fallback string, limit int) string {
	name := strings.Trim(unsafeSessionName.ReplaceAllString(value, "-"), "-")
	if name == "" {
		name = fallback
	}
	if len(name) > limit {
		name = name[:limit]
	}
	return name
}

// MuxSessionName returns the deterministic tmux/screen name used for a session.
// It is deliberately short: screen names its Unix socket "<pid>.<name>" inside
// SCREENDIR, and Unix socket paths are limited to roughly 104 bytes.
func MuxSessionName(sessionID string) string {
	hash := sha256.Sum256([]byte(sessionID))
	return fmt.Sprintf("wmux-%x", hash[:8])
}

// muxSessionNameLen is the fixed length of every MuxSessionName result.
const muxSessionNameLen = len("wmux-") + 16

// reconnectDelay is minimum doubled per attempt, capped at maximum. NewManager
// guarantees both bounds are positive.
func reconnectDelay(minimum, maximum time.Duration, attempt int) time.Duration {
	delay := minimum
	for i := 0; i < attempt && delay < maximum; i++ {
		delay *= 2
	}
	return min(delay, maximum)
}

func shellQuote(value string) string {
	if value == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func shellJoin(command string, args []string) string {
	parts := make([]string, 0, 1+len(args))
	if command != "" {
		parts = append(parts, shellQuote(command))
	}
	for _, arg := range args {
		parts = append(parts, shellQuote(arg))
	}
	return strings.Join(parts, " ")
}

// tmuxEnvironmentList is the update-environment value for a session's variables.
func tmuxEnvironmentList(env map[string]string) string {
	names := append([]string(nil), tmuxBaseEnvironment...)
	for _, key := range sortedKeys(env) {
		if !slices.Contains(names, key) {
			names = append(names, key)
		}
	}
	return strings.Join(names, " ")
}

// posixScript wraps a script so it runs under /bin/sh, not the login shell.
func posixScript(script string) string {
	return "exec /bin/sh -c " + shellQuote(script)
}

func sortedKeys(env map[string]string) []string { return slices.Sorted(maps.Keys(env)) }

func sortedEnv(env map[string]string) []string {
	keys := sortedKeys(env)
	values := make([]string, 0, len(keys))
	for _, key := range keys {
		values = append(values, key+"="+env[key])
	}
	return values
}
