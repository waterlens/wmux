package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/waterlens/wmux/internal/store"
)

const maxJSONBody = 2 << 20

// Every error code this API can return. The browser switches on these strings,
// so keeping them in one block makes the backend half of the contract greppable
// and gives removals a checklist to work from.
const (
	codeAlreadySetup          = "already_setup"
	codeFingerprintChanged    = "fingerprint_changed"
	codeHostExists            = "host_exists"
	codeHostInUse             = "host_in_use"
	codeHostUntrusted         = "host_untrusted"
	codeInternalError         = "internal_error"
	codeInvalidCredentials    = "invalid_credentials"
	codeInvalidHost           = "invalid_host"
	codeInvalidInput          = "invalid_input"
	codeInvalidOrigin         = "invalid_origin"
	codeInvalidPassword       = "invalid_password"
	codeInvalidRequest        = "invalid_request"
	codeInvalidSequence       = "invalid_sequence"
	codeInvalidSession        = "invalid_session"
	codeInvalidSetup          = "invalid_setup"
	codeNotFound              = "not_found"
	codeRateLimited           = "rate_limited"
	codeSSHConfigHostNotFound = "ssh_config_host_not_found"
	codeSSHConfigInvalid      = "ssh_config_invalid"
	codeSSHConfigUnavailable  = "ssh_config_unavailable"
	codeSSHConfigUnsupported  = "ssh_config_unsupported"
	codeSSHProbeFailed        = "ssh_probe_failed"
	codeSSHTestFailed         = "ssh_test_failed"
	codeTerminalStartFailed   = "terminal_start_failed"
	codeTerminalStopFailed    = "terminal_stop_failed"
	codeUnauthorized          = "unauthorized"
	codeUnhealthy             = "unhealthy"
)

type errorBody struct {
	Error apiError `json:"error"`
}

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		// Headers are already committed. The request logger will still record the
		// disconnected response; there is no second valid HTTP response to send.
		return
	}
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorBody{Error: apiError{Code: code, Message: message}})
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, codeInvalidRequest, "请求内容格式不正确")
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, codeInvalidRequest, "请求只能包含一个 JSON 对象")
		return false
	}
	return true
}

// sameOrigin refuses a mutation whose Origin does not belong to this server.
func (s *Server) sameOrigin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !originAllowed(r, s.config.PublicURL, s.config.TrustProxy) {
			writeError(w, http.StatusForbidden, codeInvalidOrigin, "请求来源不受信任")
			return
		}
		next(w, r)
	}
}

// internalError reports a fault the caller cannot do anything about. The log
// message is fixed so it can be searched for; action names the step that
// failed, while the response keeps the generic message the browser shows.
func (s *Server) internalError(w http.ResponseWriter, action string, err error) {
	s.logger.Error("request failed", "action", action, "error", err)
	writeError(w, http.StatusInternalServerError, codeInternalError, "服务发生内部错误")
}

// upstreamError reports a failure that came from SSH, tmux or screen.
func (s *Server) upstreamError(w http.ResponseWriter, action, code, message string, err error) {
	s.logger.Warn("upstream request failed", "action", action, "error", err)
	writeError(w, http.StatusBadGateway, code, message)
}

// handleStoreError maps a storage failure onto the response it deserves. The
// action is only used for logging, and says which query failed.
func (s *Server) handleStoreError(w http.ResponseWriter, action string, err error, notFoundMessage string) {
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, codeNotFound, notFoundMessage)
		return
	}
	if errors.Is(err, store.ErrInvalidInput) {
		// The store rejected the value itself; that is the request's fault.
		writeError(w, http.StatusBadRequest, codeInvalidInput, "请求的数据不符合要求")
		return
	}
	s.internalError(w, action, err)
}
