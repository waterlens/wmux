package api

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/waterlens/wmux/internal/store"
	"github.com/waterlens/wmux/internal/terminal"
)

func TestTerminalWebSocketReplayBoundarySeparatesHistoryFromLiveOutput(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	fixture := newAPIFixture(t, apiOptions{replayLimit: 128})
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

	// This output is produced after Attach captured its transcript snapshot.
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
	fixture := newAPIFixture(t, apiOptions{replayLimit: 1})
	id := createSessionForTest(t, ctx, fixture, map[string]any{
		"name": "Read limit", "kind": "local", "command": "cat", "persistence": "none",
	})

	connection := dialTerminalForTest(t, ctx, fixture, id, 0)
	defer connection.CloseNow()
	if event := readSocketEventForTest(t, ctx, connection); event.Type != "hello" {
		t.Fatalf("first WebSocket message = %q, want hello", event.Type)
	}
	awaitSocketEvent(t, ctx, connection, "replay_end", nil)

	// SetReadLimit is inclusive, and input reserves one byte for the frame type.
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
	fixture := newAPIFixture(t, apiOptions{replayLimit: 1})
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
	fixture := newAPIFixture(t, apiOptions{replayLimit: 1})
	id := createSessionForTest(t, ctx, fixture, map[string]any{
		"name": "生命周期", "kind": "local", "command": "cat", "persistence": "none",
	})

	response := doJSONForTest(t, ctx, fixture, http.MethodPatch, "/api/sessions/"+id, map[string]any{
		"name": "重命名后",
	})
	if response.StatusCode != http.StatusOK {
		failResponse(t, response)
	}
	var patched store.Session
	decodeResponse(t, response, &patched)
	if patched.Name != "重命名后" || patched.Generation != 1 {
		t.Fatalf("unexpected patched session: %#v", patched)
	}

	// Terminal dimensions belong to the live attachment.
	response = doJSONForTest(t, ctx, fixture, http.MethodPatch, "/api/sessions/"+id, map[string]any{
		"name": "尺寸补丁", "cols": 132, "rows": 41,
	})
	response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("size patch returned %d, want %d", response.StatusCode, http.StatusBadRequest)
	}
	stored, err := fixture.database.GetSession(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Cols != patched.Cols || stored.Rows != patched.Rows || stored.Name != patched.Name {
		t.Fatalf("rejected patch changed the session: %#v", stored)
	}

	response = doJSONForTest(t, ctx, fixture, http.MethodPost, "/api/sessions/"+id+"/restart", nil)
	if response.StatusCode != http.StatusOK {
		failResponse(t, response)
	}
	decodeResponse(t, response, &patched)
	if patched.Name != "重命名后" {
		t.Fatalf("restart lost product metadata: %#v", patched)
	}
	if patched.Generation != 2 {
		t.Fatalf("restart generation = %d, want 2", patched.Generation)
	}

	response = doJSONForTest(t, ctx, fixture, http.MethodDelete, "/api/sessions/"+id, nil)
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		failResponse(t, response)
	}
	if _, err := fixture.database.GetSession(ctx, id); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("deleted session lookup error = %v, want ErrNotFound", err)
	}
}

