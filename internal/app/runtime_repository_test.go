package app

import (
	"bytes"
	"context"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/waterlens/wmux/internal/security"
	"github.com/waterlens/wmux/internal/store"
	"github.com/waterlens/wmux/internal/terminal"
)

func openRepositoryTestStore(t *testing.T) *store.Store {
	t.Helper()
	database, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "wmux.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database
}

func TestRuntimeRepositorySaveSessionDoesNotOverwriteProductState(t *testing.T) {
	ctx := context.Background()
	database := openRepositoryTestStore(t)
	repository := &RuntimeRepository{Store: database}
	record := terminal.SessionRecord{
		ID: "session-one", Name: "Original", Persistence: terminal.PersistenceAuto,
		Shell: "/bin/sh", Args: []string{"-lc", "make watch"}, Cwd: "/work",
		Cols: 120, Rows: 36, Active: true,
	}
	if err := repository.SaveSession(ctx, record); err != nil {
		t.Fatal(err)
	}
	session, err := database.UpdateSessionName(ctx, record.ID, "Renamed by user")
	if err != nil {
		t.Fatal(err)
	}
	metadataTime := session.UpdatedAt

	// This record intentionally contains the stale name. Runtime persistence
	// must update backend/size without replacing it.
	record.ResolvedPersistence = terminal.PersistenceTmux
	record.Cols = 180
	record.Rows = 50
	if err := repository.SaveSession(ctx, record); err != nil {
		t.Fatal(err)
	}
	repository.OnSessionState(terminal.SessionStatus{
		ID: record.ID, State: terminal.StateRunning, Persistence: terminal.PersistenceTmux,
	})
	repository.OnSessionState(terminal.SessionStatus{
		ID: record.ID, State: terminal.StateRunning, Persistence: terminal.PersistenceTmux,
	})

	session, err = database.GetSession(ctx, record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if session.Name != "Renamed by user" || session.Command != "make watch" {
		t.Fatalf("runtime save overwrote product metadata: %+v", session)
	}
	if session.Backend != "tmux" || session.Status != store.SessionStatusRunning || session.Cols != 180 || session.Rows != 50 {
		t.Fatalf("runtime state was not persisted: %+v", session)
	}
	if !session.UpdatedAt.Equal(metadataTime) {
		t.Fatalf("runtime callback changed UpdatedAt from %v to %v", metadataTime, session.UpdatedAt)
	}

	records, err := repository.ListSessions(ctx)
	if err != nil || len(records) != 1 {
		t.Fatalf("ListSessions = %+v, %v", records, err)
	}
	if !records[0].Active || records[0].Name != "Renamed by user" || records[0].ResolvedPersistence != terminal.PersistenceTmux {
		t.Fatalf("restored record = %+v", records[0])
	}
	record.Active = false
	if err := repository.SaveSession(ctx, record); err != nil {
		t.Fatal(err)
	}
	records, err = repository.ListSessions(ctx)
	if err != nil || len(records) != 1 || records[0].Active {
		t.Fatalf("exited restored record = %+v, %v", records, err)
	}
}

func TestRuntimeRepositorySessionSpecDecryptsEveryCredentialType(t *testing.T) {
	ctx := context.Background()
	database := openRepositoryTestStore(t)
	key := bytes.Repeat([]byte{0x42}, security.MasterKeySize)
	repository := &RuntimeRepository{Store: database, MasterKey: key}

	tests := []struct {
		name        string
		authType    string
		credentials store.Credentials
		assert      func(*testing.T, terminal.Credential)
	}{
		{
			name: "password", authType: store.HostAuthPassword,
			credentials: store.Credentials{Password: "ssh-secret"},
			assert: func(t *testing.T, credential terminal.Credential) {
				got, ok := credential.(terminal.PasswordCredential)
				if !ok || got.Password != "ssh-secret" {
					t.Fatalf("password credential = %#v", credential)
				}
			},
		},
		{
			name: "private key", authType: store.HostAuthKey,
			credentials: store.Credentials{PrivateKey: "PEM DATA", Passphrase: "key-secret"},
			assert: func(t *testing.T, credential terminal.Credential) {
				got, ok := credential.(terminal.PrivateKeyCredential)
				if !ok || string(got.PEM) != "PEM DATA" || string(got.Passphrase) != "key-secret" {
					t.Fatalf("private-key credential = %#v", credential)
				}
			},
		},
		{
			name: "agent", authType: store.HostAuthAgent,
			assert: func(t *testing.T, credential terminal.Credential) {
				if _, ok := credential.(terminal.AgentCredential); !ok {
					t.Fatalf("agent credential = %#v", credential)
				}
			},
		},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var encrypted []byte
			var err error
			if test.authType != store.HostAuthAgent {
				encrypted, err = security.EncryptJSON(key, test.credentials)
				if err != nil {
					t.Fatal(err)
				}
			}
			host, err := database.CreateHost(ctx, store.Host{
				Name: "Host " + test.name, Address: "::1", Port: 2200 + index,
				Username: "developer", AuthType: test.authType,
				EncryptedCredentials: encrypted, Fingerprint: "SHA256:test",
			})
			if err != nil {
				t.Fatal(err)
			}
			spec, err := repository.SessionSpec(ctx, store.Session{
				ID: "spec-" + test.name, Name: "Shell", Kind: store.SessionKindSSH,
				HostID: &host.ID, Persistence: store.SessionPersistenceTmux,
				Command: "uname -a", Cols: 100, Rows: 30,
			})
			if err != nil {
				t.Fatal(err)
			}
			if spec.Host == nil || spec.Host.Address != "[::1]:"+strconv.Itoa(2200+index) || spec.Host.Fingerprint != "SHA256:test" {
				t.Fatalf("host spec = %+v", spec.Host)
			}
			if spec.Shell != "/bin/sh" || len(spec.Args) != 2 || spec.Args[1] != "uname -a" || spec.Env["WMUX_SESSION_ID"] != spec.ID {
				t.Fatalf("session spec = %+v", spec)
			}
			test.assert(t, spec.Host.Credential)
		})
	}
}

func TestRuntimeRepositorySessionSpecRejectsWrongMasterKey(t *testing.T) {
	ctx := context.Background()
	database := openRepositoryTestStore(t)
	goodKey := bytes.Repeat([]byte{1}, security.MasterKeySize)
	encrypted, err := security.EncryptJSON(goodKey, store.Credentials{Password: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	host, err := database.CreateHost(ctx, store.Host{
		Name: "Host", Address: "localhost", Port: 22, Username: "me",
		AuthType: store.HostAuthPassword, EncryptedCredentials: encrypted,
	})
	if err != nil {
		t.Fatal(err)
	}
	repository := &RuntimeRepository{Store: database, MasterKey: bytes.Repeat([]byte{2}, security.MasterKeySize)}
	if _, err := repository.SessionSpec(ctx, store.Session{
		ID: "bad-key", Name: "Bad key", Kind: store.SessionKindSSH, HostID: &host.ID,
		Persistence: store.SessionPersistenceAuto, Cols: 80, Rows: 24,
	}); err == nil {
		t.Fatal("expected credential decryption error")
	}
}
