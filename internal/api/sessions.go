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
	// Naming and creation share one critical section so defaults stay distinct.
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
	// The row owns the session id and generation, so it exists before the runtime.
	created, err := s.store.CreateSession(r.Context(), model)
	if err != nil {
		s.handleStoreError(w, err, "终端会话不存在")
		return
	}
	spec, err := s.sessionSpecs.SessionSpec(r.Context(), created)
	if err != nil {
		s.discardSessionRow(r.Context(), id)
		s.internalError(w, "准备终端配置", err)
		return
	}
	if _, err := s.terminals.Create(r.Context(), spec); err != nil {
		s.discardSessionRow(r.Context(), id)
		s.upstreamError(w, "启动终端会话", "terminal_start_failed", "无法启动会话，请检查工作目录、命令或连接设置", err)
		return
	}
	created, err = s.store.GetSession(r.Context(), id)
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
	updated, err := s.store.GetSession(r.Context(), session.ID)
	if err != nil {
		s.handleStoreError(w, err, "终端会话不存在")
		return
	}
	writeJSON(w, http.StatusOK, publicSession(updated))
}

func (s *Server) deleteSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	defer s.sessionOps.lock(id)()
	if _, err := s.store.GetSession(r.Context(), id); err != nil {
		s.handleStoreError(w, err, "终端会话不存在")
		return
	}
	warning := ""
	if s.terminals != nil {
		// An unreachable host still drops the runtime, with a warning.
		if err := s.terminals.Terminate(r.Context(), id); err != nil && !errors.Is(err, terminal.ErrSessionNotFound) {
			s.logger.Warn("terminate session for deletion", "session", id, "error", err)
			if err := s.terminals.Discard(r.Context(), id); err != nil && !errors.Is(err, terminal.ErrSessionNotFound) {
				s.logger.Warn("discard session runtime", "session", id, "error", err)
			}
			warning = "无法连接主机，远端后台会话可能仍在运行"
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
	if warning != "" {
		writeJSON(w, http.StatusOK, map[string]string{"warning": warning})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) restartSession(w http.ResponseWriter, r *http.Request) {
	if s.terminals == nil {
		writeError(w, http.StatusServiceUnavailable, "terminal_unavailable", "终端服务不可用")
		return
	}
	id := r.PathValue("id")
	defer s.sessionOps.lock(id)()
	session, err := s.store.GetSession(r.Context(), id)
	if err != nil {
		s.handleStoreError(w, err, "终端会话不存在")
		return
	}
	if err := s.terminals.StopForRestart(r.Context(), id); err != nil && !errors.Is(err, terminal.ErrSessionNotFound) {
		s.upstreamError(w, "结束待重启会话", "terminal_stop_failed", "暂时无法重启后台会话，请稍后重试", err)
		return
	}
	// Opening the next generation makes callbacks from the stopped execution stale.
	generation, err := s.store.BeginSessionRestart(r.Context(), id)
	if err != nil {
		s.handleStoreError(w, err, "终端会话不存在")
		return
	}
	spec, err := s.sessionSpecs.SessionSpec(r.Context(), session)
	if err != nil {
		s.internalError(w, "准备重启配置", err)
		return
	}
	spec.Generation = generation
	if _, err := s.terminals.Create(r.Context(), spec); err != nil {
		message := "无法启动会话，请检查工作目录、命令或连接设置"
		_ = s.store.UpdateSessionRuntime(r.Context(), id, generation, store.SessionStatusError, "", "", &message)
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

// reconnectSession retries one runtime's backend connection immediately.
func (s *Server) reconnectSession(w http.ResponseWriter, r *http.Request) {
	if s.terminals == nil {
		writeError(w, http.StatusServiceUnavailable, "terminal_unavailable", "终端服务不可用")
		return
	}
	id := r.PathValue("id")
	defer s.sessionOps.lock(id)()
	if err := s.terminals.Reconnect(id); err != nil {
		if errors.Is(err, terminal.ErrSessionNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "终端会话不存在")
			return
		}
		s.internalError(w, "重试后台连接", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// discardSessionRow removes a session that never reached a running runtime.
func (s *Server) discardSessionRow(ctx context.Context, id string) {
	if err := s.store.DeleteSession(ctx, id); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.logger.Warn("remove unstarted session", "session", id, "error", err)
	}
	if s.transcripts != nil {
		if err := s.transcripts.Remove(id); err != nil {
			s.logger.Warn("remove terminal transcript", "session", id, "error", err)
		}
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
