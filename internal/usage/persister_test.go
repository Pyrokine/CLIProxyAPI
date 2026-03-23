package usage

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestAtomicWrite_FilePermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test-file.json")
	data := []byte(`{"test": true}`)

	if err := atomicWrite(path, data); err != nil {
		t.Fatalf("atomicWrite failed: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat failed: %v", err)
	}

	perm := info.Mode().Perm()
	if perm != 0o600 {
		t.Fatalf("expected file permissions 0600, got %o", perm)
	}

	// Verify content is correct.
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read failed: %v", err)
	}
	if string(content) != string(data) {
		t.Fatalf("content mismatch: got %q, want %q", string(content), string(data))
	}
}

func TestAtomicWrite_DirectoryPermissions(t *testing.T) {
	base := t.TempDir()
	subDir := filepath.Join(base, "sub", "dir")
	path := filepath.Join(subDir, "test.json")

	if err := atomicWrite(path, []byte("data")); err != nil {
		t.Fatalf("atomicWrite failed: %v", err)
	}

	info, err := os.Stat(subDir)
	if err != nil {
		t.Fatalf("stat dir failed: %v", err)
	}

	perm := info.Mode().Perm()
	if perm != 0o700 {
		t.Fatalf("expected directory permissions 0700, got %o", perm)
	}
}

func TestAtomicWrite_ConcurrentWrites(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "concurrent.json")
	var wg sync.WaitGroup

	goroutines := 20
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(idx int) {
			defer wg.Done()
			data := []byte(`{"index":` + string(rune('0'+idx%10)) + `}`)
			_ = atomicWrite(path, data)
		}(i)
	}
	wg.Wait()

	// File should exist and be valid (not corrupted).
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read failed after concurrent writes: %v", err)
	}
	if len(content) == 0 {
		t.Fatal("file should not be empty after concurrent writes")
	}
}
