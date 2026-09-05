package api

import (
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"runtime/debug"
	"strings"
	"time"
)

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/ws/") {
			w.Header().Set("Cache-Control", "no-store")
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self' ws: wss:; font-src 'self' data:; frame-ancestors 'none'; base-uri 'self'; form-action 'self'")
		next.ServeHTTP(w, r)
	})
}

func recoverRequests(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if value := recover(); value != nil {
				logger.Error("request panic", "method", r.Method, "path", r.URL.Path, "panic", value, "stack", string(debug.Stack()))
				writeError(w, http.StatusInternalServerError, codeInternalError, "服务发生内部错误")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func requestLog(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		next.ServeHTTP(w, r)
		if r.URL.Path != "/api/health" {
			logger.Debug("request", "method", r.Method, "path", r.URL.Path, "duration", time.Since(started))
		}
	})
}

func originAllowed(r *http.Request, publicURL string, trustProxy bool) bool {
	if strings.EqualFold(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site")), "cross-site") {
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		// Non-browser clients commonly omit Origin and Sec-Fetch-Site. Modern
		// browsers do send Sec-Fetch-Site for cross-site requests, so reject the
		// explicit cross-site signal above without breaking curl/CLI access.
		return true
	}
	parsedOrigin, err := url.Parse(origin)
	if err != nil || parsedOrigin.Scheme == "" || parsedOrigin.Host == "" {
		return false
	}

	if publicURL != "" {
		parsedPublic, parseErr := url.Parse(publicURL)
		return parseErr == nil && strings.EqualFold(parsedOrigin.Scheme, parsedPublic.Scheme) && strings.EqualFold(parsedOrigin.Host, parsedPublic.Host)
	}

	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	} else if trustProxy {
		if forwarded := rightmostForwardedValue(r.Header.Get("X-Forwarded-Proto")); forwarded == "http" || forwarded == "https" {
			scheme = forwarded
		}
	}
	if !strings.EqualFold(parsedOrigin.Scheme, scheme) || !strings.EqualFold(parsedOrigin.Host, r.Host) {
		return false
	}

	// With no canonical public URL there is no DNS name allow-list. Only
	// accept literal IP/localhost Host values, which keeps the convenient LAN
	// default while preventing a rebinding domain from authorizing /api/setup
	// merely because Origin and Host contain the same attacker-controlled name.
	hostname := parsedOrigin.Hostname()
	return net.ParseIP(hostname) != nil || strings.EqualFold(hostname, "localhost")
}

func clientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		// A conventional trusted proxy appends the address it observed. The
		// left-most value is client-controlled when proxy_add_x_forwarded_for is
		// used, so walk from the trusted (right) edge instead.
		parts := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
		for index := len(parts) - 1; index >= 0; index-- {
			candidate := strings.Trim(strings.TrimSpace(parts[index]), "\"")
			if net.ParseIP(candidate) != nil {
				return candidate
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}

func rightmostForwardedValue(header string) string {
	parts := strings.Split(header, ",")
	for index := len(parts) - 1; index >= 0; index-- {
		if value := strings.ToLower(strings.TrimSpace(parts[index])); value != "" {
			return value
		}
	}
	return ""
}
