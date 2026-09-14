package artifact

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestStoreRejectsAndRebuildsCorruptContentAddressedFile(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	data := testPNGBytes(t)
	record, err := store.PutBytes(context.Background(), data, "image/png")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store.Root(), record.ID)
	if err = os.WriteFile(path, []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if file, _, errOpen := store.Open(record.ID); !errors.Is(errOpen, errIntegrity) {
		if file != nil {
			_ = file.Close()
		}
		t.Fatalf("Open() error = %v, want ErrIntegrity", errOpen)
	}

	rebuilt, err := store.PutBytes(context.Background(), data, "image/png")
	if err != nil {
		t.Fatal(err)
	}
	if rebuilt.ID != record.ID {
		t.Fatalf("rebuilt ID = %q, want %q", rebuilt.ID, record.ID)
	}
	stored, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stored, data) {
		t.Fatal("corrupt artifact was not rebuilt")
	}
}

func TestStoreQuotaCleanupRemovesCorruptUnrelatedArtifact(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	firstData := testPNGBytes(t)
	first, errPut := store.PutBytes(context.Background(), firstData, "image/png")
	if errPut != nil {
		t.Fatal(errPut)
	}
	firstPath := filepath.Join(store.Root(), first.ID)
	if errWrite := os.WriteFile(firstPath, []byte("corrupt"), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	secondData := append(append([]byte(nil), firstData...), 0)
	store.ApplyCacheConfig(CacheConfig{MaxTotalBytes: int64(len(secondData))})
	second, errPut := store.PutBytes(context.Background(), secondData, "image/png")
	if errPut != nil {
		t.Fatalf("PutBytes() with corrupt unrelated artifact: %v", errPut)
	}
	if _, errStat := os.Stat(firstPath); !errors.Is(errStat, os.ErrNotExist) {
		t.Fatalf("corrupt artifact remains after quota cleanup: %v", errStat)
	}
	if _, errStat := os.Stat(filepath.Join(store.Root(), second.ID)); errStat != nil {
		t.Fatalf("new artifact missing after quota cleanup: %v", errStat)
	}
}
