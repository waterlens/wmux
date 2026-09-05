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
	"strings"
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
	level, err := parseLogLevel(os.Getenv("WMUX_LOG_LEVEL"))
	if err != nil {
		slog.New(slog.NewTextHandler(os.Stderr, nil)).Error("invalid logging configuration", "error", err)
		os.Exit(2)
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	if err := run(logger); err != nil {
		logger.Error("wmux stopped", "error", err)
		os.Exit(1)
	}
}

func parseLogLevel(value string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, errors.New("WMUX_LOG_LEVEL must be debug, info, warn, or error")
	}
}

func run(logger *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := cfg.Ensure(); err != nil {
		return err
	}
	dataLock, err := config.AcquireDataDirLock(cfg.DataDir)
	if err != nil {
		return err
	}
	defer dataLock.Close()
	masterKey, err := security.LoadOrCreateMasterKey(cfg.MasterKeyPath)
	if err != nil {
		return err
	}
	database, err := store.Open(context.Background(), cfg.DatabasePath)
	if err != nil {
		return err
	}
	defer database.Close()
	if _, err := database.PurgeExpiredAuthSessions(context.Background()); err != nil {
		logger.Warn("purge expired login sessions", "error", err)
	}
	maintenanceContext, stopMaintenance := context.WithCancel(context.Background())
	defer stopMaintenance()
	go purgeExpiredLogins(maintenanceContext, database, logger)
	recordings, err := transcript.NewDirectory(transcript.DirectoryConfig{
		Root:         cfg.RecordingsDir,
		SegmentBytes: 1 << 20,
		MaxBytes:     16 << 20,
		SyncWrites:   false,
	})
	if err != nil {
		return err
	}
	runtimeRepository := &app.RuntimeRepository{Store: database, MasterKey: masterKey, Logger: logger}
	terminalManager, err := terminal.NewManager(terminal.Config{
		Repository:    runtimeRepository,
		Callbacks:     runtimeRepository,
		Transcripts:   recordings,
		ClientBuffer:  512,
		ReplayLimit:   8192,
		ReconnectMin:  500 * time.Millisecond,
		ReconnectMax:  15 * time.Second,
		MuxName:       dataMuxName(cfg.DataDir),
		MuxRuntimeDir: filepath.Join(cfg.DataDir, "mux"),
	})
	if err != nil {
		return err
	}
	defer terminalManager.Close()
	if err := terminalManager.Restore(context.Background()); err != nil {
		logger.Warn("some terminal sessions could not be restored", "error", err)
	}

	handler := api.New(cfg, database, masterKey, terminalManager, recordings, runtimeRepository, logger).Handler()
	httpServer := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           handler,
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
	stopMaintenance()
	var shutdownErrors []error
	httpContext, cancelHTTP := context.WithTimeout(context.Background(), 5*time.Second)
	if err := httpServer.Shutdown(httpContext); err != nil {
		shutdownErrors = append(shutdownErrors, fmt.Errorf("stop HTTP server: %w", err))
	}
	cancelHTTP()
	terminalContext, cancelTerminals := context.WithTimeout(context.Background(), 10*time.Second)
	if err := terminalManager.CloseContext(terminalContext); err != nil {
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
