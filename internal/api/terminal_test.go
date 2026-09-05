package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/waterlens/wmux/internal/store"
)

func TestLocalTerminalOverWebSocket(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	fixture := newAPIFixture(t, apiOptions{})
	sessionID := createLocalSessionOverHTTP(t, ctx, fixture.server.URL, fixture.cookie)

	wsURL := "ws" + strings.TrimPrefix(fixture.server.URL, "http") + "/ws/sessions/" + url.PathEscape(sessionID)
	connection, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: http.Header{"Cookie": []string{fixture.cookie}}})
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
