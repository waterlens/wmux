package terminal

import (
	"bytes"
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

type sshBackend struct {
	client       *ssh.Client
	session      *ssh.Session
	stdin        io.WriteCloser
	stdout       *io.PipeReader
	stdoutWriter *io.PipeWriter
	done         chan error
	kind         Persistence

	authClosers []io.Closer
	keepalive   chan struct{}
	closeOnce   sync.Once
	closeErr    error
	// input is a capacity-1 semaphore that serializes writes.
	input chan struct{}
}

func (l *execLauncher) startSSH(ctx context.Context, spec SessionSpec, requested Persistence, create bool) (b backend, resolved Persistence, err error) {
	client, closers, err := dialSSH(ctx, *spec.Host)
	if err != nil {
		return nil, "", err
	}
	stopSetupCancellation := context.AfterFunc(ctx, func() { _ = client.Close() })
	var sess *ssh.Session
	var reader *io.PipeReader
	var writer *io.PipeWriter
	// Every failure below unwinds the same way; a cancelled context wins over
	// whatever error the closing connection reported.
	defer func() {
		if err == nil {
			return
		}
		if reader != nil {
			_ = reader.Close()
			_ = writer.Close()
		}
		if sess != nil {
			_ = sess.Close()
		}
		stopSetupCancellation()
		_ = client.Close()
		closeAll(closers)
		if ctxErr := ctx.Err(); ctxErr != nil {
			err = ctxErr
		}
	}()

	if resolved, err = resolveRemote(ctx, client, requested); err != nil {
		return nil, "", err
	}
	if err = ctx.Err(); err != nil {
		return nil, "", err
	}
	// A direct remote shell has nothing to reattach to.
	if resolved == PersistenceNone && !create {
		return nil, "", ErrBackendMissing
	}
	if sess, err = client.NewSession(); err != nil {
		return nil, "", fmt.Errorf("terminal: create SSH session: %w", err)
	}
	cols, rows := terminalSize(spec)
	modes := ssh.TerminalModes{
		ssh.ECHO:          1,
		ssh.TTY_OP_ISPEED: 14400,
		ssh.TTY_OP_OSPEED: 14400,
	}
	if err = sess.RequestPty("xterm-256color", int(rows), int(cols), modes); err != nil {
		return nil, "", permanentStartError(fmt.Errorf("terminal: request SSH PTY: %w", err))
	}
	stdin, err := sess.StdinPipe()
	if err != nil {
		return nil, "", fmt.Errorf("terminal: SSH stdin: %w", err)
	}
	reader, writer = io.Pipe()
	sess.Stdout = writer
	sess.Stderr = writer

	if command := l.remoteAttachCommand(spec, resolved, BackendName(spec.ID), create); command == "" {
		err = sess.Shell()
	} else {
		err = sess.Start(posixScript(command))
	}
	if err != nil {
		return nil, "", permanentStartError(fmt.Errorf("terminal: start remote %s: %w", resolved, err))
	}

	remote := &sshBackend{
		client:       client,
		session:      sess,
		stdin:        stdin,
		stdout:       reader,
		stdoutWriter: writer,
		done:         make(chan error, 1),
		kind:         resolved,
		authClosers:  closers,
		keepalive:    make(chan struct{}),
		input:        make(chan struct{}, 1),
	}
	go func() {
		waitErr := sess.Wait()
		_ = writer.CloseWithError(waitErr)
		remote.done <- waitErr
		close(remote.done)
	}()
	if interval := keepAliveInterval(*spec.Host); interval > 0 {
		go remote.runKeepalive(interval)
	}
	stopSetupCancellation()
	return remote, resolved, nil
}

func dialSSH(ctx context.Context, host HostSpec) (*ssh.Client, []io.Closer, error) {
	if strings.TrimSpace(host.Address) == "" {
		return nil, nil, permanentStartError(errors.New("terminal: SSH address is required"))
	}
	if strings.TrimSpace(host.User) == "" {
		return nil, nil, permanentStartError(errors.New("terminal: SSH user is required"))
	}
	fingerprint := strings.TrimSpace(host.Fingerprint)
	if !strings.HasPrefix(fingerprint, "SHA256:") {
		return nil, nil, permanentStartError(errors.New("terminal: an SHA256 host key fingerprint is required"))
	}
	auth, closers, err := sshAuthContext(ctx, host.Credential)
	if err != nil {
		return nil, nil, permanentStartError(err)
	}
	closeOnError := func(err error) (*ssh.Client, []io.Closer, error) {
		closeAll(closers)
		return nil, nil, err
	}

	timeout := host.ConnectTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	hostKeyCallback, err := strictHostKeyCallback(fingerprint)
	if err != nil {
		return closeOnError(err)
	}
	config := &ssh.ClientConfig{
		User:            host.User,
		Auth:            auth,
		Timeout:         timeout,
		HostKeyCallback: hostKeyCallback,
	}
	address := sshAddress(host.Address)
	dialer := net.Dialer{Timeout: timeout, KeepAlive: keepAliveInterval(host)}
	conn, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return closeOnError(fmt.Errorf("terminal: dial SSH %s: %w", address, err))
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(timeout))
	}
	stopHandshakeCancellation := context.AfterFunc(ctx, func() { _ = conn.Close() })
	sshConn, chans, reqs, err := ssh.NewClientConn(conn, address, config)
	stopHandshakeCancellation()
	if err != nil {
		_ = conn.Close()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return closeOnError(ctxErr)
		}
		handshakeErr := fmt.Errorf("terminal: SSH handshake %s: %w", address, err)
		text := strings.ToLower(err.Error())
		if isPermanentStartError(err) || strings.Contains(text, "unable to authenticate") || strings.Contains(text, "no supported methods remain") || strings.Contains(text, "host key") {
			handshakeErr = permanentStartError(handshakeErr)
		}
		return closeOnError(handshakeErr)
	}
	_ = conn.SetDeadline(time.Time{})
	return ssh.NewClient(sshConn, chans, reqs), closers, nil
}

