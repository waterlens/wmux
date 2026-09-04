package api

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
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

type liveAPIFixture struct {
	server   *httptest.Server
	database *store.Store
	manager  *terminal.Manager
	cookie   string
}

func newLiveAPIFixture(t *testing.T, ctx context.Context) *liveAPIFixture {
	t.Helper()
	dir := t.TempDir()
	database, err := store.Open(ctx, filepath.Join(dir, "wmux.db"))
	if err != nil {
		t.Fatal(err)
	}
	recordings, err := transcript.NewDirectory(transcript.DirectoryConfig{Root: filepath.Join(dir, "recordings"), SyncWrites: true})
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	key := bytes.Repeat([]byte{7}, 32)
	repository := &app.RuntimeRepository{Store: database, MasterKey: key, Logger: logger}
	manager, err := terminal.NewManager(terminal.Config{
		Repository:   repository,
		Callbacks:    repository,
		Transcripts:  recordings,
		ReplayLimit:  1,
		ReconnectMin: 5 * time.Millisecond,
		ReconnectMax: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	application := New(config.Config{SessionTTL: time.Hour}, database, key, manager, recordings, logger)
	httpServer := httptest.NewServer(application.Handler())
	fixture := &liveAPIFixture{server: httpServer, database: database, manager: manager}
	fixture.cookie = setupOverHTTP(t, ctx, httpServer.URL)
	t.Cleanup(func() {
		httpServer.Close()
		_ = manager.Close()
		_ = database.Close()
	})
	return fixture
}

func TestTerminalWebSocketControlReplayAndShutdownReason(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	fixture := newLiveAPIFixture(t, ctx)
	id := createSessionForTest(t, ctx, fixture, map[string]any{
		"name": "WebSocket", "kind": "local", "command": "cat", "persistence": "none",
	})

	first := dialTerminalForTest(t, ctx, fixture, id, 0)
	defer first.CloseNow()
	if hello := awaitSocketEvent(t, ctx, first, "hello", nil); hello.Writer == nil || !*hello.Writer {
		t.Fatalf("first client hello = %#v, want writer", hello)
	}
	second := dialTerminalForTest(t, ctx, fixture, id, 0)
	defer second.CloseNow()
	if hello := awaitSocketEvent(t, ctx, second, "hello", nil); hello.Writer == nil || *hello.Writer {
		t.Fatalf("second client hello = %#v, want read-only", hello)
	}

	if err := second.Write(ctx, websocket.MessageText, []byte(`{"type":"take_control"}`)); err != nil {
		t.Fatal(err)
	}
	awaitSocketEvent(t, ctx, second, "writer", func(event socketEvent) bool { return event.Writer != nil && *event.Writer })
	awaitSocketEvent(t, ctx, first, "writer", func(event socketEvent) bool { return event.Writer != nil && !*event.Writer })

	if err := second.Write(ctx, websocket.MessageText, []byte(`{"type":"resize","cols":141,"rows":43}`)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		session, err := fixture.database.GetSession(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if session.Cols == 141 && session.Rows == 43 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("resize was not persisted: %dx%d", session.Cols, session.Rows)
		}
		time.Sleep(10 * time.Millisecond)
	}

	firstSequence := sendAndAwaitOutput(t, ctx, second, "WMUX_FRAME_ONE\n", 0)
	secondSequence := sendAndAwaitOutput(t, ctx, second, "WMUX_FRAME_TWO\n", firstSequence)
	if secondSequence <= firstSequence {
		t.Fatalf("output sequence did not advance: %d -> %d", firstSequence, secondSequence)
	}
	_ = first.Close(websocket.StatusNormalClosure, "replay test")
	_ = second.Close(websocket.StatusNormalClosure, "replay test")

	replay := dialTerminalForTest(t, ctx, fixture, id, 0)
	defer replay.CloseNow()
	hello := awaitSocketEvent(t, ctx, replay, "hello", nil)
	if hello.Sequence != 1 || !hello.Truncated {
		t.Fatalf("replay hello = %#v, want oldest sequence 1 and truncated", hello)
	}
	if err := fixture.manager.CloseContext(ctx); err != nil {
		t.Fatal(err)
	}
	disconnect := awaitSocketEvent(t, ctx, replay, "disconnect", nil)
	if disconnect.Status != "reconnecting" || disconnect.Reason != string(terminal.AttachmentServerShutdown) {
		t.Fatalf("shutdown event = %#v", disconnect)
	}
}

func TestSessionPatchRestartAndDeleteLifecycle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	fixture := newLiveAPIFixture(t, ctx)
	id := createSessionForTest(t, ctx, fixture, map[string]any{
		"name": "生命周期", "kind": "local", "command": "cat", "persistence": "none",
	})

	response := doJSONForTest(t, ctx, fixture, http.MethodPatch, "/api/sessions/"+id, map[string]any{
		"name": "重命名后", "cols": 132, "rows": 41,
	})
	if response.StatusCode != http.StatusOK {
		failResponse(t, response)
	}
	var patched store.Session
	decodeResponse(t, response, &patched)
	if patched.Name != "重命名后" || patched.Cols != 132 || patched.Rows != 41 {
		t.Fatalf("unexpected patched session: %#v", patched)
	}

	response = doJSONForTest(t, ctx, fixture, http.MethodPost, "/api/sessions/"+id+"/restart", nil)
	if response.StatusCode != http.StatusOK {
		failResponse(t, response)
	}
	decodeResponse(t, response, &patched)
	if patched.Name != "重命名后" {
		t.Fatalf("restart lost product metadata: %#v", patched)
	}

	response = doJSONForTest(t, ctx, fixture, http.MethodDelete, "/api/sessions/"+id, nil)
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		failResponse(t, response)
	}
	if _, err := fixture.database.GetSession(ctx, id); err != store.ErrNotFound {
		t.Fatalf("deleted session lookup error = %v, want ErrNotFound", err)
	}
}

