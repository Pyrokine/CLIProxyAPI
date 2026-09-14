package artifact

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

const testPNGBase64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+/p9sAAAAASUVORK5CYII="

func testPNGBytes(t *testing.T) []byte {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString(testPNGBase64)
	if err != nil {
		t.Fatalf("DecodeString test PNG: %v", err)
	}
	return data
}

func TestRootSeparatesConfigPaths(t *testing.T) {
	writable := t.TempDir()
	t.Setenv("WRITABLE_PATH", writable)

	first := Root(filepath.Join(t.TempDir(), "first.yaml"))
	second := Root(filepath.Join(t.TempDir(), "second.yaml"))
	if first == second {
		t.Fatalf("Root() returned shared path %q", first)
	}
	if !strings.HasPrefix(first, filepath.Join(writable, "artifacts", "instances")+string(filepath.Separator)) {
		t.Fatalf("Root() = %q, want instance namespace under WRITABLE_PATH", first)
	}
	if got, want := filepath.Base(first), "images"; got != want {
		t.Fatalf("Root() base = %q, want %q", got, want)
	}
}

func TestStoreCloseCleanerIsIrreversible(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	store.ApplyCacheConfig(CacheConfig{CleanupInterval: time.Hour})
	store.StartCleaner()
	store.CloseCleaner()
	store.StartCleaner()

	store.cleanerMu.Lock()
	defer store.cleanerMu.Unlock()
	if !store.cleanerClosed {
		t.Fatal("cleaner was not marked closed")
	}
	if store.cleanerStop != nil || store.cleanerDone != nil {
		t.Fatal("closed cleaner was restarted")
	}
}

func TestStoreInstancesCannotReadOrCleanPeerArtifacts(t *testing.T) {
	writable := t.TempDir()
	t.Setenv("WRITABLE_PATH", writable)
	storeA, errA := NewStore(Root(filepath.Join(t.TempDir(), "service-a.yaml")))
	if errA != nil {
		t.Fatalf("NewStore(service A): %v", errA)
	}
	storeB, errB := NewStore(Root(filepath.Join(t.TempDir(), "service-b.yaml")))
	if errB != nil {
		t.Fatalf("NewStore(service B): %v", errB)
	}
	recordA, errPutA := storeA.PutBytes(context.Background(), testPNGBytes(t), "image/png")
	if errPutA != nil {
		t.Fatalf("PutBytes(service A): %v", errPutA)
	}
	if file, _, errOpen := storeB.Open(recordA.ID); !errors.Is(errOpen, os.ErrNotExist) {
		if file != nil {
			_ = file.Close()
		}
		t.Fatalf("service B Open(service A artifact) error = %v", errOpen)
	}

	recordB, errPutB := storeB.PutBytes(
		context.Background(),
		append(testPNGBytes(t), byte(0)),
		"image/png",
	)
	if errPutB != nil {
		t.Fatalf("PutBytes(service B): %v", errPutB)
	}
	old := time.Now().Add(-48 * time.Hour)
	if errChtimes := os.Chtimes(filepath.Join(storeB.Root(), recordB.ID), old, old); errChtimes != nil {
		t.Fatalf("Chtimes(service B artifact): %v", errChtimes)
	}
	storeB.ApplyCacheConfig(CacheConfig{Retention: 24 * time.Hour})
	if _, errCleanup := storeB.Cleanup(context.Background()); errCleanup != nil {
		t.Fatalf("Cleanup(service B): %v", errCleanup)
	}
	if _, errStat := os.Stat(filepath.Join(storeA.Root(), recordA.ID)); errStat != nil {
		t.Fatalf("service A artifact changed by service B cleanup: %v", errStat)
	}
}

