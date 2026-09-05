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

func TestRuntimeStateCallbacksPreserveProductMetadata(t *testing.T) {
	ctx := context.Background()
	database := openRepositoryTestStore(t)
	repository := &RuntimeRepository{Store: database}
	created, err := database.CreateSession(ctx, store.Session{
		ID: "session-one", Name: "Original", Kind: store.SessionKindLocal,
		Persistence: store.SessionPersistenceAuto, Command: "make watch",
		Cwd: "/work", Cols: 180, Rows: 50,
	})
	if err != nil {
		t.Fatal(err)
	}
	session, err := database.UpdateSessionName(ctx, created.ID, "Renamed by user")
	if err != nil {
		t.Fatal(err)
	}
	metadataTime := session.UpdatedAt

	status := terminal.SessionStatus{
		ID: created.ID, Generation: created.Generation,
		State: terminal.StateRunning, Persistence: terminal.PersistenceTmux,
	}
	repository.OnSessionState(status)
	repository.OnSessionState(status)

	session, err = database.GetSession(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if session.Name != "Renamed by user" || session.Command != "make watch" {
		t.Fatalf("runtime callback overwrote product metadata: %+v", session)
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
	record := records[0]
	if !record.Active || record.Spec.ID != created.ID || record.ResolvedPersistence != terminal.PersistenceTmux {
		t.Fatalf("restored record = %+v", record)
	}
	if record.Spec.Generation != created.Generation {
		t.Fatalf("restored generation = %d, want %d", record.Spec.Generation, created.Generation)
	}
	if record.Spec.Env["WMUX_SESSION_ID"] != created.ID || record.Spec.Env["COLORTERM"] != "truecolor" {
		t.Fatalf("restored record environment = %+v", record.Spec.Env)
	}
	if record.Spec.Shell != "/bin/sh" || len(record.Spec.Args) != 2 || record.Spec.Args[1] != "make watch" {
		t.Fatalf("restored record command = %q %+v", record.Spec.Shell, record.Spec.Args)
	}

	repository.OnSessionState(terminal.SessionStatus{
		ID: created.ID, Generation: created.Generation,
		State: terminal.StateExited, Persistence: terminal.PersistenceTmux,
	})
	records, err = repository.ListSessions(ctx)
	if err != nil || len(records) != 1 || records[0].Active {
		t.Fatalf("exited restored record = %+v, %v", records, err)
	}
}

func TestStaleGenerationCallbackIsIgnored(t *testing.T) {
	ctx := context.Background()
	database := openRepositoryTestStore(t)
	repository := &RuntimeRepository{Store: database}
	created, err := database.CreateSession(ctx, store.Session{
		ID: "restarted", Name: "Restarted", Kind: store.SessionKindLocal,
		Persistence: store.SessionPersistenceTmux, Cols: 120, Rows: 36,
	})
	if err != nil {
		t.Fatal(err)
	}
	generation, err := database.BeginSessionRestart(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := repository.SessionSpec(ctx, mustGetSession(t, database, created.ID))
	if err != nil {
		t.Fatal(err)
	}
	if spec.Generation != generation {
		t.Fatalf("spec generation = %d, want %d", spec.Generation, generation)
	}

	// The previous execution reports its exit after the restart.
	repository.OnSessionState(terminal.SessionStatus{
		ID: created.ID, Generation: created.Generation,
		State: terminal.StateExited, Persistence: terminal.PersistenceTmux,
		LastError: "terminal: backend session no longer exists",
	})
	session := mustGetSession(t, database, created.ID)
	if session.Status != store.SessionStatusConnecting || session.Error != nil {
		t.Fatalf("stale generation callback was applied: %+v", session)
	}

	repository.OnSessionState(terminal.SessionStatus{
		ID: created.ID, Generation: spec.Generation,
		State: terminal.StateRunning, Persistence: terminal.PersistenceTmux,
	})
	session = mustGetSession(t, database, created.ID)
	if session.Status != store.SessionStatusRunning {
		t.Fatalf("current generation callback was not applied: %+v", session)
	}
}

func mustGetSession(t *testing.T, database *store.Store, id string) store.Session {
	t.Helper()
	session, err := database.GetSession(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return session
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
			if spec.Shell != "/bin/sh" || len(spec.Args) != 2 || spec.Args[1] != "uname -a" {
				t.Fatalf("session spec = %+v", spec)
			}
			if spec.Env["WMUX_SESSION_ID"] != spec.ID || spec.Env["COLORTERM"] != "truecolor" {
				t.Fatalf("session spec environment = %+v", spec.Env)
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
