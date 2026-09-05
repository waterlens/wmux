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
		s.internalError(w, "list sessions", err)
		return
	}
	result := make([]sessionResponse, 0, len(sessions))
	for _, session := range sessions {
		result = append(result, publicSession(session))
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) createSession(w http.ResponseWriter, r *http.Request) {
	var input sessionInput
	if !decodeJSON(w, r, &input) {
		return
	}
	if err := input.normalize(); err != nil {
		writeError(w, http.StatusBadRequest, codeInvalidSession, err.Error())
		return
	}
	id, err := store.NewID("ses")
	if err != nil {
		s.internalError(w, "generate session id", err)
		return
	}
	autoNamed := input.Name == ""
	model := store.Session{
		ID:          id,
		Name:        input.Name,
		Kind:        input.Kind,
		Cwd:         input.Cwd,
		Command:     input.Command,
		Persistence: input.Persistence,
		Status:      store.SessionStatusConnecting,
		Cols:        int(terminal.DefaultCols),
		Rows:        int(terminal.DefaultRows),
	}
	if input.Kind == store.SessionKindSSH {
		host, hostErr := s.store.GetHost(r.Context(), input.HostID)
		if hostErr != nil {
			s.handleStoreError(w, "read SSH host", hostErr, "SSH 主机不存在")
			return
		}
		if host.Fingerprint == "" {
			writeError(w, http.StatusConflict, codeHostUntrusted, "请先验证并信任 SSH 主机密钥")
			return
		}
		model.HostID = &host.ID
		if model.Name == "" {
			model.Name = host.Name
		}
	}
	if model.Name == "" {
		model.Name = "本机终端"
	}
	// The row owns the session id and generation, so it exists before the runtime.
	created, err := s.reserveSessionRow(r.Context(), model, autoNamed)
	if err != nil {
		s.handleStoreError(w, "reserve session row", err, "终端会话不存在")
		return
	}
	spec, err := s.runtime.SessionSpec(r.Context(), created)
	if err != nil {
		s.discardSessionRow(r.Context(), id)
		s.internalError(w, "build session spec", err)
		return
	}
	if err := s.terminals.Create(spec); err != nil {
		s.discardSessionRow(r.Context(), id)
		s.upstreamError(w, "start session runtime", codeTerminalStartFailed, "无法启动会话，请检查工作目录、命令或连接设置", err)
		return
	}
	created, err = s.store.GetSession(r.Context(), id)
	if err != nil {
		s.internalError(w, "read created session", err)
		return
	}
	writeJSON(w, http.StatusCreated, publicSession(created))
}

func (s *Server) updateSession(w http.ResponseWriter, r *http.Request) {
	session, err := s.store.GetSession(r.Context(), r.PathValue("id"))
	if err != nil {
		s.handleStoreError(w, "read session", err, "终端会话不存在")
		return
	}
	var patch sessionPatch
	if !decodeJSON(w, r, &patch) {
		return
	}
	if patch.Name != nil {
		name := strings.TrimSpace(*patch.Name)
		if name == "" || len(name) > 80 {
			writeError(w, http.StatusBadRequest, codeInvalidSession, "会话名称不能为空且不能超过 80 个字符")
			return
		}
		if _, err := s.store.UpdateSessionName(r.Context(), session.ID, name); err != nil {
			s.handleStoreError(w, "rename session", err, "终端会话不存在")
			return
		}
	}
	updated, err := s.store.GetSession(r.Context(), session.ID)
	if err != nil {
		s.handleStoreError(w, "read session", err, "终端会话不存在")
		return
	}
	writeJSON(w, http.StatusOK, publicSession(updated))
}

