package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

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
		Cols: 120, Rows: 36,
	})
	if err != nil {
		t.Fatal(err)
	}
	if session.HostName == nil || *session.HostName != "Dev box" || session.Cols != 120 || session.Rows != 36 || session.Status != SessionStatusConnecting {
		t.Fatalf("created session = %+v", session)
	}

	now = now.Add(time.Minute)
	if err := s.UpdateSessionRuntime(ctx, session.ID, session.Generation, SessionStatusConnecting, "tmux", "wmux-123", nil); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateSessionSize(ctx, session.ID, 180, 50); err != nil {
		t.Fatal(err)
	}
	if err := s.TouchSession(ctx, session.ID, now); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateSessionRuntime(ctx, session.ID, session.Generation, SessionStatusRunning, "", "", nil); err != nil {
		t.Fatal(err)
	}
	session, err = s.GetSession(ctx, session.ID)
	if err != nil || session.BackendName != "wmux-123" || session.Cols != 180 || session.Rows != 50 || session.LastAttachedAt == nil || !session.LastAttachedAt.Equal(now) || session.Status != SessionStatusRunning {
		t.Fatalf("atomically updated session = %+v, %v", session, err)
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
		Cols: 120, Rows: 36,
	})
	if err != nil {
		t.Fatal(err)
	}
	message := "process failed"
	if err := s.UpdateSessionRuntime(ctx, local.ID, local.Generation, SessionStatusExited, "", "", &message); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetSession(ctx, local.ID)
	if err != nil || got.Status != SessionStatusExited || got.Error == nil || *got.Error != message {
		t.Fatalf("exited session = %+v, %v", got, err)
	}
	if err := s.UpdateSessionRuntime(ctx, local.ID, local.Generation, "not-a-status", "", "", nil); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("unsupported runtime status error = %v", err)
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
		Cols: 120, Rows: 36,
	})
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	second, err := s.CreateSession(ctx, Session{
		Name: "Second", Kind: SessionKindLocal, Persistence: SessionPersistenceAuto,
		Cols: 120, Rows: 36,
	})
	if err != nil {
		t.Fatal(err)
	}
	originalUpdatedAt := first.UpdatedAt

	now = now.Add(time.Hour)
	if err := s.UpdateSessionRuntime(ctx, first.ID, first.Generation, SessionStatusRunning, "tmux", "wmux-first", nil); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateSessionRuntime(ctx, first.ID, first.Generation, SessionStatusRunning, "tmux", "wmux-first", nil); err != nil {
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

func TestSessionGenerationIsolatesRestartsFromLateRuntimeCallbacks(t *testing.T) {
	now := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
	s := openTestStore(t, &now)
	ctx := context.Background()
	session, err := s.CreateSession(ctx, Session{
		ID: "restarted", Name: "Restarted", Kind: SessionKindLocal,
		Persistence: SessionPersistenceAuto, Cols: 120, Rows: 36,
	})
	if err != nil {
		t.Fatal(err)
	}
	if session.Generation != 1 {
		t.Fatalf("created generation = %d, want 1", session.Generation)
	}
	message := "backend session no longer exists"
	if err := s.UpdateSessionRuntime(ctx, session.ID, session.Generation, SessionStatusExited, "", "", &message); err != nil {
		t.Fatal(err)
	}

	generation, err := s.BeginSessionRestart(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if generation != 2 {
		t.Fatalf("restarted generation = %d, want 2", generation)
	}
	restarted, err := s.GetSession(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if restarted.Status != SessionStatusConnecting || restarted.Error != nil {
		t.Fatalf("restart did not clear the previous execution: %+v", restarted)
	}

	// The stopped execution reports its exit after the restart began.
	staleError := "connection lost"
	if err := s.UpdateSessionRuntime(ctx, session.ID, 1, SessionStatusExited, "tmux", "wmux-restarted", &staleError); err != nil {
		t.Fatalf("stale runtime callback = %v, want silently ignored", err)
	}
	got, err := s.GetSession(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != SessionStatusConnecting || got.Error != nil || got.Backend != "" {
		t.Fatalf("stale generation overwrote the current execution: %+v", got)
	}

	if err := s.UpdateSessionRuntime(ctx, session.ID, generation, SessionStatusRunning, "tmux", "wmux-restarted", nil); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetSession(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != SessionStatusRunning || got.Backend != "tmux" || got.Generation != 2 {
		t.Fatalf("current generation callback was not applied: %+v", got)
	}
	if _, err := s.BeginSessionRestart(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("BeginSessionRestart on a missing session = %v, want ErrNotFound", err)
	}
	if err := s.UpdateSessionRuntime(ctx, "missing", 1, SessionStatusRunning, "", "", nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("runtime callback for a missing session = %v, want ErrNotFound", err)
	}
}
