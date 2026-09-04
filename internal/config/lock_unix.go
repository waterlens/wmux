//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package config

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

func acquirePlatformLock(path string) (*DataDirLock, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open data directory lock: %w", err)
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("secure data directory lock: %w", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, ErrDataDirLocked
		}
		return nil, fmt.Errorf("lock data directory: %w", err)
	}
	return &DataDirLock{file: file, path: path}, nil
}

func unlockPlatformFile(file *os.File) error {
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_UN); err != nil {
		return fmt.Errorf("unlock data directory: %w", err)
	}
	return nil
}
