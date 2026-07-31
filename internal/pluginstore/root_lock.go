package pluginstore

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

var pluginRootLocks sync.Map

type pluginRootMutex struct {
	mu sync.Mutex
}

// RootLock serializes plugin artifact mutations for one resolved plugin root.
type RootLock struct {
	root   string
	mutex  *pluginRootMutex
	once   sync.Once
	locked bool
}

// LockRoot acquires the process-wide artifact mutation lock for root.
func LockRoot(root string) (*RootLock, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		root = "plugins"
	}
	absoluteRoot, errAbs := filepath.Abs(root)
	if errAbs != nil {
		return nil, fmt.Errorf("resolve plugin root: %w", errAbs)
	}
	if errMkdir := os.MkdirAll(absoluteRoot, 0o755); errMkdir != nil {
		return nil, fmt.Errorf("create plugin root: %w", errMkdir)
	}
	resolvedRoot, errResolve := filepath.EvalSymlinks(absoluteRoot)
	if errResolve != nil {
		return nil, fmt.Errorf("resolve plugin root links: %w", errResolve)
	}
	lockRoot := rootLockIdentity(resolvedRoot)
	value, _ := pluginRootLocks.LoadOrStore(lockRoot, &pluginRootMutex{})
	mutex, _ := value.(*pluginRootMutex)
	if mutex == nil {
		return nil, fmt.Errorf("plugin root lock unavailable: %s", resolvedRoot)
	}
	mutex.mu.Lock()
	return &RootLock{root: lockRoot, mutex: mutex, locked: true}, nil
}

func (l *RootLock) matches(root string) bool {
	if l == nil || !l.locked {
		return false
	}
	root = strings.TrimSpace(root)
	if root == "" {
		root = "plugins"
	}
	absoluteRoot, errAbs := filepath.Abs(root)
	if errAbs != nil {
		return false
	}
	resolvedRoot, errResolve := filepath.EvalSymlinks(absoluteRoot)
	if errResolve != nil {
		return false
	}
	return rootLockIdentity(resolvedRoot) == l.root
}

// Unlock releases the plugin root mutation lock.
func (l *RootLock) Unlock() {
	if l == nil {
		return
	}
	l.once.Do(func() {
		if l.locked && l.mutex != nil {
			l.locked = false
			l.mutex.mu.Unlock()
		}
	})
}
