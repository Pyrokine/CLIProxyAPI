package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

var postgresTestDriverSeq atomic.Uint64

type postgresTestState struct {
	mu                  sync.Mutex
	execCalls           int
	queryCalls          int
	beginCalls          int
	commitCalls         int
	failExec            bool
	id                  string
	payload             string
	localPath           string
	localMissingAtQuery bool
}

type postgresTestDriver struct {
	state *postgresTestState
}

func (d postgresTestDriver) Open(string) (driver.Conn, error) {
	return &postgresTestConn{state: d.state}, nil
}

type postgresTestConn struct {
	state *postgresTestState
}

func (*postgresTestConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare is not supported")
}

func (*postgresTestConn) Close() error {
	return nil
}

func (c *postgresTestConn) Begin() (driver.Tx, error) {
	c.state.mu.Lock()
	c.state.beginCalls++
	c.state.mu.Unlock()
	return &postgresTestTx{state: c.state}, nil
}

type postgresTestTx struct {
	state *postgresTestState
}

func (t *postgresTestTx) Commit() error {
	t.state.mu.Lock()
	t.state.commitCalls++
	t.state.mu.Unlock()
	return nil
}

func (*postgresTestTx) Rollback() error {
	return nil
}

func (c *postgresTestConn) ExecContext(
	_ context.Context,
	query string,
	args []driver.NamedValue,
) (driver.Result, error) {
	c.state.mu.Lock()
	defer c.state.mu.Unlock()
	c.state.execCalls++
	if c.state.failExec {
		return nil, errors.New("remote failure")
	}
	if strings.Contains(strings.ToUpper(query), "DELETE") {
		c.state.id = ""
		c.state.payload = ""
		return driver.RowsAffected(1), nil
	}
	if len(args) >= 2 {
		c.state.id = fmt.Sprint(args[0].Value)
		switch value := args[1].Value.(type) {
		case []byte:
			c.state.payload = string(value)
		case string:
			c.state.payload = value
		default:
			c.state.payload = fmt.Sprint(value)
		}
	}
	return driver.RowsAffected(1), nil
}

func (c *postgresTestConn) QueryContext(
	_ context.Context,
	query string,
	_ []driver.NamedValue,
) (driver.Rows, error) {
	c.state.mu.Lock()
	defer c.state.mu.Unlock()
	c.state.queryCalls++
	if c.state.localPath != "" {
		_, errStat := os.Stat(c.state.localPath)
		c.state.localMissingAtQuery = errors.Is(errStat, os.ErrNotExist)
	}
	if strings.Contains(query, "SELECT content") {
		return &postgresTestContentRows{payload: c.state.payload}, nil
	}
	return &postgresTestRows{
		id:      c.state.id,
		payload: c.state.payload,
	}, nil
}

type postgresTestContentRows struct {
	payload string
	read    bool
}

func (*postgresTestContentRows) Columns() []string {
	return []string{"content"}
}

func (*postgresTestContentRows) Close() error {
	return nil
}

func (r *postgresTestContentRows) Next(dest []driver.Value) error {
	if r.read || r.payload == "" {
		return io.EOF
	}
	r.read = true
	dest[0] = r.payload
	return nil
}

type postgresTestRows struct {
	id      string
	payload string
	read    bool
}

func (*postgresTestRows) Columns() []string {
	return []string{"id", "content", "created_at", "updated_at"}
}

func (*postgresTestRows) Close() error {
	return nil
}

func (r *postgresTestRows) Next(dest []driver.Value) error {
	if r.read || r.id == "" {
		return io.EOF
	}
	r.read = true
	now := time.Now()
	dest[0] = r.id
	dest[1] = r.payload
	dest[2] = now
	dest[3] = now
	return nil
}

