package api

import (
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/waterlens/wmux/internal/security"
	"github.com/waterlens/wmux/internal/sshx"
	"github.com/waterlens/wmux/internal/store"
)

func (s *Server) listHosts(w http.ResponseWriter, r *http.Request) {
	hosts, err := s.store.ListHosts(r.Context())
	if err != nil {
		s.internalError(w, "列出 SSH 主机", err)
		return
	}
	result := make([]hostResponse, 0, len(hosts))
	for _, host := range hosts {
		result = append(result, publicHost(host))
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) createHost(w http.ResponseWriter, r *http.Request) {
	var input hostInput
	if !decodeJSON(w, r, &input) {
		return
	}
	if err := input.normalize(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_host", err.Error())
		return
	}
	credentials := credentialsFromInput(input)
	encrypted, err := s.encryptCredentials(credentials, input.AuthType)
	if err != nil {
		s.internalError(w, "加密 SSH 凭据", err)
		return
	}
	host, err := s.store.CreateHost(r.Context(), store.Host{
		Name:                 input.Name,
		Address:              input.Address,
		Port:                 input.Port,
		Username:             input.Username,
		AuthType:             input.AuthType,
		EncryptedCredentials: encrypted,
	})
	if err != nil {
		s.internalError(w, "创建 SSH 主机", err)
		return
	}
	writeJSON(w, http.StatusCreated, publicHost(host))
}

func (s *Server) updateHost(w http.ResponseWriter, r *http.Request) {
	host, err := s.store.GetHost(r.Context(), r.PathValue("id"))
	if err != nil {
		s.handleStoreError(w, err, "SSH 主机不存在")
		return
	}
	credentials, err := s.decryptCredentials(host)
	if err != nil {
		s.internalError(w, "解密 SSH 凭据", err)
		return
	}
	var patch hostPatch
	if !decodeJSON(w, r, &patch) {
		return
	}
	input := hostInput{
		Name:       host.Name,
		Address:    host.Address,
		Port:       host.Port,
		Username:   host.Username,
		AuthType:   host.AuthType,
		Password:   stringPointer(credentials.Password),
		PrivateKey: stringPointer(credentials.PrivateKey),
		Passphrase: stringPointer(credentials.Passphrase),
	}
	oldAuthType := input.AuthType
	if patch.Name != nil {
		input.Name = *patch.Name
	}
	if patch.Address != nil {
		input.Address = *patch.Address
	}
	if patch.Port != nil {
		input.Port = *patch.Port
	}
	if patch.Username != nil {
		input.Username = *patch.Username
	}
	if patch.AuthType != nil {
		input.AuthType = *patch.AuthType
	}
	if input.AuthType != oldAuthType {
		input.Password = nil
		input.PrivateKey = nil
		input.Passphrase = nil
	}
	if patch.Password != nil && *patch.Password != "" {
		input.Password = patch.Password
	}
	if patch.PrivateKey != nil && strings.TrimSpace(*patch.PrivateKey) != "" {
		input.PrivateKey = patch.PrivateKey
	}
	if patch.Passphrase != nil {
		input.Passphrase = patch.Passphrase
	}
	if err := input.normalize(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_host", err.Error())
		return
	}
	encrypted, err := s.encryptCredentials(credentialsFromInput(input), input.AuthType)
	if err != nil {
		s.internalError(w, "加密 SSH 凭据", err)
		return
	}
	connectionChanged := host.Address != input.Address || host.Port != input.Port
	host.Name = input.Name
	host.Address = input.Address
	host.Port = input.Port
	host.Username = input.Username
	host.AuthType = input.AuthType
	host.EncryptedCredentials = encrypted
	if connectionChanged {
		host.Fingerprint = ""
	}
	host, err = s.store.UpdateHost(r.Context(), host)
	if err != nil {
		s.handleStoreError(w, err, "SSH 主机不存在")
		return
	}
	if s.terminals != nil {
		s.terminals.RefreshHost(host.ID)
	}
	writeJSON(w, http.StatusOK, publicHost(host))
}

func (s *Server) deleteHost(w http.ResponseWriter, r *http.Request) {
	err := s.store.DeleteHost(r.Context(), r.PathValue("id"))
	if err != nil {
		if errors.Is(err, store.ErrInUse) {
			writeError(w, http.StatusConflict, "host_in_use", "仍有会话使用这台主机")
			return
		}
		s.handleStoreError(w, err, "SSH 主机不存在")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) probeHost(w http.ResponseWriter, r *http.Request) {
	host, err := s.store.GetHost(r.Context(), r.PathValue("id"))
	if err != nil {
		s.handleStoreError(w, err, "SSH 主机不存在")
		return
	}
	probe := s.probeSSH
	if probe == nil {
		probe = sshx.Probe
	}
	fingerprint, algorithm, err := probe(r.Context(), sshAddress(host.Address, host.Port), host.Username)
	if err != nil {
		s.upstreamError(w, "探测 SSH 主机指纹", "ssh_probe_failed", "无法读取 SSH 主机指纹，请检查地址和网络连接", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"fingerprint": fingerprint, "algorithm": algorithm})
}

func (s *Server) trustHost(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Fingerprint string `json:"fingerprint"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	host, err := s.store.GetHost(r.Context(), r.PathValue("id"))
	if err != nil {
		s.handleStoreError(w, err, "SSH 主机不存在")
		return
	}
	probe := s.probeSSH
	if probe == nil {
		probe = sshx.Probe
	}
	actual, _, err := probe(r.Context(), sshAddress(host.Address, host.Port), host.Username)
	if err != nil {
		s.upstreamError(w, "重新探测 SSH 主机指纹", "ssh_probe_failed", "无法再次读取 SSH 主机指纹，请检查地址和网络连接", err)
		return
	}
	if subtle.ConstantTimeCompare([]byte(actual), []byte(input.Fingerprint)) != 1 {
		writeError(w, http.StatusConflict, "fingerprint_changed", "SSH 主机密钥在确认期间发生变化")
		return
	}
	host.Fingerprint = actual
	if _, err := s.store.UpdateHost(r.Context(), host); err != nil {
		s.internalError(w, "保存 SSH 主机密钥", err)
		return
	}
	if s.terminals != nil {
		s.terminals.RefreshHost(host.ID)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) testHost(w http.ResponseWriter, r *http.Request) {
	host, err := s.store.GetHost(r.Context(), r.PathValue("id"))
	if err != nil {
		s.handleStoreError(w, err, "SSH 主机不存在")
		return
	}
	credentials, err := s.decryptCredentials(host)
	if err != nil {
		s.internalError(w, "解密 SSH 凭据", err)
		return
	}
	started := time.Now()
	err = sshx.Test(r.Context(), sshx.Target{
		Address:     sshAddress(host.Address, host.Port),
		Username:    host.Username,
		Fingerprint: host.Fingerprint,
		Credentials: sshx.Credentials{
			AuthType:   host.AuthType,
			Password:   credentials.Password,
			PrivateKey: credentials.PrivateKey,
			Passphrase: credentials.Passphrase,
		},
	})
	if err != nil {
		s.upstreamError(w, "测试 SSH 主机连接", "ssh_test_failed", "SSH 连接失败，请检查主机状态、指纹和认证信息", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "latencyMs": time.Since(started).Milliseconds()})
}

func (s *Server) encryptCredentials(credentials store.Credentials, authType string) ([]byte, error) {
	if authType == store.HostAuthAgent {
		return nil, nil
	}
	return security.EncryptJSON(s.masterKey, credentials)
}

func (s *Server) decryptCredentials(host store.Host) (store.Credentials, error) {
	var credentials store.Credentials
	if len(host.EncryptedCredentials) == 0 {
		return credentials, nil
	}
	if err := security.DecryptJSON(s.masterKey, host.EncryptedCredentials, &credentials); err != nil {
		return store.Credentials{}, err
	}
	return credentials, nil
}

func credentialsFromInput(input hostInput) store.Credentials {
	credentials := store.Credentials{}
	if input.Password != nil {
		credentials.Password = *input.Password
	}
	if input.PrivateKey != nil {
		credentials.PrivateKey = *input.PrivateKey
	}
	if input.Passphrase != nil {
		credentials.Passphrase = *input.Passphrase
	}
	return credentials
}

func stringPointer(value string) *string {
	return &value
}

func publicHost(host store.Host) hostResponse {
	return hostResponse{
		ID:          host.ID,
		Name:        host.Name,
		Address:     host.Address,
		Port:        host.Port,
		Username:    host.Username,
		AuthType:    host.AuthType,
		Fingerprint: host.Fingerprint,
		HasSecret:   len(host.EncryptedCredentials) != 0,
		CreatedAt:   host.CreatedAt,
		UpdatedAt:   host.UpdatedAt,
	}
}

func (s *Server) handleStoreError(w http.ResponseWriter, err error, notFoundMessage string) {
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", notFoundMessage)
		return
	}
	s.internalError(w, "数据库操作失败", err)
}
