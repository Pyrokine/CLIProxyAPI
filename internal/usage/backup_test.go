package usage

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	internallogging "github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

func TestUsageBackupRoundTripPreservesCanonicalEvents(t *testing.T) {
	ctx := context.Background()
	source, err := openStore(filepath.Join(t.TempDir(), dbFileName))
	if err != nil {
		t.Fatalf("OpenStore(source): %v", err)
	}
	defer func() { _ = source.Close() }()

	budget := 4096
	breakdown := coreusage.NewPartialSubsetTokenBreakdown(101, 44, 0, 202, 33, 380)
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
			Generate:            false,
			ClientIP:            "203.0.113.21",
			XForwardedFor:       "198.51.100.8, 203.0.113.21",
			UserAgent:           "backup-test-client/1.0",
			AccountingVersion:   coreusage.TokenAccountingSchemaVersion,
			TokenBreakdown:      breakdown,
			MetadataTruncated:   true,
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
				CacheReadTokens: 44,
				TotalTokens:     380,
			},
			generateRecorded: true,
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
	requestCost, requestReceipt, err := buildPricingReceipt(
		&details[0],
		pricingSnapshot{priceFn: sourcePrice, revision: "backup-request-revision"},
		time.Date(2026, 7, 29, 9, 8, 8, 0, time.UTC),
	)
	if err != nil {
		t.Fatalf("buildPricingReceipt(source): %v", err)
	}
	details[0].PricingReceipt = requestReceipt
	details[0].costMicro = requestCost.Micro
	details[0].CostStatus = requestCost.Status
	details[0].CostReason = requestCost.Reason
	details[0].costTrusted = true
	if added, skipped, errImport := importDetails(ctx, source, sourcePrice, details); errImport != nil {
		t.Fatalf("importDetails(source): %v", errImport)
	} else if added != len(details) || skipped != 0 {
		t.Fatalf("importDetails(source) = added %d skipped %d, want %d/0", added, skipped, len(details))
	}
	requestLogUpdatedAt := time.Date(2026, 7, 29, 9, 8, 8, 0, time.UTC)
	if errArtifact := persistRequestLogArtifact(
		ctx, source.db, internallogging.RequestLogArtifact{
			RequestID: "request-backup-1",
			State:     internallogging.RequestLogAvailable,
			Location:  internallogging.RequestLogLocationHome,
			HomeIP:    "192.0.2.45",
			Revision:  42,
			UpdatedAt: requestLogUpdatedAt,
		},
	); errArtifact != nil {
		t.Fatalf("persistRequestLogArtifact(source): %v", errArtifact)
	}

	sourceEvents, err := source.queryBackupEventSet(ctx, time.Time{}, time.Time{}, EventFilters{})
	if err != nil {
		t.Fatalf("QueryBackupEvents(source): %v", err)
	}
	var sourceLogEvent *FlatDetail
	for i := range sourceEvents {
		if sourceEvents[i].RequestID == "request-backup-1" {
			sourceLogEvent = &sourceEvents[i]
			break
		}
	}
	if sourceLogEvent == nil {
		t.Fatal("source backup event with request log artifact was not found")
	}
	if sourceLogEvent.RequestLogState != internallogging.RequestLogAvailable ||
		sourceLogEvent.RequestLogLocation != internallogging.RequestLogLocationHome ||
		sourceLogEvent.RequestLogHomeIP != "192.0.2.45" ||
		sourceLogEvent.RequestLogRevision != 42 ||
		!sourceLogEvent.RequestLogUpdatedAt.Equal(requestLogUpdatedAt) {
		t.Fatalf("source request log artifact = %+v", sourceLogEvent)
	}
	if sourceEvents[0].costMicro <= 0 || !sourceEvents[0].costTrusted ||
		sourceEvents[0].CostStatus != costStatusPartial ||
		sourceEvents[0].CostReason != "token accounting contains unclassified tokens" {
		t.Fatalf(
			"source backup cost = (%d, %t, %q, %q), want trusted partial value", sourceEvents[0].costMicro,
			sourceEvents[0].costTrusted, sourceEvents[0].CostStatus, sourceEvents[0].CostReason,
		)
	}
	if sourceEvents[0].PricingReceipt == nil ||
		sourceEvents[0].PricingReceipt.PriceRevision != "backup-request-revision" {
		t.Fatalf("source backup pricing receipt = %#v", sourceEvents[0].PricingReceipt)
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

	target, err := openStore(filepath.Join(t.TempDir(), dbFileName))
	if err != nil {
		t.Fatalf("OpenStore(target): %v", err)
	}
	defer func() { _ = target.Close() }()
	targetPrice := func(string) (float64, float64, float64, bool) {
		return 100, 200, 50, true
	}
	result, err := importBytesWithSecret(ctx, target, targetPrice, payload, backupKey)
	if err != nil {
		t.Fatalf("ImportBytesWithSecret(target): %v", err)
	}
	if result.Added != int64(len(sourceEvents)) || result.Skipped != 0 || result.Format != "usageBackupV4" {
		t.Fatalf("ImportBytesWithSecret(target) = %+v", result)
	}
	targetEvents, err := target.queryBackupEventSet(ctx, time.Time{}, time.Time{}, EventFilters{})
	if err != nil {
		t.Fatalf("QueryBackupEvents(target): %v", err)
	}
	if !reflect.DeepEqual(targetEvents, sourceEvents) {
		t.Fatalf("backup round trip changed events\nsource=%#v\ntarget=%#v", sourceEvents, targetEvents)
	}

	repeated, err := importBytesWithSecret(ctx, target, targetPrice, payload, backupKey)
	if err != nil {
		t.Fatalf("ImportBytesWithSecret(repeated): %v", err)
	}
	if repeated.Added != 0 || repeated.Skipped != int64(len(sourceEvents)) {
		t.Fatalf("ImportBytesWithSecret(repeated) = %+v", repeated)
	}
}

