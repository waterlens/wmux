package store

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

func openTestStore(t *testing.T, now *time.Time) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "data", "wmux.db")
	s, err := OpenWithOptions(context.Background(), path, Options{Now: func() time.Time { return *now }})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return s
}

func TestOpenMigratesAndConfiguresSQLite(t *testing.T) {
	now := time.Date(2026, 9, 4, 10, 11, 12, 0, time.UTC)
	root := t.TempDir()
	path := filepath.Join(root, "private", "wmux.db")
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	var version, foreignKeys, busyTimeout int
	var journalMode string
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow("PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow("PRAGMA busy_timeout").Scan(&busyTimeout); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow("PRAGMA journal_mode").Scan(&journalMode); err != nil {
		t.Fatal(err)
	}
	if version != currentSchemaVersion || foreignKeys != 1 || busyTimeout != 5000 || journalMode != "wal" {
		t.Fatalf("pragmas: version=%d foreign_keys=%d busy_timeout=%d journal=%q", version, foreignKeys, busyTimeout, journalMode)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("database mode = %o", info.Mode().Perm())
	}
	_ = now
}

func TestOpenIsSafeConcurrently(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wmux.db")
	const workers = 4
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	stores := make(chan *Store, workers)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := Open(context.Background(), path)
			stores <- s
			errs <- err
		}()
	}
	wg.Wait()
	close(stores)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent Open: %v", err)
		}
	}
	for s := range stores {
		if s != nil {
			_ = s.Close()
		}
	}
}

func TestSingleUserSetupAndPassword(t *testing.T) {
	now := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
	s := openTestStore(t, &now)
	ctx := context.Background()
	setup, err := s.IsSetup(ctx)
	if err != nil || setup {
		t.Fatalf("initial IsSetup = %v, %v", setup, err)
	}
	if _, err := s.GetUser(ctx); !errors.Is(err, ErrNotFound) {
		t.Fatalf("initial GetUser error = %v", err)
	}
	if err := s.Setup(ctx, "owner", "hash-one"); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if err := s.Setup(ctx, "other", "hash-two"); !errors.Is(err, ErrAlreadySetup) {
		t.Fatalf("second Setup error = %v", err)
	}
	user, err := s.GetUserByUsername(ctx, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if user.Username != "owner" || user.PasswordHash != "hash-one" || !user.CreatedAt.Equal(now) {
		t.Fatalf("user = %+v", user)
	}
	if _, err := s.GetUserByUsername(ctx, "OWNER"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong username error = %v", err)
	}
	now = now.Add(time.Hour)
	if err := s.UpdatePassword(ctx, "hash-three"); err != nil {
		t.Fatal(err)
	}
	user, err = s.GetUser(ctx)
	if err != nil || user.PasswordHash != "hash-three" || !user.UpdatedAt.Equal(now) {
		t.Fatalf("updated user = %+v, %v", user, err)
	}
}

func TestSetupAllowsExactlyOneConcurrentCaller(t *testing.T) {
	now := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
	s := openTestStore(t, &now)
	const workers = 8
	var wg sync.WaitGroup
	results := make(chan error, workers)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- s.Setup(context.Background(), "owner", "hash")
		}()
	}
	wg.Wait()
	close(results)
	var successes, already int
	for err := range results {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrAlreadySetup):
			already++
		default:
			t.Fatalf("unexpected Setup error: %v", err)
		}
	}
	if successes != 1 || already != workers-1 {
		t.Fatalf("successes=%d already=%d", successes, already)
	}
}

func TestAuthSessionLifecycle(t *testing.T) {
	now := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
	s := openTestStore(t, &now)
	ctx := context.Background()
	hash := bytes.Repeat([]byte{3}, 32)
	auth, err := s.CreateAuthSession(ctx, hash, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(auth.ID) != 32 || !bytes.Equal(auth.TokenHash, hash) {
		t.Fatalf("auth session = %+v", auth)
	}
	got, err := s.GetAuthSession(ctx, hash)
	if err != nil || got.ID != auth.ID {
		t.Fatalf("GetAuthSession = %+v, %v", got, err)
	}
	now = now.Add(10 * time.Minute)
	if err := s.TouchAuthSession(ctx, hash); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetAuthSession(ctx, hash)
	if err != nil || !got.LastSeenAt.Equal(now) {
		t.Fatalf("touched session = %+v, %v", got, err)
	}
	now = now.Add(time.Hour)
	if _, err := s.GetAuthSession(ctx, hash); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired session error = %v", err)
	}
	count, err := s.PurgeExpiredAuthSessions(ctx)
	if err != nil || count != 1 {
		t.Fatalf("PurgeExpiredAuthSessions = %d, %v", count, err)
	}
	if _, err := s.CreateAuthSession(ctx, []byte("short"), now.Add(time.Hour)); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("short token hash error = %v", err)
	}
}

