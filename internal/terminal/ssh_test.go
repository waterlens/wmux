package terminal

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestStrictHostKeyFingerprint(t *testing.T) {
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := ssh.FingerprintSHA256(key)
	callback, err := StrictHostKeyCallback(fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if err := callback("ignored", nil, key); err != nil {
		t.Fatalf("matching fingerprint rejected: %v", err)
	}

	wrong := fingerprint[:len(fingerprint)-1] + "x"
	callback, err = StrictHostKeyCallback(wrong)
	if err != nil {
		t.Fatal(err)
	}
	if err := callback("ignored", nil, key); err == nil || !strings.Contains(err.Error(), "host key mismatch") {
		t.Fatalf("mismatched fingerprint error = %v", err)
	}
	if _, err := StrictHostKeyCallback(ssh.FingerprintLegacyMD5(key)); err == nil {
		t.Fatal("legacy MD5 fingerprint was accepted")
	}
}

func TestPrivateKeyCredentialWithPassphrase(t *testing.T) {
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKeyWithPassphrase(private, "wmux-test", []byte("correct horse"))
	if err != nil {
		t.Fatal(err)
	}
	encoded := pem.EncodeToMemory(block)
	methods, closers, err := SSHAuthMethods(t.Context(), PrivateKeyCredential{PEM: encoded, Passphrase: []byte("correct horse")})
	if err != nil {
		t.Fatal(err)
	}
	if len(methods) != 1 || len(closers) != 0 {
		t.Fatalf("auth methods = %d, closers = %d", len(methods), len(closers))
	}
	if _, _, err := SSHAuthMethods(t.Context(), PrivateKeyCredential{PEM: encoded, Passphrase: []byte("wrong")}); err == nil {
		t.Fatal("wrong private-key passphrase was accepted")
	}
}

func TestPasswordAndAgentCredentialsValidate(t *testing.T) {
	methods, closers, err := SSHAuthMethods(t.Context(), PasswordCredential{Password: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	if len(methods) != 1 || len(closers) != 0 {
		t.Fatalf("auth methods = %d, closers = %d", len(methods), len(closers))
	}
	if _, _, err := SSHAuthMethods(t.Context(), PasswordCredential{}); err == nil {
		t.Fatal("empty password was accepted")
	}
	if _, _, err := SSHAuthMethods(t.Context(), AgentCredential{Socket: t.TempDir() + "/missing-agent.sock"}); err == nil {
		t.Fatal("missing SSH agent socket was accepted")
	}
}

func TestRemoteAttachCommandQuotesValues(t *testing.T) {
	command := newExecLauncher(Config{}).remoteAttachCommand(SessionSpec{
		Cwd:   "/tmp/a'b",
		Shell: "/bin/zsh",
		Args:  []string{"-l", "argument with spaces"},
		Env:   map[string]string{"SAFE_NAME": "a'b", "COLORTERM": "truecolor"},
	}, PersistenceTmux, "wmux-safe", true)
	for _, wanted := range []string{
		"export SAFE_NAME='a'\"'\"'b'",
		"export COLORTERM='truecolor'",
		"tmux -L 'wmux' -f /dev/null set-option -g update-environment",
		"';' new-session -d",
		"/tmp/a", "/bin/zsh", "argument with spaces",
		"status off", "prefix None", "mouse on", "escape-time 10", "focus-events on",
		"terminal-features", "xterm*:hyperlinks",
		"terminal-overrides", "xterm*:Tc",
	} {
		if !strings.Contains(command, wanted) {
			t.Fatalf("remote command %q does not contain %q", command, wanted)
		}
	}
}

func TestRemoteAttachCommandsUseIsolatedMuxAndExpandHome(t *testing.T) {
	l := newExecLauncher(Config{MuxName: "private wmux"})
	tmux := l.remoteAttachCommand(SessionSpec{Cwd: "~/projects/demo"}, PersistenceTmux, "wmux-demo", true)
	for _, wanted := range []string{
		"tmux -L 'private-wmux' -f /dev/null",
		"has-session -t '=wmux-demo'",
		"attach-session -t '=wmux-demo'",
		`-c "$HOME"/'projects/demo'`,
		"status off",
		"prefix None",
		"mouse on",
		"terminal-features ',xterm*:hyperlinks'",
	} {
		if !strings.Contains(tmux, wanted) {
			t.Fatalf("tmux command %q does not contain %q", tmux, wanted)
		}
	}
	if strings.Contains(tmux, "kill-server") {
		t.Fatalf("tmux command targets the whole server: %q", tmux)
	}
	if strings.Contains(tmux, "allow-passthrough") {
		t.Fatalf("tmux command enables unsafe passthrough: %q", tmux)
	}

	// Attach-only launches create and re-run nothing.
	attachOnly := l.remoteAttachCommand(SessionSpec{Shell: "/bin/sh", Args: []string{"-lc", "make"}}, PersistenceTmux, "wmux-demo", false)
	if strings.Contains(attachOnly, "new-session") || strings.Contains(attachOnly, "make") {
		t.Fatalf("attach-only tmux command can still create a session: %q", attachOnly)
	}
	for _, wanted := range []string{"wmux: session wmux-demo no longer exists on this host", ">&2; exit 3", "attach-session -t '=wmux-demo'"} {
		if !strings.Contains(attachOnly, wanted) {
			t.Fatalf("attach-only tmux command %q does not contain %q", attachOnly, wanted)
		}
	}
	attachOnlyScreen := l.remoteAttachCommand(SessionSpec{Shell: "/bin/sh", Args: []string{"-lc", "make"}}, PersistenceScreen, "wmux-demo", false)
	if strings.Contains(attachOnlyScreen, "-dmS") {
		t.Fatalf("attach-only screen command can still create a session: %q", attachOnlyScreen)
	}

	screen := l.remoteAttachCommand(SessionSpec{Cwd: "~"}, PersistenceScreen, "wmux-demo", true)
	for _, wanted := range []string{
		`wmux/s-private-wmux"`,
		`wmux_screen_rc="$wmux_screen_root/screenrc"`,
		`export SCREENDIR="$wmux_screen_sockets"`,
		`screen -c "$wmux_screen_rc"`,
		`cd -- "$HOME"`,
		"hardstatus off",
		"escape ^^^",
	} {
		if !strings.Contains(screen, wanted) {
			t.Fatalf("screen command %q does not contain %q", screen, wanted)
		}
	}
}

func TestRemoteTerminateCommandsTargetOnlyOneIsolatedSession(t *testing.T) {
	l := newExecLauncher(Config{MuxName: "private wmux"})
	name := "wmux-session"
	tmux := l.remoteTerminateCommand(PersistenceTmux, name)
	if tmux != "tmux -L 'private-wmux' -f /dev/null kill-session -t '=wmux-session'" {
		t.Fatalf("tmux termination command = %q", tmux)
	}
	if strings.Contains(tmux, "kill-server") {
		t.Fatalf("tmux termination targets the server: %q", tmux)
	}
	screen := l.remoteTerminateCommand(PersistenceScreen, name)
	if !strings.Contains(screen, `screen -c "$wmux_screen_rc" -S 'wmux-session' -X quit`) {
		t.Fatalf("screen termination does not target the exact session: %q", screen)
	}
}

func TestDialSSHHandshakeHonorsContextCancellation(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr == nil {
			accepted <- conn
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	started := time.Now()
	client, closers, err := dialSSH(ctx, HostSpec{
		Address:        listener.Addr().String(),
		User:           "test",
		Fingerprint:    "SHA256:not-used-before-server-key",
		Credential:     PasswordCredential{Password: "secret"},
		ConnectTimeout: 5 * time.Second,
	})
	if client != nil {
		_ = client.Close()
	}
	closeAll(closers)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("dialSSH error = %v, want context deadline", err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("SSH handshake cancellation took %s", elapsed)
	}
	select {
	case conn := <-accepted:
		_ = conn.Close()
	case <-time.After(time.Second):
		t.Fatal("test SSH listener never accepted a connection")
	}
}