func TestStorePutBytesWritesContentAddressedImage(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	data := testPNGBytes(t)

	record, err := store.PutBytes(context.Background(), data, "image/png")
	if err != nil {
		t.Fatalf("PutBytes: %v", err)
	}
	if !strings.HasPrefix(record.ID, imageIDPrefix) || !strings.HasSuffix(record.ID, ".png") {
		t.Fatalf("record ID = %q, want img_<sha256>.png", record.ID)
	}
	if record.MIMEType != "image/png" {
		t.Fatalf("MIMEType = %q, want image/png", record.MIMEType)
	}
	if record.Size != int64(len(data)) {
		t.Fatalf("Size = %d, want %d", record.Size, len(data))
	}

	stored, err := os.ReadFile(filepath.Join(store.Root(), record.ID))
	if err != nil {
		t.Fatalf("ReadFile stored artifact: %v", err)
	}
	if !bytes.Equal(stored, data) {
		t.Fatal("stored artifact bytes differ from input")
	}
	rootInfo, err := os.Stat(store.Root())
	if err != nil {
		t.Fatalf("Stat root: %v", err)
	}
	if got := rootInfo.Mode().Perm(); got != 0o700 {
		t.Fatalf("root mode = %o, want 0700", got)
	}
	fileInfo, err := os.Stat(filepath.Join(store.Root(), record.ID))
	if err != nil {
		t.Fatalf("Stat artifact: %v", err)
	}
	if got := fileInfo.Mode().Perm(); got != 0o600 {
		t.Fatalf("artifact mode = %o, want 0600", got)
	}

	again, err := store.PutBytes(context.Background(), data, "image/png")
	if err != nil {
		t.Fatalf("PutBytes duplicate: %v", err)
	}
	if again.ID != record.ID || again.SHA256 != record.SHA256 {
		t.Fatalf("duplicate record = %#v, want same ID/SHA256 as %#v", again, record)
	}
}

func TestStorePutDataURLAndOctetStreamImage(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	record, err := store.PutDataURL(context.Background(), "data:image/png;base64,"+testPNGBase64)
	if err != nil {
		t.Fatalf("PutDataURL: %v", err)
	}
	if record.MIMEType != "image/png" {
		t.Fatalf("data URL MIMEType = %q, want image/png", record.MIMEType)
	}

	octetRecord, err := store.PutBytes(context.Background(), testPNGBytes(t), "application/octet-stream")
	if err != nil {
		t.Fatalf("PutBytes octet-stream sniffed image: %v", err)
	}
	if octetRecord.MIMEType != "image/png" {
		t.Fatalf("octet-stream MIMEType = %q, want sniffed image/png", octetRecord.MIMEType)
	}
}

func TestStoreRejectsInvalidMIMEAndOversizedData(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	if _, err = store.PutBytes(context.Background(), []byte("bad"), "image/svg+xml"); !errors.Is(
		err, errUnsupportedMIME,
	) {
		t.Fatalf("PutBytes svg err = %v, want ErrUnsupportedMIME", err)
	}

	limitedStore, err := NewStore(t.TempDir(), withMaxBytes(8))
	if err != nil {
		t.Fatalf("NewStore limited: %v", err)
	}
	if _, err = limitedStore.PutBytes(context.Background(), []byte("123456789"), "image/png"); !errors.Is(
		err, errTooLarge,
	) {
		t.Fatalf("PutBytes oversized err = %v, want ErrTooLarge", err)
	}
	oversizedDataURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("123456789"))
	if _, err = limitedStore.PutDataURL(context.Background(), oversizedDataURL); !errors.Is(err, errTooLarge) {
		t.Fatalf("PutDataURL oversized err = %v, want ErrTooLarge", err)
	}
}

func TestStoreValidateIDAndOpenRejectTraversal(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	for _, id := range []string{
		"", "../x.png", "dir/x.png", `dir\\x.png`, "img_nothex.png", "img_" + strings.Repeat("a", 64) + ".svg",
	} {
		if err = ValidateID(id); !errors.Is(err, errInvalidID) {
			t.Fatalf("ValidateID(%q) = %v, want ErrInvalidID", id, err)
		}
		if _, _, err = store.Open(id); !errors.Is(err, errInvalidID) {
			t.Fatalf("Open(%q) = %v, want ErrInvalidID", id, err)
		}
	}
}

func TestStoreOpenRejectsSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink no-follow test is Unix-only")
	}
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err = store.ensureRoot(); err != nil {
		t.Fatalf("ensureRoot: %v", err)
	}

	outside := filepath.Join(t.TempDir(), "outside.png")
	if err = os.WriteFile(outside, testPNGBytes(t), 0o600); err != nil {
		t.Fatalf("WriteFile outside: %v", err)
	}
	id := imageIDPrefix + strings.Repeat("a", 64) + ".png"
	if err = os.Symlink(outside, filepath.Join(store.Root(), id)); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	if file, _, err := store.Open(id); err == nil {
		_ = file.Close()
		t.Fatal("Open symlink succeeded, want error")
	}
}

