package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/waterlens/wmux/internal/app"
	"github.com/waterlens/wmux/internal/config"
	"github.com/waterlens/wmux/internal/security"
	"github.com/waterlens/wmux/internal/store"
	"github.com/waterlens/wmux/internal/terminal"
	"github.com/waterlens/wmux/internal/transcript"
)

// apiFixture is the whole server stack on a temporary data directory: the
// handlers, the store, the terminal runtime and an httptest server for the
// tests that speak real HTTP or WebSocket.
type apiFixture struct {
	api        *Server
	server     *httptest.Server
	database   *store.Store
	manager    *terminal.Manager
	recordings *transcript.Directory
	cookie     string
}

// apiOptions collects the few knobs individual tests need; the zero value is
// the default fixture.
type apiOptions struct {
	config config.Config
	// replayLimit caps the frames a new attachment replays. Zero keeps the
	// manager's own default.
	replayLimit int
	// skipSetup leaves the server without an administrator, for the tests that
	// drive /api/setup themselves.
	skipSetup bool
}

func newAPIFixture(t *testing.T, options apiOptions) *apiFixture {
	t.Helper()
	dir := t.TempDir()
	database, err := store.Open(context.Background(), filepath.Join(dir, "wmux.db"))
	if err != nil {
		t.Fatal(err)
	}
	recordings, err := transcript.NewDirectory(transcript.DirectoryConfig{
		Root:       filepath.Join(dir, "recordings"),
		SyncWrites: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	key := bytes.Repeat([]byte{7}, security.MasterKeySize)
	repository := &app.RuntimeRepository{Store: database, MasterKey: key, Logger: logger}
	manager, err := terminal.NewManager(terminal.Config{
		Repository:   repository,
		Callbacks:    repository,
		Transcripts:  recordings,
		ReplayLimit:  options.replayLimit,
		ReconnectMin: 5 * time.Millisecond,
		ReconnectMax: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := options.config
	if cfg.SessionTTL == 0 {
		cfg.SessionTTL = time.Hour
	}
	application := New(cfg, database, key, manager, recordings, repository, logger)
	httpServer := httptest.NewServer(application.Handler())
	t.Cleanup(func() {
		httpServer.Close()
		_ = manager.Close()
		_ = database.Close()
	})
	fixture := &apiFixture{
		api:        application,
		server:     httpServer,
		database:   database,
		manager:    manager,
		recordings: recordings,
	}
	if !options.skipSetup {
		fixture.setUp(t)
	}
	return fixture
}

// setUp creates the administrator and records the login cookie. Tests that
// exercise setup itself skip it and drive the route directly.
func (f *apiFixture) setUp(t *testing.T) string {
	t.Helper()
	response := performJSON(t, f.api.Handler(), http.MethodPost, "/api/setup", map[string]any{
		"username": "owner",
		"password": "a-long-test-password",
	}, "")
	if response.Code != http.StatusCreated || len(response.Result().Cookies()) == 0 {
		t.Fatalf("setup: %d %s", response.Code, response.Body.String())
	}
	f.cookie = response.Result().Cookies()[0].String()
	return f.cookie
}

func performJSON(t *testing.T, handler http.Handler, method, path string, body any, cookie string) *httptest.ResponseRecorder {
	t.Helper()
	return performJSONWithOrigin(t, handler, method, path, body, cookie, "")
}

func performJSONWithOrigin(t *testing.T, handler http.Handler, method, path string, body any, cookie, origin string) *httptest.ResponseRecorder {
	t.Helper()
	var encoded io.Reader
	if body != nil {
		value, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		encoded = bytes.NewReader(value)
	}
	request := httptest.NewRequest(method, path, encoded)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if cookie != "" {
		request.Header.Set("Cookie", cookie)
	}
	if origin != "" {
		request.Header.Set("Origin", origin)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}
