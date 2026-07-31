package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func newObjectStoreForTest(t *testing.T, handler http.Handler) *ObjectTokenStore {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	store, err := NewObjectTokenStore(ObjectStoreConfig{
		Endpoint:  strings.TrimPrefix(server.URL, "http://"),
		Bucket:    "test-bucket",
		AccessKey: "access-key",
		SecretKey: "secret-key",
		LocalRoot: t.TempDir(),
		PathStyle: true,
	})
	if err != nil {
		t.Fatalf("NewObjectTokenStore() error = %v", err)
	}
	return store
}

func writeObjectStoreLocation(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet || !r.URL.Query().Has("location") {
		return false
	}
	w.Header().Set("Content-Type", "application/xml")
	_, _ = io.WriteString(
		w,
		`<LocationConstraint xmlns="http://s3.amazonaws.com/doc/2006-03-01/"></LocationConstraint>`,
	)
	return true
}

func successfulObjectPut(w http.ResponseWriter, r *http.Request) {
	if writeObjectStoreLocation(w, r) {
		return
	}
	if r.Method != http.MethodPut {
		http.Error(w, "unexpected method "+r.Method+" "+r.URL.String(), http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("ETag", `"test-etag"`)
	w.WriteHeader(http.StatusOK)
}

func TestObjectTokenStorePersistsDisabledCredentialWithoutExistingFile(t *testing.T) {
	store := newObjectStoreForTest(t, http.HandlerFunc(successfulObjectPut))
	auth := &cliproxyauth.Auth{
		ID:       "disabled.json",
		FileName: "disabled.json",
		Disabled: true,
		Metadata: map[string]any{"type": "codex"},
	}
	path, errSave := store.Save(context.Background(), auth)
	if errSave != nil {
		t.Fatalf("Save() error = %v", errSave)
	}
	if path == "" {
		t.Fatal("Save() returned an empty path for disabled credential")
	}
	raw, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatalf("read disabled credential: %v", errRead)
	}
	var metadata map[string]any
	if errDecode := json.Unmarshal(raw, &metadata); errDecode != nil {
		t.Fatalf("decode disabled credential: %v", errDecode)
	}
	if disabled, _ := metadata["disabled"].(bool); !disabled {
		t.Fatalf("persisted disabled value = %#v, want true", metadata["disabled"])
	}
	listed, errList := store.List(context.Background())
	if errList != nil {
		t.Fatalf("List() error = %v", errList)
	}
	if len(listed) != 1 || listed[0] == nil || !listed[0].Disabled || listed[0].Status != cliproxyauth.StatusDisabled {
		t.Fatalf("disabled credential round trip = %#v", listed)
	}
}

func TestObjectTokenStoreRemoteFailureRestoresNewLocalFile(t *testing.T) {
	store := newObjectStoreForTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if writeObjectStoreLocation(w, r) {
			return
		}
		http.Error(w, "remote failure", http.StatusInternalServerError)
	}))
	auth := &cliproxyauth.Auth{
		ID:       "rollback.json",
		FileName: "rollback.json",
		Metadata: map[string]any{"type": "xai"},
	}
	path := store.AuthDir() + string(os.PathSeparator) + auth.FileName
	if _, errSave := store.Save(context.Background(), auth); !errors.Is(
		errSave,
		cliproxyauth.ErrAuthDurableStateUncertain,
	) {
		t.Fatalf("Save() error = %v, want durable state uncertainty", errSave)
	}
	if _, errStat := os.Stat(path); !os.IsNotExist(errStat) {
		t.Fatalf("failed remote save left local credential: %v", errStat)
	}
}

func TestObjectTokenStoreDeleteRemoteFailureRestoresLocalFile(t *testing.T) {
	var failDelete atomic.Bool
	store := newObjectStoreForTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && failDelete.Load() {
			http.Error(w, "remote failure", http.StatusInternalServerError)
			return
		}
		successfulObjectPut(w, r)
	}))
	auth := &cliproxyauth.Auth{
		ID:       "delete-rollback.json",
		FileName: "delete-rollback.json",
		Metadata: map[string]any{"type": "xai"},
	}
	path, errSave := store.Save(context.Background(), auth)
	if errSave != nil {
		t.Fatalf("Save() error = %v", errSave)
	}
	before, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatalf("read local credential: %v", errRead)
	}
	failDelete.Store(true)
	if errDelete := store.Delete(context.Background(), path); !errors.Is(
		errDelete,
		cliproxyauth.ErrAuthDurableStateUncertain,
	) {
		t.Fatalf("Delete() error = %v, want durable state uncertainty", errDelete)
	}
	after, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatalf("read restored credential: %v", errRead)
	}
	if string(after) != string(before) {
		t.Fatalf("restored credential = %s, want %s", after, before)
	}
}

