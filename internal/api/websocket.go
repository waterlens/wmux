package api

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/coder/websocket"
	"github.com/waterlens/wmux/internal/store"
	"github.com/waterlens/wmux/internal/terminal"
)

const (
	// The binary frame layout below is the browser contract; keep in sync with
	// client/src/terminalProtocol.ts.
	clientInputFrame  = byte(0)
	serverOutputFrame = byte(1)
	// outputFrameHeaderBytes is the frame type byte plus a big-endian sequence.
	outputFrameHeaderBytes = 9
	maxSocketMessage       = 128 << 10

	// socketStatePeriod is the heartbeat that re-checks the login and state.
	socketStatePeriod = 2 * time.Second
	socketPingPeriod  = 30 * time.Second
	socketPingTimeout = 10 * time.Second
)

type socketControl struct {
	Type string `json:"type"`
	Cols int    `json:"cols,omitempty"`
	Rows int    `json:"rows,omitempty"`
}

type socketEvent struct {
	Type       string `json:"type"`
	Status     string `json:"status,omitempty"`
	Backend    string `json:"backend,omitempty"`
	Generation int    `json:"generation,omitempty"`
	Writer     *bool  `json:"writer,omitempty"`
	Sequence   uint64 `json:"sequence,omitempty"`
	Truncated  bool   `json:"truncated,omitempty"`
	Reason     string `json:"reason,omitempty"`
	Message    string `json:"message,omitempty"`
}

func (s *Server) terminalSocket(w http.ResponseWriter, r *http.Request) {
	// The heartbeat re-reads this login, so the stream cannot outlive it.
	auth, ok := r.Context().Value(authContextKey{}).(store.AuthSession)
	if !ok {
		writeError(w, http.StatusUnauthorized, codeUnauthorized, "请先登录")
		return
	}
	if !originAllowed(r, s.config.PublicURL, s.config.TrustProxy) {
		writeError(w, http.StatusForbidden, codeInvalidOrigin, "WebSocket 来源不受信任")
		return
	}
	id := r.PathValue("id")
	if _, err := s.store.GetSession(r.Context(), id); err != nil {
		s.handleStoreError(w, "read session", err, "终端会话不存在")
		return
	}
	after := uint64(0)
	if value := r.URL.Query().Get("since"); value != "" {
		parsed, err := strconv.ParseUint(value, 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, codeInvalidSequence, "无效的终端输出序号")
			return
		}
		after = parsed
	}
	clientID, err := store.NewID("client")
	if err != nil {
		s.internalError(w, "generate socket client id", err)
		return
	}

	connection, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true, // originAllowed above handles proxy-aware policy.
		CompressionMode:    websocket.CompressionDisabled,
	})
	if err != nil {
		return
	}
	defer connection.CloseNow()
	connection.SetReadLimit(maxSocketMessage)

	attachment, err := s.terminals.Attach(r.Context(), id, clientID, after)
	if err != nil {
		s.logger.Warn("attach terminal WebSocket", "session", id, "error", err)
		if errors.Is(err, terminal.ErrSessionNotFound) {
			// A row without a runtime means a restart is in flight.
			_ = writeSocketJSON(r.Context(), connection, socketEvent{Type: "disconnect", Status: "reconnecting", Reason: string(terminal.AttachmentRestarted)})
			_ = connection.Close(websocket.StatusTryAgainLater, "session restarting")
			return
		}
		_ = writeSocketJSON(r.Context(), connection, socketEvent{Type: "error", Message: "终端会话暂时不可用"})
		_ = connection.Close(websocket.StatusPolicyViolation, "session unavailable")
		return
	}
	defer attachment.Close()
	_ = s.store.TouchSession(r.Context(), id, time.Now())

	status, err := s.terminals.Status(id)
	if err != nil {
		_ = connection.Close(websocket.StatusInternalError, "session status unavailable")
		return
	}
	writer := attachment.IsWriter()
	delivered := attachment.OldestSequence
	if err := writeSocketJSON(r.Context(), connection, socketEvent{
		Type:       "hello",
		Status:     publicTerminalState(status.State),
		Backend:    string(status.Persistence),
		Generation: status.Generation,
		Writer:     &writer,
		Sequence:   delivered,
		Truncated:  attachment.Truncated,
		Message:    publicTerminalMessage(status),
	}); err != nil {
		return
	}
	for _, frame := range attachment.Initial {
		if err := writeOutputFrame(r.Context(), connection, frame); err != nil {
			return
		}
		delivered = frame.Sequence
	}
	// Attach snapshots the transcript and subscribes under one lock, so this
	// marker is an exact replay/live boundary.
	delivered = attachment.LatestSequence
	if err := writeSocketJSON(r.Context(), connection, socketEvent{
		Type:     "replay_end",
		Sequence: delivered,
	}); err != nil {
		return
	}

	stopPings := pingSocket(r.Context(), connection)
	defer stopPings()

	readDone := make(chan error, 1)
	controlOut := make(chan socketEvent, 16)
	go func() {
		readDone <- s.readTerminalSocket(r.Context(), connection, id, attachment, controlOut)
	}()

	ticker := time.NewTicker(socketStatePeriod)
	defer ticker.Stop()
	frames := attachment.Frames
	writerChanges := attachment.WriterChanges
	states := attachment.States
	closed := attachment.Closed
	for {
		select {
		case <-r.Context().Done():
			return
		case <-readDone:
			return
		case event := <-controlOut:
			if err := writeSocketJSON(r.Context(), connection, event); err != nil {
				return
			}
		case writer, ok := <-writerChanges:
			if !ok {
				writerChanges = nil
				continue
			}
			if err := writeSocketJSON(r.Context(), connection, socketEvent{Type: "writer", Writer: &writer}); err != nil {
				return
			}
		case status, ok := <-states:
			if !ok {
				states = nil
				continue
			}
			// Terminal outcomes come from closeTerminalSocket; a restart also
			// passes through StateTerminating.
			if publicTerminalState(status.State) == "exited" {
				continue
			}
			if err := writeSocketJSON(r.Context(), connection, terminalStateEvent(status, attachment.IsWriter(), delivered)); err != nil {
				return
			}
		case reason := <-closed:
			s.closeTerminalSocket(r.Context(), connection, reason, frames, delivered)
			return
		case frame, ok := <-frames:
			if !ok {
				// Closed is closed before Frames, so the reason is already buffered.
				reason := <-closed
				s.closeTerminalSocket(r.Context(), connection, reason, frames, delivered)
				return
			}
			if err := writeOutputFrame(r.Context(), connection, frame); err != nil {
				return
			}
			delivered = frame.Sequence
		case <-ticker.C:
			if _, err := s.store.GetAuthSession(r.Context(), auth.TokenHash); err != nil {
				_ = writeSocketJSON(r.Context(), connection, socketEvent{Type: "disconnect", Status: "exited", Reason: "unauthorized"})
				_ = connection.Close(websocket.StatusPolicyViolation, "unauthorized")
				return
			}
			status, err := s.terminals.Status(id)
			if errors.Is(err, terminal.ErrSessionNotFound) {
				_ = writeSocketJSON(r.Context(), connection, socketEvent{Type: "state", Status: "exited", Writer: boolPointer(false), Sequence: delivered})
				return
			}
			if err != nil {
				return
			}
			if publicTerminalState(status.State) == "exited" {
				continue
			}
			if err := writeSocketJSON(r.Context(), connection, terminalStateEvent(status, attachment.IsWriter(), delivered)); err != nil {
				return
			}
		}
	}
}

