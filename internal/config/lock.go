//go:build unix

package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

var ErrDataDirLocked = errors.New("config: data directory is already in use")

// DataDirLock is an advisory flock held for the lifetime of one wmux process.
// Closing it releases the lock; Close may be called repeatedly. It is not
// meant to be closed concurrently: the process takes exactly one lock and
// releases it from a single deferred call.
type DataDirLock struct {
	file *os.File
}

// AcquireDataDirLock takes a non-blocking, cross-process lock in dataDir. A
// second wmux process pointed at the same directory receives ErrDataDirLocked.
// The lock file contains the owning PID for operator diagnostics.
func AcquireDataDirLock(dataDir string) (*DataDirLock, error) {
	if strings.TrimSpace(dataDir) == "" {
		return nil, errors.New("config: data directory is empty")
	}
	if err := ensurePrivateDir(dataDir); err != nil {
		return nil, err
	}
	path := filepath.Join(dataDir, "wmux.lock")
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return nil, fmt.Errorf("config: lock path %q must be a regular file", path)
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("config: inspect data directory lock: %w", err)
	}

	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("config: open data directory lock: %w", err)
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("config: secure data directory lock: %w", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, ErrDataDirLocked
		}
		return nil, fmt.Errorf("config: lock data directory: %w", err)
	}
	lock := &DataDirLock{file: file}
	if err := lock.writeOwner(); err != nil {
		_ = lock.Close()
		return nil, err
	}
	return lock, nil
}

func (l *DataDirLock) writeOwner() error {
	if err := l.file.Truncate(0); err != nil {
		return fmt.Errorf("config: truncate data directory lock: %w", err)
	}
	if _, err := l.file.Seek(0, 0); err != nil {
		return fmt.Errorf("config: seek data directory lock: %w", err)
	}
	if _, err := fmt.Fprintf(l.file, "%d\n", os.Getpid()); err != nil {
		return fmt.Errorf("config: write data directory lock owner: %w", err)
	}
	if err := l.file.Sync(); err != nil {
		return fmt.Errorf("config: sync data directory lock owner: %w", err)
	}
	return nil
}

func (l *DataDirLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	file := l.file
	l.file = nil
	var unlockErr error
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_UN); err != nil {
		unlockErr = fmt.Errorf("config: unlock data directory: %w", err)
	}
	return errors.Join(unlockErr, file.Close())
}