func TestStoreTouchesArtifactOnDuplicatePutAndOpen(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	data := testPNGBytes(t)
	record, err := store.PutBytes(context.Background(), data, "image/png")
	if err != nil {
		t.Fatalf("PutBytes: %v", err)
	}
	oldTime := time.Now().Add(-2 * time.Hour)
	path := filepath.Join(store.Root(), record.ID)
	if err = os.Chtimes(path, oldTime, oldTime); err != nil {
		t.Fatalf("Chtimes before duplicate PutBytes: %v", err)
	}
	duplicate, err := store.PutBytes(context.Background(), data, "image/png")
	if err != nil {
		t.Fatalf("PutBytes duplicate: %v", err)
	}
	if !duplicate.ModTime.After(oldTime) {
		t.Fatalf("duplicate ModTime = %v, want after %v", duplicate.ModTime, oldTime)
	}
	if err = os.Chtimes(path, oldTime, oldTime); err != nil {
		t.Fatalf("Chtimes before Open: %v", err)
	}
	file, opened, err := store.Open(record.ID)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	_ = file.Close()
	if !opened.ModTime.After(oldTime) {
		t.Fatalf("opened ModTime = %v, want after %v", opened.ModTime, oldTime)
	}
}

func TestStorePutBytesEnforcesMaxTotalBytes(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	store.ApplyCacheConfig(CacheConfig{MaxTotalBytes: 900 * 1024})
	data1 := append(append([]byte(nil), testPNGBytes(t)...), bytes.Repeat([]byte{1}, 700*1024)...)
	data2 := append(append([]byte(nil), testPNGBytes(t)...), bytes.Repeat([]byte{2}, 700*1024)...)
	record1, err := store.PutBytes(context.Background(), data1, "image/png")
	if err != nil {
		t.Fatalf("PutBytes record1: %v", err)
	}
	oldTime := time.Now().Add(-2 * time.Hour)
	if err = os.Chtimes(filepath.Join(store.Root(), record1.ID), oldTime, oldTime); err != nil {
		t.Fatalf("Chtimes record1: %v", err)
	}
	record2, err := store.PutBytes(context.Background(), data2, "image/png")
	if err != nil {
		t.Fatalf("PutBytes record2: %v", err)
	}
	if _, err = os.Stat(filepath.Join(store.Root(), record1.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("record1 stat err = %v, want os.ErrNotExist", err)
	}
	if _, err = os.Stat(filepath.Join(store.Root(), record2.ID)); err != nil {
		t.Fatalf("record2 should remain: %v", err)
	}
	stats, err := store.Stats()
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.FileCount != 1 || stats.TotalBytes != record2.Size {
		t.Fatalf("Stats = %#v, want only record2 size %d", stats, record2.Size)
	}
}

func TestStoreRejectsArtifactLargerThanTotalCacheLimit(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	data := testPNGBytes(t)
	store.ApplyCacheConfig(CacheConfig{MaxTotalBytes: int64(len(data) - 1)})

	if _, err = store.PutBytes(context.Background(), data, "image/png"); !errors.Is(err, errTooLarge) {
		t.Fatalf("PutBytes error = %v, want ErrTooLarge", err)
	}
	stats, err := store.Stats()
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats != (StoreStats{}) {
		t.Fatalf("Stats = %#v, want empty", stats)
	}
}

func TestStoreConcurrentPutBytesKeepsOneProtectedArtifact(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	base := testPNGBytes(t)
	store.ApplyCacheConfig(CacheConfig{MaxTotalBytes: int64(len(base) + 3)})

	for round := range 100 {
		dataA := append(append([]byte(nil), base...), byte(round), byte(round>>8), 1)
		dataB := append(append([]byte(nil), base...), byte(round), byte(round>>8), 2)
		start := make(chan struct{})
		records := make(chan Record, 2)
		errorsCh := make(chan error, 2)
		var wg sync.WaitGroup
		for _, data := range [][]byte{dataA, dataB} {
			wg.Add(1)
			go func(payload []byte) {
				defer wg.Done()
				<-start
				record, errPut := store.PutBytes(context.Background(), payload, "image/png")
				if errPut != nil {
					errorsCh <- errPut
					return
				}
				records <- record
			}(data)
		}
		close(start)
		wg.Wait()
		close(records)
		close(errorsCh)
		for errPut := range errorsCh {
			t.Fatalf("round %d PutBytes: %v", round, errPut)
		}

		existing := 0
		for record := range records {
			if _, errStat := os.Stat(filepath.Join(store.Root(), record.ID)); errStat == nil {
				existing++
			} else if !errors.Is(errStat, os.ErrNotExist) {
				t.Fatalf("round %d stat %s: %v", round, record.ID, errStat)
			}
		}
		if existing != 1 {
			t.Fatalf("round %d existing returned artifacts = %d, want 1", round, existing)
		}
		stats, errStats := store.Stats()
		if errStats != nil {
			t.Fatalf("round %d Stats: %v", round, errStats)
		}
		if stats.FileCount != 1 {
			t.Fatalf("round %d FileCount = %d, want 1", round, stats.FileCount)
		}
	}
}

func TestStoresSharingRootSerializeConcurrentPutBytes(t *testing.T) {
	root := t.TempDir()
	storeA, err := NewStore(root)
	if err != nil {
		t.Fatalf("NewStore A: %v", err)
	}
	storeB, err := NewStore(root)
	if err != nil {
		t.Fatalf("NewStore B: %v", err)
	}
	assertStoresSerializeConcurrentPutBytes(t, root, storeA, storeB)
}

func TestStoresSharingRootThroughSymlinkUseSameMutex(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink root test is Unix-only")
	}
	root := t.TempDir()
	link := filepath.Join(t.TempDir(), "artifact-root")
	if err := os.Symlink(root, link); err != nil {
		t.Fatalf("symlink artifact root: %v", err)
	}
	storeA, err := NewStore(root)
	if err != nil {
		t.Fatalf("NewStore real root: %v", err)
	}
	storeB, err := NewStore(link)
	if err != nil {
		t.Fatalf("NewStore symlink root: %v", err)
	}
	if storeA.sharedFilesMu != storeB.sharedFilesMu {
		t.Fatal("real and symlink artifact roots use different mutexes")
	}
	assertStoresSerializeConcurrentPutBytes(t, root, storeA, storeB)
}

