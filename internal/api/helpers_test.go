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
		Root:   filepath.Join(dir, "recordings"),
		Limits: transcript.Limits{SyncWrites: true},
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

func storedCredentialsForTest(t *testing.T, ctx context.Context, fixture *apiFixture, hostID string) store.Credentials {
	t.Helper()
	host, err := fixture.database.GetHost(ctx, hostID)
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := fixture.api.decryptCredentials(host)
	if err != nil {
		t.Fatal(err)
	}
	return credentials
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
			if len(payload) < outputFrameHeaderBytes || payload[0] != serverOutputFrame {
				t.Fatalf("invalid terminal output frame: %x", payload)
			}
			tail.lastSequence = binary.BigEndian.Uint64(payload[1:outputFrameHeaderBytes])
			output.Write(payload[outputFrameHeaderBytes:])
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

// sendAndAwaitOutput reports the sequence of the frame that echoed input
// back, for the tests that only care about the sequence advancing.
func sendAndAwaitOutput(t *testing.T, ctx context.Context, connection *websocket.Conn, input string, after uint64) uint64 {
	t.Helper()
	sequence, _ := sendAndCollectOutput(t, ctx, connection, input, strings.TrimSpace(input), after)
	return sequence
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
		if messageType != websocket.MessageBinary || len(message) < outputFrameHeaderBytes || message[0] != serverOutputFrame {
			continue
		}
		sequence := binary.BigEndian.Uint64(message[1:outputFrameHeaderBytes])
		if sequence <= after {
			continue
		}
		lastSequence = sequence
		output.Write(message[outputFrameHeaderBytes:])
		if bytes.Contains(output.Bytes(), []byte(marker)) {
			return lastSequence, output.Bytes()
		}
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

func assertAPIError(t *testing.T, response *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	result := response.Result()
	defer result.Body.Close()
	if result.StatusCode != status {
		t.Fatalf("status = %d, want %d", result.StatusCode, status)
	}
	var body errorBody
	if err := json.NewDecoder(result.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Code != code {
		t.Fatalf("error code = %q, want %q", body.Error.Code, code)
	}
}

func responseErrorCode(t *testing.T, response *httptest.ResponseRecorder) string {
	t.Helper()
	result := response.Result()
	defer result.Body.Close()
	var body errorBody
	if err := json.NewDecoder(result.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	return body.Error.Code
}