func strictHostKeyCallback(fingerprint string) (ssh.HostKeyCallback, error) {
	fingerprint = strings.TrimSpace(fingerprint)
	if !strings.HasPrefix(fingerprint, "SHA256:") {
		return nil, errors.New("terminal: an SHA256 host key fingerprint is required")
	}
	return func(_ string, _ net.Addr, key ssh.PublicKey) error {
		actual := ssh.FingerprintSHA256(key)
		if len(actual) != len(fingerprint) || subtle.ConstantTimeCompare([]byte(actual), []byte(fingerprint)) != 1 {
			return permanentStartError(fmt.Errorf("terminal: SSH host key mismatch: got %s", actual))
		}
		return nil
	}, nil
}

func sshAuthContext(ctx context.Context, credential Credential) ([]ssh.AuthMethod, []io.Closer, error) {
	switch value := credential.(type) {
	case PasswordCredential:
		if value.Password == "" {
			return nil, nil, errors.New("terminal: SSH password is empty")
		}
		return []ssh.AuthMethod{ssh.Password(value.Password)}, nil, nil
	case PrivateKeyCredential:
		if len(value.PEM) == 0 {
			return nil, nil, errors.New("terminal: SSH private key is empty")
		}
		var signer ssh.Signer
		var err error
		if len(value.Passphrase) != 0 {
			signer, err = ssh.ParsePrivateKeyWithPassphrase(value.PEM, value.Passphrase)
		} else {
			signer, err = ssh.ParsePrivateKey(value.PEM)
		}
		if err != nil {
			return nil, nil, fmt.Errorf("terminal: parse SSH private key: %w", err)
		}
		return []ssh.AuthMethod{ssh.PublicKeys(signer)}, nil, nil
	case AgentCredential:
		socket := value.Socket
		if socket == "" {
			socket = os.Getenv("SSH_AUTH_SOCK")
		}
		if socket == "" {
			return nil, nil, errors.New("terminal: SSH agent socket is not configured")
		}
		conn, err := (&net.Dialer{}).DialContext(ctx, "unix", socket)
		if err != nil {
			return nil, nil, fmt.Errorf("terminal: connect SSH agent: %w", err)
		}
		agentClient := agent.NewClient(conn)
		return []ssh.AuthMethod{ssh.PublicKeysCallback(agentClient.Signers)}, []io.Closer{conn}, nil
	case nil:
		return nil, nil, errors.New("terminal: SSH credential is required")
	default:
		return nil, nil, fmt.Errorf("terminal: unsupported SSH credential %T", credential)
	}
}