func TestObjectTokenStoreSameLocalContentStillChecksRemote(t *testing.T) {
	var calls atomic.Int32
	store := newObjectStoreForTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if writeObjectStoreLocation(w, r) {
			return
		}
		if calls.Add(1) == 1 {
			successfulObjectPut(w, r)
			return
		}
		http.Error(w, "remote failure", http.StatusInternalServerError)
	}))
	auth := &cliproxyauth.Auth{
		ID:       "retry.json",
		FileName: "retry.json",
		Metadata: map[string]any{"type": "xai"},
	}
	path, errSave := store.Save(context.Background(), auth)
	if errSave != nil {
		t.Fatalf("initial Save() error = %v", errSave)
	}
	before, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatalf("read initial local credential: %v", errRead)
	}
	if _, errSave = store.Save(context.Background(), auth.Clone()); !errors.Is(
		errSave,
		cliproxyauth.ErrAuthDurableStateUncertain,
	) {
		t.Fatalf("repeated Save() error = %v, want durable state uncertainty", errSave)
	}
	if calls.Load() < 2 {
		t.Fatalf("remote PUT calls = %d, want at least 2", calls.Load())
	}
	after, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatalf("read local credential after failed retry: %v", errRead)
	}
	if string(after) != string(before) {
		t.Fatalf("local credential changed after failed retry: %s, want %s", after, before)
	}
}

func TestObjectTokenStoreDeleteIfVersionPublishesConditionalTombstone(t *testing.T) {
	authRaw := []byte(`{"type":"codex","token":"old"}`)
	var tombstoneRaw []byte
	var path string
	var deletionStarted atomic.Bool
	var localMissingAtRemote atomic.Bool
	store := newObjectStoreForTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if deletionStarted.Load() {
			_, errStat := os.Stat(path)
			localMissingAtRemote.Store(errors.Is(errStat, os.ErrNotExist))
		}
		if writeObjectStoreLocation(w, r) {
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Query().Has("versioning"):
			w.Header().Set("Content-Type", "application/xml")
			_, _ = io.WriteString(
				w,
				`<VersioningConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Status>Enabled</Status></VersioningConfiguration>`,
			)
		case r.Method == http.MethodHead:
			w.Header().Set("ETag", `"credential-etag"`)
			w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
			w.Header().Set("X-Amz-Version-Id", "version-1")
			w.Header().Set("Content-Length", fmt.Sprint(len(authRaw)))
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && r.URL.Query().Get("versionId") == "version-1":
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("ETag", `"credential-etag"`)
			w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
			w.Header().Set("X-Amz-Version-Id", "version-1")
			w.Header().Set("Content-Length", fmt.Sprint(len(authRaw)))
			_, _ = w.Write(authRaw)
		case r.Method == http.MethodPut:
			if got := r.Header.Get("If-Match"); got != `"credential-etag"` {
				http.Error(w, "missing conditional ETag", http.StatusPreconditionFailed)
				return
			}
			tombstoneRaw, _ = io.ReadAll(r.Body)
			w.Header().Set("ETag", `"tombstone-etag"`)
			w.Header().Set("X-Amz-Version-Id", "version-2")
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodDelete && r.URL.Query().Get("versionId") == "version-1":
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected request", http.StatusBadRequest)
		}
	}))
	path = filepath.Join(store.AuthDir(), "versioned.json")
	if errWrite := os.WriteFile(path, authRaw, 0o600); errWrite != nil {
		t.Fatalf("write mirrored auth: %v", errWrite)
	}
	version, errVersion := cliproxyauth.ReadPersistedVersion(path)
	if errVersion != nil {
		t.Fatalf("ReadPersistedVersion() error = %v", errVersion)
	}
	deletionStarted.Store(true)
	deleted, errDelete := store.DeleteIfVersion(context.Background(), path, version)
	if errDelete != nil || !deleted {
		t.Fatalf("DeleteIfVersion() = (%t, %v), want deleted", deleted, errDelete)
	}
	tombstoneStart := bytes.IndexByte(tombstoneRaw, '{')
	tombstoneEnd := bytes.LastIndexByte(tombstoneRaw, '}')
	if tombstoneStart < 0 || tombstoneEnd < tombstoneStart {
		t.Fatalf("conditional tombstone request body is not JSON: %q", tombstoneRaw)
	}
	tombstone, ok := parseObjectStoreAuthTombstone(tombstoneRaw[tombstoneStart : tombstoneEnd+1])
	if !ok || tombstone.DeletedVersionID != "version-1" || tombstone.DeletedSHA256 != fmt.Sprintf("%x", version.SHA256) {
		t.Fatalf("published tombstone = %#v, raw=%s", tombstone, tombstoneRaw)
	}
	if !localMissingAtRemote.Load() {
		t.Fatal("remote mutation started before the expected local version was captured")
	}
	if _, errStat := os.Stat(path); !os.IsNotExist(errStat) {
		t.Fatalf("mirrored credential remains after deletion: %v", errStat)
	}
}