func TestUsageBackupV3RemainsReadableWithoutPricingReceipt(t *testing.T) {
	const backupKey = "portable-backup-key-for-v3-compatibility"
	detail := FlatDetail{
		Timestamp: time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC),
		Model:     "legacy-model",
		Tokens:    tokenStats{InputTokens: 1, TotalTokens: 1},
	}
	v4Payload, err := MarshalUsageBackup([]FlatDetail{detail}, backupKey, time.Now())
	if err != nil {
		t.Fatalf("MarshalUsageBackup: %v", err)
	}
	content := decodeUsageBackupContentForTest(t, v4Payload, backupKey)
	if content.Events[0].PricingReceipt != nil {
		t.Fatal("legacy fixture unexpectedly contains a pricing receipt")
	}
	plaintext, err := json.Marshal(content)
	if err != nil {
		t.Fatalf("marshal v3 content: %v", err)
	}
	var envelope usageBackupEnvelope
	if err = json.Unmarshal(v4Payload, &envelope); err != nil {
		t.Fatalf("decode v4 envelope: %v", err)
	}
	salt, err := base64.RawStdEncoding.DecodeString(envelope.KeyDerivation.Salt)
	if err != nil {
		t.Fatalf("decode v3 fixture salt: %v", err)
	}
	encryptionKey, _, err := deriveUsageBackupKeys(backupKey, salt)
	if err != nil {
		t.Fatalf("derive v3 fixture key: %v", err)
	}
	gcm, err := newUsageBackupAEAD(encryptionKey)
	if err != nil {
		t.Fatalf("create v3 fixture AEAD: %v", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		t.Fatalf("generate v3 fixture nonce: %v", err)
	}
	envelope.Version = usageBackupLegacyVersion
	envelope.Nonce = base64.RawStdEncoding.EncodeToString(nonce)
	envelope.Ciphertext = base64.RawStdEncoding.EncodeToString(
		gcm.Seal(nil, nonce, plaintext, usageBackupAAD(envelope.Version, envelope.KeyDerivation)),
	)
	v3Payload, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("marshal v3 envelope: %v", err)
	}

	parsed, format, err := parseAnyWithSecret(v3Payload, backupKey)
	if err != nil {
		t.Fatalf("ParseAnyWithSecret(v3): %v", err)
	}
	if format != "usageBackupV3" || len(parsed) != 1 || parsed[0].PricingReceipt != nil {
		t.Fatalf("parsed v3 backup = format %q events %#v", format, parsed)
	}
}