func TestStoresSharingRootThroughDanglingSymlinkUseSameMutex(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink root test is Unix-only")
	}
	base := t.TempDir()
	root := filepath.Join(base, "artifact-root")
	link := filepath.Join(t.TempDir(), "artifact-root-link")
	if err := os.Symlink(root, link); err != nil {
		t.Fatalf("symlink dangling artifact root: %v", err)
	}
	storeA, err := NewStore(link)
	if err != nil {
		t.Fatalf("NewStore dangling symlink root: %v", err)
	}
	if err = os.Mkdir(root, 0o700); err != nil {
		t.Fatalf("create symlink target: %v", err)
	}
	storeB, err := NewStore(root)
	if err != nil {
		t.Fatalf("NewStore real root: %v", err)
	}
	if storeA.sharedFilesMu != storeB.sharedFilesMu {
		t.Fatal("dangling symlink and later real artifact root use different mutexes")
	}
	assertStoresSerializeConcurrentPutBytes(t, root, storeA, storeB)
}

func assertStoresSerializeConcurrentPutBytes(t *testing.T, root string, storeA, storeB *Store) {
	t.Helper()
	base := testPNGBytes(t)
	cacheCfg := CacheConfig{MaxTotalBytes: int64(len(base) + 3)}
	storeA.ApplyCacheConfig(cacheCfg)
	storeB.ApplyCacheConfig(cacheCfg)

	for round := range 100 {
		dataA := append(append([]byte(nil), base...), byte(round), byte(round>>8), 1)
		dataB := append(append([]byte(nil), base...), byte(round), byte(round>>8), 2)
		start := make(chan struct{})
		records := make(chan Record, 2)
		errorsCh := make(chan error, 2)
		var wg sync.WaitGroup
		for index, store := range []*Store{storeA, storeB} {
			payload := dataA
			if index == 1 {
				payload = dataB
			}
			wg.Add(1)
			go func(target *Store, data []byte) {
				defer wg.Done()
				<-start
				record, errPut := target.PutBytes(context.Background(), data, "image/png")
				if errPut != nil {
					errorsCh <- errPut
					return
				}
				records <- record
			}(store, payload)
		}
		close(start)
		wg.Wait()
		close(records)
		close(errorsCh)
		for errPut := range errorsCh {
			t.Fatalf("round %d PutBytes: %v", round, errPut)
		}

		existing := 0
		for record := range records {
			if _, errStat := os.Stat(filepath.Join(root, record.ID)); errStat == nil {
				existing++
			} else if !errors.Is(errStat, os.ErrNotExist) {
				t.Fatalf("round %d stat %s: %v", round, record.ID, errStat)
			}
		}
		if existing != 1 {
			t.Fatalf("round %d existing returned artifacts = %d, want 1", round, existing)
		}
		stats, errStats := storeA.Stats()
		if errStats != nil {
			t.Fatalf("round %d Stats: %v", round, errStats)
		}
		if stats.FileCount != 1 {
			t.Fatalf("round %d FileCount = %d, want 1", round, stats.FileCount)
		}
	}
}

