package usage

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	_ "modernc.org/sqlite"
)

func TestPersistRequestLogArtifactRejectsOlderRevision(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	updatedAt := time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC)
	newer := logging.RequestLogArtifact{
		RequestID: "request-revision",
		State:     logging.RequestLogAvailable,
		Location:  logging.RequestLogLocationLocal,
		FileName:  "request-revision.log",
		Revision:  20,
		UpdatedAt: updatedAt,
	}
	if err := persistRequestLogArtifact(ctx, store.db, newer); err != nil {
		t.Fatalf("persist newer artifact: %v", err)
	}
	for _, older := range []logging.RequestLogArtifact{
		{
			RequestID: "request-revision",
			State:     logging.RequestLogPending,
			Revision:  19,
			UpdatedAt: updatedAt.Add(-time.Second),
		},
		{
			RequestID: "request-revision",
			State:     logging.RequestLogPublishFailed,
			Reason:    "same revision",
			Revision:  20,
			UpdatedAt: updatedAt.Add(time.Second),
		},
	} {
		if err := persistRequestLogArtifact(ctx, store.db, older); err != nil {
			t.Fatalf("persist stale artifact: %v", err)
		}
	}

	got, found, err := store.queryRequestLogArtifact(ctx, newer.RequestID)
	if err != nil {
		t.Fatalf("QueryRequestLogArtifact: %v", err)
	}
	if !found {
		t.Fatal("request log artifact not found")
	}
	if got.State != newer.State || got.Location != newer.Location || got.FileName != newer.FileName ||
		got.Revision != newer.Revision || !got.UpdatedAt.Equal(newer.UpdatedAt) {
		t.Fatalf("stored artifact = %#v, want %#v", got, newer)
	}
}

func TestOpenStoreAdvancesRequestLogRevisionPastPersistedRows(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "events.db")
	store, err := openStore(dbPath)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	baseline := logging.PrepareRequestLogArtifact(
		logging.RequestLogArtifact{
			RequestID: "request-restart-baseline",
			State:     logging.RequestLogPending,
		},
	).Revision
	persistedRevision := baseline + 1_000_000
	updatedAt := time.Now().UTC()
	if _, err = store.db.ExecContext(
		context.Background(),
		upsertRequestLogArtifactSQL,
		"request-restart",
		logging.RequestLogAvailable,
		logging.RequestLogLocationLocal,
		"request-restart.log",
		"",
		"",
		persistedRevision,
		updatedAt.UnixNano(),
	); err != nil {
		_ = store.Close()
		t.Fatalf("persist future revision: %v", err)
	}
	if err = store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	reopened, err := openStore(dbPath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer func() { _ = reopened.Close() }()
	expired := logging.PrepareRequestLogArtifact(
		logging.RequestLogArtifact{
			RequestID: "request-restart",
			State:     logging.RequestLogExpired,
			Reason:    "retention cleanup after restart",
		},
	)
	if expired.Revision <= persistedRevision {
		t.Fatalf("new revision = %d, want greater than persisted revision %d", expired.Revision, persistedRevision)
	}
	if err = persistRequestLogArtifact(context.Background(), reopened.db, expired); err != nil {
		t.Fatalf("persist post-restart artifact: %v", err)
	}
	stored, found, err := reopened.queryRequestLogArtifact(context.Background(), expired.RequestID)
	if err != nil || !found {
		t.Fatalf("QueryRequestLogArtifact = found %t, error %v", found, err)
	}
	if stored.State != logging.RequestLogExpired || stored.Revision != expired.Revision {
		t.Fatalf("stored artifact = %#v, want post-restart expired state", stored)
	}
}

func TestWriterShutdownDrainsRequestLogArtifactsInOrder(t *testing.T) {
	store := openTestStore(t)
	writer := newWriter(store, nil)
	writer.start(context.Background())

	if !writer.SubmitRequestLogArtifact(
		logging.RequestLogArtifact{
			RequestID: "request-drain",
			State:     logging.RequestLogPending,
		},
	) {
		t.Fatal("pending artifact was not accepted")
	}
	if !writer.SubmitRequestLogArtifact(
		logging.RequestLogArtifact{
			RequestID: "request-drain",
			State:     logging.RequestLogAvailable,
			Location:  logging.RequestLogLocationLocal,
			FileName:  "request-drain.log",
		},
	) {
		t.Fatal("available artifact was not accepted")
	}
	writer.Stop()

	got, found, err := store.queryRequestLogArtifact(context.Background(), "request-drain")
	if err != nil {
		t.Fatalf("QueryRequestLogArtifact: %v", err)
	}
	if !found {
		t.Fatal("request log artifact was not drained during shutdown")
	}
	if got.State != logging.RequestLogAvailable || got.Location != logging.RequestLogLocationLocal ||
		got.FileName != "request-drain.log" {
		t.Fatalf("stored artifact = %#v, want final available state", got)
	}
}

