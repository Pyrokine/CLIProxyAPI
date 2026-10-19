package auth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestDecodeCredentialMetadataPreservesNumbersAndObjectContract(t *testing.T) {
	metadata, err := DecodeCredentialMetadata([]byte(`{"value":9007199254740993,"priority":4.0,"request_retry":2e0,"weight":1e0,"nested":[1.25,{"precise":0.1000000000000000000001,"tiny":1e-10000}]}`))
	if err != nil {
		t.Fatalf("DecodeCredentialMetadata() error = %v", err)
	}
	if metadata["value"] != json.Number("9007199254740993") {
		t.Fatalf("large JSON integer was not preserved: %v", metadata["value"])
	}
	nested := metadata["nested"].([]any)
	precise := nested[1].(map[string]any)
	if precise["precise"] != json.Number("0.1000000000000000000001") || precise["tiny"] != json.Number("1e-10000") {
		t.Fatalf("precise JSON numbers were not preserved: %v", precise)
	}
	if metadata["priority"] != float64(4) || metadata["request_retry"] != float64(2) || nested[0] != float64(1.25) {
		t.Fatalf("lossless float64 consumer compatibility changed: %v", metadata)
	}
	auth := &Auth{Metadata: metadata}
	ApplyAuthPriorityMetadata(auth, metadata)
	if auth.Attributes["priority"] != "4" {
		t.Fatalf("priority = %q, want 4", auth.Attributes["priority"])
	}
	if retry, specified := auth.RequestRetryOverride(); !specified || retry != 2 {
		t.Fatalf("retry = %d, specified=%v", retry, specified)
	}
	if err := ApplyAuthWeightMetadata(auth, metadata); err != nil || auth.Attributes[AttributeWeight] != "1" {
		t.Fatalf("scientific notation weight changed: %v", err)
	}
	for _, raw := range []string{"null", "[]", `"text"`, `{"value":1} {"value":2}`, `{"value":1} trailing`} {
		if _, err := DecodeCredentialMetadata([]byte(raw)); err == nil {
			t.Fatalf("invalid credential object accepted: %s", raw)
		}
	}
}

func TestBindPersistedAuthDigestPreservesFileVersionOnQuotaError(t *testing.T) {
	auth := &Auth{Provider: "plugin", Metadata: map[string]any{"serial": 1}}
	original, err := DigestPersistedJSON([]byte(`{"type":"plugin","serial":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := BindPersistedAuthDigest(auth, original); err != nil {
		t.Fatal(err)
	}
	attributes := auth.Attributes
	updated, err := DigestPersistedJSON([]byte(`{"type":"plugin","serial":9007199254740993}`))
	if err != nil {
		t.Fatal(err)
	}
	auth.Metadata["serial"] = float64(9007199254740992)
	if err := BindPersistedAuthDigest(auth, updated); err == nil {
		t.Fatal("unproven quota material was accepted")
	}
	if auth.Attributes[PersistedSHA256Attribute] == attributes[PersistedSHA256Attribute] {
		t.Fatal("quota proof failure left an outdated file digest")
	}
	if auth.quotaPersistedRevision != "" || auth.quotaPersistedSHA256 != "" {
		t.Fatal("quota proof failure retained a previous material proof")
	}
}

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

func TestPersistedVersionForAuthPreservesLargeIntegerIdentity(t *testing.T) {
	for _, serial := range []int64{9007199254740992, 9007199254740993} {
		auth := &Auth{Storage: struct {
			quotaRevisionRawStorage
			Serial int64 `json:"serial"`
		}{Serial: serial}}
		version, err := PersistedVersionForAuth("synthetic.json", auth)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(map[string]any{"serial": serial})
		if err != nil {
			t.Fatal(err)
		}
		expected, err := DigestPersistedJSON(raw)
		if err != nil || version.SHA256 != expected {
			t.Fatalf("persisted file version lost numeric material: %v", err)
		}
	}
}

func TestDigestPersistedJSONPreservesNumericCompatibility(t *testing.T) {
	expected, err := DigestPersistedJSON([]byte(`{"value":1}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{`{"value":1.0}`, `{"value":1e0}`} {
		actual, err := DigestPersistedJSON([]byte(raw))
		if err != nil || actual != expected {
			t.Fatalf("lossless legacy numeric digest changed: %v", err)
		}
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
