package sshx

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/waterlens/wmux/internal/terminal"
	"golang.org/x/crypto/ssh"
)

func TestProbeAndStrictPasswordTest(t *testing.T) {
	t.Parallel()
	address, fingerprint := startTestSSHServer(t, "secret")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	gotFingerprint, algorithm, err := Probe(ctx, address, "tester")
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if gotFingerprint != fingerprint || algorithm != ssh.KeyAlgoED25519 {
		t.Fatalf("Probe = (%q, %q), want (%q, %q)", gotFingerprint, algorithm, fingerprint, ssh.KeyAlgoED25519)
	}

	err = Test(ctx, Target{
		Address:     address,
		Username:    "tester",
		Fingerprint: fingerprint,
		Credential:  terminal.PasswordCredential{Password: "secret"},
	})
	if err != nil {
		t.Fatalf("Test valid target: %v", err)
	}

	err = Test(ctx, Target{
		Address:     address,
		Username:    "tester",
		Fingerprint: "SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		Credential:  terminal.PasswordCredential{Password: "secret"},
	})
	if err == nil {
		t.Fatal("Test accepted a changed host key")
	}
}

func TestProbeHonorsContextDeadline(t *testing.T) {
	t.Parallel()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr == nil {
			defer connection.Close()
			_, _ = io.Copy(io.Discard, connection)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, _, err = Probe(ctx, listener.Addr().String(), "tester")
	if err == nil {
		t.Fatal("Probe unexpectedly succeeded against a stalled server")
	}
	if time.Since(started) > time.Second {
		t.Fatalf("Probe ignored context deadline: %v", time.Since(started))
	}
}

func startTestSSHServer(t *testing.T, password string) (string, string) {
	t.Helper()
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	configuration := &ssh.ServerConfig{
		PasswordCallback: func(metadata ssh.ConnMetadata, provided []byte) (*ssh.Permissions, error) {
			if metadata.User() == "tester" && string(provided) == password {
				return nil, nil
			}
			return nil, errors.New("authentication rejected")
		},
	}
	configuration.AddHostKey(signer)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			connection, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			go serveTestSSHConnection(connection, configuration)
		}
	}()
	return listener.Addr().String(), ssh.FingerprintSHA256(signer.PublicKey())
}

func serveTestSSHConnection(connection net.Conn, configuration *ssh.ServerConfig) {
	defer connection.Close()
	_, channels, requests, err := ssh.NewServerConn(connection, configuration)
	if err != nil {
		return
	}
	go ssh.DiscardRequests(requests)
	for channelRequest := range channels {
		if channelRequest.ChannelType() != "session" {
			_ = channelRequest.Reject(ssh.UnknownChannelType, "unsupported channel")
			continue
		}
		channel, channelRequests, err := channelRequest.Accept()
		if err != nil {
			continue
		}
		go func() {
			defer channel.Close()
			for request := range channelRequests {
				if request.Type != "exec" {
					_ = request.Reply(false, nil)
					continue
				}
				command := ""
				if len(request.Payload) >= 4 {
					length := int(binary.BigEndian.Uint32(request.Payload[:4]))
					if length <= len(request.Payload)-4 {
						command = string(request.Payload[4 : 4+length])
					}
				}
				if command != "true" {
					_ = request.Reply(false, nil)
					return
				}
				_ = request.Reply(true, nil)
				status := make([]byte, 4)
				binary.BigEndian.PutUint32(status, 0)
				_, _ = channel.SendRequest("exit-status", false, status)
				return
			}
		}()
	}
}
