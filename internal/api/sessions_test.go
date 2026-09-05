package api

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/waterlens/wmux/internal/store"
)

func TestAvailableSessionNameUsesFirstFreeSuffix(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database, err := store.Open(ctx, filepath.Join(t.TempDir(), "wmux.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	for _, name := range []string{"本机终端", "本机终端 2", "本机终端 4"} {
		if _, err := database.CreateSession(ctx, store.Session{Name: name, Kind: store.SessionKindLocal, Cols: 120, Rows: 36}); err != nil {
			t.Fatal(err)
		}
	}
	server := &Server{store: database}
	name, err := server.availableSessionName(ctx, "本机终端")
	if err != nil {
		t.Fatal(err)
	}
	if name != "本机终端 3" {
		t.Fatalf("name = %q, want %q", name, "本机终端 3")
	}
}

func TestPublicSessionHidesRuntimeDiagnostics(t *testing.T) {
	t.Parallel()
	detail := "dial tcp 10.0.0.1:22: connection refused"
	result := publicSession(store.Session{
		BackendName: "wmux-ses_internal-deadbeef",
		Status:      store.SessionStatusReconnecting,
		Error:       &detail,
	})
	if result.BackendName != "" {
		t.Fatalf("backend name leaked: %q", result.BackendName)
	}
	if result.Error == nil || *result.Error == detail {
		t.Fatalf("runtime detail leaked: %#v", result.Error)
	}
}