func TestWriterSubmitRequestLogArtifactBlocksWhenQueueIsFull(t *testing.T) {
	store := openTestStore(t)
	writer := newWriter(store, nil)
	writer.artifactCh = make(chan logging.RequestLogArtifact, 1)
	if !writer.SubmitRequestLogArtifact(
		logging.RequestLogArtifact{
			RequestID: "request-full",
			State:     logging.RequestLogPending,
		},
	) {
		t.Fatal("pending artifact was not accepted")
	}

	submitted := make(chan bool, 1)
	go func() {
		submitted <- writer.SubmitRequestLogArtifact(
			logging.RequestLogArtifact{
				RequestID: "request-full",
				State:     logging.RequestLogAvailable,
				Location:  logging.RequestLogLocationLocal,
				FileName:  "request-full.log",
			},
		)
	}()
	select {
	case <-submitted:
		t.Fatal("SubmitRequestLogArtifact returned while the writer queue was full")
	case <-time.After(50 * time.Millisecond):
	}

	writer.start(context.Background())
	select {
	case accepted := <-submitted:
		if !accepted {
			t.Fatal("blocked request log artifact was rejected after queue capacity became available")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("blocked request log artifact did not resume after queue capacity became available")
	}
	writer.Stop()

	got, found, err := store.queryRequestLogArtifact(context.Background(), "request-full")
	if err != nil {
		t.Fatalf("QueryRequestLogArtifact: %v", err)
	}
	if !found || got.State != logging.RequestLogAvailable || got.FileName != "request-full.log" {
		t.Fatalf("stored artifact = %#v, want final available state", got)
	}
	stats := writer.stats()
	if stats.ArtifactAccepted != 2 || stats.ArtifactCommitted != 2 || stats.ArtifactRejected != 0 {
		t.Fatalf("writer stats = %#v, want 2 artifacts accepted and committed", stats)
	}
}

func TestMigrateV4ToV5CreatesRequestLogArtifactSchema(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "events.db")
	store, err := openStore(dbPath)
	if err != nil {
		t.Fatalf("create current store: %v", err)
	}
	writer := newWriter(store, nil)
	detail := FlatDetail{
		Timestamp: time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC),
		RequestID: "request-v4",
		Model:     "migration-model",
		Tokens:    tokenStats{TotalTokens: 1},
	}
	if err = writer.flush([]FlatDetail{detail}); err != nil {
		_ = store.Close()
		t.Fatalf("seed event: %v", err)
	}
	if err = store.Close(); err != nil {
		t.Fatalf("close current store: %v", err)
	}

	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open database for downgrade: %v", err)
	}
	if _, err = raw.Exec(
		`
		DROP INDEX IF EXISTS idx_events_request_id;
		DROP INDEX IF EXISTS idx_request_log_artifact_state_updated;
		DROP INDEX IF EXISTS idx_request_log_artifact_updated;
		DROP TABLE IF EXISTS request_log_artifact;
		UPDATE meta SET value = '4' WHERE key = 'schema_version';
	`,
	); err != nil {
		_ = raw.Close()
		t.Fatalf("downgrade database to v4: %v", err)
	}
	if err = raw.Close(); err != nil {
		t.Fatalf("close downgraded database: %v", err)
	}

	migrated, err := openStore(dbPath)
	if err != nil {
		t.Fatalf("OpenStore v4 database: %v", err)
	}
	defer func() { _ = migrated.Close() }()

	var version string
	if err = migrated.db.QueryRow("SELECT value FROM meta WHERE key = 'schema_version'").Scan(&version); err != nil {
		t.Fatalf("query schema version: %v", err)
	}
	if version != CurrentSchemaVersion {
		t.Fatalf("schema version = %q, want %q", version, CurrentSchemaVersion)
	}
	for objectType, name := range map[string]string{
		"table": "request_log_artifact",
		"index": "idx_events_request_id",
	} {
		var count int
		if err = migrated.db.QueryRow(
			"SELECT COUNT(*) FROM sqlite_master WHERE type = ? AND name = ?",
			objectType,
			name,
		).Scan(&count); err != nil {
			t.Fatalf("query %s %s: %v", objectType, name, err)
		}
		if count != 1 {
			t.Fatalf("%s %s count = %d, want 1", objectType, name, count)
		}
	}
	exists, err := migrated.hasEventRequestID(context.Background(), detail.RequestID)
	if err != nil {
		t.Fatalf("HasEventRequestID: %v", err)
	}
	if !exists {
		t.Fatal("legacy event request ID did not survive migration")
	}
	if _, found, err := migrated.queryRequestLogArtifact(context.Background(), detail.RequestID); err != nil || found {
		t.Fatalf("legacy artifact lookup = found %t, error %v; want no lifecycle row", found, err)
	}
}
