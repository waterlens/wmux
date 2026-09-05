package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/waterlens/wmux/internal/api"
	"github.com/waterlens/wmux/internal/app"
	"github.com/waterlens/wmux/internal/config"
	"github.com/waterlens/wmux/internal/security"
	"github.com/waterlens/wmux/internal/store"
	"github.com/waterlens/wmux/internal/terminal"
	"github.com/waterlens/wmux/internal/transcript"
	"github.com/waterlens/wmux/internal/version"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.New(slog.NewTextHandler(os.Stderr, nil)).Error("invalid configuration", "error", err)
		os.Exit(2)
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))
	if err := run(cfg, logger); err != nil {
		logger.Error("wmux stopped", "error", err)
		os.Exit(1)
	}
}

func run(cfg config.Config, logger *slog.Logger) error {
	if err := cfg.Ensure(); err != nil {
		return err
	}
	dataLock, err := config.AcquireDataDirLock(cfg.DataDir)
	if err != nil {
		return err
	}
	defer dataLock.Close()
	runtime, err := openRuntime(cfg, logger)
	if err != nil {
		return err
	}
	defer runtime.close()
	return runtime.serve(cfg, logger)
}

// runtime is the long-lived state one wmux process owns: the database, the
// terminal manager and the HTTP handler built on top of them.
type runtime struct {
	database        *store.Store
	terminals       *terminal.Manager
	handler         http.Handler
	stopMaintenance context.CancelFunc
}

// openRuntime opens the database, restores the terminal sessions that survived
// the last stop and builds the HTTP handler. The caller owns the result and
// must call close, whether or not serve succeeds.
func openRuntime(cfg config.Config, logger *slog.Logger) (*runtime, error) {
	masterKey, err := security.LoadOrCreateMasterKey(cfg.MasterKeyPath)
	if err != nil {
		return nil, err
	}
	database, err := store.Open(context.Background(), cfg.DatabasePath)
	if err != nil {
		return nil, err
	}
	if _, err := database.PurgeExpiredAuthSessions(context.Background()); err != nil {
		logger.Warn("purge expired login sessions", "error", err)
	}
	recordings, err := transcript.NewDirectory(transcript.DirectoryConfig{
		Root: cfg.RecordingsDir,
		Limits: transcript.Limits{
			SegmentBytes: 1 << 20,
			MaxBytes:     16 << 20,
		},
	})
	if err != nil {
		database.Close()
		return nil, err
	}
	repository := &app.RuntimeRepository{Store: database, MasterKey: masterKey, Logger: logger}
	terminals, err := terminal.NewManager(terminal.Config{
		Repository:    repository,
		Callbacks:     repository,
		Transcripts:   recordings,
		ClientBuffer:  512,
		ReplayLimit:   8192,
		ReconnectMin:  500 * time.Millisecond,
		ReconnectMax:  15 * time.Second,
		MuxName:       dataMuxName(cfg.DataDir),
		MuxRuntimeDir: filepath.Join(cfg.DataDir, "mux"),
	})
	if err != nil {
		database.Close()
		return nil, err
	}
	if err := terminals.Restore(context.Background()); err != nil {
		logger.Warn("some terminal sessions could not be restored", "error", err)
	}
	maintenanceContext, stopMaintenance := context.WithCancel(context.Background())
	go purgeExpiredLogins(maintenanceContext, database, logger)
	return &runtime{
		database:        database,
		terminals:       terminals,
		handler:         api.New(cfg, database, masterKey, terminals, recordings, repository, logger).Handler(),
		stopMaintenance: stopMaintenance,
	}, nil
}

func (rt *runtime) close() {
	rt.terminals.Close()
	rt.stopMaintenance()
	rt.database.Close()
}

// serve accepts requests until a signal arrives, then stops the HTTP server
// before detaching the terminal sessions so no request outlives its runtime.
func (rt *runtime) serve(cfg config.Config, logger *slog.Logger) error {
	httpServer := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           rt.handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	serverErrors := make(chan error, 1)
	go func() {
		logger.Info("wmux is ready", "address", "http://"+cfg.ListenAddr, "version", version.Version, "commit", version.Commit)
		serverErrors <- httpServer.ListenAndServe()
	}()

	shutdownSignals := make(chan os.Signal, 1)
	signal.Notify(shutdownSignals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(shutdownSignals)
	select {
	case err := <-serverErrors:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case received := <-shutdownSignals:
		logger.Info("wmux received shutdown signal", "signal", received)
		// Restore the process defaults so a second signal can force an exit.
		signal.Stop(shutdownSignals)
	}

	logger.Info("wmux is shutting down")
	rt.stopMaintenance()
	var shutdownErrors []error
	httpContext, cancelHTTP := context.WithTimeout(context.Background(), 5*time.Second)
	if err := httpServer.Shutdown(httpContext); err != nil {
		shutdownErrors = append(shutdownErrors, fmt.Errorf("stop HTTP server: %w", err))
	}
	cancelHTTP()
	terminalContext, cancelTerminals := context.WithTimeout(context.Background(), 10*time.Second)
	if err := rt.terminals.CloseContext(terminalContext); err != nil {
		shutdownErrors = append(shutdownErrors, fmt.Errorf("detach terminal sessions: %w", err))
	}
	cancelTerminals()
	return errors.Join(shutdownErrors...)
}

func dataMuxName(dataDir string) string {
	digest := sha256.Sum256([]byte(filepath.Clean(dataDir)))
	return fmt.Sprintf("wmux-%x", digest[:4])
}

func purgeExpiredLogins(ctx context.Context, database *store.Store, logger *slog.Logger) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if count, err := database.PurgeExpiredAuthSessions(ctx); err != nil {
				logger.Warn("purge expired login sessions", "error", err)
			} else if count > 0 {
				logger.Debug("purged expired login sessions", "count", count)
			}
		}
	}
}