func TestHostPatchPreservesSecretAndReferencedHostCannotBeDeleted(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	fixture := newLiveAPIFixture(t, ctx)

	response := doJSONForTest(t, ctx, fixture, http.MethodPost, "/api/hosts", map[string]any{
		"name": "家庭服务器", "address": "192.0.2.10", "port": 22,
		"username": "owner", "authType": "password", "password": "secret-value",
	})
	if response.StatusCode != http.StatusCreated {
		failResponse(t, response)
	}
	var created hostResponse
	decodeResponse(t, response, &created)
	if !created.HasSecret {
		t.Fatal("created host did not report its encrypted credential")
	}

	stored, err := fixture.database.GetHost(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	stored.Fingerprint = "SHA256:test-fingerprint"
	if _, err := fixture.database.UpdateHost(ctx, stored); err != nil {
		t.Fatal(err)
	}
	response = doJSONForTest(t, ctx, fixture, http.MethodPatch, "/api/hosts/"+created.ID, map[string]any{"name": "重命名服务器"})
	if response.StatusCode != http.StatusOK {
		failResponse(t, response)
	}
	var patched hostResponse
	decodeResponse(t, response, &patched)
	if patched.Name != "重命名服务器" || !patched.HasSecret || patched.Fingerprint == "" {
		t.Fatalf("host patch lost an unchanged field: %#v", patched)
	}

	hostID := created.ID
	if _, err := fixture.database.CreateSession(ctx, store.Session{
		Name: "uses-host", Kind: store.SessionKindSSH, HostID: &hostID,
	}); err != nil {
		t.Fatal(err)
	}
	response = doJSONForTest(t, ctx, fixture, http.MethodDelete, "/api/hosts/"+created.ID, nil)
	defer response.Body.Close()
	if response.StatusCode != http.StatusConflict {
		failResponse(t, response)
	}
}

func createSessionForTest(t *testing.T, ctx context.Context, fixture *liveAPIFixture, value map[string]any) string {
	t.Helper()
	response := doJSONForTest(t, ctx, fixture, http.MethodPost, "/api/sessions", value)
	if response.StatusCode != http.StatusCreated {
		failResponse(t, response)
	}
	var session store.Session
	decodeResponse(t, response, &session)
	return session.ID
}

func doJSONForTest(t *testing.T, ctx context.Context, fixture *liveAPIFixture, method, path string, value any) *http.Response {
	t.Helper()
	var body io.Reader
	if value != nil {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, fixture.server.URL+path, body)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Cookie", fixture.cookie)
	if value != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func decodeResponse(t *testing.T, response *http.Response, destination any) {
	t.Helper()
	defer response.Body.Close()
	if err := json.NewDecoder(response.Body).Decode(destination); err != nil {
		t.Fatal(err)
	}
}

func failResponse(t *testing.T, response *http.Response) {
	t.Helper()
	defer response.Body.Close()
	payload, _ := io.ReadAll(response.Body)
	t.Fatalf("request returned %d: %s", response.StatusCode, payload)
}

func dialTerminalForTest(t *testing.T, ctx context.Context, fixture *liveAPIFixture, id string, since uint64) *websocket.Conn {
	t.Helper()
	address := "ws" + strings.TrimPrefix(fixture.server.URL, "http") + "/ws/sessions/" + url.PathEscape(id) + "?since=" + fmt.Sprint(since)
	connection, _, err := websocket.Dial(ctx, address, &websocket.DialOptions{HTTPHeader: http.Header{"Cookie": []string{fixture.cookie}}})
	if err != nil {
		t.Fatal(err)
	}
	return connection
}

func awaitSocketEvent(t *testing.T, ctx context.Context, connection *websocket.Conn, eventType string, accept func(socketEvent) bool) socketEvent {
	t.Helper()
	for {
		messageType, payload, err := connection.Read(ctx)
		if err != nil {
			t.Fatalf("read WebSocket event %q: %v", eventType, err)
		}
		if messageType != websocket.MessageText {
			continue
		}
		var event socketEvent
		if err := json.Unmarshal(payload, &event); err != nil {
			t.Fatalf("decode WebSocket event: %v", err)
		}
		if event.Type == eventType && (accept == nil || accept(event)) {
			return event
		}
	}
}

func sendAndAwaitOutput(t *testing.T, ctx context.Context, connection *websocket.Conn, input string, after uint64) uint64 {
	t.Helper()
	payload := append([]byte{clientInputFrame}, []byte(input)...)
	if err := connection.Write(ctx, websocket.MessageBinary, payload); err != nil {
		t.Fatal(err)
	}
	for {
		messageType, message, err := connection.Read(ctx)
		if err != nil {
			t.Fatalf("read terminal output: %v", err)
		}
		if messageType != websocket.MessageBinary || len(message) < 9 || message[0] != serverOutputFrame {
			continue
		}
		sequence := binary.BigEndian.Uint64(message[1:9])
		if sequence > after && bytes.Contains(message[9:], []byte(strings.TrimSpace(input))) {
			return sequence
		}
	}
}
