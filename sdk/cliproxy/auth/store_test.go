package auth

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDigestPersistedJSONPreservesLargeIntegerIdentity(t *testing.T) {
	left, errLeft := DigestPersistedJSON([]byte(`{"value":9007199254740992}`))
	if errLeft != nil {
		t.Fatalf("DigestPersistedJSON(left) error = %v", errLeft)
	}
	right, errRight := DigestPersistedJSON([]byte(`{"value":9007199254740993}`))
	if errRight != nil {
		t.Fatalf("DigestPersistedJSON(right) error = %v", errRight)
	}
	if left == right {
		t.Fatal("distinct large JSON integers produced the same digest")
	}
}

func TestDigestPersistedJSONRejectsMultipleValues(t *testing.T) {
	if _, err := DigestPersistedJSON([]byte(`{"value":1} {"value":2}`)); err == nil {
		t.Fatal("DigestPersistedJSON accepted multiple JSON values")
	}
}

func TestRestoreCapturedFileDoesNotReplaceConcurrentCredential(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "credential.json")
	tempPath := filepath.Join(dir, ".captured.json")
	captured := []byte(`{"token":"captured"}`)
	concurrent := []byte(`{"token":"concurrent"}`)
	if err := os.WriteFile(tempPath, captured, 0o600); err != nil {
		t.Fatalf("write captured credential: %v", err)
	}
	if err := os.WriteFile(path, concurrent, 0o600); err != nil {
		t.Fatalf("write concurrent credential: %v", err)
	}

	if err := restoreCapturedFile(path, tempPath); err != nil {
		t.Fatalf("restoreCapturedFile() error = %v", err)
	}
	current, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatalf("read concurrent credential: %v", errRead)
	}
	if string(current) != string(concurrent) {
		t.Fatalf("credential = %s, want concurrent content %s", current, concurrent)
	}
	if _, errStat := os.Stat(tempPath); !os.IsNotExist(errStat) {
		t.Fatalf("captured temporary file remains: %v", errStat)
	}
}