func TestUsageBackupEventDefaultsLegacyGenerateAndRejectsMalformedAccounting(t *testing.T) {
	legacy, err := (usageBackupEvent{Timestamp: time.Now(), Model: "legacy"}).flatDetail("")
	if err != nil {
		t.Fatalf("legacy flatDetail: %v", err)
	}
	if !legacy.Generate || legacy.generateRecorded {
		t.Fatalf("legacy Generate = %t recorded=%t, want true/false", legacy.Generate, legacy.generateRecorded)
	}

	malformed := usageBackupEvent{
		Timestamp:         time.Now(),
		Model:             "malformed",
		AccountingVersion: coreusage.TokenAccountingSchemaVersion,
		TokenBreakdown: &coreusage.TokenBreakdown{
			SchemaVersion: coreusage.TokenAccountingSchemaVersion,
			Quality:       coreusage.TokenAccountingQualityComplete,
			TotalTokens:   1,
		},
	}
	if _, err = malformed.flatDetail(""); err == nil {
		t.Fatal("flatDetail accepted malformed token accounting")
	}
}

func TestUsageBackupPreservesDistinctStoredFingerprintsAfterMetadataTruncation(t *testing.T) {
	ctx := context.Background()
	source, err := openStore(filepath.Join(t.TempDir(), dbFileName))
	if err != nil {
		t.Fatalf("OpenStore(source): %v", err)
	}
	defer func() { _ = source.Close() }()

	requestIDPrefix := strings.Repeat("x", maxEventMetadataStringBytes)
	timestamp := time.Date(2026, 8, 9, 3, 2, 1, 0, time.UTC)
	details := []FlatDetail{
		{Timestamp: timestamp, Model: "model", RequestID: requestIDPrefix + "a", Tokens: tokenStats{TotalTokens: 1}},
		{Timestamp: timestamp, Model: "model", RequestID: requestIDPrefix + "b", Tokens: tokenStats{TotalTokens: 1}},
	}
	if added, skipped, errImport := importDetails(ctx, source, nil, details); errImport != nil {
		t.Fatalf("importDetails(source): %v", errImport)
	} else if added != len(details) || skipped != 0 {
		t.Fatalf("importDetails(source) = added %d skipped %d, want %d/0", added, skipped, len(details))
	}

	sourceEvents, err := source.queryBackupEventSet(ctx, time.Time{}, time.Time{}, EventFilters{})
	if err != nil {
		t.Fatalf("QueryBackupEvents(source): %v", err)
	}
	if len(sourceEvents) != 2 || sourceEvents[0].RequestID != sourceEvents[1].RequestID {
		t.Fatalf("source events did not reproduce truncated identity collision: %#v", sourceEvents)
	}
	if sourceEvents[0].fingerprint == sourceEvents[1].fingerprint {
		t.Fatal("source events lost their distinct stored fingerprints")
	}

	const backupKey = "portable-backup-key-for-truncated-metadata"
	payload, err := MarshalUsageBackup(sourceEvents, backupKey, time.Now())
	if err != nil {
		t.Fatalf("MarshalUsageBackup: %v", err)
	}
	target, err := openStore(filepath.Join(t.TempDir(), dbFileName))
	if err != nil {
		t.Fatalf("OpenStore(target): %v", err)
	}
	defer func() { _ = target.Close() }()

	result, err := importBytesWithSecret(ctx, target, nil, payload, backupKey)
	if err != nil {
		t.Fatalf("ImportBytesWithSecret(target): %v", err)
	}
	if result.Added != 2 || result.Skipped != 0 {
		t.Fatalf("ImportBytesWithSecret(target) = %+v, want two added events", result)
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
	plaintext, err := gcm.Open(nil, nonce, ciphertext, usageBackupAAD(envelope.Version, envelope.KeyDerivation))
	if err != nil {
		t.Fatalf("decrypt backup content: %v", err)
	}
	var content usageBackupContent
	if err = json.Unmarshal(plaintext, &content); err != nil {
		t.Fatalf("decode backup content: %v", err)
	}
	return content
}
