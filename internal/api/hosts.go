package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/waterlens/wmux/internal/app"
	"github.com/waterlens/wmux/internal/security"
	"github.com/waterlens/wmux/internal/sshx"
	"github.com/waterlens/wmux/internal/store"
)

func (s *Server) listHosts(w http.ResponseWriter, r *http.Request) {
	hosts, err := s.store.ListHosts(r.Context())
	if err != nil {
		s.internalError(w, "list SSH hosts", err)
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
		writeError(w, http.StatusBadRequest, codeInvalidHost, err.Error())
		return
	}
	credentials := credentialsFromInput(input)
	encrypted, err := s.encryptCredentials(credentials, input.AuthType)
	if err != nil {
		s.internalError(w, "encrypt SSH credentials", err)
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
		s.internalError(w, "create SSH host", err)
		return
	}
	writeJSON(w, http.StatusCreated, publicHost(host))
}

func (s *Server) updateHost(w http.ResponseWriter, r *http.Request) {
	host, err := s.store.GetHost(r.Context(), r.PathValue("id"))
	if err != nil {
		s.handleStoreError(w, "read SSH host", err, "SSH 主机不存在")
		return
	}
	stored, err := s.decryptCredentials(host)
	if err != nil {
		s.internalError(w, "decrypt SSH credentials", err)
		return
	}
	// Decoding over the stored values is the merge: encoding/json only assigns
	// the keys the request actually sent, so anything it omits keeps what is
	// already persisted. The three credentials stay nil so the merge below can
	// tell "absent" from "sent empty".
	input := hostInput{
		Name:     host.Name,
		Address:  host.Address,
		Port:     host.Port,
		Username: host.Username,
		AuthType: host.AuthType,
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	// Switching the authentication type discards the secrets that belonged to
	// the previous one; every other edit inherits what it did not send.
	if input.AuthType == host.AuthType {
		input.inheritCredentials(stored)
	}
	if err := input.normalize(); err != nil {
		writeError(w, http.StatusBadRequest, codeInvalidHost, err.Error())
		return
	}
	encrypted, err := s.encryptCredentials(credentialsFromInput(input), input.AuthType)
	if err != nil {
		s.internalError(w, "encrypt SSH credentials", err)
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
		s.handleStoreError(w, "update SSH host", err, "SSH 主机不存在")
		return
	}
	s.logger.Debug("retried host sessions after an edit", "host", host.ID, "sessions", s.terminals.RefreshHost(host.ID))
	writeJSON(w, http.StatusOK, publicHost(host))
}

func (s *Server) deleteHost(w http.ResponseWriter, r *http.Request) {
	err := s.store.DeleteHost(r.Context(), r.PathValue("id"))
	if err != nil {
		if errors.Is(err, store.ErrInUse) {
			writeError(w, http.StatusConflict, codeHostInUse, "仍有会话使用这台主机")
			return
		}
		s.handleStoreError(w, "delete SSH host", err, "SSH 主机不存在")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) probeHost(w http.ResponseWriter, r *http.Request) {
	host, err := s.store.GetHost(r.Context(), r.PathValue("id"))
	if err != nil {
		s.handleStoreError(w, "read SSH host", err, "SSH 主机不存在")
		return
	}
	fingerprint, algorithm, err := s.probeSSH(r.Context(), app.SSHAddress(host), host.Username)
	if err != nil {
		s.upstreamError(w, "probe SSH host key", codeSSHProbeFailed, "无法读取 SSH 主机指纹，请检查地址和网络连接", err)
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
		s.handleStoreError(w, "read SSH host", err, "SSH 主机不存在")
		return
	}
	actual, _, err := s.probeSSH(r.Context(), app.SSHAddress(host), host.Username)
	if err != nil {
		s.upstreamError(w, "re-probe SSH host key", codeSSHProbeFailed, "无法再次读取 SSH 主机指纹，请检查地址和网络连接", err)
		return
	}
	// The fingerprint is a public value the user just read off the screen, so a
	// plain comparison is enough.
	if actual != input.Fingerprint {
		writeError(w, http.StatusConflict, codeFingerprintChanged, "SSH 主机密钥在确认期间发生变化")
		return
	}
	// Only the fingerprint is written, so a concurrent host edit survives.
	if err := s.store.UpdateHostFingerprint(r.Context(), host.ID, actual); err != nil {
		s.handleStoreError(w, "trust SSH host key", err, "SSH 主机不存在")
		return
	}
	s.logger.Debug("retried host sessions after trusting a new key", "host", host.ID, "sessions", s.terminals.RefreshHost(host.ID))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) testHost(w http.ResponseWriter, r *http.Request) {
	host, err := s.store.GetHost(r.Context(), r.PathValue("id"))
	if err != nil {
		s.handleStoreError(w, "read SSH host", err, "SSH 主机不存在")
		return
	}
	credentials, err := s.decryptCredentials(host)
	if err != nil {
		s.internalError(w, "decrypt SSH credentials", err)
		return
	}
	credential, err := app.TerminalCredential(host.AuthType, credentials)
	if err != nil {
		s.internalError(w, "prepare SSH credential", err)
		return
	}
	started := time.Now()
	err = sshx.Test(r.Context(), sshx.Target{
		Address:     app.SSHAddress(host),
		Username:    host.Username,
		Fingerprint: host.Fingerprint,
		Credential:  credential,
	})
	if err != nil {
		s.upstreamError(w, "test SSH host connection", codeSSHTestFailed, "SSH 连接失败，请检查主机状态、指纹和认证信息", err)
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
