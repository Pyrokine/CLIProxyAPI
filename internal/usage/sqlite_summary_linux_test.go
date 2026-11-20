package usage

import (
	"context"
	"database/sql"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestSummaryGrainsBackfillBoundedDisk(t *testing.T) {
	const helper = "CPA_TEST_SUMMARY_DISK_LIMIT"
	if os.Getenv(helper) != "1" {
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		command := exec.CommandContext(ctx, executable, "-test.run=^TestSummaryGrainsBackfillBoundedDisk$", "-test.v")
		command.Env = append(os.Environ(), helper+"=1")
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("bounded-disk migration: %v\n%s", err, output)
		}
		t.Logf("%s", output)
		return
	}

	path := filepath.Join(t.TempDir(), "events.db")
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=cache_size(-64000)&_pragma=temp_store(FILE)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schemaSQL); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO dim_model(name) VALUES('large-history')"); err != nil {
		t.Fatal(err)
	}
	const events = 8193
	if _, err := db.Exec(
		`WITH RECURSIVE seq(n) AS (
		SELECT 1 UNION ALL SELECT n+1 FROM seq WHERE n<?
	) INSERT INTO events(fingerprint,ts_ns,model_id,failed,input_tokens,total_tokens,cost_micro,cost_status,metadata)
	SELECT randomblob(20),1735689600000000000+n,1,0,1,1,7,'complete',
	       json_object('latency_ms',3,'payload',?) FROM seq`, events, strings.Repeat("x", 8192),
	); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	// Limit only this child: the database fits, but a sorter copying every metadata value three times does not.
	var original syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &original); err != nil {
		t.Fatal(err)
	}
	const limit = 128 << 20
	if before.Size() >= limit || original.Max < limit {
		t.Fatalf("fixture=%d, file limit=%d, hard limit=%d", before.Size(), limit, original.Max)
	}
	signal.Ignore(syscall.SIGXFSZ)
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &syscall.Rlimit{Cur: limit, Max: original.Max}); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &original); err != nil {
			t.Error(err)
		}
	}()

	store, err := openStore(path)
	if err != nil {
		t.Fatalf("migrate %d-byte v9 database with %d-byte file limit: %v", before.Size(), limit, err)
	}
	defer func() { _ = store.Close() }()
	var retained int
	if err := store.db.QueryRow(
		"SELECT COUNT(*) FROM events WHERE length(metadata)>8192",
	).Scan(&retained); err != nil || retained != events {
		t.Fatalf("retained events=%d, want=%d: %v", retained, events, err)
	}
	for _, grain := range []int64{1, nsPerTenMinute, nsPerHour} {
		var requests, tokens, cost, latency int64
		if err := store.db.QueryRow(
			`SELECT SUM(requests),SUM(total_tokens),SUM(cost_micro),SUM(latency_ms_total)
			FROM summary_bucket WHERE grain_ns=?`, grain,
		).Scan(&requests, &tokens, &cost, &latency); err != nil {
			t.Fatal(err)
		}
		if requests != events || tokens != events || cost != events*7 || latency != events*3 {
			t.Fatalf("grain=%d requests=%d tokens=%d cost=%d latency=%d", grain, requests, tokens, cost, latency)
		}
	}
	t.Logf("migrated v9 fixture: events=%d database_bytes=%d per_file_limit=%d", events, before.Size(), limit)
}
