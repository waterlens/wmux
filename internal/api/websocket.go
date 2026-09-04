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
	"github.com/waterlens/wmux/internal/terminal"
)

const (
	clientInputFrame  = byte(0)
	serverOutputFrame = byte(1)
	maxSocketMessage  = 128 << 10
)

type socketControl struct {
	Type string `json:"type"`
	Cols int    `json:"cols,omitempty"`
	Rows int    `json:"rows,omitempty"`
}

type socketEvent struct {
	Type      string `json:"type"`
	ClientID  string `json:"clientId,omitempty"`
	Status    string `json:"status,omitempty"`
	Backend   string `json:"backend,omitempty"`
	Writer    *bool  `json:"writer,omitempty"`
	WriterID  string `json:"writerId,omitempty"`
	Clients   int    `json:"clients,omitempty"`
	Sequence  uint64 `json:"sequence,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
	Reason    string `json:"reason,omitempty"`
	Message   string `json:"message,omitempty"`
}

func (s *Server) terminalSocket(w http.ResponseWriter, r *http.Request) {
	if s.terminals == nil {
		writeError(w, http.StatusServiceUnavailable, "terminal_unavailable", "终端服务不可用")
		return
	}
	if !originAllowed(r, s.config.PublicURL, s.config.TrustProxy) {
		writeError(w, http.StatusForbidden, "invalid_origin", "WebSocket 来源不受信任")
		return
	}
	id := r.PathValue("id")
	if _, err := s.store.GetSession(r.Context(), id); err != nil {
		s.handleStoreError(w, err, "终端会话不存在")
		return
	}
	after := uint64(0)
	if value := r.URL.Query().Get("since"); value != "" {
		parsed, err := strconv.ParseUint(value, 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_sequence", "无效的终端输出序号")
			return
		}
		after = parsed
	}
	clientID, err := newID("client")
	if err != nil {
		s.internalError(w, "生成终端客户端 ID", err)
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
		s.logger.Warn("attach terminal WebSocket", "session_id", id, "error", err)
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
		Type:      "hello",
		ClientID:  clientID,
		Status:    publicTerminalState(status.State),
		Backend:   string(status.Persistence),
		Writer:    &writer,
		WriterID:  status.WriterID,
		Clients:   status.Clients,
		Sequence:  delivered,
		Truncated: attachment.Truncated,
		Message:   publicTerminalMessage(status),
	}); err != nil {
		return
	}
	for _, frame := range attachment.Initial {
		if err := writeOutputFrame(r.Context(), connection, frame); err != nil {
			return
		}
		delivered = frame.Sequence
	}
	// Attach takes the transcript snapshot and installs the live subscriber
	// under the same runtime lock.  Sending this marker before we start reading
	// Frames therefore gives clients an exact replay/live boundary: every
	// binary frame before it is durable history and every binary frame after it
	// was published after that snapshot.
	delivered = attachment.LatestSequence
	if err := writeSocketJSON(r.Context(), connection, socketEvent{
		Type:     "replay_end",
		Sequence: delivered,
	}); err != nil {
		return
	}

	readDone := make(chan error, 1)
	controlOut := make(chan socketEvent, 16)
	go func() {
		readDone <- s.readTerminalSocket(r.Context(), connection, id, attachment, controlOut)
	}()

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	frames := attachment.Frames
	writerChanges := attachment.WriterChanges
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
			if ok {
				if err := writeSocketJSON(r.Context(), connection, socketEvent{Type: "writer", Writer: &writer}); err != nil {
					return
				}
			} else {
				writerChanges = nil
			}
		case reason := <-closed:
			s.closeTerminalSocket(r.Context(), connection, reason)
			return
		case frame, ok := <-frames:
			if !ok {
				// The terminal closes Closed before Frames, so the reason is
				// already buffered even when this select observes Frames first.
				reason := <-closed
				s.closeTerminalSocket(r.Context(), connection, reason)
				return
			}
			if err := writeOutputFrame(r.Context(), connection, frame); err != nil {
				return
			}
			delivered = frame.Sequence
		case <-ticker.C:
			status, err := s.terminals.Status(id)
			if errors.Is(err, terminal.ErrSessionNotFound) {
				_ = writeSocketJSON(r.Context(), connection, socketEvent{Type: "state", Status: "exited", Writer: boolPointer(false)})
				return
			}
			if err != nil {
				return
			}
			writer := attachment.IsWriter()
			if err := writeSocketJSON(r.Context(), connection, socketEvent{
				Type:     "state",
				Status:   publicTerminalState(status.State),
				Backend:  string(status.Persistence),
				Writer:   &writer,
				WriterID: status.WriterID,
				Clients:  status.Clients,
				Sequence: delivered,
				Message:  publicTerminalMessage(status),
			}); err != nil {
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
	return "终端暂时不可用，请检查会话配置或主机连接"
}

func (s *Server) closeTerminalSocket(ctx context.Context, connection *websocket.Conn, reason terminal.AttachmentCloseReason) {
	switch reason {
	case terminal.AttachmentExited:
		_ = writeSocketJSON(ctx, connection, socketEvent{Type: "state", Status: "exited", Writer: boolPointer(false), Reason: string(reason)})
		_ = connection.Close(websocket.StatusNormalClosure, "session exited")
	case terminal.AttachmentServerShutdown:
		_ = writeSocketJSON(ctx, connection, socketEvent{Type: "disconnect", Status: "reconnecting", Writer: boolPointer(false), Reason: string(reason)})
		_ = connection.Close(websocket.StatusServiceRestart, "server restarting")
	case terminal.AttachmentClientClosed:
		return
	default:
		_ = writeSocketJSON(ctx, connection, socketEvent{Type: "disconnect", Status: "reconnecting", Writer: boolPointer(false), Reason: string(reason)})
		_ = connection.Close(websocket.StatusTryAgainLater, "terminal stream interrupted")
	}
}

func writeOutputFrame(ctx context.Context, connection *websocket.Conn, frame terminal.OutputFrame) error {
	payload := make([]byte, 9+len(frame.Data))
	payload[0] = serverOutputFrame
	binary.BigEndian.PutUint64(payload[1:9], frame.Sequence)
	copy(payload[9:], frame.Data)
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
