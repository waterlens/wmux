package api

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"strings"

	"github.com/waterlens/wmux/internal/sshconfig"
	"github.com/waterlens/wmux/internal/store"
)

type sshHostConnection struct {
	address  string
	port     int
	username string
}

func (s *Server) discoverSSHConfig(w http.ResponseWriter, r *http.Request) {
	result, err := s.sshConfig.Discover(r.Context())
	if err != nil {
		s.writeSSHConfigError(w, err)
		return
	}
	hosts, err := s.store.ListHosts(r.Context())
	if err != nil {
		s.internalError(w, "匹配 SSH 配置与已有主机", err)
		return
	}
	existing := make(map[sshHostConnection]string, len(hosts))
	for _, host := range hosts {
		key := normalizedSSHHostConnection(host.Address, host.Port, host.Username)
		if _, found := existing[key]; !found {
			existing[key] = host.ID
		}
	}
	candidates := make([]sshConfigCandidateResponse, 0, len(result.Candidates))
	for _, candidate := range result.Candidates {
		response := publicSSHConfigCandidate(candidate)
		response.ExistingHostID = existing[normalizedSSHHostConnection(candidate.Address, candidate.Port, candidate.Username)]
		candidates = append(candidates, response)
	}
	writeJSON(w, http.StatusOK, sshConfigResponse{
		Available:  result.Available,
		Source:     s.publicSSHConfigSource(),
		Candidates: candidates,
	})
}

func (s *Server) publicSSHConfigSource() string {
	if configured := strings.TrimSpace(s.config.SSHConfigPath); configured != "" {
		return configured
	}
	return "~/.ssh/config"
}

func (s *Server) importSSHConfig(w http.ResponseWriter, r *http.Request) {
	var input sshConfigImportInput
	if !decodeJSON(w, r, &input) {
		return
	}
	alias := strings.TrimSpace(input.Alias)
	if alias == "" {
		writeError(w, http.StatusBadRequest, codeInvalidRequest, "SSH config 别名不能为空")
		return
	}

	// Resolve again at the mutation boundary. Never trust fields returned by a
	// previous discovery request: ~/.ssh/config may have changed in between.
	candidate, err := s.sshConfig.Resolve(r.Context(), alias)
	if err != nil {
		if errors.Is(err, sshconfig.ErrAliasNotFound) {
			writeError(w, http.StatusNotFound, codeSSHConfigHostNotFound, "SSH config 中不存在这个主机别名")
			return
		}
		s.writeSSHConfigError(w, err)
		return
	}
	if len(candidate.Unsupported) != 0 {
		writeError(w, http.StatusUnprocessableEntity, codeSSHConfigUnsupported, "该 SSH 配置使用了暂不支持的连接指令")
		return
	}
	resolved := hostInput{
		Name:     alias,
		Address:  candidate.Address,
		Port:     candidate.Port,
		Username: candidate.Username,
		AuthType: store.HostAuthAgent,
	}
	if err := resolved.normalize(); err != nil {
		writeError(w, http.StatusUnprocessableEntity, codeSSHConfigInvalid, "SSH config 中的主机配置无效")
		return
	}

	host, exists, err := s.createImportedSSHHost(r.Context(), resolved)
	if err != nil {
		s.internalError(w, "导入 SSH config 主机", err)
		return
	}
	if exists {
		writeError(w, http.StatusConflict, codeHostExists, "相同地址、端口和用户名的 SSH 主机已存在")
		return
	}
	writeJSON(w, http.StatusCreated, publicHost(host))
}

func (s *Server) createImportedSSHHost(ctx context.Context, resolved hostInput) (store.Host, bool, error) {
	// SQLite does not impose a uniqueness constraint on connection tuples.
	// Serialize only the duplicate check and insert so a double-click or two
	// tabs cannot import the same endpoint twice. Response I/O happens unlocked.
	s.hostImportMu.Lock()
	defer s.hostImportMu.Unlock()
	hosts, err := s.store.ListHosts(ctx)
	if err != nil {
		return store.Host{}, false, err
	}
	resolvedKey := normalizedSSHHostConnection(resolved.Address, resolved.Port, resolved.Username)
	for _, host := range hosts {
		if normalizedSSHHostConnection(host.Address, host.Port, host.Username) == resolvedKey {
			return store.Host{}, true, nil
		}
	}
	host, err := s.store.CreateHost(ctx, store.Host{
		Name:        resolved.Name,
		Address:     resolved.Address,
		Port:        resolved.Port,
		Username:    resolved.Username,
		AuthType:    store.HostAuthAgent,
		Fingerprint: "",
		// Deliberately do not copy IdentityFile contents or any credential.
		// Import also does not probe the network; trust remains an explicit step.
		EncryptedCredentials: nil,
	})
	if err != nil {
		return store.Host{}, false, err
	}
	return host, false, nil
}

func normalizedSSHHostConnection(address string, port int, username string) sshHostConnection {
	if port == 0 {
		port = 22
	}
	return sshHostConnection{
		address:  strings.ToLower(strings.TrimSpace(address)),
		port:     port,
		username: strings.TrimSpace(username),
	}
}

func publicSSHConfigCandidate(candidate sshconfig.Candidate) sshConfigCandidateResponse {
	unsupported := append([]string{}, candidate.Unsupported...)
	return sshConfigCandidateResponse{
		Alias:           candidate.Alias,
		Address:         candidate.Address,
		Port:            candidate.Port,
		Username:        candidate.Username,
		HasIdentityFile: candidate.HasIdentityFile,
		Unsupported:     unsupported,
	}
}

func (s *Server) writeSSHConfigError(w http.ResponseWriter, err error) {
	if sshConfigUnavailable(err) {
		// Do not include the path, parsed line, command, or file contents in the
		// response or structured log. The public error is intentionally stable.
		s.logger.Warn("SSH config is unavailable")
		writeError(w, http.StatusServiceUnavailable, codeSSHConfigUnavailable, "无法读取 SSH config")
		return
	}
	s.logger.Warn("SSH config is invalid")
	writeError(w, http.StatusUnprocessableEntity, codeSSHConfigInvalid, "SSH config 格式无效")
}

func sshConfigUnavailable(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) {
		return true
	}
	var pathError *fs.PathError
	return errors.As(err, &pathError)
}