func newPostgresStoreForTest(t *testing.T) (*PostgresStore, *postgresTestState) {
	t.Helper()
	state := &postgresTestState{}
	driverName := fmt.Sprintf("cliproxy-postgres-test-%d", postgresTestDriverSeq.Add(1))
	sql.Register(driverName, postgresTestDriver{state: state})
	db, errOpen := sql.Open(driverName, "")
	if errOpen != nil {
		t.Fatalf("sql.Open() error = %v", errOpen)
	}
	t.Cleanup(func() { _ = db.Close() })
	authDir := filepath.Join(t.TempDir(), "auths")
	if errMkdir := os.MkdirAll(authDir, 0o700); errMkdir != nil {
		t.Fatalf("create auth directory: %v", errMkdir)
	}
	return &PostgresStore{
		db:      db,
		cfg:     PostgresStoreConfig{AuthTable: defaultAuthTable},
		authDir: authDir,
	}, state
}

func TestPostgresStorePersistsDisabledCredentialWithoutExistingFile(t *testing.T) {
	store, _ := newPostgresStoreForTest(t)
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
	listed, errList := store.List(context.Background())
	if errList != nil {
		t.Fatalf("List() error = %v", errList)
	}
	if len(listed) != 1 || listed[0] == nil || !listed[0].Disabled || listed[0].Status != cliproxyauth.StatusDisabled {
		t.Fatalf("disabled credential round trip = %#v", listed)
	}
}

func TestPostgresStoreRemoteFailureRestoresNewLocalFile(t *testing.T) {
	store, state := newPostgresStoreForTest(t)
	state.failExec = true
	auth := &cliproxyauth.Auth{
		ID:       "rollback.json",
		FileName: "rollback.json",
		Metadata: map[string]any{"type": "xai"},
	}
	path := filepath.Join(store.AuthDir(), auth.FileName)
	if _, errSave := store.Save(context.Background(), auth); !errors.Is(
		errSave,
		cliproxyauth.ErrAuthDurableStateUncertain,
	) {
		t.Fatalf("Save() error = %v, want durable state uncertainty", errSave)
	}
	if _, errStat := os.Stat(path); !os.IsNotExist(errStat) {
		t.Fatalf("failed database save left local credential: %v", errStat)
	}
}