func (s *Server) deleteSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	defer s.sessionOps.lock(id)()
	if _, err := s.store.GetSession(r.Context(), id); err != nil {
		s.handleStoreError(w, "read session", err, "终端会话不存在")
		return
	}
	warning := ""
	// An unreachable host still drops the runtime, with a warning.
	if err := s.terminals.Terminate(r.Context(), id); err != nil && !errors.Is(err, terminal.ErrSessionNotFound) {
		s.logger.Warn("terminate session for deletion", "session", id, "error", err)
		if err := s.terminals.Discard(r.Context(), id); err != nil && !errors.Is(err, terminal.ErrSessionNotFound) {
			s.logger.Warn("discard session runtime", "session", id, "error", err)
		}
		warning = "无法连接主机，远端后台会话可能仍在运行"
	}
	if err := s.store.DeleteSession(r.Context(), id); err != nil {
		s.handleStoreError(w, "delete session", err, "终端会话不存在")
		return
	}
	if err := s.transcripts.Remove(id); err != nil {
		s.logger.Warn("remove terminal transcript", "session", id, "error", err)
	}
	if warning != "" {
		writeJSON(w, http.StatusOK, map[string]string{"warning": warning})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) restartSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	defer s.sessionOps.lock(id)()
	session, err := s.store.GetSession(r.Context(), id)
	if err != nil {
		s.handleStoreError(w, "read session", err, "终端会话不存在")
		return
	}
	if err := s.terminals.StopForRestart(r.Context(), id); err != nil && !errors.Is(err, terminal.ErrSessionNotFound) {
		s.upstreamError(w, "stop session for restart", codeTerminalStopFailed, "暂时无法重启后台会话，请稍后重试", err)
		return
	}
	// Opening the next generation makes callbacks from the stopped execution stale.
	generation, err := s.store.BeginSessionRestart(r.Context(), id)
	if err != nil {
		s.handleStoreError(w, "begin session restart", err, "终端会话不存在")
		return
	}
	spec, err := s.runtime.SessionSpec(r.Context(), session)
	if err != nil {
		s.internalError(w, "build restart spec", err)
		return
	}
	spec.Generation = generation
	if err := s.terminals.Create(spec); err != nil {
		message := "无法启动会话，请检查工作目录、命令或连接设置"
		_ = s.store.UpdateSessionRuntime(r.Context(), id, generation, store.SessionStatusError, "", &message)
		s.upstreamError(w, "restart session runtime", codeTerminalStartFailed, message, err)
		return
	}
	restarted, err := s.store.GetSession(r.Context(), id)
	if err != nil {
		s.internalError(w, "read restarted session", err)
		return
	}
	writeJSON(w, http.StatusOK, publicSession(restarted))
}

// reconnectSession retries one runtime's backend connection immediately.
func (s *Server) reconnectSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	defer s.sessionOps.lock(id)()
	if err := s.terminals.Reconnect(id); err != nil {
		if errors.Is(err, terminal.ErrSessionNotFound) {
			writeError(w, http.StatusNotFound, codeNotFound, "终端会话不存在")
			return
		}
		s.internalError(w, "retry backend connection", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// discardSessionRow removes a session that never reached a running runtime.
func (s *Server) discardSessionRow(ctx context.Context, id string) {
	if err := s.store.DeleteSession(ctx, id); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.logger.Warn("remove unstarted session", "session", id, "error", err)
	}
	if err := s.transcripts.Remove(id); err != nil {
		s.logger.Warn("remove terminal transcript", "session", id, "error", err)
	}
}

// reserveSessionRow resolves the generated default name and inserts the row in
// one critical section, so two concurrent creates cannot pick the same name.
// Building the spec, starting the runtime and writing the response all happen
// unlocked.
func (s *Server) reserveSessionRow(ctx context.Context, model store.Session, autoNamed bool) (store.Session, error) {
	s.sessionNameMu.Lock()
	defer s.sessionNameMu.Unlock()
	if autoNamed {
		name, err := s.availableSessionName(ctx, model.Name)
		if err != nil {
			return store.Session{}, err
		}
		model.Name = name
	}
	return s.store.CreateSession(ctx, model)
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

// publicSession maps a stored row onto the wire shape, replacing runtime
// diagnostics with a message that is safe to show.
func publicSession(session store.Session) sessionResponse {
	response := sessionResponse{
		ID:             session.ID,
		Name:           session.Name,
		Kind:           session.Kind,
		HostID:         session.HostID,
		HostName:       session.HostName,
		Cwd:            session.Cwd,
		Command:        session.Command,
		Persistence:    session.Persistence,
		Backend:        session.Backend,
		Status:         session.Status,
		Generation:     session.Generation,
		Cols:           session.Cols,
		Rows:           session.Rows,
		CreatedAt:      session.CreatedAt,
		UpdatedAt:      session.UpdatedAt,
		LastAttachedAt: session.LastAttachedAt,
	}
	if session.Error != nil {
		message := "会话暂时不可用，请检查工作目录、命令或连接设置"
		if session.Status == store.SessionStatusReconnecting {
			message = "后台连接已中断，wmux 正在尝试恢复"
		}
		response.Error = &message
	}
	return response
}
