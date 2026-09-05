package api

import (
	"bytes"
	"net/http"
	"testing"
)

func TestSetupLoginAndProtectedHostCRUD(t *testing.T) {
	t.Parallel()
	server := newAPIFixture(t, apiOptions{skipSetup: true}).api

	status := performJSON(t, server.Handler(), http.MethodGet, "/api/status", nil, "")
	if status.Code != http.StatusOK || !bytes.Contains(status.Body.Bytes(), []byte(`"setupRequired":true`)) {
		t.Fatalf("unexpected initial status: %d %s", status.Code, status.Body.String())
	}

	setup := performJSON(t, server.Handler(), http.MethodPost, "/api/setup", map[string]any{
		"username": "owner",
		"password": "a-long-test-password",
	}, "")
	if setup.Code != http.StatusCreated {
		t.Fatalf("setup: %d %s", setup.Code, setup.Body.String())
	}
	cookie := setup.Result().Cookies()[0]

	unauthorized := performJSON(t, server.Handler(), http.MethodGet, "/api/hosts", nil, "")
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("expected unauthorized, got %d", unauthorized.Code)
	}

	host := performJSON(t, server.Handler(), http.MethodPost, "/api/hosts", map[string]any{
		"name":     "Lab",
		"address":  "192.0.2.10",
		"port":     22,
		"username": "dev",
		"authType": "password",
		"password": "server-secret",
	}, cookie.String())
	if host.Code != http.StatusCreated {
		t.Fatalf("create host: %d %s", host.Code, host.Body.String())
	}
	if bytes.Contains(host.Body.Bytes(), []byte("server-secret")) || !bytes.Contains(host.Body.Bytes(), []byte(`"hasSecret":true`)) {
		t.Fatalf("host response leaked or omitted credential state: %s", host.Body.String())
	}

	logout := performJSON(t, server.Handler(), http.MethodPost, "/api/logout", nil, cookie.String())
	if logout.Code != http.StatusNoContent {
		t.Fatalf("logout: %d %s", logout.Code, logout.Body.String())
	}
	login := performJSON(t, server.Handler(), http.MethodPost, "/api/login", map[string]any{
		"username": "owner",
		"password": "a-long-test-password",
	}, "")
	if login.Code != http.StatusOK || len(login.Result().Cookies()) == 0 {
		t.Fatalf("login: %d %s", login.Code, login.Body.String())
	}
	loginCookie := login.Result().Cookies()[0].String()
	passwordChange := performJSON(t, server.Handler(), http.MethodPost, "/api/me/password", map[string]any{
		"currentPassword": "a-long-test-password",
		"newPassword":     "a-different-long-password",
	}, loginCookie)
	if passwordChange.Code != http.StatusNoContent || len(passwordChange.Result().Cookies()) == 0 {
		t.Fatalf("change password: %d %s", passwordChange.Code, passwordChange.Body.String())
	}
	if oldSession := performJSON(t, server.Handler(), http.MethodGet, "/api/me", nil, loginCookie); oldSession.Code != http.StatusUnauthorized {
		t.Fatalf("old login survived password change: %d", oldSession.Code)
	}
	newCookie := passwordChange.Result().Cookies()[0].String()
	if newSession := performJSON(t, server.Handler(), http.MethodGet, "/api/me", nil, newCookie); newSession.Code != http.StatusOK {
		t.Fatalf("replacement login is invalid: %d %s", newSession.Code, newSession.Body.String())
	}
}

func TestRejectsCrossOriginMutation(t *testing.T) {
	t.Parallel()
	server := newAPIFixture(t, apiOptions{skipSetup: true}).api
	recorder := performJSONWithOrigin(t, server.Handler(), http.MethodPost, "/api/setup", map[string]any{
		"username": "owner",
		"password": "a-long-test-password",
	}, "", "https://attacker.example")
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("expected forbidden origin, got %d %s", recorder.Code, recorder.Body.String())
	}
}
