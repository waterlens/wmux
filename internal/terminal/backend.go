package terminal

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

type backend interface {
	io.Reader
	io.Writer
	WriteContext(context.Context, []byte) (int, error)
	Resize(cols, rows uint16) error
	Wait(context.Context) error
	Close() error
	Terminate(context.Context) error
	Reconnectable(error) bool
}

type backendLauncher interface {
	start(ctx context.Context, spec SessionSpec, resolved Persistence) (backend, Persistence, error)
	terminate(ctx context.Context, spec SessionSpec, resolved Persistence) error
}

type launcher struct {
	tmuxPath   string
	screenPath string
	muxName    string
	runtimeDir string
	screenMu   *sync.Mutex
}

const tmuxHyperlinkFeatures = ",xterm*:hyperlinks"

func newLauncher(cfg Config) launcher {
	name := unsafeSessionName.ReplaceAllString(cfg.MuxName, "-")
	name = strings.Trim(name, "-")
	if name == "" {
		name = "wmux"
	}
	if len(name) > 32 {
		name = name[:32]
	}
	return launcher{tmuxPath: cfg.TmuxPath, screenPath: cfg.ScreenPath, muxName: name, runtimeDir: cfg.MuxRuntimeDir, screenMu: &sync.Mutex{}}
}

func (l launcher) start(ctx context.Context, spec SessionSpec, resolved Persistence) (backend, Persistence, error) {
	if spec.Host != nil {
		return l.startSSH(ctx, spec, resolved)
	}
	return l.startLocal(ctx, spec, resolved)
}

func (l launcher) terminate(ctx context.Context, spec SessionSpec, resolved Persistence) error {
	if resolved == "" || resolved == PersistenceAuto {
		return nil
	}
	if spec.Host != nil {
		return l.terminateSSH(ctx, spec, resolved)
	}
	return l.terminateLocal(ctx, spec, resolved)
}

func (l launcher) resolveLocal(requested Persistence) (Persistence, string, error) {
	if requested == "" {
		requested = PersistenceAuto
	}
	find := func(explicit, fallback string) string {
		if explicit != "" {
			if _, err := exec.LookPath(explicit); err == nil {
				return explicit
			}
			return ""
		}
		path, _ := exec.LookPath(fallback)
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

// IsPermanentStartError reports whether reconnecting without a configuration
// refresh would repeat the same failure (for example bad credentials).
func IsPermanentStartError(err error) bool { return isPermanentStartError(err) }

func isTerminalEOF(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, syscall.EIO)
}

var unsafeSessionName = regexp.MustCompile(`[^A-Za-z0-9_-]+`)

func backendName(id string) string {
	name := unsafeSessionName.ReplaceAllString(id, "-")
	name = strings.Trim(name, "-")
	if name == "" {
		name = "session"
	}
	if len(name) > 40 {
		name = name[:40]
	}
	hash := sha256.Sum256([]byte(id))
	return fmt.Sprintf("wmux-%s-%x", name, hash[:4])
}

// BackendName returns the deterministic tmux/screen name used for a session.
// Storage and diagnostics can use it without duplicating sanitization rules.
func BackendName(sessionID string) string {
	return backendName(sessionID)
}

func reconnectDelay(minimum, maximum time.Duration, attempt int) time.Duration {
	if minimum <= 0 {
		minimum = 250 * time.Millisecond
	}
	if maximum <= 0 {
		maximum = 10 * time.Second
	}
	if maximum < minimum {
		maximum = minimum
	}
	delay := minimum
	for i := 0; i < attempt && delay < maximum/2; i++ {
		delay *= 2
	}
	if delay > maximum {
		return maximum
	}
	return delay
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

func sortedEnv(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	values := make([]string, 0, len(keys))
	for _, key := range keys {
		values = append(values, key+"="+env[key])
	}
	return values
}