func (s *Server) readTerminalSocket(ctx context.Context, connection *websocket.Conn, sessionID string, attachment *terminal.Attachment, controlOut chan<- socketEvent) error {
	for {
		messageType, payload, err := connection.Read(ctx)
		if err != nil {
			return err
		}
		switch messageType {
		case websocket.MessageBinary:
			if len(payload) < 1 || payload[0] != clientInputFrame {
				nonBlockingEvent(controlOut, socketEvent{Type: "error", Message: "无效的终端输入帧"})
				continue
			}
			if len(payload) == 1 {
				continue
			}
			writeCtx, cancelWrite := context.WithTimeout(ctx, 10*time.Second)
			_, writeErr := attachment.WriteContext(writeCtx, payload[1:])
			cancelWrite()
			if writeErr != nil {
				if errors.Is(writeErr, terminal.ErrNotWriter) {
					nonBlockingEvent(controlOut, socketEvent{Type: "writer", Writer: boolPointer(false)})
					continue
				}
				if errors.Is(writeErr, terminal.ErrUnavailable) {
					nonBlockingEvent(controlOut, socketEvent{Type: "state", Status: "reconnecting", Writer: boolPointer(attachment.IsWriter())})
					continue
				}
				if errors.Is(writeErr, context.DeadlineExceeded) {
					// A slow write keeps the stream and the backend alive.
					nonBlockingEvent(controlOut, socketEvent{Type: "error", Message: "终端未响应输入，请稍后重试"})
					continue
				}
				return writeErr
			}
		case websocket.MessageText:
			var control socketControl
			if err := json.Unmarshal(payload, &control); err != nil {
				nonBlockingEvent(controlOut, socketEvent{Type: "error", Message: "无效的终端控制消息"})
				continue
			}
			switch control.Type {
			case "resize":
				if !validSize(control.Cols, control.Rows) {
					nonBlockingEvent(controlOut, socketEvent{Type: "error", Message: "终端尺寸超出允许范围"})
					continue
				}
				if err := attachment.Resize(uint16(control.Cols), uint16(control.Rows)); err != nil {
					if errors.Is(err, terminal.ErrNotWriter) || errors.Is(err, terminal.ErrUnavailable) {
						continue
					}
					return err
				}
				_ = s.store.UpdateSessionSize(ctx, sessionID, control.Cols, control.Rows)
			case "take_control":
				if err := attachment.TakeControl(); err != nil {
					return err
				}
			default:
				nonBlockingEvent(controlOut, socketEvent{Type: "error", Message: "不支持的终端控制消息"})
			}
		}
	}
}