func TestPostgresStoreDeleteFailureRestoresLocalFile(t *testing.T) {
	store, state := newPostgresStoreForTest(t)
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
	state.mu.Lock()
	state.failExec = true
	state.mu.Unlock()
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

func TestPostgresStoreDeleteIfVersionCapturesLocalBeforeDatabaseMutation(t *testing.T) {
	store, state := newPostgresStoreForTest(t)
	auth := &cliproxyauth.Auth{
		ID:       "versioned.json",
		FileName: "versioned.json",
		Metadata: map[string]any{"type": "codex", "token": "old"},
	}
	path, errSave := store.Save(context.Background(), auth)
	if errSave != nil {
		t.Fatalf("Save() error = %v", errSave)
	}
	version, errVersion := cliproxyauth.ReadPersistedVersion(path)
	if errVersion != nil {
		t.Fatalf("ReadPersistedVersion() error = %v", errVersion)
	}
	state.mu.Lock()
	state.localPath = path
	state.mu.Unlock()

	deleted, errDelete := store.DeleteIfVersion(context.Background(), path, version)
	if errDelete != nil || !deleted {
		t.Fatalf("DeleteIfVersion() = (%t, %v), want deleted", deleted, errDelete)
	}
	state.mu.Lock()
	localMissingAtQuery := state.localMissingAtQuery
	beginCalls := state.beginCalls
	queryCalls := state.queryCalls
	commitCalls := state.commitCalls
	state.mu.Unlock()
	if !localMissingAtQuery {
		t.Fatal("database mutation started before the expected local version was captured")
	}
	if beginCalls != 1 || queryCalls != 1 || commitCalls != 1 {
		t.Fatalf("database calls = begin:%d query:%d commit:%d, want 1 each", beginCalls, queryCalls, commitCalls)
	}
	if _, errStat := os.Stat(path); !errors.Is(errStat, os.ErrNotExist) {
		t.Fatalf("local credential remains after committed deletion: %v", errStat)
	}
}

func TestPostgresStoreRecoversInterruptedVersionedDeletionBeforeDatabaseSync(t *testing.T) {
	store, state := newPostgresStoreForTest(t)
	auth := &cliproxyauth.Auth{
		ID:       "interrupted.json",
		FileName: "interrupted.json",
		Metadata: map[string]any{"type": "codex", "token": "old"},
	}
	path, errSave := store.Save(context.Background(), auth)
	if errSave != nil {
		t.Fatalf("Save() error = %v", errSave)
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
	state.mu.Lock()
	persistedID := state.id
	persistedPayload := state.payload
	state.mu.Unlock()
	if persistedID != "" || persistedPayload != "" {
		t.Fatalf("database credential remains after recovery: id=%q payload=%q", persistedID, persistedPayload)
	}
	if _, errStat := os.Stat(path); !errors.Is(errStat, os.ErrNotExist) {
		t.Fatalf("interrupted credential was restored locally: %v", errStat)
	}
	pending, errPending := cliproxyauth.RecoverDurablePersistedFileDeletions(store.AuthDir())
	if errPending != nil || len(pending) != 0 {
		t.Fatalf("pending deletions after recovery = %#v, %v", pending, errPending)
	}
}

func TestPostgresStoreDeleteIfVersionMismatchHasNoDatabaseSideEffect(t *testing.T) {
	store, state := newPostgresStoreForTest(t)
	auth := &cliproxyauth.Auth{
		ID:       "versioned.json",
		FileName: "versioned.json",
		Metadata: map[string]any{"type": "codex", "token": "old"},
	}
	path, errSave := store.Save(context.Background(), auth)
	if errSave != nil {
		t.Fatalf("Save() error = %v", errSave)
	}
	version, errVersion := cliproxyauth.ReadPersistedVersion(path)
	if errVersion != nil {
		t.Fatalf("ReadPersistedVersion() error = %v", errVersion)
	}
	if errWrite := os.WriteFile(path, []byte(`{"type":"codex","token":"new"}`), 0o600); errWrite != nil {
		t.Fatalf("replace local credential: %v", errWrite)
	}
	state.mu.Lock()
	state.beginCalls = 0
	state.queryCalls = 0
	state.commitCalls = 0
	state.mu.Unlock()

	deleted, errDelete := store.DeleteIfVersion(context.Background(), path, version)
	if errDelete != nil || deleted {
		t.Fatalf("DeleteIfVersion() = (%t, %v), want version mismatch", deleted, errDelete)
	}
	state.mu.Lock()
	beginCalls := state.beginCalls
	queryCalls := state.queryCalls
	commitCalls := state.commitCalls
	state.mu.Unlock()
	if beginCalls != 0 || queryCalls != 0 || commitCalls != 0 {
		t.Fatalf("version mismatch touched database: begin:%d query:%d commit:%d", beginCalls, queryCalls, commitCalls)
	}
	written, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatalf("read replacement credential: %v", errRead)
	}
	if string(written) != `{"type":"codex","token":"new"}` {
		t.Fatalf("replacement credential changed: %s", written)
	}
}

func TestPostgresStoreSameLocalContentStillChecksDatabase(t *testing.T) {
	store, state := newPostgresStoreForTest(t)
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
	state.mu.Lock()
	state.failExec = true
	beforeCalls := state.execCalls
	state.mu.Unlock()
	if _, errSave = store.Save(context.Background(), auth.Clone()); !errors.Is(
		errSave,
		cliproxyauth.ErrAuthDurableStateUncertain,
	) {
		t.Fatalf("repeated Save() error = %v, want durable state uncertainty", errSave)
	}
	state.mu.Lock()
	afterCalls := state.execCalls
	state.mu.Unlock()
	if afterCalls != beforeCalls+1 {
		t.Fatalf("database writes after retry = %d, want %d", afterCalls, beforeCalls+1)
	}
	after, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatalf("read local credential after failed retry: %v", errRead)
	}
	if string(after) != string(before) {
		t.Fatalf("local credential changed after failed retry: %s, want %s", after, before)
	}
}
