package api

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
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
	return newLiveAPIFixtureWithReplayLimit(t, ctx, 1)
}

func newLiveAPIFixtureWithReplayLimit(t *testing.T, ctx context.Context, replayLimit int) *liveAPIFixture {
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
		ReplayLimit:  replayLimit,
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

func TestTerminalWebSocketReplayBoundarySeparatesHistoryFromLiveOutput(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	fixture := newLiveAPIFixtureWithReplayLimit(t, ctx, 128)
	id := createSessionForTest(t, ctx, fixture, map[string]any{
		"name": "Replay boundary", "kind": "local", "command": "cat", "persistence": "none",
	})

	writer := dialTerminalForTest(t, ctx, fixture, id, 0)
	defer writer.CloseNow()
	if event := readSocketEventForTest(t, ctx, writer); event.Type != "hello" {
		t.Fatalf("first WebSocket message = %q, want hello", event.Type)
	}
	if event := awaitSocketEvent(t, ctx, writer, "replay_end", nil); event.Sequence != 0 {
		t.Fatalf("initial replay boundary = %d, want 0", event.Sequence)
	}
	waitForTerminalState(t, ctx, fixture.manager, id, terminal.StateRunning)

	history := "WMUX_HISTORY_BEGIN\x1b[5nWMUX_HISTORY_END\n"
	historySequence, historyOutput := sendAndCollectOutput(t, ctx, writer, history, "\x1b[5nWMUX_HISTORY_END", 0)
	if !bytes.Contains(historyOutput, []byte("\x1b[5n")) {
		t.Fatalf("live history %q does not contain CSI 5n", historyOutput)
	}

	replay := dialTerminalForTest(t, ctx, fixture, id, 0)
	defer replay.CloseNow()
	if event := readSocketEventForTest(t, ctx, replay); event.Type != "hello" {
		t.Fatalf("first replay WebSocket message = %q, want hello", event.Type)
	}

	// This output is produced after Attach captured its transcript snapshot. It
	// may already be queued while the initial replay is being written, but must
	// never cross the explicit boundary or reuse a replay sequence.
	live := "WMUX_LIVE_AFTER_ATTACH\n"
	if err := writer.Write(ctx, websocket.MessageBinary, append([]byte{clientInputFrame}, []byte(live)...)); err != nil {
		t.Fatal(err)
	}

	var replayOutput bytes.Buffer
	var liveOutput bytes.Buffer
	seenSequences := make(map[uint64]struct{})
	nextSequence := uint64(1)
	boundary := uint64(0)
	replaying := true
	for !bytes.Contains(liveOutput.Bytes(), []byte(strings.TrimSpace(live))) {
		messageType, payload, err := replay.Read(ctx)
		if err != nil {
			t.Fatalf("read replay stream: %v", err)
		}
		switch messageType {
		case websocket.MessageText:
			var event socketEvent
			if err := json.Unmarshal(payload, &event); err != nil {
				t.Fatalf("decode replay event: %v", err)
			}
			if replaying {
				if event.Type != "replay_end" {
					t.Fatalf("event %q appeared between hello and replay_end", event.Type)
				}
				boundary = event.Sequence
				replaying = false
			}
		case websocket.MessageBinary:
			if len(payload) < 9 || payload[0] != serverOutputFrame {
				t.Fatalf("invalid terminal output frame: %x", payload)
			}
			sequence := binary.BigEndian.Uint64(payload[1:9])
			if _, duplicate := seenSequences[sequence]; duplicate {
				t.Fatalf("terminal sequence %d was delivered more than once", sequence)
			}
			if sequence != nextSequence {
				t.Fatalf("terminal sequence jumped from %d to %d", nextSequence-1, sequence)
			}
			seenSequences[sequence] = struct{}{}
			nextSequence++
			if replaying {
				replayOutput.Write(payload[9:])
			} else {
				if sequence <= boundary {
					t.Fatalf("live sequence %d did not follow replay boundary %d", sequence, boundary)
				}
				liveOutput.Write(payload[9:])
			}
		}
	}
	if replaying {
		t.Fatal("live output arrived before replay_end")
	}
	if boundary != historySequence {
		t.Fatalf("replay_end sequence = %d, want snapshot sequence %d", boundary, historySequence)
	}
	if !bytes.Contains(replayOutput.Bytes(), []byte("\x1b[5n")) {
		t.Fatalf("initial replay %q does not contain the stale CSI 5n query", replayOutput.Bytes())
	}
}

func TestTerminalWebSocketReadLimitBoundary(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	fixture := newLiveAPIFixture(t, ctx)
	id := createSessionForTest(t, ctx, fixture, map[string]any{
		"name": "Read limit", "kind": "local", "command": "cat", "persistence": "none",
	})

	connection := dialTerminalForTest(t, ctx, fixture, id, 0)
	defer connection.CloseNow()
	if event := readSocketEventForTest(t, ctx, connection); event.Type != "hello" {
		t.Fatalf("first WebSocket message = %q, want hello", event.Type)
	}
	awaitSocketEvent(t, ctx, connection, "replay_end", nil)

	// SetReadLimit is inclusive. Keep this boundary explicit because terminal
	// input reserves one byte for its binary frame type, so callers may send at
	// most maxSocketMessage-1 input bytes in a single message.
	exact := paddedSocketControlForTest(maxSocketMessage)
	if err := connection.Write(ctx, websocket.MessageText, exact); err != nil {
		t.Fatalf("write exact-limit message: %v", err)
	}
	if event := awaitSocketEvent(t, ctx, connection, "error", nil); event.Message != "不支持的终端控制消息" {
		t.Fatalf("exact-limit response = %#v", event)
	}

	oversized := paddedSocketControlForTest(maxSocketMessage + 1)
	if err := connection.Write(ctx, websocket.MessageText, oversized); err != nil {
		t.Fatalf("write oversized message: %v", err)
	}
	for {
		_, _, err := connection.Read(ctx)
		if err == nil {
			continue
		}
		if status := websocket.CloseStatus(err); status != websocket.StatusMessageTooBig {
			t.Fatalf("oversized message close status = %v, want %v (error: %v)", status, websocket.StatusMessageTooBig, err)
		}
		break
	}
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

func TestDeleteDormantPersistentSSHSessionsWithoutContactingUnreachableHost(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	fixture := newLiveAPIFixture(t, ctx)

	response := doJSONForTest(t, ctx, fixture, http.MethodPost, "/api/hosts", map[string]any{
		"name": "unreachable", "address": "127.0.0.1", "port": 1,
		"username": "owner", "authType": "password", "password": "secret-value",
	})
	if response.StatusCode != http.StatusCreated {
		failResponse(t, response)
	}
	var host hostResponse
	decodeResponse(t, response, &host)
	storedHost, err := fixture.database.GetHost(ctx, host.ID)
	if err != nil {
		t.Fatal(err)
	}
	storedHost.Fingerprint = "SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	if _, err := fixture.database.UpdateHost(ctx, storedHost); err != nil {
		t.Fatal(err)
	}

	hostID := host.ID
	if err := fixture.database.SaveRuntimeSession(ctx, store.Session{
		ID: "ses_dormant_exited", Name: "dormant exited", Kind: store.SessionKindSSH, HostID: &hostID,
		Persistence: "tmux", Backend: "tmux", BackendName: terminal.BackendName("ses_dormant_exited"),
		Status: store.SessionStatusExited, Cols: 120, Rows: 36,
	}); err != nil {
		t.Fatal(err)
	}
	if err := fixture.database.SaveRuntimeSession(ctx, store.Session{
		ID: "ses_dormant_error", Name: "dormant error", Kind: store.SessionKindSSH, HostID: &hostID,
		Persistence: "tmux", Backend: "tmux", BackendName: terminal.BackendName("ses_dormant_error"),
		Status: store.SessionStatusConnecting, Cols: 120, Rows: 36,
	}); err != nil {
		t.Fatal(err)
	}
	if err := fixture.database.UpdateSessionRuntime(ctx, "ses_dormant_error", store.SessionStatusError, "tmux", terminal.BackendName("ses_dormant_error"), nil); err != nil {
		t.Fatal(err)
	}
	if err := fixture.manager.Restore(ctx); err != nil {
		t.Fatal(err)
	}

	for _, id := range []string{"ses_dormant_exited", "ses_dormant_error"} {
		status, err := fixture.manager.Status(id)
		if err != nil {
			t.Fatal(err)
		}
		if status.State != terminal.StateExited {
			t.Fatalf("restored session %s state = %s, want exited", id, status.State)
		}

		response = doJSONForTest(t, ctx, fixture, http.MethodDelete, "/api/sessions/"+id, nil)
		response.Body.Close()
		if response.StatusCode != http.StatusNoContent {
			t.Fatalf("delete dormant session %s returned %d, want %d", id, response.StatusCode, http.StatusNoContent)
		}
		if _, err := fixture.manager.Status(id); !errors.Is(err, terminal.ErrSessionNotFound) {
			t.Fatalf("manager retained deleted session %s: %v", id, err)
		}
		if _, err := fixture.database.GetSession(ctx, id); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("database retained deleted session %s: %v", id, err)
		}
	}

	response = doJSONForTest(t, ctx, fixture, http.MethodDelete, "/api/hosts/"+host.ID, nil)
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("delete formerly referenced host returned %d, want %d", response.StatusCode, http.StatusNoContent)
	}

	response = doJSONForTest(t, ctx, fixture, http.MethodPost, "/api/sessions/ses_dormant_exited/restart", nil)
	defer response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("restart deleted session returned %d, want %d", response.StatusCode, http.StatusNotFound)
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