func TestObjectTokenStoreRecoversInterruptedVersionedDeletionBeforeRemoteSync(t *testing.T) {
	authRaw := []byte(`{"type":"codex","token":"old"}`)
	var remoteDeletes atomic.Int32
	store := newObjectStoreForTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if writeObjectStoreLocation(w, r) {
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Query().Has("versioning"):
			w.Header().Set("Content-Type", "application/xml")
			_, _ = io.WriteString(
				w,
				`<VersioningConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Status>Enabled</Status></VersioningConfiguration>`,
			)
		case r.Method == http.MethodHead:
			w.Header().Set("ETag", `"credential-etag"`)
			w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
			w.Header().Set("X-Amz-Version-Id", "version-1")
			w.Header().Set("Content-Length", fmt.Sprint(len(authRaw)))
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && r.URL.Query().Get("versionId") == "version-1":
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("ETag", `"credential-etag"`)
			w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
			w.Header().Set("X-Amz-Version-Id", "version-1")
			w.Header().Set("Content-Length", fmt.Sprint(len(authRaw)))
			_, _ = w.Write(authRaw)
		case r.Method == http.MethodPut:
			w.Header().Set("ETag", `"tombstone-etag"`)
			w.Header().Set("X-Amz-Version-Id", "version-2")
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodDelete && r.URL.Query().Get("versionId") == "version-1":
			remoteDeletes.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected request", http.StatusBadRequest)
		}
	}))
	path := filepath.Join(store.AuthDir(), "interrupted.json")
	if errWrite := os.WriteFile(path, authRaw, 0o600); errWrite != nil {
		t.Fatalf("write mirrored auth: %v", errWrite)
	}
	version, errVersion := cliproxyauth.ReadPersistedVersion(path)
	if errVersion != nil {
		t.Fatalf("ReadPersistedVersion() error = %v", errVersion)
	}
	if _, matched, errPrepare := cliproxyauth.PrepareDurablePersistedFileDeletion(path, version); errPrepare != nil || !matched {
		t.Fatalf("PrepareDurablePersistedFileDeletion() matched = %v, error = %v", matched, errPrepare)
	}

	if errRecover := store.recoverAuthDeletions(context.Background()); errRecover != nil {
		t.Fatalf("recoverAuthDeletions() error = %v", errRecover)
	}
	if remoteDeletes.Load() != 1 {
		t.Fatalf("remote deletes = %d, want 1", remoteDeletes.Load())
	}
	if _, errStat := os.Stat(path); !errors.Is(errStat, os.ErrNotExist) {
		t.Fatalf("interrupted credential was restored locally: %v", errStat)
	}
	pending, errPending := cliproxyauth.RecoverDurablePersistedFileDeletions(store.AuthDir())
	if errPending != nil || len(pending) != 0 {
		t.Fatalf("pending deletions after recovery = %#v, %v", pending, errPending)
	}
}

func TestObjectTokenStoreDeleteIfVersionMismatchHasNoRemoteSideEffect(t *testing.T) {
	var calls atomic.Int32
	store := newObjectStoreForTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if writeObjectStoreLocation(w, r) {
			return
		}
		http.Error(w, "unexpected remote request", http.StatusInternalServerError)
	}))
	path := filepath.Join(store.AuthDir(), "versioned.json")
	if errWrite := os.WriteFile(path, []byte(`{"type":"codex","token":"old"}`), 0o600); errWrite != nil {
		t.Fatalf("write original credential: %v", errWrite)
	}
	version, errVersion := cliproxyauth.ReadPersistedVersion(path)
	if errVersion != nil {
		t.Fatalf("ReadPersistedVersion() error = %v", errVersion)
	}
	if errWrite := os.WriteFile(path, []byte(`{"type":"codex","token":"new"}`), 0o600); errWrite != nil {
		t.Fatalf("replace local credential: %v", errWrite)
	}
	beforeCalls := calls.Load()

	deleted, errDelete := store.DeleteIfVersion(context.Background(), path, version)
	if errDelete != nil || deleted {
		t.Fatalf("DeleteIfVersion() = (%t, %v), want version mismatch", deleted, errDelete)
	}
	if afterCalls := calls.Load(); afterCalls != beforeCalls {
		t.Fatalf("version mismatch issued %d remote requests", afterCalls-beforeCalls)
	}
	written, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatalf("read replacement credential: %v", errRead)
	}
	if string(written) != `{"type":"codex","token":"new"}` {
		t.Fatalf("replacement credential changed: %s", written)
	}
}

func TestParseObjectStoreAuthTombstone(t *testing.T) {
	raw := []byte(`{"deleted_version_id":"version-1","deleted_sha256":"abcd","_cliproxy_auth_deleted":true}`)
	tombstone, ok := parseObjectStoreAuthTombstone(raw)
	if !ok {
		t.Fatal("parseObjectStoreAuthTombstone() rejected valid tombstone")
	}
	if tombstone.DeletedVersionID != "version-1" || tombstone.DeletedSHA256 != "abcd" {
		t.Fatalf("tombstone = %#v", tombstone)
	}
	if _, ok = parseObjectStoreAuthTombstone([]byte(`{"type":"codex"}`)); ok {
		t.Fatal("parseObjectStoreAuthTombstone() accepted ordinary auth JSON")
	}
}