func TestDeleteDormantPersistentSSHSessionsWithoutContactingUnreachableHost(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	fixture := newAPIFixture(t, apiOptions{replayLimit: 1})

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
	for id, status := range map[string]string{
		"ses_dormant_exited": store.SessionStatusExited,
		"ses_dormant_error":  store.SessionStatusError,
	} {
		created, err := fixture.database.CreateSession(ctx, store.Session{
			ID: id, Name: id, Kind: store.SessionKindSSH, HostID: &hostID,
			Persistence: "tmux", Cols: 120, Rows: 36,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := fixture.database.UpdateSessionRuntime(ctx, id, created.Generation, status, "tmux", nil); err != nil {
			t.Fatal(err)
		}
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
	fixture := newAPIFixture(t, apiOptions{replayLimit: 1})

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
		Cols: 120, Rows: 36,
	}); err != nil {
		t.Fatal(err)
	}
	response = doJSONForTest(t, ctx, fixture, http.MethodDelete, "/api/hosts/"+created.ID, nil)
	defer response.Body.Close()
	if response.StatusCode != http.StatusConflict {
		failResponse(t, response)
	}
}

func createSessionForTest(t *testing.T, ctx context.Context, fixture *apiFixture, value map[string]any) string {
	t.Helper()
	response := doJSONForTest(t, ctx, fixture, http.MethodPost, "/api/sessions", value)
	if response.StatusCode != http.StatusCreated {
		failResponse(t, response)
	}
	var session store.Session
	decodeResponse(t, response, &session)
	return session.ID
}

func doJSONForTest(t *testing.T, ctx context.Context, fixture *apiFixture, method, path string, value any) *http.Response {
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

func dialTerminalForTest(t *testing.T, ctx context.Context, fixture *apiFixture, id string, since uint64) *websocket.Conn {
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

// socketTail is everything a terminal socket delivered before it closed.
type socketTail struct {
	events       []socketEvent
	output       []byte
	lastSequence uint64
	closeStatus  websocket.StatusCode
}

func readSocketToClose(t *testing.T, ctx context.Context, connection *websocket.Conn) socketTail {
	t.Helper()
	var tail socketTail
	var output bytes.Buffer
	for {
		messageType, payload, err := connection.Read(ctx)
		if err != nil {
			tail.closeStatus = websocket.CloseStatus(err)
			tail.output = output.Bytes()
			return tail
		}
		switch messageType {
		case websocket.MessageBinary:
			if len(payload) < 9 || payload[0] != serverOutputFrame {
				t.Fatalf("invalid terminal output frame: %x", payload)
			}
			tail.lastSequence = binary.BigEndian.Uint64(payload[1:9])
			output.Write(payload[9:])
		case websocket.MessageText:
			var event socketEvent
			if err := json.Unmarshal(payload, &event); err != nil {
				t.Fatalf("decode terminal event: %v", err)
			}
			tail.events = append(tail.events, event)
		}
	}
}

func paddedSocketControlForTest(size int) []byte {
	prefix := []byte(`{"type":"unsupported"}`)
	payload := bytes.Repeat([]byte{' '}, size)
	copy(payload, prefix)
	return payload
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

func TestDeleteRunningSessionOnUnreachableHostWarnsAndReleasesTheHost(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	fixture := newAPIFixture(t, apiOptions{replayLimit: 1})

	response := doJSONForTest(t, ctx, fixture, http.MethodPost, "/api/hosts", map[string]any{
		"name": "unreachable", "address": "127.0.0.1", "port": 1,
		"username": "owner", "authType": "password", "password": "secret-value",
	})
	if response.StatusCode != http.StatusCreated {
		failResponse(t, response)
	}
	var host hostResponse
	decodeResponse(t, response, &host)
	if err := fixture.database.UpdateHostFingerprint(ctx, host.ID, "SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"); err != nil {
		t.Fatal(err)
	}

	hostID := host.ID
	id := "ses_unreachable_running"
	created, err := fixture.database.CreateSession(ctx, store.Session{
		ID: id, Name: id, Kind: store.SessionKindSSH, HostID: &hostID,
		Persistence: "tmux", Cols: 120, Rows: 36,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.database.UpdateSessionRuntime(ctx, id, created.Generation, store.SessionStatusRunning, "tmux", nil); err != nil {
		t.Fatal(err)
	}
	if err := fixture.manager.Restore(ctx); err != nil {
		t.Fatal(err)
	}
	waitForTerminalState(t, ctx, fixture.manager, id, terminal.StateDisconnected, terminal.StateError)

	response = doJSONForTest(t, ctx, fixture, http.MethodDelete, "/api/sessions/"+id, nil)
	if response.StatusCode != http.StatusOK {
		failResponse(t, response)
	}
	var deleted struct {
		Warning string `json:"warning"`
	}
	decodeResponse(t, response, &deleted)
	if deleted.Warning == "" {
		t.Fatal("deleting a session on an unreachable host did not warn about the surviving backend")
	}
	if _, err := fixture.database.GetSession(ctx, id); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("database retained the deleted session: %v", err)
	}
	if _, err := fixture.manager.Status(id); !errors.Is(err, terminal.ErrSessionNotFound) {
		t.Fatalf("manager retained the deleted session: %v", err)
	}

	response = doJSONForTest(t, ctx, fixture, http.MethodDelete, "/api/hosts/"+host.ID, nil)
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("delete formerly referenced host returned %d, want %d", response.StatusCode, http.StatusNoContent)
	}
}

func TestReconnectSessionRetriesBackendOrReportsMissingSession(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	fixture := newAPIFixture(t, apiOptions{replayLimit: 1})
	id := createSessionForTest(t, ctx, fixture, map[string]any{
		"name": "重试连接", "kind": "local", "command": "cat", "persistence": "none",
	})

	response := doJSONForTest(t, ctx, fixture, http.MethodPost, "/api/sessions/"+id+"/reconnect", nil)
	if response.StatusCode != http.StatusNoContent {
		failResponse(t, response)
	}
	response.Body.Close()

	response = doJSONForTest(t, ctx, fixture, http.MethodPost, "/api/sessions/ses_missing/reconnect", nil)
	if response.StatusCode != http.StatusNotFound {
		failResponse(t, response)
	}
	var failure errorBody
	decodeResponse(t, response, &failure)
	if failure.Error.Code != "not_found" {
		t.Fatalf("reconnect of an unknown session = %#v", failure.Error)
	}
}

func TestTerminalWebSocketClosesWhenTheLoginIsRevoked(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	fixture := newAPIFixture(t, apiOptions{replayLimit: 1})
	id := createSessionForTest(t, ctx, fixture, map[string]any{
		"name": "登出", "kind": "local", "command": "cat", "persistence": "none",
	})

	connection := dialTerminalForTest(t, ctx, fixture, id, 0)
	defer connection.CloseNow()
	awaitSocketEvent(t, ctx, connection, "hello", nil)
	awaitSocketEvent(t, ctx, connection, "replay_end", nil)

	response := doJSONForTest(t, ctx, fixture, http.MethodPost, "/api/logout", nil)
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("logout returned %d, want %d", response.StatusCode, http.StatusNoContent)
	}

	// The heartbeat re-reads the login every two seconds.
	deadline, cancelDeadline := context.WithTimeout(ctx, 3*time.Second)
	defer cancelDeadline()
	tail := readSocketToClose(t, deadline, connection)
	if tail.closeStatus != websocket.StatusPolicyViolation {
		t.Fatalf("close status = %v, want %v", tail.closeStatus, websocket.StatusPolicyViolation)
	}
	unauthorized := false
	for _, event := range tail.events {
		if event.Type == "disconnect" && event.Reason == "unauthorized" {
			unauthorized = true
		}
	}
	if !unauthorized {
		t.Fatal("revoked login did not produce an unauthorized disconnect event")
	}
}

func TestTerminalWebSocketDeliversBufferedOutputBeforeExit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	fixture := newAPIFixture(t, apiOptions{replayLimit: 128})
	// The session writes its payload in many small chunks and then exits.
	id := createSessionForTest(t, ctx, fixture, map[string]any{
		"name": "尾部输出", "kind": "local", "persistence": "none",
		"command": `read -r _; i=0; while [ $i -lt 120 ]; do printf 'WMUX_TAIL_%s\n' "$i"; i=$((i+1)); done`,
	})

	connection := dialTerminalForTest(t, ctx, fixture, id, 0)
	defer connection.CloseNow()
	awaitSocketEvent(t, ctx, connection, "hello", nil)
	awaitSocketEvent(t, ctx, connection, "replay_end", nil)
	waitForTerminalState(t, ctx, fixture.manager, id, terminal.StateRunning)

	if err := connection.Write(ctx, websocket.MessageBinary, []byte{clientInputFrame, '\n'}); err != nil {
		t.Fatal(err)
	}

	tail := readSocketToClose(t, ctx, connection)
	if tail.closeStatus != websocket.StatusNormalClosure {
		t.Fatalf("close status = %v, want %v", tail.closeStatus, websocket.StatusNormalClosure)
	}
	exits := 0
	for _, event := range tail.events {
		if event.Status != "exited" {
			continue
		}
		exits++
		// The exit reports the last frame the client received.
		if event.Sequence != tail.lastSequence {
			t.Fatalf("exited sequence = %d, want the last delivered frame %d", event.Sequence, tail.lastSequence)
		}
	}
	if exits != 1 {
		t.Fatalf("session end was reported %d times, want once: %q", exits, tail.output)
	}
	for _, marker := range []string{"WMUX_TAIL_0", "WMUX_TAIL_60", "WMUX_TAIL_119"} {
		if !bytes.Contains(tail.output, []byte(marker)) {
			t.Fatalf("terminal output %q is missing %q", tail.output, marker)
		}
	}
}

func TestRestartClosesAttachedSocketForReconnect(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	fixture := newAPIFixture(t, apiOptions{replayLimit: 1})
	id := createSessionForTest(t, ctx, fixture, map[string]any{
		"name": "重启", "kind": "local", "command": "cat", "persistence": "none",
	})

	connection := dialTerminalForTest(t, ctx, fixture, id, 0)
	defer connection.CloseNow()
	awaitSocketEvent(t, ctx, connection, "hello", nil)
	awaitSocketEvent(t, ctx, connection, "replay_end", nil)
	waitForTerminalState(t, ctx, fixture.manager, id, terminal.StateRunning)

	response := doJSONForTest(t, ctx, fixture, http.MethodPost, "/api/sessions/"+id+"/restart", nil)
	if response.StatusCode != http.StatusOK {
		failResponse(t, response)
	}
	var restarted store.Session
	decodeResponse(t, response, &restarted)
	if restarted.Generation != 2 {
		t.Fatalf("restart generation = %d, want 2", restarted.Generation)
	}

	tail := readSocketToClose(t, ctx, connection)
	if tail.closeStatus != websocket.StatusTryAgainLater {
		t.Fatalf("close status = %v, want %v", tail.closeStatus, websocket.StatusTryAgainLater)
	}
	announced := false
	for _, event := range tail.events {
		if event.Type == "disconnect" && event.Reason == string(terminal.AttachmentRestarted) {
			announced = true
			break
		}
		if event.Status == "exited" {
			t.Fatalf("restart reported an exit before the reconnect: %#v", tail.events)
		}
	}
	if !announced {
		t.Fatal("restart did not tell the attached browser to reconnect")
	}
}

func waitForTerminalState(t *testing.T, ctx context.Context, manager *terminal.Manager, sessionID string, wanted ...terminal.SessionState) {
	t.Helper()
	for {
		status, err := manager.Status(sessionID)
		if err != nil {
			t.Fatal(err)
		}
		for _, state := range wanted {
			if status.State == state {
				return
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("terminal state = %s, want one of %v: %v", status.State, wanted, ctx.Err())
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func TestTerminalWebSocketAsksBrowserToRetryWhileRuntimeIsMissing(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	fixture := newAPIFixture(t, apiOptions{replayLimit: 1})
	// A row without a runtime is the window between stopping and re-creating.
	id := "ses_restart_window"
	if _, err := fixture.database.CreateSession(ctx, store.Session{ID: id, Name: id, Kind: store.SessionKindLocal, Cols: 120, Rows: 36}); err != nil {
		t.Fatal(err)
	}

	connection := dialTerminalForTest(t, ctx, fixture, id, 0)
	defer connection.CloseNow()
	tail := readSocketToClose(t, ctx, connection)
	if tail.closeStatus != websocket.StatusTryAgainLater {
		t.Fatalf("close status = %v, want %v", tail.closeStatus, websocket.StatusTryAgainLater)
	}
	reconnecting := false
	for _, event := range tail.events {
		if event.Type == "disconnect" && event.Status == "reconnecting" {
			reconnecting = true
		}
	}
	if !reconnecting {
		t.Fatal("a missing runtime did not tell the browser to reconnect")
	}
}
