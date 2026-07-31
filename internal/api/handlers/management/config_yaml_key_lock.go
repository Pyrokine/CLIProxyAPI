package management

import (
	"errors"
	"fmt"
	"os"
	"sync"
)

var configYAMLKeyProcessMu sync.Mutex

func withConfigYAMLRedactionKeyLock(
	root *os.Root,
	keyName string,
	fn func() ([32]byte, error),
) (key [32]byte, err error) {
	configYAMLKeyProcessMu.Lock()
	defer configYAMLKeyProcessMu.Unlock()

	lockName := keyName + ".lock"
	if info, errInspect := root.Lstat(lockName); errInspect == nil {
		if !info.Mode().IsRegular() {
			return key, fmt.Errorf("config YAML redaction key lock is not a regular file")
		}
	} else if !os.IsNotExist(errInspect) {
		return key, fmt.Errorf("inspect config YAML redaction key lock: %w", errInspect)
	}
	file, err := root.OpenFile(lockName, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return key, fmt.Errorf("open config YAML redaction key lock: %w", err)
	}
	locked := false
	defer func() {
		if locked {
			err = errors.Join(err, unlockConfigYAMLRedactionKeyFile(file))
		}
		err = errors.Join(err, file.Close())
	}()

	fileInfo, err := file.Stat()
	if err != nil {
		return key, fmt.Errorf("inspect config YAML redaction key lock: %w", err)
	}
	pathInfo, err := root.Lstat(lockName)
	if err != nil {
		return key, fmt.Errorf("inspect config YAML redaction key lock path: %w", err)
	}
	if !pathInfo.Mode().IsRegular() || !os.SameFile(fileInfo, pathInfo) {
		return key, fmt.Errorf("config YAML redaction key lock is not a regular file")
	}
	if err = secureConfigYAMLRedactionKeyFile(file, false); err != nil {
		return key, fmt.Errorf("secure config YAML redaction key lock: %w", err)
	}
	if err = lockConfigYAMLRedactionKeyFile(file); err != nil {
		return key, fmt.Errorf("lock config YAML redaction key: %w", err)
	}
	locked = true
	return fn()
}
