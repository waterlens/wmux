package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/waterlens/wmux/internal/store"
	"github.com/waterlens/wmux/internal/terminal"
)

func (s *Server) listSessions(w http.ResponseWriter, r *http.Request) {
	sessions, err := s.store.ListSessions(r.Context())
	if err != nil {
		s.internalError(w, "列出终端会话", err)
		return
	}
	result := make([]store.Session, 0, len(sessions))
	for _, session := range sessions {
		result = append(result, publicSession(session))
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) createSession(w http.ResponseWriter, r *http.Request) {
	if s.terminals == nil {
		writeError(w, http.StatusServiceUnavailable, "terminal_unavailable", "终端服务不可用")
		return
	}
	var input sessionInput
	if !decodeJSON(w, r, &input) {
		return
	}
	if err := input.normalize(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_session", err.Error())
		return
	}
	autoNamed := input.Name == ""
	// Default-name selection and runtime persistence are one in-process
	// critical section so two devices cannot create indistinguishable defaults.
	s.sessionNameMu.Lock()
	defer s.sessionNameMu.Unlock()
	id, err := newID("ses")
	if err != nil {
		s.internalError(w, "生成会话 ID", err)
		return
	}
	model := store.Session{
		ID:          id,
		Kind:        input.Kind,
		Cwd:         input.Cwd,
		Command:     input.Command,
		Persistence: input.Persistence,
		Status:      store.SessionStatusConnecting,
		Cols:        120,
		Rows:        36,
	}
	if input.Kind == store.SessionKindSSH {
		host, hostErr := s.store.GetHost(r.Context(), input.HostID)
		if hostErr != nil {
			s.handleStoreError(w, hostErr, "SSH 主机不存在")
			return
		}
		if host.Fingerprint == "" {
			writeError(w, http.StatusConflict, "host_untrusted", "请先验证并信任 SSH 主机密钥")
			return
		}
		model.HostID = &host.ID
		if input.Name == "" {
			input.Name = host.Name
		}
	}
	if input.Name == "" {
		input.Name = "本机终端"
	}
	if autoNamed {
		input.Name, err = s.availableSessionName(r.Context(), input.Name)
		if err != nil {
			s.internalError(w, "生成默认会话名称", err)
			return
		}
	}
	model.Name = input.Name
	spec, err := s.sessionSpecs.SessionSpec(r.Context(), model)
	if err != nil {
		s.internalError(w, "准备终端配置", err)
		return
	}
	if _, err := s.terminals.Create(r.Context(), spec); err != nil {
		s.upstreamError(w, "启动终端会话", "terminal_start_failed", "无法启动会话，请检查工作目录、命令或连接设置", err)
		return
	}
	created, err := s.store.GetSession(r.Context(), id)
	if err != nil {
		s.internalError(w, "读取新会话", err)
		return
	}
	writeJSON(w, http.StatusCreated, publicSession(created))
}

func (s *Server) updateSession(w http.ResponseWriter, r *http.Request) {
	session, err := s.store.GetSession(r.Context(), r.PathValue("id"))
	if err != nil {
		s.handleStoreError(w, err, "终端会话不存在")
		return
	}
	var patch sessionPatch
	if !decodeJSON(w, r, &patch) {
		return
	}
	if patch.Name != nil {
		name := strings.TrimSpace(*patch.Name)
		if name == "" || len(name) > 80 {
			writeError(w, http.StatusBadRequest, "invalid_session", "会话名称不能为空且不能超过 80 个字符")
			return
		}
		if _, err := s.store.UpdateSessionName(r.Context(), session.ID, name); err != nil {
			s.handleStoreError(w, err, "终端会话不存在")
			return
		}
	}
	cols, rows := session.Cols, session.Rows
	if patch.Cols != nil {
		cols = *patch.Cols
	}
	if patch.Rows != nil {
		rows = *patch.Rows
	}
	if !validSize(cols, rows) {
		writeError(w, http.StatusBadRequest, "invalid_size", "终端尺寸超出允许范围")
		return
	}
	if cols != session.Cols || rows != session.Rows {
		if err := s.store.UpdateSessionSize(r.Context(), session.ID, cols, rows); err != nil {
			s.handleStoreError(w, err, "终端会话不存在")
			return
		}
	}
	updated, err := s.store.GetSession(r.Context(), session.ID)
	if err != nil {
		s.handleStoreError(w, err, "终端会话不存在")
		return
	}
	writeJSON(w, http.StatusOK, publicSession(updated))
}

func (s *Server) deleteSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.store.GetSession(r.Context(), id); err != nil {
		s.handleStoreError(w, err, "终端会话不存在")
		return
	}
	if s.terminals != nil {
		if err := s.stopTerminalSession(r.Context(), id); err != nil {
			s.upstreamError(w, "结束终端会话", "terminal_stop_failed", "暂时无法结束后台会话；会话仍已保留，请稍后重试", err)
			return
		}
	}
	if err := s.store.DeleteSession(r.Context(), id); err != nil {
		s.handleStoreError(w, err, "终端会话不存在")
		return
	}
	if s.transcripts != nil {
		if err := s.transcripts.Remove(id); err != nil {
			s.logger.Warn("remove terminal transcript", "session", id, "error", err)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) restartSession(w http.ResponseWriter, r *http.Request) {
	if s.terminals == nil {
		writeError(w, http.StatusServiceUnavailable, "terminal_unavailable", "终端服务不可用")
		return
	}
	id := r.PathValue("id")
	session, err := s.store.GetSession(r.Context(), id)
	if err != nil {
		s.handleStoreError(w, err, "终端会话不存在")
		return
	}
	if err := s.stopTerminalSession(r.Context(), id); err != nil {
		s.upstreamError(w, "结束待重启会话", "terminal_stop_failed", "暂时无法重启后台会话，请稍后重试", err)
		return
	}
	if err := s.store.UpdateSessionStatus(r.Context(), id, store.SessionStatusConnecting, nil, nil); err != nil {
		s.internalError(w, "重置会话状态", err)
		return
	}
	spec, err := s.sessionSpecs.SessionSpec(r.Context(), session)
	if err != nil {
		s.internalError(w, "准备重启配置", err)
		return
	}
	if _, err := s.terminals.Create(r.Context(), spec); err != nil {
		message := "无法启动会话，请检查工作目录、命令或连接设置"
		_ = s.store.UpdateSessionStatus(r.Context(), id, store.SessionStatusError, nil, &message)
		s.upstreamError(w, "重启终端会话", "terminal_start_failed", message, err)
		return
	}
	restarted, err := s.store.GetSession(r.Context(), id)
	if err != nil {
		s.internalError(w, "读取重启会话", err)
		return
	}
	writeJSON(w, http.StatusOK, publicSession(restarted))
}

// stopTerminalSession preserves the destructive semantics for an active
// backend, while allowing terminal metadata that is already exited (or stuck
// at a permanent launch error) to be removed without contacting an
// unreachable host. DiscardContext rejects active sessions, so this decision
// remains inside Manager rather than trusting a potentially stale DB status.
func (s *Server) stopTerminalSession(ctx context.Context, id string) error {
	if s.terminals == nil {
		return nil
	}
	err := s.terminals.DiscardContext(ctx, id)
	switch {
	case err == nil, errors.Is(err, terminal.ErrSessionNotFound):
		return nil
	case errors.Is(err, terminal.ErrSessionActive):
		err = s.terminals.Terminate(ctx, id)
		if err == nil || errors.Is(err, terminal.ErrSessionNotFound) {
			return nil
		}
		return err
	default:
		return err
	}
}

func (s *Server) availableSessionName(ctx context.Context, base string) (string, error) {
	sessions, err := s.store.ListSessions(ctx)
	if err != nil {
		return "", err
	}
	used := make(map[string]struct{}, len(sessions))
	for _, session := range sessions {
		used[session.Name] = struct{}{}
	}
	if _, exists := used[base]; !exists {
		return base, nil
	}
	for suffix := 2; ; suffix++ {
		candidate := fmt.Sprintf("%s %d", base, suffix)
		if _, exists := used[candidate]; !exists {
			return candidate, nil
		}
	}
}

func (s *Server) upstreamError(w http.ResponseWriter, action, code, message string, err error) {
	s.logger.Warn(action, "error", err)
	writeError(w, http.StatusBadGateway, code, message)
}

func publicSession(session store.Session) store.Session {
	session.BackendName = ""
	if session.Error != nil {
		message := "会话暂时不可用，请检查工作目录、命令或连接设置"
		if session.Status == store.SessionStatusReconnecting {
			message = "后台连接已中断，wmux 正在尝试恢复"
		}
		session.Error = &message
	}
	return session
}