func resolveRemote(ctx context.Context, client *ssh.Client, requested Persistence) (Persistence, error) {
	if requested == "" {
		requested = PersistenceAuto
	}
	probe := func(command string) (bool, error) {
		s, err := client.NewSession()
		if err != nil {
			return false, err
		}
		defer s.Close()
		err = runSSHSession(ctx, s, posixScript("command -v "+command+" >/dev/null 2>&1"))
		if err == nil {
			return true, nil
		}
		var exitErr *ssh.ExitError
		if errors.As(err, &exitErr) {
			return false, nil
		}
		return false, err
	}
	switch requested {
	case PersistenceAuto:
		exists, err := probe("tmux")
		if err != nil {
			return "", fmt.Errorf("terminal: probe remote tmux: %w", err)
		}
		if exists {
			return PersistenceTmux, nil
		}
		exists, err = probe("screen")
		if err != nil {
			return "", fmt.Errorf("terminal: probe remote screen: %w", err)
		}
		if exists {
			return PersistenceScreen, nil
		}
		return PersistenceNone, nil
	case PersistenceTmux:
		exists, err := probe("tmux")
		if err != nil {
			return "", fmt.Errorf("terminal: probe remote tmux: %w", err)
		}
		if !exists {
			return "", permanentStartError(errors.New("terminal: tmux was requested but is unavailable on the SSH host"))
		}
		return PersistenceTmux, nil
	case PersistenceScreen:
		exists, err := probe("screen")
		if err != nil {
			return "", fmt.Errorf("terminal: probe remote screen: %w", err)
		}
		if !exists {
			return "", permanentStartError(errors.New("terminal: screen was requested but is unavailable on the SSH host"))
		}
		return PersistenceScreen, nil
	case PersistenceNone:
		return PersistenceNone, nil
	default:
		return "", permanentStartError(fmt.Errorf("terminal: unsupported persistence %q", requested))
	}
}

func runSSHSession(ctx context.Context, session *ssh.Session, command string) error {
	if err := session.Start(command); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- session.Wait() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		_ = session.Close()
		return ctx.Err()
	}
}

var validEnvName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// remoteAttachCommand builds the POSIX script that attaches to, and with create
// first creates, the remote persistent session.
func (l *execLauncher) remoteAttachCommand(spec SessionSpec, resolved Persistence, name string, create bool) string {
	env := spec.Env
	exports := make([]string, 0, len(env))
	for _, entry := range sortedEnv(env) {
		key, value, _ := strings.Cut(entry, "=")
		if validEnvName.MatchString(key) {
			exports = append(exports, "export "+key+"="+shellQuote(value))
		}
	}
	program := ""
	if spec.Shell != "" {
		program = "exec " + shellJoin(spec.Shell, spec.Args)
	}
	missing := "echo " + shellQuote("wmux: session "+name+" no longer exists on this host") +
		" >&2; exit " + fmt.Sprint(remoteMissingExitStatus)

	switch resolved {
	case PersistenceTmux:
		tmux := "tmux -L " + shellQuote(l.muxName) + " -f /dev/null"
		target := shellQuote("=" + name)
		// update-environment and new-session run in one invocation; ";" is quoted
		// because it is tmux's command separator, not the shell's.
		spawn := tmux + " set-option -g update-environment " + shellQuote(tmuxEnvironmentList(env)) +
			" ';' new-session -d -s " + shellQuote(name)
		if spec.Cwd != "" {
			spawn += " -c " + remotePath(spec.Cwd)
		}
		if spec.Shell != "" {
			// tmux runs this through default-shell -c, so it is one quoted word.
			spawn += " " + shellQuote(shellJoin(spec.Shell, spec.Args))
		}
		if !create {
			spawn = missing
		}
		parts := append([]string(nil), exports...)
		parts = append(parts,
			"if ! "+tmux+" has-session -t "+target+" 2>/dev/null; then "+spawn+"; fi",
			tmux+" set-option -g status off",
			tmux+" set-option -g prefix None",
			tmux+" set-option -g prefix2 None",
			tmux+" set-option -g mouse on",
			// Unknown terminal features on an older remote are non-fatal.
			remoteTmuxAppend(tmux, "terminal-features", tmuxHyperlinkFeatures, "xterm*:hyperlinks"),
			remoteTmuxAppend(tmux, "terminal-overrides", tmuxTrueColorOverride, "xterm*:Tc"),
			"exec "+tmux+" attach-session -t "+target,
		)
		return strings.Join(parts, "; ")
	case PersistenceScreen:
		parts := append([]string(nil), exports...)
		parts = append(parts, remoteScreenSetup(l.muxName)...)
		if spec.Cwd != "" {
			parts = append(parts, "cd -- "+remotePath(spec.Cwd))
		}
		screen := `screen -c "$wmux_screen_rc"`
		marker := shellQuote("[.]" + name + "[[:space:]]")
		spawn := screen + " -dmS " + shellQuote(name)
		if program != "" {
			spawn += " sh -lc " + shellQuote(program)
		}
		if !create {
			spawn = missing
		}
		parts = append(parts, "if ! "+screen+" -ls | grep -q "+marker+"; then "+spawn+"; fi")
		parts = append(parts, "exec "+screen+" -x "+shellQuote(name))
		return strings.Join(parts, "; ")
	case PersistenceNone:
		parts := append([]string(nil), exports...)
		if spec.Cwd != "" {
			parts = append(parts, "cd -- "+remotePath(spec.Cwd))
		}
		if len(parts) == 0 && program == "" {
			return ""
		}
		if program == "" {
			program = `exec "${SHELL:-/bin/sh}" -l`
		}
		return strings.Join(append(parts, program), "; ")
	default:
		return ""
	}
}

