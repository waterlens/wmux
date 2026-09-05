// Package api exposes wmux's authenticated JSON and WebSocket API.
package api

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/waterlens/wmux/internal/app"
	"github.com/waterlens/wmux/internal/config"
	"github.com/waterlens/wmux/internal/sshconfig"
	"github.com/waterlens/wmux/internal/sshx"
	"github.com/waterlens/wmux/internal/store"
	"github.com/waterlens/wmux/internal/terminal"
	"github.com/waterlens/wmux/internal/transcript"
	"github.com/waterlens/wmux/internal/version"
	"github.com/waterlens/wmux/internal/webui"
)

// Server owns the HTTP adapters around durable storage and terminal runtime.
type Server struct {
	config        config.Config
	store         *store.Store
	masterKey     []byte
	terminals     *terminal.Manager
	transcripts   *transcript.Directory
	logger        *slog.Logger
	loginRate     *failureWindow
	mux           *http.ServeMux
	sessionNameMu sync.Mutex
	hostImportMu  sync.Mutex
	sessionOps    keyedMutex
	runtime       *app.RuntimeRepository
	sshConfig     sshConfigDiscoverer
	probeSSH      sshHostKeyProbe
}

type sshConfigDiscoverer interface {
	Discover(context.Context) (sshconfig.Result, error)
	Resolve(context.Context, string) (sshconfig.Candidate, error)
}

type sshHostKeyProbe func(context.Context, string, string) (string, string, error)

// New constructs the complete HTTP application. The terminal manager, the
// transcript directory and the runtime repository are invariants, not options:
// every session route needs all three, so the handlers never test them for nil.
func New(cfg config.Config, database *store.Store, masterKey []byte, terminals *terminal.Manager, transcripts *transcript.Directory, runtime *app.RuntimeRepository, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	s := &Server{
		config:      cfg,
		store:       database,
		masterKey:   append([]byte(nil), masterKey...),
		terminals:   terminals,
		transcripts: transcripts,
		logger:      logger,
		loginRate:   newFailureWindow(6, 5*time.Minute),
		mux:         http.NewServeMux(),
		runtime:     runtime,
		sshConfig:   sshconfig.New(cfg.SSHConfigPath),
		probeSSH:    sshx.Probe,
	}
	s.routes()
	return s
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /api/health", s.health)
	s.mux.HandleFunc("GET /api/status", s.status)
	s.mux.HandleFunc("POST /api/setup", s.sameOrigin(s.setup))
	s.mux.HandleFunc("POST /api/login", s.sameOrigin(s.login))
	s.mux.HandleFunc("POST /api/logout", s.sameOrigin(s.requireAuth(s.logout)))
	s.mux.HandleFunc("GET /api/me", s.requireAuth(s.me))
	s.mux.HandleFunc("POST /api/me/password", s.sameOrigin(s.requireAuth(s.changePassword)))

	s.mux.HandleFunc("GET /api/hosts", s.requireAuth(s.listHosts))
	s.mux.HandleFunc("GET /api/hosts/ssh-config", s.requireAuth(s.discoverSSHConfig))
	s.mux.HandleFunc("POST /api/hosts/import-ssh-config", s.sameOrigin(s.requireAuth(s.importSSHConfig)))
	s.mux.HandleFunc("POST /api/hosts", s.sameOrigin(s.requireAuth(s.createHost)))
	s.mux.HandleFunc("PATCH /api/hosts/{id}", s.sameOrigin(s.requireAuth(s.updateHost)))
	s.mux.HandleFunc("DELETE /api/hosts/{id}", s.sameOrigin(s.requireAuth(s.deleteHost)))
	s.mux.HandleFunc("POST /api/hosts/{id}/probe", s.sameOrigin(s.requireAuth(s.probeHost)))
	s.mux.HandleFunc("POST /api/hosts/{id}/trust", s.sameOrigin(s.requireAuth(s.trustHost)))
	s.mux.HandleFunc("POST /api/hosts/{id}/test", s.sameOrigin(s.requireAuth(s.testHost)))

	s.mux.HandleFunc("GET /api/sessions", s.requireAuth(s.listSessions))
	s.mux.HandleFunc("POST /api/sessions", s.sameOrigin(s.requireAuth(s.createSession)))
	s.mux.HandleFunc("PATCH /api/sessions/{id}", s.sameOrigin(s.requireAuth(s.updateSession)))
	s.mux.HandleFunc("DELETE /api/sessions/{id}", s.sameOrigin(s.requireAuth(s.deleteSession)))
	s.mux.HandleFunc("POST /api/sessions/{id}/restart", s.sameOrigin(s.requireAuth(s.restartSession)))
	s.mux.HandleFunc("POST /api/sessions/{id}/reconnect", s.sameOrigin(s.requireAuth(s.reconnectSession)))
	s.mux.HandleFunc("GET /ws/sessions/{id}", s.requireAuth(s.terminalSocket))

	s.mux.Handle("/", webui.Handler())
}

// Handler returns the hardened HTTP handler.
func (s *Server) Handler() http.Handler {
	return recoverRequests(s.logger, requestLog(s.logger, securityHeaders(s.mux)))
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Ping(r.Context()); err != nil {
		writeError(w, http.StatusServiceUnavailable, "unhealthy", "数据库不可用")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": version.Version, "commit": version.Commit})
}

func (s *Server) sameOrigin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !originAllowed(r, s.config.PublicURL, s.config.TrustProxy) {
			writeError(w, http.StatusForbidden, "invalid_origin", "请求来源不受信任")
			return
		}
		next(w, r)
	}
}
