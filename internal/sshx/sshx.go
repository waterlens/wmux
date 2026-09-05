// Package sshx implements the short-lived SSH operations used while managing
// hosts. Interactive terminal connections live in internal/terminal.
package sshx

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/waterlens/wmux/internal/terminal"
	"golang.org/x/crypto/ssh"
)

const defaultTimeout = 8 * time.Second

// Target contains only data needed for a short-lived SSH connection.
type Target struct {
	Address     string
	Username    string
	Fingerprint string
	Credential  terminal.Credential
}

// Probe returns the public key fingerprint without trusting or authenticating
// the server. The caller must show it to the user before persisting trust.
func Probe(ctx context.Context, address, username string) (fingerprint, algorithm string, err error) {
	ctx, cancel := context.WithTimeout(ctx, defaultTimeout)
	defer cancel()

	connection, err := (&net.Dialer{}).DialContext(ctx, "tcp", address)
	if err != nil {
		return "", "", fmt.Errorf("sshx: dial ssh host: %w", err)
	}
	defer connection.Close()
	applyDeadline(ctx, connection)

	config := &ssh.ClientConfig{
		User:    username,
		Timeout: defaultTimeout,
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			fingerprint = ssh.FingerprintSHA256(key)
			algorithm = key.Type()
			// Returning a non-nil error aborts the handshake before any
			// authentication is attempted; the captured values are read below.
			return errors.New("sshx: host key captured")
		},
	}
	_, _, _, handshakeErr := ssh.NewClientConn(connection, address, config)
	if fingerprint != "" {
		return fingerprint, algorithm, nil
	}
	if handshakeErr != nil {
		return "", "", fmt.Errorf("sshx: read ssh host key: %w", handshakeErr)
	}
	return "", "", errors.New("sshx: ssh host offered no host key")
}

// Test authenticates with strict host-key verification and executes `true`.
func Test(ctx context.Context, target Target) error {
	ctx, cancel := context.WithTimeout(ctx, defaultTimeout)
	defer cancel()
	if target.Fingerprint == "" {
		return errors.New("sshx: ssh host key is not trusted yet")
	}

	// Interactive terminal sessions authenticate and pin host keys the same way,
	// so both paths share one implementation in internal/terminal.
	auth, closers, err := terminal.SSHAuthMethods(ctx, target.Credential)
	if err != nil {
		return fmt.Errorf("sshx: prepare ssh authentication: %w", err)
	}
	defer func() {
		for _, closer := range closers {
			_ = closer.Close()
		}
	}()
	hostKeyCallback, err := terminal.StrictHostKeyCallback(target.Fingerprint)
	if err != nil {
		return fmt.Errorf("sshx: pin ssh host key: %w", err)
	}

	config := &ssh.ClientConfig{
		User:            target.Username,
		Auth:            auth,
		Timeout:         defaultTimeout,
		HostKeyCallback: hostKeyCallback,
	}

	netConn, err := (&net.Dialer{}).DialContext(ctx, "tcp", target.Address)
	if err != nil {
		return fmt.Errorf("sshx: dial ssh host: %w", err)
	}
	applyDeadline(ctx, netConn)
	clientConn, channels, requests, err := ssh.NewClientConn(netConn, target.Address, config)
	if err != nil {
		_ = netConn.Close()
		return fmt.Errorf("sshx: ssh authentication failed: %w", err)
	}
	client := ssh.NewClient(clientConn, channels, requests)
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("sshx: open ssh test session: %w", err)
	}
	defer session.Close()
	if err := session.Run("true"); err != nil {
		return fmt.Errorf("sshx: ssh test command failed: %w", err)
	}
	return nil
}

func applyDeadline(ctx context.Context, connection net.Conn) {
	if deadline, ok := ctx.Deadline(); ok {
		_ = connection.SetDeadline(deadline)
	}
}