func TestStoreStatsAndCleanup(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	data1 := testPNGBytes(t)
	data2 := append(append([]byte(nil), data1...), byte(0))
	record1, err := store.PutBytes(context.Background(), data1, "image/png")
	if err != nil {
		t.Fatalf("PutBytes record1: %v", err)
	}
	record2, err := store.PutBytes(context.Background(), data2, "image/png")
	if err != nil {
		t.Fatalf("PutBytes record2: %v", err)
	}
	oldTime := time.Now().Add(-2 * time.Hour)
	newTime := time.Now()
	if err = os.Chtimes(filepath.Join(store.Root(), record1.ID), oldTime, oldTime); err != nil {
		t.Fatalf("Chtimes record1: %v", err)
	}
	if err = os.Chtimes(filepath.Join(store.Root(), record2.ID), newTime, newTime); err != nil {
		t.Fatalf("Chtimes record2: %v", err)
	}
	if err = os.WriteFile(filepath.Join(store.Root(), "ignored.txt"), []byte("ignored"), 0o600); err != nil {
		t.Fatalf("WriteFile ignored: %v", err)
	}
	oldTemp := filepath.Join(store.Root(), tempFilePrefix+"old")
	newTemp := filepath.Join(store.Root(), tempFilePrefix+"new")
	if err = os.WriteFile(oldTemp, []byte("old"), 0o600); err != nil {
		t.Fatalf("WriteFile old temp: %v", err)
	}
	if err = os.Chtimes(
		oldTemp, time.Now().Add(-defaultTempFileRetention-time.Hour),
		time.Now().Add(-defaultTempFileRetention-time.Hour),
	); err != nil {
		t.Fatalf("Chtimes old temp: %v", err)
	}
	if err = os.WriteFile(newTemp, []byte("new"), 0o600); err != nil {
		t.Fatalf("WriteFile new temp: %v", err)
	}

	stats, err := store.Stats()
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.FileCount != 2 || stats.TotalBytes != record1.Size+record2.Size {
		t.Fatalf("Stats = %#v, want two artifact files totaling %d", stats, record1.Size+record2.Size)
	}

	store.ApplyCacheConfig(CacheConfig{Retention: time.Hour, MaxTotalBytes: record2.Size})
	result, err := store.Cleanup(context.Background())
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if result.DeletedFiles != 1 || result.DeletedTempFiles != 1 {
		t.Fatalf("Cleanup result = %#v, want one artifact and one temp file deleted", result)
	}
	if _, err = os.Stat(filepath.Join(store.Root(), record1.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("record1 stat err = %v, want os.ErrNotExist", err)
	}
	if _, err = os.Stat(filepath.Join(store.Root(), record2.ID)); err != nil {
		t.Fatalf("record2 should remain: %v", err)
	}
	if _, err = os.Stat(oldTemp); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old temp stat err = %v, want os.ErrNotExist", err)
	}
	if _, err = os.Stat(newTemp); err != nil {
		t.Fatalf("new temp should remain: %v", err)
	}
	if _, err = os.Stat(filepath.Join(store.Root(), "ignored.txt")); err != nil {
		t.Fatalf("ignored file should remain: %v", err)
	}
}

func TestStoreRemoteHTTPClientRejectsHTTPProxy(t *testing.T) {
	store, err := NewStore(t.TempDir(), WithProxyURL("http://127.0.0.1:8080"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if _, err = store.remoteHTTPClient(); err == nil || !strings.Contains(err.Error(), "HTTP proxies") {
		t.Fatalf("remoteHTTPClient() error = %v", err)
	}
}

func TestStoreSetProxyURLUpdatesDownloadSnapshot(t *testing.T) {
	store, err := NewStore(t.TempDir(), WithProxyURL("socks5://127.0.0.1:1080"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	store.SetProxyURL("direct")
	if got := store.proxyURLSnapshot(); got != "direct" {
		t.Fatalf("proxyURLSnapshot() = %q", got)
	}
	if _, err = store.remoteHTTPClient(); err != nil {
		t.Fatalf("remoteHTTPClient() error = %v", err)
	}
}

func TestStorePutRemoteImageRejectsPrivateURL(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	if _, _, err = store.PutRemoteImage(context.Background(), "http://127.0.0.1/image.png"); err == nil {
		t.Fatal("PutRemoteImage private URL succeeded, want error")
	}
}
