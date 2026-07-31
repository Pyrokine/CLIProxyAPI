package usage

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestUsageBackupRoundTripPreservesCanonicalEvents(t *testing.T) {
	ctx := context.Background()
	source, err := OpenStore(filepath.Join(t.TempDir(), dbFileName))
	if err != nil {
		t.Fatalf("OpenStore(source): %v", err)
	}
	defer func() { _ = source.Close() }()

	budget := 4096
	details := []FlatDetail{
		{
			Timestamp:           time.Date(2026, 7, 29, 9, 8, 7, 654321000, time.UTC),
			Model:               "gpt-5.4",
			Source:              "codex-user.json",
			AuthIndex:           "codex-user@example.com",
			APIKey:              "sk-backup-roundtrip-secret-value",
			Provider:            "codex",
			LatencyMs:           321,
			RequestID:           "request-backup-1",
			TimeToFirstByteMs:   123,
			TotalDurationMs:     456,
			Completed:           true,
			ReasoningEffort:     "high",
			ServiceTier:         "priority",
			RequestServiceTier:  "priority",
			ResponseServiceTier: "default",
			ExecutorType:        "openai-response",
			Thinking:            &Thinking{Intensity: "high", Mode: "enabled", Level: "deep", Budget: &budget},
			ResponseHeaders: http.Header{
				"Content-Type":     {"application/json"},
				"X-Request-ID":     {"upstream-request-1"},
				"Set-Cookie":       {"must-not-be-persisted=1"},
				"X-Upstream-Token": {"must-be-masked"},
			},
			Tokens: tokenStats{
				InputTokens:     101,
				OutputTokens:    202,
				ReasoningTokens: 33,
				CachedTokens:    44,
				TotalTokens:     380,
			},
		},
		{
			Timestamp: time.Date(2026, 7, 29, 9, 9, 8, 123456000, time.UTC),
			Model:     "claude-sonnet-4-5",
			Source:    "claude-user.json",
			Provider:  "claude",
			RequestID: "request-backup-2",
			Completed: false,
			Tokens: tokenStats{
				InputTokens:  17,
				OutputTokens: 19,
				TotalTokens:  36,
			},
			Failed: true,
		},
	}
	sourcePrice := func(string) (float64, float64, float64, bool) {
		return 1.25, 2.5, 0.5, true
	}
	if added, skipped, errImport := importDetails(ctx, source, sourcePrice, details); errImport != nil {
		t.Fatalf("importDetails(source): %v", errImport)
	} else if added != len(details) || skipped != 0 {
		t.Fatalf("importDetails(source) = added %d skipped %d, want %d/0", added, skipped, len(details))
	}

	sourceEvents, err := source.QueryBackupEvents(ctx, time.Time{}, time.Time{}, EventFilters{})
	if err != nil {
		t.Fatalf("QueryBackupEvents(source): %v", err)
	}
	if sourceEvents[0].costMicro <= 0 || !sourceEvents[0].costTrusted {
		t.Fatalf("source backup cost = (%d, %t), want trusted positive value", sourceEvents[0].costMicro, sourceEvents[0].costTrusted)
	}
	const backupKey = "portable-backup-key-for-roundtrip"
	payload, err := MarshalUsageBackup(sourceEvents, backupKey, time.Date(2026, 7, 29, 10, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("MarshalUsageBackup: %v", err)
	}
	for _, plaintext := range []string{
		details[0].APIKey,
		details[0].Model,
		details[0].RequestID,
		"hmac-sha256:",
		"must...sked",
	} {
		if bytes.Contains(payload, []byte(plaintext)) {
			t.Fatalf("backup contains protected plaintext %q: %s", plaintext, payload)
		}
	}
	if _, err = ParseUsageBackup(payload, "wrong-backup-key"); err == nil ||
		!strings.Contains(err.Error(), "key that created the backup") {
		t.Fatalf("ParseUsageBackup(wrong key) error = %v", err)
	}
	unsafeEvent := sourceEvents[0]
	unsafeEvent.ResponseHeaders = http.Header{"Authorization": {"Bearer unredacted-backup-token"}}
	if _, err = MarshalUsageBackup([]FlatDetail{unsafeEvent}, backupKey, time.Now()); err == nil ||
		!strings.Contains(err.Error(), "unredacted credential") {
		t.Fatalf("MarshalUsageBackup(unredacted header) error = %v", err)
	}

	var tampered usageBackupEnvelope
	if err = json.Unmarshal(payload, &tampered); err != nil {
		t.Fatalf("decode backup for tamper test: %v", err)
	}
	ciphertext, err := base64.RawStdEncoding.DecodeString(tampered.Ciphertext)
	if err != nil || len(ciphertext) == 0 {
		t.Fatalf("decode ciphertext: %v", err)
	}
	ciphertext[len(ciphertext)/2] ^= 0x01
	tampered.Ciphertext = base64.RawStdEncoding.EncodeToString(ciphertext)
	tamperedPayload, err := json.Marshal(tampered)
	if err != nil {
		t.Fatalf("encode tampered backup: %v", err)
	}
	if _, err = ParseUsageBackup(tamperedPayload, backupKey); err == nil ||
		!strings.Contains(err.Error(), "decryption failed") {
		t.Fatalf("ParseUsageBackup(tampered ciphertext) error = %v", err)
	}

	withoutAPIKey := sourceEvents[1]
	withoutAPIKey.APIKey = ""
	noKeyPayload, err := MarshalUsageBackup([]FlatDetail{withoutAPIKey}, backupKey, time.Now())
	if err != nil {
		t.Fatalf("MarshalUsageBackup(no API key): %v", err)
	}
	if _, err = ParseUsageBackup(noKeyPayload, "wrong-backup-key"); err == nil ||
		!strings.Contains(err.Error(), "key that created the backup") {
		t.Fatalf("ParseUsageBackup(no API key, wrong key) error = %v", err)
	}

	secondPayload, err := MarshalUsageBackup(sourceEvents, backupKey, time.Date(2026, 7, 29, 10, 1, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("MarshalUsageBackup(second): %v", err)
	}
	firstContent := decodeUsageBackupContentForTest(t, payload, backupKey)
	secondContent := decodeUsageBackupContentForTest(t, secondPayload, backupKey)
	firstIdentity := firstContent.Events[0].APIKeyIdentity
	secondIdentity := secondContent.Events[0].APIKeyIdentity
	if firstIdentity == "" || secondIdentity == "" || firstIdentity == secondIdentity {
		t.Fatalf("backup identities = %q and %q, want distinct salted identities", firstIdentity, secondIdentity)
	}

	target, err := OpenStore(filepath.Join(t.TempDir(), dbFileName))
	if err != nil {
		t.Fatalf("OpenStore(target): %v", err)
	}
	defer func() { _ = target.Close() }()
	targetPrice := func(string) (float64, float64, float64, bool) {
		return 100, 200, 50, true
	}
	result, err := ImportBytesWithSecret(ctx, target, targetPrice, payload, backupKey)
	if err != nil {
		t.Fatalf("ImportBytesWithSecret(target): %v", err)
	}
	if result.Added != int64(len(sourceEvents)) || result.Skipped != 0 || result.Format != "usageBackupV3" {
		t.Fatalf("ImportBytesWithSecret(target) = %+v", result)
	}
	targetEvents, err := target.QueryBackupEvents(ctx, time.Time{}, time.Time{}, EventFilters{})
	if err != nil {
		t.Fatalf("QueryBackupEvents(target): %v", err)
	}
	if !reflect.DeepEqual(targetEvents, sourceEvents) {
		t.Fatalf("backup round trip changed events\nsource=%#v\ntarget=%#v", sourceEvents, targetEvents)
	}

	repeated, err := ImportBytesWithSecret(ctx, target, targetPrice, payload, backupKey)
	if err != nil {
		t.Fatalf("ImportBytesWithSecret(repeated): %v", err)
	}
	if repeated.Added != 0 || repeated.Skipped != int64(len(sourceEvents)) {
		t.Fatalf("ImportBytesWithSecret(repeated) = %+v", repeated)
	}
}

func decodeUsageBackupContentForTest(t *testing.T, payload []byte, secret string) usageBackupContent {
	t.Helper()
	var envelope usageBackupEnvelope
	if err := json.Unmarshal(payload, &envelope); err != nil {
		t.Fatalf("decode backup envelope: %v", err)
	}
	salt, err := base64.RawStdEncoding.DecodeString(envelope.KeyDerivation.Salt)
	if err != nil {
		t.Fatalf("decode backup salt: %v", err)
	}
	encryptionKey, _, err := deriveUsageBackupKeys(secret, salt)
	if err != nil {
		t.Fatalf("derive backup keys: %v", err)
	}
	gcm, err := newUsageBackupAEAD(encryptionKey)
	if err != nil {
		t.Fatalf("create backup AEAD: %v", err)
	}
	nonce, err := base64.RawStdEncoding.DecodeString(envelope.Nonce)
	if err != nil {
		t.Fatalf("decode backup nonce: %v", err)
	}
	ciphertext, err := base64.RawStdEncoding.DecodeString(envelope.Ciphertext)
	if err != nil {
		t.Fatalf("decode backup ciphertext: %v", err)
	}
	plaintext, err := gcm.Open(nil, nonce, ciphertext, usageBackupAAD(envelope.KeyDerivation))
	if err != nil {
		t.Fatalf("decrypt backup content: %v", err)
	}
	var content usageBackupContent
	if err = json.Unmarshal(plaintext, &content); err != nil {
		t.Fatalf("decode backup content: %v", err)
	}
	return content
}