func remoteTmuxAppend(tmux, option, value, marker string) string {
	return "if ! " + tmux + " show-options -gqv " + option + " 2>/dev/null | grep -Fq -- " + shellQuote(marker) + "; then " +
		tmux + " set-option -as " + option + " " + shellQuote(value) + " 2>/dev/null || :; fi"
}

func remotePath(path string) string {
	if path == "~" {
		return `"$HOME"`
	}
	if strings.HasPrefix(path, "~/") {
		rest := strings.TrimPrefix(path, "~/")
		if rest == "" {
			return `"$HOME"`
		}
		return `"$HOME"/` + shellQuote(rest)
	}
	return shellQuote(path)
}

func remoteScreenSetup(namespace string) []string {
	configLines := []string{
		"startup_message off",
		"hardstatus off",
		"caption splitonly",
		"vbell off",
		"escape ^^^",
	}
	quotedLines := make([]string, 0, len(configLines))
	for _, line := range configLines {
		quotedLines = append(quotedLines, shellQuote(line))
	}
	return []string{
		`wmux_screen_root="${XDG_CACHE_HOME:-$HOME/.cache}/wmux/mux/screen-` + namespace + `"`,
		`wmux_screen_sockets="$wmux_screen_root/sockets"`,
		`wmux_screen_rc="$wmux_screen_root/screenrc"`,
		`mkdir -p -- "$wmux_screen_sockets"`,
		`chmod 700 "$wmux_screen_root" "$wmux_screen_sockets"`,
		`umask 077`,
		`printf '%s\n' ` + strings.Join(quotedLines, " ") + ` > "$wmux_screen_rc"`,
		`export SCREENDIR="$wmux_screen_sockets"`,
	}
}

func (l *execLauncher) terminateSSH(ctx context.Context, spec SessionSpec, resolved Persistence) error {
	client, closers, err := dialSSH(ctx, *spec.Host)
	if err != nil {
		return err
	}
	stopCancellation := context.AfterFunc(ctx, func() { _ = client.Close() })
	defer stopCancellation()
	defer client.Close()
	defer closeAll(closers)
	sess, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("terminal: create SSH termination session: %w", err)
	}
	defer sess.Close()
	name := BackendName(spec.ID)
	output, err := runSSHOutput(ctx, sess, posixScript(l.remoteTerminateCommand(resolved, name)))
	if err != nil && !sessionAbsent(resolved, output) {
		return fmt.Errorf("terminal: terminate remote %s: %w", resolved, err)
	}
	return nil
}

