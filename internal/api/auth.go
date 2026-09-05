package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/waterlens/wmux/internal/security"
	"github.com/waterlens/wmux/internal/store"
	"github.com/waterlens/wmux/internal/version"
)

const authCookieName = "wmux_session"

type authContextKey struct{}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	setup, err := s.store.IsSetup(r.Context())
	if err != nil {
		s.internalError(w, "read setup state", err)
		return
	}
	_, authenticated := s.authenticate(r)
	writeJSON(w, http.StatusOK, map[string]any{
		"setupRequired": !setup,
		"authenticated": authenticated,
		"version":       version.Version,
		"commit":        version.Commit,
	})
}

func (s *Server) setup(w http.ResponseWriter, r *http.Request) {
	var input setupInput
	if !decodeJSON(w, r, &input) {
		return
	}
	if err := input.normalize(); err != nil {
		writeError(w, http.StatusBadRequest, codeInvalidSetup, err.Error())
		return
	}
	hash, err := security.HashPassword(input.Password)
	if err != nil {
		s.internalError(w, "hash password", err)
		return
	}
	if err := s.store.Setup(r.Context(), input.Username, hash); err != nil {
		if errors.Is(err, store.ErrAlreadySetup) {
			writeError(w, http.StatusConflict, codeAlreadySetup, "wmux 已完成初始化")
			return
		}
		s.internalError(w, "create administrator", err)
		return
	}
	if err := s.issueLogin(r.Context(), w); err != nil {
		s.internalError(w, "create login session", err)
		return
	}
	user, err := s.store.GetUser(r.Context())
	if err != nil {
		s.internalError(w, "read administrator", err)
		return
	}
	writeJSON(w, http.StatusCreated, publicUser(user))
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	key := clientIP(r, s.config.TrustProxy)
	if !s.loginRate.allowed(key, time.Now()) {
		w.Header().Set("Retry-After", "300")
		writeError(w, http.StatusTooManyRequests, codeRateLimited, "登录尝试过多，请稍后再试")
		return
	}
	var input loginInput
	if !decodeJSON(w, r, &input) {
		return
	}
	input.Username = strings.TrimSpace(input.Username)
	user, err := s.store.GetUserByUsername(r.Context(), input.Username)
	if err != nil {
		s.loginRate.fail(key, time.Now())
		writeError(w, http.StatusUnauthorized, codeInvalidCredentials, "用户名或密码错误")
		return
	}
	valid, verifyErr := security.VerifyPassword(input.Password, user.PasswordHash)
	if verifyErr != nil {
		s.logger.Error("stored password hash is invalid", "error", verifyErr)
	}
	if !valid {
		s.loginRate.fail(key, time.Now())
		writeError(w, http.StatusUnauthorized, codeInvalidCredentials, "用户名或密码错误")
		return
	}
	if err := s.issueLogin(r.Context(), w); err != nil {
		s.internalError(w, "create login session", err)
		return
	}
	s.loginRate.clear(key)
	writeJSON(w, http.StatusOK, publicUser(user))
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(authCookieName); err == nil {
		_ = s.store.DeleteAuthSession(r.Context(), security.HashToken(cookie.Value))
	}
	s.clearCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	user, err := s.store.GetUser(r.Context())
	if err != nil {
		s.internalError(w, "read administrator", err)
		return
	}
	writeJSON(w, http.StatusOK, publicUser(user))
}

func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) {
	var input struct {
		CurrentPassword string `json:"currentPassword"`
		NewPassword     string `json:"newPassword"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	if len(input.NewPassword) < 10 || len(input.NewPassword) > 1024 {
		writeError(w, http.StatusBadRequest, codeInvalidPassword, "新密码需要包含 10 到 1024 个字符")
		return
	}
	user, err := s.store.GetUser(r.Context())
	if err != nil {
		s.internalError(w, "read administrator", err)
		return
	}
	valid, err := security.VerifyPassword(input.CurrentPassword, user.PasswordHash)
	if err != nil {
		s.logger.Error("stored password hash is invalid", "error", err)
	}
	if !valid {
		writeError(w, http.StatusUnauthorized, codeInvalidCredentials, "当前密码不正确")
		return
	}
	hash, err := security.HashPassword(input.NewPassword)
	if err != nil {
		s.internalError(w, "hash password", err)
		return
	}
	if err := s.store.UpdatePassword(r.Context(), hash); err != nil {
		s.internalError(w, "update password", err)
		return
	}
	if err := s.store.DeleteAllAuthSessions(r.Context()); err != nil {
		s.internalError(w, "revoke login sessions", err)
		return
	}
	if err := s.issueLogin(r.Context(), w); err != nil {
		s.internalError(w, "refresh login session", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth, ok := s.authenticate(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, codeUnauthorized, "请先登录")
			return
		}
		if time.Since(auth.LastSeenAt) > 15*time.Minute {
			_ = s.store.TouchAuthSession(r.Context(), auth.TokenHash)
		}
		next(w, r.WithContext(context.WithValue(r.Context(), authContextKey{}, auth)))
	}
}

func (s *Server) authenticate(r *http.Request) (store.AuthSession, bool) {
	cookie, err := r.Cookie(authCookieName)
	if err != nil || cookie.Value == "" {
		return store.AuthSession{}, false
	}
	auth, err := s.store.GetAuthSession(r.Context(), security.HashToken(cookie.Value))
	return auth, err == nil
}

func (s *Server) issueLogin(ctx context.Context, w http.ResponseWriter) error {
	token, err := security.GenerateToken()
	if err != nil {
		return err
	}
	if _, err := s.store.CreateAuthSession(ctx, security.HashToken(token), time.Now().Add(s.config.SessionTTL)); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     authCookieName,
		Value:    token,
		Path:     "/",
		MaxAge:   int(s.config.SessionTTL.Seconds()),
		Expires:  time.Now().Add(s.config.SessionTTL),
		HttpOnly: true,
		Secure:   s.config.CookieSecure,
		SameSite: http.SameSiteStrictMode,
	})
	return nil
}

func (s *Server) clearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     authCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		Expires:  time.Unix(1, 0),
		HttpOnly: true,
		Secure:   s.config.CookieSecure,
		SameSite: http.SameSiteStrictMode,
	})
}

func publicUser(user store.User) map[string]any {
	return map[string]any{"username": user.Username, "createdAt": user.CreatedAt}
}