func readSocketEventForTest(t *testing.T, ctx context.Context, connection *websocket.Conn) socketEvent {
	t.Helper()
	messageType, payload, err := connection.Read(ctx)
	if err != nil {
		t.Fatalf("read WebSocket event: %v", err)
	}
	if messageType != websocket.MessageText {
		t.Fatalf("WebSocket message type = %v, want text", messageType)
	}
	var event socketEvent
	if err := json.Unmarshal(payload, &event); err != nil {
		t.Fatalf("decode WebSocket event: %v", err)
	}
	return event
}

func paddedSocketControlForTest(size int) []byte {
	prefix := []byte(`{"type":"unsupported"}`)
	payload := bytes.Repeat([]byte{' '}, size)
	copy(payload, prefix)
	return payload
}

func waitForTerminalState(t *testing.T, ctx context.Context, manager *terminal.Manager, sessionID string, wanted terminal.SessionState) {
	t.Helper()
	for {
		status, err := manager.Status(sessionID)
		if err != nil {
			t.Fatal(err)
		}
		if status.State == wanted {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("terminal state = %s, want %s: %v", status.State, wanted, ctx.Err())
		case <-time.After(5 * time.Millisecond):
		}
	}
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

func sendAndCollectOutput(t *testing.T, ctx context.Context, connection *websocket.Conn, input, marker string, after uint64) (uint64, []byte) {
	t.Helper()
	payload := append([]byte{clientInputFrame}, []byte(input)...)
	if err := connection.Write(ctx, websocket.MessageBinary, payload); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	lastSequence := after
	for {
		messageType, message, err := connection.Read(ctx)
		if err != nil {
			t.Fatalf("read terminal output: %v; output=%q", err, output.Bytes())
		}
		if messageType != websocket.MessageBinary || len(message) < 9 || message[0] != serverOutputFrame {
			continue
		}
		sequence := binary.BigEndian.Uint64(message[1:9])
		if sequence <= after {
			continue
		}
		lastSequence = sequence
		output.Write(message[9:])
		if bytes.Contains(output.Bytes(), []byte(marker)) {
			return lastSequence, output.Bytes()
		}
	}
}
