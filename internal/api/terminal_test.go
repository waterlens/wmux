package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/waterlens/wmux/internal/app"
	"github.com/waterlens/wmux/internal/config"
	"github.com/waterlens/wmux/internal/store"
	"github.com/waterlens/wmux/internal/terminal"
	"github.com/waterlens/wmux/internal/transcript"
)

func TestLocalTerminalOverWebSocket(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	dir := t.TempDir()
	database, err := store.Open(ctx, filepath.Join(dir, "wmux.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	recordings, err := transcript.NewDirectory(transcript.DirectoryConfig{Root: filepath.Join(dir, "recordings"), SyncWrites: true})
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	key := bytes.Repeat([]byte{3}, 32)
	repository := &app.RuntimeRepository{Store: database, MasterKey: key, Logger: logger}
	manager, err := terminal.NewManager(terminal.Config{Repository: repository, Callbacks: repository, Transcripts: recordings})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })

	server := New(config.Config{SessionTTL: time.Hour}, database, key, manager, recordings, logger)
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)

	cookie := setupOverHTTP(t, ctx, httpServer.URL)
	sessionID := createLocalSessionOverHTTP(t, ctx, httpServer.URL, cookie)

	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/ws/sessions/" + url.PathEscape(sessionID)
	connection, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: http.Header{"Cookie": []string{cookie}}})
	if err != nil {
		t.Fatal(err)
	}
	defer connection.CloseNow()

	var output bytes.Buffer
	for !bytes.Contains(output.Bytes(), []byte("WMUX_E2E_OUTPUT")) {
		messageType, payload, err := connection.Read(ctx)
		if err != nil {
			t.Fatalf("read terminal output: %v; output=%q", err, output.String())
		}
		if messageType == websocket.MessageBinary && len(payload) >= 9 && payload[0] == serverOutputFrame {
			output.Write(payload[9:])
		}
	}
}

func setupOverHTTP(t *testing.T, ctx context.Context, baseURL string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"username": "owner", "password": "integration-password"})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/setup", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		payload, _ := io.ReadAll(response.Body)
		t.Fatalf("setup returned %d: %s", response.StatusCode, payload)
	}
	cookies := response.Cookies()
	if len(cookies) == 0 {
		t.Fatal("setup response did not set login cookie")
	}
	return cookies[0].String()
}

func createLocalSessionOverHTTP(t *testing.T, ctx context.Context, baseURL, cookie string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"name":        "Integration",
		"kind":        "local",
		"cwd":         "",
		"command":     "printf 'WMUX_E2E_OUTPUT\\n'",
		"persistence": "none",
	})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/sessions", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Cookie", cookie)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		payload, _ := io.ReadAll(response.Body)
		t.Fatalf("create session returned %d: %s", response.StatusCode, payload)
	}
	var session store.Session
	if err := json.NewDecoder(response.Body).Decode(&session); err != nil {
		t.Fatal(err)
	}
	if session.ID == "" {
		t.Fatal("created session has no ID")
	}
	return session.ID
}
