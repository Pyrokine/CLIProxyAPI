//go:build windows

package pluginstore

import (
	"strings"
	"testing"
	"time"
)

func TestLockRootSerializesCaseVariantPaths(t *testing.T) {
	root := t.TempDir()
	first, errFirst := LockRoot(root)
	if errFirst != nil {
		t.Fatalf("LockRoot(first) error = %v", errFirst)
	}

	acquired := make(chan *RootLock, 1)
	errors := make(chan error, 1)
	go func() {
		second, errSecond := LockRoot(strings.ToUpper(root))
		if errSecond != nil {
			errors <- errSecond
			return
		}
		acquired <- second
	}()
	select {
	case second := <-acquired:
		second.Unlock()
		first.Unlock()
		t.Fatal("case-variant root acquired while the first lock was held")
	case errSecond := <-errors:
		first.Unlock()
		t.Fatalf("LockRoot(case variant) error = %v", errSecond)
	case <-time.After(20 * time.Millisecond):
	}

	first.Unlock()
	select {
	case second := <-acquired:
		second.Unlock()
	case errSecond := <-errors:
		t.Fatalf("LockRoot(case variant) error = %v", errSecond)
	case <-time.After(time.Second):
		t.Fatal("case-variant root did not acquire after unlock")
	}
}