func TestHostCRUD(t *testing.T) {
	now := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
	s := openTestStore(t, &now)
	ctx := context.Background()
	host, err := s.CreateHost(ctx, Host{
		Name:                 "Production",
		Address:              "server.example.test",
		Port:                 22,
		Username:             "deploy",
		AuthType:             HostAuthKey,
		EncryptedCredentials: []byte{1, 2, 3},
		Fingerprint:          "SHA256:abc",
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.GetHost(ctx, host.ID)
	if err != nil || got.Name != "Production" || !bytes.Equal(got.EncryptedCredentials, []byte{1, 2, 3}) {
		t.Fatalf("GetHost = %+v, %v", got, err)
	}
	host.Name = "Prod renamed"
	host.Port = 2222
	host.EncryptedCredentials = []byte{4, 5}
	now = now.Add(time.Minute)
	host, err = s.UpdateHost(ctx, host)
	if err != nil || host.Name != "Prod renamed" || host.Port != 2222 || !host.UpdatedAt.Equal(now) {
		t.Fatalf("UpdateHost = %+v, %v", host, err)
	}
	hosts, err := s.ListHosts(ctx)
	if err != nil || len(hosts) != 1 || hosts[0].ID != host.ID {
		t.Fatalf("ListHosts = %+v, %v", hosts, err)
	}
	if err := s.DeleteHost(ctx, host.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetHost(ctx, host.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted GetHost error = %v", err)
	}
	if err := s.DeleteHost(ctx, host.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second DeleteHost error = %v", err)
	}
}

func TestSessionCRUDAndHostJoin(t *testing.T) {
	now := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
	s := openTestStore(t, &now)
	ctx := context.Background()
	host, err := s.CreateHost(ctx, Host{
		Name: "Dev box", Address: "10.0.0.2", Port: 22,
		Username: "me", AuthType: HostAuthAgent,
	})
	if err != nil {
		t.Fatal(err)
	}
	session, err := s.CreateSession(ctx, Session{
		Name: "Remote shell", Kind: SessionKindSSH, HostID: &host.ID,
		Cwd: "/srv/app", Persistence: SessionPersistenceTmux,
	})
	if err != nil {
		t.Fatal(err)
	}
	if session.HostName == nil || *session.HostName != "Dev box" || session.Cols != 120 || session.Rows != 36 || session.Status != SessionStatusConnecting {
		t.Fatalf("created session = %+v", session)
	}

	now = now.Add(time.Minute)
	if err := s.UpdateSessionBackend(ctx, session.ID, "tmux", "wmux-123"); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateSessionSize(ctx, session.ID, 180, 50); err != nil {
		t.Fatal(err)
	}
	if err := s.TouchSession(ctx, session.ID, now); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateSessionStatus(ctx, session.ID, SessionStatusRunning, nil, nil); err != nil {
		t.Fatal(err)
	}
	session, err = s.GetSession(ctx, session.ID)
	if err != nil || session.BackendName != "wmux-123" || session.Cols != 180 || session.Rows != 50 || session.LastAttachedAt == nil || !session.LastAttachedAt.Equal(now) || session.Status != SessionStatusRunning {
		t.Fatalf("atomically updated session = %+v, %v", session, err)
	}

	session.Name = "Renamed"
	session.Command = "make watch"
	session.Persistence = SessionPersistenceScreen
	now = now.Add(time.Minute)
	session, err = s.UpdateSession(ctx, session)
	if err != nil || session.Name != "Renamed" || session.Status != SessionStatusRunning || !session.UpdatedAt.Equal(now) {
		t.Fatalf("UpdateSession = %+v, %v", session, err)
	}
	sessions, err := s.ListSessions(ctx)
	if err != nil || len(sessions) != 1 || sessions[0].HostName == nil {
		t.Fatalf("ListSessions = %+v, %v", sessions, err)
	}
	if err := s.DeleteHost(ctx, host.ID); !errors.Is(err, ErrInUse) {
		t.Fatalf("referenced host deletion error = %v, want ErrInUse", err)
	}
	if err := s.DeleteSession(ctx, session.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteHost(ctx, host.ID); err != nil {
		t.Fatal(err)
	}
}

func TestSessionValidationAndExit(t *testing.T) {
	now := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
	s := openTestStore(t, &now)
	ctx := context.Background()
	if _, err := s.CreateSession(ctx, Session{Name: "bad", Kind: SessionKindSSH}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("missing host error = %v", err)
	}
	local, err := s.CreateSession(ctx, Session{
		Name: "Local", Kind: SessionKindLocal, Persistence: SessionPersistenceNone,
	})
	if err != nil {
		t.Fatal(err)
	}
	exitCode := 7
	message := "process failed"
	if err := s.UpdateSessionStatus(ctx, local.ID, SessionStatusExited, &exitCode, &message); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetSession(ctx, local.ID)
	if err != nil || got.ExitCode == nil || *got.ExitCode != 7 || got.Error == nil || *got.Error != message {
		t.Fatalf("exited session = %+v, %v", got, err)
	}
	if err := s.DeleteSession(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete missing error = %v", err)
	}
}

func TestRuntimeUpdatesPreserveProductFieldsAndListOrder(t *testing.T) {
	now := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
	s := openTestStore(t, &now)
	ctx := context.Background()
	first, err := s.CreateSession(ctx, Session{
		Name: "First", Kind: SessionKindLocal, Persistence: SessionPersistenceAuto,
	})
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	second, err := s.CreateSession(ctx, Session{
		Name: "Second", Kind: SessionKindLocal, Persistence: SessionPersistenceAuto,
	})
	if err != nil {
		t.Fatal(err)
	}
	originalUpdatedAt := first.UpdatedAt

	now = now.Add(time.Hour)
	if err := s.UpdateSessionRuntime(ctx, first.ID, SessionStatusRunning, "tmux", "wmux-first", nil); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateSessionRuntime(ctx, first.ID, SessionStatusRunning, "tmux", "wmux-first", nil); err != nil {
		t.Fatalf("idempotent runtime update: %v", err)
	}
	if err := s.UpdateSessionSize(ctx, first.ID, 200, 60); err != nil {
		t.Fatal(err)
	}
	if err := s.TouchSession(ctx, first.ID, now); err != nil {
		t.Fatal(err)
	}
	first, err = s.GetSession(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !first.UpdatedAt.Equal(originalUpdatedAt) {
		t.Fatalf("runtime activity changed UpdatedAt from %v to %v", originalUpdatedAt, first.UpdatedAt)
	}
	sessions, err := s.ListSessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 2 || sessions[0].ID != second.ID || sessions[1].ID != first.ID {
		t.Fatalf("runtime activity reordered sessions: %+v", sessions)
	}

	now = now.Add(time.Minute)
	first, err = s.UpdateSessionName(ctx, first.ID, "First renamed")
	if err != nil {
		t.Fatal(err)
	}
	if first.Name != "First renamed" || !first.UpdatedAt.Equal(now) {
		t.Fatalf("renamed session = %+v", first)
	}
	sessions, err = s.ListSessions(ctx)
	if err != nil || sessions[0].ID != first.ID {
		t.Fatalf("metadata update did not reorder sessions: %+v, %v", sessions, err)
	}
}

func TestStaleProductAndRuntimeUpdatesDoNotOverwriteEachOther(t *testing.T) {
	now := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
	s := openTestStore(t, &now)
	ctx := context.Background()
	session, err := s.CreateSession(ctx, Session{
		ID: "shared", Name: "Original", Kind: SessionKindLocal,
		Persistence: SessionPersistenceAuto,
	})
	if err != nil {
		t.Fatal(err)
	}
	stale := session
	if err := s.UpdateSessionRuntime(ctx, session.ID, SessionStatusRunning, "tmux", "wmux-shared", nil); err != nil {
		t.Fatal(err)
	}
	stale.Name = "Product rename"
	stale.Cols = 144
	if _, err := s.UpdateSession(ctx, stale); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetSession(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "Product rename" || got.Status != SessionStatusRunning || got.Backend != "tmux" || got.BackendName != "wmux-shared" {
		t.Fatalf("stale product update overwrote runtime fields: %+v", got)
	}

	if err := s.SaveRuntimeSession(ctx, Session{
		ID: "shared", Name: "Stale runtime name", Kind: SessionKindLocal,
		Persistence: SessionPersistenceAuto, Backend: "screen", BackendName: "wmux-shared",
		Status: SessionStatusConnecting, Cols: 160, Rows: 48,
	}); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetSession(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "Product rename" || got.Backend != "screen" || got.Cols != 160 || got.Status != SessionStatusRunning {
		t.Fatalf("runtime save overwrote product fields or state: %+v", got)
	}
	if err := s.SaveRuntimeSession(ctx, Session{
		ID: "shared", Name: "ignored", Kind: SessionKindLocal,
		Persistence: SessionPersistenceAuto, Backend: "screen", BackendName: "wmux-shared",
		Status: SessionStatusExited, Cols: 160, Rows: 48,
	}); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetSession(ctx, session.ID)
	if err != nil || got.Status != SessionStatusExited {
		t.Fatalf("inactive runtime save = %+v, %v", got, err)
	}
}
