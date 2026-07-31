package xai

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"sync"
)

var tokenFileProcessMu sync.Mutex

func withTokenFileLock(root *os.Root, name string, fn func() error) (err error) {
	tokenFileProcessMu.Lock()
	defer tokenFileProcessMu.Unlock()

	lockName := fmt.Sprintf(".xai-token-%x.lock", sha256.Sum256([]byte(name)))
	if info, errInspect := root.Lstat(lockName); errInspect == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("xai token storage: lock is not a regular file")
		}
	} else if !os.IsNotExist(errInspect) {
		return fmt.Errorf("xai token storage: inspect lock: %w", errInspect)
	}
	file, err := root.OpenFile(lockName, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("xai token storage: open lock: %w", err)
	}
	locked := false
	defer func() {
		if locked {
			err = errors.Join(err, unlockTokenFile(file))
		}
		err = errors.Join(err, file.Close())
	}()

	fileInfo, err := file.Stat()
	if err != nil {
		return fmt.Errorf("xai token storage: inspect lock file: %w", err)
	}
	pathInfo, err := root.Lstat(lockName)
	if err != nil {
		return fmt.Errorf("xai token storage: inspect lock path: %w", err)
	}
	if !pathInfo.Mode().IsRegular() || !os.SameFile(fileInfo, pathInfo) {
		return fmt.Errorf("xai token storage: lock is not a regular file")
	}
	if err = secureTokenFile(file, false); err != nil {
		return fmt.Errorf("xai token storage: secure lock: %w", err)
	}
	if err = lockTokenFile(file); err != nil {
		return fmt.Errorf("xai token storage: lock file: %w", err)
	}
	locked = true
	return fn()
}
