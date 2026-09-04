// Package sshx implements the short-lived SSH operations used while managing
// hosts. Interactive terminal connections live in internal/terminal.
package sshx

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

const defaultTimeout = 8 * time.Second

// Credentials is the decrypted authentication material for one host.
type Credentials struct {
	AuthType    string
	Password    string
	PrivateKey  string
	Passphrase  string
	AgentSocket string
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
		return "", "", fmt.Errorf("连接 SSH 主机: %w", err)
	}
	defer connection.Close()
	applyDeadline(ctx, connection)

	captured := errors.New("ssh host key captured")
	config := &ssh.ClientConfig{
		User:    username,
		Timeout: defaultTimeout,
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			fingerprint = ssh.FingerprintSHA256(key)
			algorithm = key.Type()
			return captured
		},
	}
	_, _, _, handshakeErr := ssh.NewClientConn(connection, address, config)
	if fingerprint != "" {
		return fingerprint, algorithm, nil
	}
	if handshakeErr != nil {
		return "", "", fmt.Errorf("读取 SSH host key: %w", handshakeErr)
	}
	return "", "", errors.New("SSH 主机未提供 host key")
}

// Test authenticates with strict host-key verification and executes `true`.
func Test(ctx context.Context, target Target) error {
	ctx, cancel := context.WithTimeout(ctx, defaultTimeout)
	defer cancel()
	if target.Fingerprint == "" {
		return errors.New("请先验证并信任 SSH host key")
	}

	auth, cleanup, err := authMethod(target.Credentials)
	if err != nil {
		return err
	}
	defer cleanup()

	config := &ssh.ClientConfig{
		User:    target.Username,
		Auth:    []ssh.AuthMethod{auth},
		Timeout: defaultTimeout,
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			actual := ssh.FingerprintSHA256(key)
			if subtle.ConstantTimeCompare([]byte(actual), []byte(target.Fingerprint)) != 1 {
				return fmt.Errorf("SSH host key 已变化：期望 %s，实际 %s", target.Fingerprint, actual)
			}
			return nil
		},
	}

	netConn, err := (&net.Dialer{}).DialContext(ctx, "tcp", target.Address)
	if err != nil {
		return fmt.Errorf("连接 SSH 主机: %w", err)
	}
	applyDeadline(ctx, netConn)
	clientConn, channels, requests, err := ssh.NewClientConn(netConn, target.Address, config)
	if err != nil {
		_ = netConn.Close()
		return fmt.Errorf("SSH 验证失败: %w", err)
	}
	client := ssh.NewClient(clientConn, channels, requests)
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("创建 SSH 测试会话: %w", err)
	}
	defer session.Close()
	if err := session.Run("true"); err != nil {
		return fmt.Errorf("SSH 测试命令失败: %w", err)
	}
	return nil
}

func authMethod(credentials Credentials) (ssh.AuthMethod, func(), error) {
	switch credentials.AuthType {
	case "password":
		if credentials.Password == "" {
			return nil, func() {}, errors.New("SSH 密码为空")
		}
		return ssh.Password(credentials.Password), func() {}, nil
	case "privateKey":
		var signer ssh.Signer
		var err error
		if credentials.Passphrase != "" {
			signer, err = ssh.ParsePrivateKeyWithPassphrase([]byte(credentials.PrivateKey), []byte(credentials.Passphrase))
		} else {
			signer, err = ssh.ParsePrivateKey([]byte(credentials.PrivateKey))
		}
		if err != nil {
			return nil, func() {}, fmt.Errorf("解析 SSH 私钥: %w", err)
		}
		return ssh.PublicKeys(signer), func() {}, nil
	case "agent":
		socket := strings.TrimSpace(credentials.AgentSocket)
		if socket == "" {
			socket = os.Getenv("SSH_AUTH_SOCK")
		}
		if socket == "" {
			return nil, func() {}, errors.New("SSH_AUTH_SOCK 未设置")
		}
		connection, err := net.DialTimeout("unix", socket, defaultTimeout)
		if err != nil {
			return nil, func() {}, fmt.Errorf("连接 SSH agent: %w", err)
		}
		return ssh.PublicKeysCallback(agent.NewClient(connection).Signers), func() { _ = connection.Close() }, nil
	default:
		return nil, func() {}, errors.New("不支持的 SSH 认证方式")
	}
}

func applyDeadline(ctx context.Context, connection net.Conn) {
	if deadline, ok := ctx.Deadline(); ok {
		_ = connection.SetDeadline(deadline)
	}
}