func (l *execLauncher) remoteTerminateCommand(resolved Persistence, name string) string {
	if resolved == PersistenceTmux {
		tmux := "tmux -L " + shellQuote(l.muxName) + " -f /dev/null"
		return tmux + " kill-session -t " + shellQuote("="+name)
	}
	parts := remoteScreenSetup(l.muxName)
	screen := `screen -c "$wmux_screen_rc"`
	marker := shellQuote("[.]" + name + "[[:space:]]")
	quit := screen + ` -S ` + shellQuote(name) + ` -X quit`
	parts = append(parts, "wmux_screen_attempt=0")
	parts = append(parts, `while `+screen+` -ls | grep -q `+marker+`; do `+
		quit+`; wmux_screen_attempt=$((wmux_screen_attempt + 1)); `+
		`if [ "$wmux_screen_attempt" -ge 40 ]; then echo 'wmux: screen session did not stop' >&2; exit 1; fi; `+
		`sleep 0.05; done`)
	return strings.Join(parts, "; ")
}

func runSSHOutput(ctx context.Context, session *ssh.Session, command string) ([]byte, error) {
	var output bytes.Buffer
	session.Stdout = &output
	session.Stderr = &output
	if err := runSSHSession(ctx, session, command); err != nil {
		return output.Bytes(), err
	}
	return output.Bytes(), nil
}

func (b *sshBackend) Read(p []byte) (int, error) { return b.stdout.Read(p) }

// WriteContext waits for its turn under the caller's context.
func (b *sshBackend) WriteContext(ctx context.Context, p []byte) (int, error) {
	select {
	case b.input <- struct{}{}:
	case <-ctx.Done():
		return 0, ctx.Err()
	}
	defer func() { <-b.input }()
	return b.stdin.Write(p)
}

func (b *sshBackend) Resize(cols, rows uint16) error {
	return b.session.WindowChange(int(rows), int(cols))
}

func (b *sshBackend) Wait(ctx context.Context) error {
	select {
	case err, ok := <-b.done:
		if !ok {
			return nil
		}
		// The attach script reports a vanished session with a dedicated status.
		var exitErr *ssh.ExitError
		if b.kind != PersistenceNone && errors.As(err, &exitErr) && exitErr.ExitStatus() == remoteMissingExitStatus {
			return ErrBackendMissing
		}
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *sshBackend) Close() error {
	b.closeOnce.Do(func() {
		close(b.keepalive)
		b.closeErr = errors.Join(b.stdin.Close(), b.session.Close(), b.client.Close(), b.stdout.Close(), b.stdoutWriter.Close())
		closeAll(b.authClosers)
	})
	return b.closeErr
}

// Terminate ends a non-persistent remote shell. killBackend never routes a tmux
// or screen session here; those are killed through a fresh control connection.
func (b *sshBackend) Terminate(ctx context.Context) error {
	stopCancellation := context.AfterFunc(ctx, func() { _ = b.client.Close() })
	if err := b.session.Signal(ssh.SIGKILL); err != nil && !errors.Is(err, io.EOF) {
		stopCancellation()
		return err
	}
	stopCancellation()
	_ = b.Close()
	return nil
}

func (b *sshBackend) Reconnectable(err error) bool {
	if b.kind == PersistenceNone || err == nil || errors.Is(err, ErrBackendMissing) {
		return false
	}
	var exitErr *ssh.ExitError
	return !errors.As(err, &exitErr)
}

func (b *sshBackend) runKeepalive(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-b.keepalive:
			return
		case <-ticker.C:
			if _, _, err := b.client.SendRequest("keepalive@openssh.com", true, nil); err != nil {
				_ = b.client.Close()
				return
			}
		}
	}
}

func keepAliveInterval(host HostSpec) time.Duration {
	if host.KeepAliveInterval < 0 {
		return 0
	}
	if host.KeepAliveInterval == 0 {
		return 15 * time.Second
	}
	return host.KeepAliveInterval
}

func sshAddress(address string) string {
	if _, _, err := net.SplitHostPort(address); err == nil {
		return address
	}
	if net.ParseIP(address) != nil {
		return net.JoinHostPort(address, "22")
	}
	if !strings.Contains(address, ":") {
		return net.JoinHostPort(address, "22")
	}
	return address
}

func closeAll(closers []io.Closer) {
	for _, closer := range closers {
		_ = closer.Close()
	}
}
