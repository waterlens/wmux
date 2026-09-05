package api

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestLocalTerminalOverWebSocket(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	fixture := newAPIFixture(t, apiOptions{})
	sessionID := createSessionForTest(t, ctx, fixture, map[string]any{
		"name": "Integration", "kind": "local", "cwd": "",
		"command": `printf 'WMUX_E2E_OUTPUT\n'`, "persistence": "none",
	})

	connection := dialTerminalForTest(t, ctx, fixture, sessionID, 0)
	defer connection.CloseNow()

	var output bytes.Buffer
	for !bytes.Contains(output.Bytes(), []byte("WMUX_E2E_OUTPUT")) {
		messageType, payload, err := connection.Read(ctx)
		if err != nil {
			t.Fatalf("read terminal output: %v; output=%q", err, output.String())
		}
		if messageType == websocket.MessageBinary && len(payload) >= outputFrameHeaderBytes && payload[0] == serverOutputFrame {
			output.Write(payload[outputFrameHeaderBytes:])
		}
	}
}
