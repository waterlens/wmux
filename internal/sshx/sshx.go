// Package sshx implements the short-lived SSH operations used while managing
// hosts. Interactive terminal connections live in internal/terminal.
package sshx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"time"

	"github.com/waterlens/wmux/internal/store"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

const defaultTimeout = 8 * time.Second

// Credentials is the decrypted authentication material for one host.
type Credentials struct {
	AuthType   string
	Password   string
	PrivateKey string
	Passphrase string
}

// Target contains only data needed for a short-lived SSH connection.
type Target struct {
	Address     string
	Username    string
	Fingerprint string
	Credentials Credentials
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

	auth, cleanup, err := authMethod(target.Credentials)
	if err != nil {
		return err
	}
	if cleanup != nil {
		defer cleanup.Close()
	}

	config := &ssh.ClientConfig{
		User:    target.Username,
		Auth:    []ssh.AuthMethod{auth},
		Timeout: defaultTimeout,
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			// Fingerprints are public values shown to the user, so a plain
			// comparison is enough.
			actual := ssh.FingerprintSHA256(key)
			if actual != target.Fingerprint {
				return fmt.Errorf("sshx: ssh host key changed: want %s, got %s", target.Fingerprint, actual)
			}
			return nil
		},
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

// authMethod builds the SSH authentication method for one host. The returned
// io.Closer is nil unless the method owns a resource, which only the agent
// branch does.
func authMethod(credentials Credentials) (ssh.AuthMethod, io.Closer, error) {
	switch credentials.AuthType {
	case store.HostAuthPassword:
		if credentials.Password == "" {
			return nil, nil, errors.New("sshx: ssh password is empty")
		}
		return ssh.Password(credentials.Password), nil, nil
	case store.HostAuthKey:
		var signer ssh.Signer
		var err error
		if credentials.Passphrase != "" {
			signer, err = ssh.ParsePrivateKeyWithPassphrase([]byte(credentials.PrivateKey), []byte(credentials.Passphrase))
		} else {
			signer, err = ssh.ParsePrivateKey([]byte(credentials.PrivateKey))
		}
		if err != nil {
			return nil, nil, fmt.Errorf("sshx: parse ssh private key: %w", err)
		}
		return ssh.PublicKeys(signer), nil, nil
	case store.HostAuthAgent:
		socket := os.Getenv("SSH_AUTH_SOCK")
		if socket == "" {
			return nil, nil, errors.New("sshx: SSH_AUTH_SOCK is not set")
		}
		connection, err := net.DialTimeout("unix", socket, defaultTimeout)
		if err != nil {
			return nil, nil, fmt.Errorf("sshx: dial ssh agent: %w", err)
		}
		return ssh.PublicKeysCallback(agent.NewClient(connection).Signers), connection, nil
	default:
		return nil, nil, fmt.Errorf("sshx: unsupported ssh authentication type %q", credentials.AuthType)
	}
}

func applyDeadline(ctx context.Context, connection net.Conn) {
	if deadline, ok := ctx.Deadline(); ok {
		_ = connection.SetDeadline(deadline)
	}
}
