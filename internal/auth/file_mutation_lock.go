package auth

import (
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

type fileMutationLock struct {
	mu   sync.Mutex
	refs int
}

var fileMutationLocks = struct {
	sync.Mutex
	byPath map[string]*fileMutationLock
}{byPath: make(map[string]*fileMutationLock)}

// LockFileMutation serializes process-local mutations and watcher reads of one auth file
func LockFileMutation(path string) func() {
	key := fileMutationLockKey(path)
	fileMutationLocks.Lock()
	lock := fileMutationLocks.byPath[key]
	if lock == nil {
		lock = &fileMutationLock{}
		fileMutationLocks.byPath[key] = lock
	}
	lock.refs++
	fileMutationLocks.Unlock()

	lock.mu.Lock()
	var once sync.Once
	return func() {
		once.Do(
			func() {
				lock.mu.Unlock()
				fileMutationLocks.Lock()
				lock.refs--
				if lock.refs == 0 && fileMutationLocks.byPath[key] == lock {
					delete(fileMutationLocks.byPath, key)
				}
				fileMutationLocks.Unlock()
			},
		)
	}
}

func fileMutationLockKey(path string) string {
	path = strings.TrimSpace(path)
	if absolute, errAbsolute := filepath.Abs(path); errAbsolute == nil {
		path = absolute
	}
	path = filepath.Clean(path)
	if runtime.GOOS == "windows" {
		path = strings.ToLower(path)
	}
	return path
}
