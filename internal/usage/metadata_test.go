package usage

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

func TestEventMetadataBoundsLargeFieldsAndHeaders(t *testing.T) {
	detail := FlatDetail{
		RequestID: strings.Repeat("请求", 4096),
		Thinking: &Thinking{
			Intensity: strings.Repeat("强", 4096),
		},
		ResponseHeaders: http.Header{
			"X-Large": {strings.Repeat("x", 1<<20)},
		},
	}

	encoded, err := encodeEventMetadata(&detail)
	if err != nil {
		t.Fatalf("encodeEventMetadata: %v", err)
	}
	raw, ok := encoded.(string)
	if !ok {
		t.Fatalf("encoded metadata type = %T, want string", encoded)
	}
	if len(raw) > maxEventMetadataBytes {
		t.Fatalf("encoded metadata = %d bytes, limit %d", len(raw), maxEventMetadataBytes)
	}

	var decoded FlatDetail
	if err = decodeEventMetadata(&decoded, sql.NullString{String: raw, Valid: true}); err != nil {
		t.Fatalf("decodeEventMetadata: %v", err)
	}
	if !decoded.MetadataTruncated {
		t.Fatal("MetadataTruncated = false for bounded metadata")
	}
	if len(decoded.RequestID) > maxEventMetadataStringBytes {
		t.Fatalf("RequestID = %d bytes, limit %d", len(decoded.RequestID), maxEventMetadataStringBytes)
	}
	if decoded.Thinking == nil || len(decoded.Thinking.Intensity) > maxEventMetadataStringBytes {
		t.Fatalf("Thinking.Intensity was not bounded: %#v", decoded.Thinking)
	}
}

func TestEventMetadataRoundTripPreservesUsageV2Fields(t *testing.T) {
	breakdown := coreusage.NewPartialSubsetTokenBreakdown(10, 2, 1, 5, 3, 20)
	detail := FlatDetail{
		Generate:          false,
		ClientIP:          "203.0.113.9",
		XForwardedFor:     "198.51.100.1, 203.0.113.9",
		UserAgent:         "client/1.2.3",
		AccountingVersion: coreusage.TokenAccountingSchemaVersion,
		TokenBreakdown:    breakdown,
		generateRecorded:  true,
	}

	encoded, err := encodeEventMetadata(&detail)
	if err != nil {
		t.Fatalf("encodeEventMetadata: %v", err)
	}
	raw, ok := encoded.(string)
	if !ok {
		t.Fatalf("encoded metadata type = %T, want string", encoded)
	}
	var decoded FlatDetail
	if err = decodeEventMetadata(&decoded, sql.NullString{String: raw, Valid: true}); err != nil {
		t.Fatalf("decodeEventMetadata: %v", err)
	}
	if decoded.Generate || !decoded.generateRecorded {
		t.Fatalf("Generate = %t recorded=%t, want false/true", decoded.Generate, decoded.generateRecorded)
	}
	if decoded.ClientIP != detail.ClientIP || decoded.XForwardedFor != detail.XForwardedFor || decoded.UserAgent != detail.UserAgent {
		t.Fatalf("client metadata changed: %+v", decoded)
	}
	if decoded.AccountingVersion != coreusage.TokenAccountingSchemaVersion || decoded.TokenBreakdown != breakdown {
		t.Fatalf(
			"accounting metadata changed: version=%d breakdown=%+v", decoded.AccountingVersion, decoded.TokenBreakdown,
		)
	}
}

func TestDecodeEventMetadataDefaultsLegacyGenerateToTrue(t *testing.T) {
	var decoded FlatDetail
	if err := decodeEventMetadata(&decoded, sql.NullString{String: `{"completed":true}`, Valid: true}); err != nil {
		t.Fatalf("decodeEventMetadata: %v", err)
	}
	if !decoded.Generate || decoded.generateRecorded {
		t.Fatalf("Generate = %t recorded=%t, want true/false", decoded.Generate, decoded.generateRecorded)
	}

	var empty FlatDetail
	if err := decodeEventMetadata(&empty, sql.NullString{}); err != nil {
		t.Fatalf("decode empty metadata: %v", err)
	}
	if !empty.Generate || empty.generateRecorded {
		t.Fatalf("empty Generate = %t recorded=%t, want true/false", empty.Generate, empty.generateRecorded)
	}
}

func TestDecodeEventMetadataRejectsMalformedAccounting(t *testing.T) {
	malformed, err := json.Marshal(
		eventMetadata{
			AccountingVersion: coreusage.TokenAccountingSchemaVersion,
			TokenBreakdown: &coreusage.TokenBreakdown{
				SchemaVersion: coreusage.TokenAccountingSchemaVersion,
				Quality:       coreusage.TokenAccountingQualityComplete,
				TotalTokens:   5,
			},
		},
	)
	if err != nil {
		t.Fatalf("marshal malformed metadata: %v", err)
	}
	if err = decodeEventMetadata(&FlatDetail{}, sql.NullString{String: string(malformed), Valid: true}); err == nil {
		t.Fatal("decodeEventMetadata accepted malformed accounting")
	}
}

func TestTruncateUTF8BytesPreservesValidEncoding(t *testing.T) {
	value, truncated := truncateUTF8Bytes("中文字符", 5)
	if !truncated {
		t.Fatal("truncateUTF8Bytes did not report truncation")
	}
	if value != "中" {
		t.Fatalf("truncated value = %q, want 中", value)
	}
}
