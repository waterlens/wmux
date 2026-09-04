package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

var ErrDataDirLocked = errors.New("config: data directory is already in use")

// DataDirLock is an operating-system lock held for the lifetime of one wmux
// process. Closing it releases the lock; Close is safe to call repeatedly.
type DataDirLock struct {
	mu            sync.Mutex
	file          *os.File
	path          string
	removeOnClose bool
}

// AcquireDataDirLock takes a non-blocking, cross-process lock in dataDir. A
// second wmux process pointed at the same directory receives ErrDataDirLocked.
// The lock file contains the owning PID for operator diagnostics.
func AcquireDataDirLock(dataDir string) (*DataDirLock, error) {
	if strings.TrimSpace(dataDir) == "" {
		return nil, fmt.Errorf("data directory is empty")
	}
	if err := ensurePrivateDir(dataDir); err != nil {
		return nil, err
	}
	path := filepath.Join(dataDir, "wmux.lock")
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return nil, fmt.Errorf("lock path %q must be a regular file", path)
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("inspect data directory lock: %w", err)
	}

	lock, err := acquirePlatformLock(path)
	if err != nil {
		return nil, err
	}
	if err := lock.writeOwner(); err != nil {
		_ = lock.Close()
		return nil, err
	}
	return lock, nil
}

func (l *DataDirLock) writeOwner() error {
	if err := l.file.Truncate(0); err != nil {
		return fmt.Errorf("truncate data directory lock: %w", err)
	}
	if _, err := l.file.Seek(0, 0); err != nil {
		return fmt.Errorf("seek data directory lock: %w", err)
	}
	if _, err := fmt.Fprintf(l.file, "%d\n", os.Getpid()); err != nil {
		return fmt.Errorf("write data directory lock owner: %w", err)
	}
	if err := l.file.Sync(); err != nil {
		return fmt.Errorf("sync data directory lock owner: %w", err)
	}
	return nil
}

func (l *DataDirLock) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	file := l.file
	l.file = nil
	unlockErr := unlockPlatformFile(file)
	closeErr := file.Close()
	var removeErr error
	if l.removeOnClose {
		if err := os.Remove(l.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			removeErr = err
		}
	}
	return errors.Join(unlockErr, closeErr, removeErr)
}
