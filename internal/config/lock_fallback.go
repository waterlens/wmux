//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd

package config

import (
	"errors"
	"fmt"
	"os"
)

// Platforms without flock use exclusive lock-file creation. The file is
// removed on orderly shutdown. This keeps builds portable; wmux's supported
// native terminal targets use the advisory-lock implementation above.
func acquirePlatformLock(path string) (*DataDirLock, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, ErrDataDirLocked
		}
		return nil, fmt.Errorf("create data directory lock: %w", err)
	}
	return &DataDirLock{file: file, path: path, removeOnClose: true}, nil
}

func unlockPlatformFile(*os.File) error { return nil }
