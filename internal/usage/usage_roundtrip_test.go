package usage

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
)

func TestUsageEventExportAuthenticatesCanonicalData(t *testing.T) {
	var detail FlatDetail
	if err := json.Unmarshal(
		[]byte((`{"timestamp":"2026-11-09T01:02:03.123456789Z","model":"fixture","source":"source","` +
			`auth_index":"auth","api_key":"synthetic-key","request_id":"request","tokens":{"input_tokens":` +
			`9223372036854775805,"output_tokens":1,"total_tokens":9223372036854775806},"failed":false}`)),
		&detail,
	); err != nil {
		t.Fatal(err)
	}
	detail.Source = TagIdentityCredentialSource("account@example.test")
	details := []FlatDetail{detail}
	if err := PrepareUsageEventExport(details, "synthetic-export-key"); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(details)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"", "wrong-key"} {
		if _, _, err := parseAnyWithSecret(encoded, key); err == nil {
			t.Fatalf("accepted key %q", key)
		}
	}
	restored, _, err := parseAnyWithSecret(encoded, "synthetic-export-key")
	if err != nil {
		t.Fatal(err)
	}
	if restored[0].Source !=
		detail.Source ||
		restored[0].APIKey !=
			detail.APIKey ||
		restored[0].RequestID !=
			detail.RequestID ||
		!reflect.DeepEqual(
			restored[0].Tokens, detail.Tokens,
		) ||
		!restored[0].fingerprintTrusted {
		t.Fatalf("canonical identity changed: %+v", restored[0])
	}
	// JavaScript may round int64 display values beyond MaxInt64; sealed data remains authoritative.
	encoded = []byte(
		`[{"tokens":{"total_tokens":9223372036854776000},"import_data":"` + details[0].ImportData + `"}]`,
	)
	if _, _, err := parseAnyWithSecret(encoded, "synthetic-export-key"); err != nil {
		t.Fatalf("display integer overrode canonical data: %v", err)
	}
	details[0].ImportData = details[0].ImportData[:len(details[0].ImportData)-4] + "AAAA"
	tampered, err := json.Marshal(details)
	if err != nil {
		t.Fatal(err)
	}
	store, err := openStore(filepath.Join(t.TempDir(), dbFileName))
	if err != nil {
		t.Fatal(err)
	}
	defer func(closeResource func() error) {
		if err := closeResource(); err != nil {
			t.Errorf("close usage import store: %v", err)
		}
	}(store.Close)
	mixed := append(
		[]byte(
			`[{"timestamp":"2026-11-09T00:00:00Z","model":"valid","tokens":{"total_tokens":1}},`,
		),
		tampered[1:]...,
	)
	if _, err = importBytesWithSecret(
		context.Background(), store, pricingSnapshot{}, mixed, "synthetic-export-key",
	); !errors.Is(
		err,
		ErrInvalidImport,
	) {
		t.Fatalf("tampered import: %v", err)
	}
	var count int
	if err := store.database().QueryRow("SELECT COUNT(*) FROM events").Scan(&count); err != nil || count != 0 {
		t.Fatalf("failed import partially committed: %d %v", count, err)
	}
}

func TestLegacyCSVImportRetainsRequestIDAndOptionalFields(t *testing.T) {
	payload := []byte(("timestamp,model,result,input_tokens,output_tokens,total_tokens,request" +
		"_id\n2026-11-09T00:00:00Z,fixture,success,2,3,5,legacy-request\n"))
	details, _, err := parseAny(payload)
	if err != nil || len(details) != 1 {
		t.Fatalf("legacy CSV: %v", err)
	}
	if details[0].RequestID != "legacy-request" || details[0].APIKey != "" || details[0].AuthIndex != "" {
		t.Fatalf("legacy identity: %+v", details[0])
	}
}

func TestUsageDBSizeStatErrorsRemainVisibleAndRecover(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, dbFileName), []byte("1234567"), 0600); err != nil {
		t.Fatal(err)
	}
	original := statUsageFile
	defer func() { statUsageFile = original }()
	p := &Persister{baseDir: dir, retention: config.UsageRetention{MaxDBSizeMB: 1}}
	for _, name := range []string{dbFileName, dbFileName + "-wal", dbFileName + "-shm"} {
		for _, cause := range []error{os.ErrPermission, syscall.EIO} {
			statUsageFile = func(path string) (os.FileInfo, error) {
				if filepath.Base(path) == name {
					return nil, &os.PathError{Op: "stat", Path: path, Err: cause}
				}
				return original(path)
			}
			size, err := p.DBSizeWithError()
			if size != -1 || !errors.Is(err, cause) {
				t.Fatalf("%s: size=%d error=%v", name, size, err)
			}
			size, _, _, _, _, err = p.DBSizeStatusWithError()
			if size != -1 || !errors.Is(err, cause) {
				t.Fatalf("status %s: size=%d error=%v", name, size, err)
			}
			if p.DBSize() != -1 {
				t.Fatal("legacy entry returned successful size")
			}
			if _, err := p.ImportContext(context.Background(), []byte("[]")); !errors.Is(err, cause) {
				t.Fatalf("capacity check hid stat error: %v", err)
			}
			p.dbSizeCapped.Store(true)
			if !p.refreshDBSizeCap() {
				t.Fatal("stat failure cleared last measured cap")
			}
			p.dbSizeCapped.Store(false)
			if p.refreshDBSizeCap() {
				t.Fatal("stat failure fabricated a reached cap")
			}
		}
	}
	statUsageFile = original
	size, _, _, _, capped, err := p.DBSizeStatusWithError()
	if err != nil || size != 7 || capped {
		t.Fatalf("recovery size=%d capped=%v error=%v", size, capped, err)
	}
	if p.refreshDBSizeCap() {
		t.Fatal("recovered size cap did not clear")
	}
	nilSize, err := (*Persister)(nil).DBSizeWithError()
	if nilSize != 0 || err != nil {
		t.Fatalf("nil persister: %d %v", nilSize, err)
	}
}