// pingSocket detects a silent peer; the returned function stops it.
func pingSocket(ctx context.Context, connection *websocket.Conn) func() {
	pingCtx, stop := context.WithCancel(ctx)
	go func() {
		ticker := time.NewTicker(socketPingPeriod)
		defer ticker.Stop()
		for {
			select {
			case <-pingCtx.Done():
				return
			case <-ticker.C:
				timeout, cancel := context.WithTimeout(pingCtx, socketPingTimeout)
				err := connection.Ping(timeout)
				cancel()
				if err != nil {
					if pingCtx.Err() == nil {
						connection.CloseNow()
					}
					return
				}
			}
		}
	}()
	return stop
}

func terminalStateEvent(status terminal.SessionStatus, writer bool, delivered uint64) socketEvent {
	return socketEvent{
		Type:       "state",
		Status:     publicTerminalState(status.State),
		Backend:    string(status.Persistence),
		Generation: status.Generation,
		Writer:     &writer,
		Sequence:   delivered,
		Message:    publicTerminalMessage(status),
	}
}

func publicTerminalState(state terminal.SessionState) string {
	switch state {
	case terminal.StateConnecting:
		return "connecting"
	case terminal.StateRunning:
		return "running"
	case terminal.StateDisconnected:
		return "reconnecting"
	case terminal.StateError:
		return "error"
	case terminal.StateTerminating, terminal.StateTerminated, terminal.StateExited:
		return "exited"
	default:
		return "error"
	}
}

func publicTerminalMessage(status terminal.SessionStatus) string {
	if status.LastError == "" {
		return ""
	}
	// A missing tmux/screen session is an ordinary outcome, not a fault.
	if publicTerminalState(status.State) == "exited" && status.LastError == terminal.ErrMuxSessionMissing.Error() {
		return "后台会话已不存在"
	}
	return "终端暂时不可用，请检查会话配置或主机连接"
}

// closeTerminalSocket flushes the runtime's remaining output and reports why
// the stream ended.
func (s *Server) closeTerminalSocket(ctx context.Context, connection *websocket.Conn, reason terminal.AttachmentCloseReason, frames <-chan terminal.OutputFrame, delivered uint64) {
	if reason == terminal.AttachmentClientClosed {
		return
	}
	for frame := range frames {
		if err := writeOutputFrame(ctx, connection, frame); err != nil {
			return
		}
		delivered = frame.Sequence
	}
	switch reason {
	case terminal.AttachmentExited:
		_ = writeSocketJSON(ctx, connection, socketEvent{Type: "state", Status: "exited", Writer: boolPointer(false), Sequence: delivered, Reason: string(reason)})
		_ = connection.Close(websocket.StatusNormalClosure, "session exited")
	case terminal.AttachmentRestarted:
		_ = writeSocketJSON(ctx, connection, socketEvent{Type: "disconnect", Status: "reconnecting", Writer: boolPointer(false), Sequence: delivered, Reason: string(reason)})
		_ = connection.Close(websocket.StatusTryAgainLater, "session restarting")
	case terminal.AttachmentServerShutdown:
		_ = writeSocketJSON(ctx, connection, socketEvent{Type: "disconnect", Status: "reconnecting", Writer: boolPointer(false), Sequence: delivered, Reason: string(reason)})
		_ = connection.Close(websocket.StatusServiceRestart, "server restarting")
	default:
		_ = writeSocketJSON(ctx, connection, socketEvent{Type: "disconnect", Status: "reconnecting", Writer: boolPointer(false), Sequence: delivered, Reason: string(reason)})
		_ = connection.Close(websocket.StatusTryAgainLater, "terminal stream interrupted")
	}
}

func writeOutputFrame(ctx context.Context, connection *websocket.Conn, frame terminal.OutputFrame) error {
	payload := make([]byte, outputFrameHeaderBytes+len(frame.Data))
	payload[0] = serverOutputFrame
	binary.BigEndian.PutUint64(payload[1:outputFrameHeaderBytes], frame.Sequence)
	copy(payload[outputFrameHeaderBytes:], frame.Data)
	writeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return connection.Write(writeCtx, websocket.MessageBinary, payload)
}

func writeSocketJSON(ctx context.Context, connection *websocket.Conn, event socketEvent) error {
	payload, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("encode WebSocket event: %w", err)
	}
	writeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return connection.Write(writeCtx, websocket.MessageText, payload)
}

func nonBlockingEvent(channel chan<- socketEvent, event socketEvent) {
	select {
	case channel <- event:
	default:
	}
}

func boolPointer(value bool) *bool {
	return &value
}

// validSize bounds the terminal geometry a browser may request.
func validSize(cols, rows int) bool {
	return cols >= 20 && cols <= 1000 && rows >= 5 && rows <= 500
}
