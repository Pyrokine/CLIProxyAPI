package usage

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"math"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

func requestObservationFields(detail FlatDetail) []string {
	billing, _ := json.Marshal(detail.BillingMetadata)
	return []string{
		detail.Model, detail.Provider, detail.ClientModel, detail.UpstreamURL, detail.UpstreamModel,
		detail.ResponseURL, detail.ResponseURLSource, detail.ResponseURLStatus, detail.UpstreamNextHopStatus,
		strconv.FormatBool(detail.CacheReadReported), string(billing),
		detail.ResponseModel, detail.ResponseProvider, detail.ResponseProviderSource, detail.ServiceTier,
		detail.RequestServiceTier, detail.ResponseServiceTier, detail.RequestSpeed, detail.ResponseSpeed,
	}
}

func TestRequestObservationsCacheWritePersistence(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 11, 20, 1, 0, 0, 0, time.UTC)
	for index, flag := range []*bool{nil, new(false), new(true), nil, new(false)} {
		quantity := int64(0)
		if index >= 3 {
			quantity = 7
		}
		want := flag
		if index == 3 {
			want = new(true)
		}
		detail := FlatDetail{
			Timestamp: base.Add(time.Duration(index) * time.Second), Model: "synthetic-model", Provider: "openai",
			CacheWriteReported: flag,
			Tokens: tokenStats{
				InputTokens: 10, CacheWriteTokens: quantity, CachedTokens: quantity, TotalTokens: 10,
			},
		}
		tx, err := store.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := persistEvent(ctx, tx, store, &detail, nil); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		if index == 3 {
			if _, err := store.db.ExecContext(
				ctx, `UPDATE events SET metadata=json_remove(metadata,'$.cache_write_reported') WHERE ts_ns=?`,
				detail.Timestamp.UnixNano(),
			); err != nil {
				t.Fatal(err)
			}
		}
		full, _, err := store.queryEventDetails(
			ctx, detail.Timestamp, detail.Timestamp.Add(time.Nanosecond), EventFilters{}, 1, 50, "timestamp", false,
		)
		if err != nil || len(full) != 1 {
			t.Fatalf("full: %+v %v", full, err)
		}
		compact, _, err := store.queryCompactEventDetails(
			ctx, detail.Timestamp, detail.Timestamp.Add(time.Nanosecond), EventFilters{}, 1, 50, "timestamp", false,
		)
		if err != nil || len(compact) != 1 {
			t.Fatalf("compact: %+v %v", compact, err)
		}
		for _, got := range []FlatDetail{full[0], compact[0]} {
			if !reflect.DeepEqual(got.CacheWriteReported, want) || got.Tokens.CacheWriteTokens != quantity {
				t.Fatalf("write state changed: want=%v quantity=%d got=%+v", want, quantity, got)
			}
		}
		backup := append([]FlatDetail(nil), full...)
		if err := PrepareUsageEventExport(backup, "synthetic-export-secret"); err != nil {
			t.Fatal(err)
		}
		for _, records := range [][]FlatDetail{full, backup} {
			raw, err := json.Marshal(records)
			if err != nil {
				t.Fatal(err)
			}
			restored, _, err := parseAnyWithSecret(raw, "synthetic-export-secret")
			if err != nil || len(restored) != 1 || !reflect.DeepEqual(
				restored[0].CacheWriteReported, want,
			) || restored[0].Tokens.CacheWriteTokens != quantity {
				t.Fatalf("JSON/backup state changed: %+v %v", restored, err)
			}
		}
	}
}

func TestRequestObservationsCacheWriteImportsAndLegacy(t *testing.T) {
	for _, test := range []struct {
		flag     string
		quantity int64
		want     *bool
	}{
		{"", 0, nil}, {"", 7, new(true)}, {"false", 7, new(false)}, {"true", 0, new(true)},
	} {
		var csvData bytes.Buffer
		writer := csv.NewWriter(&csvData)
		for _, row := range [][]string{
			{
				"timestamp", "model", "result", "input_tokens", "output_tokens", "cached_tokens", "total_tokens",
				"cache_read_tokens", "cache_write_tokens", "cache_write_reported",
			},
			{
				"2026-11-20T01:00:00Z", "synthetic-model", "success", "10", "1", strconv.FormatInt(test.quantity+1, 10),
				"11", "1", strconv.FormatInt(test.quantity, 10), test.flag,
			},
		} {
			if err := writer.Write(row); err != nil {
				t.Fatal(err)
			}
		}
		writer.Flush()
		if err := writer.Error(); err != nil {
			t.Fatal(err)
		}
		jsonFlag := ""
		if test.flag != "" {
			jsonFlag = `,"cache_write_reported":` + test.flag
		}
		plain := `[{"timestamp":"2026-11-20T01:00:00Z","model":"synthetic-model"` + jsonFlag + `,"tokens":{"input_tokens":10,"output_tokens":1,"total_tokens":11,"cache_read_tokens":1,"cache_write_tokens":` + strconv.FormatInt(
			test.quantity, 10,
		) + `}}]`
		for _, raw := range [][]byte{csvData.Bytes(), []byte(plain)} {
			details, _, err := parseAny(raw)
			if err != nil || len(details) != 1 || !reflect.DeepEqual(
				details[0].CacheWriteReported, test.want,
			) || details[0].Tokens.CacheWriteTokens != test.quantity || details[0].Tokens.CacheReadTokens != 1 {
				t.Fatalf("import lost report state: %+v %v", details, err)
			}
		}
		stats := NewRequestStatistics()
		stats.record(
			context.Background(), coreusage.Record{
				Model: "synthetic-model", Provider: "claude", Detail: coreusage.Detail{
					InputTokens: 10, CacheCreationTokens: test.quantity,
					CacheWriteReported: coreusage.NormalizeCacheWriteReported(test.want, 0),
				},
			},
		)
		flat := flattenSnapshot(stats.snapshot())
		if len(flat) != 1 || !reflect.DeepEqual(flat[0].CacheWriteReported, test.want) {
			t.Fatalf("legacy snapshot state=%+v", flat)
		}
		copied := stats.snapshot()
		for _, api := range copied.APIs {
			for _, model := range api.Models {
				for index := range model.Details {
					if model.Details[index].CacheWriteReported != nil {
						*model.Details[index].CacheWriteReported = !*model.Details[index].CacheWriteReported
					}
				}
			}
		}
		if next := flattenSnapshot(stats.snapshot()); !reflect.DeepEqual(next[0].CacheWriteReported, test.want) {
			t.Fatal("snapshot exposed mutable report state")
		}
	}
}

func TestRequestObservationsPersistCompactAndExport(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 11, 18, 1, 0, 0, 0, time.UTC)
	detail := FlatDetail{
		Timestamp: base, Model: "routed-model", Provider: "codex", ClientModel: "client-alias",
		APIKey: "synthetic-client-key",
		UpstreamURL: "https://user:synthetic-secret@upstream.example.test/v1/responses?api_key=synthetic-key&region=" + strings.Repeat(
			"a", 1024,
		),
		ResponseURL:       "https://user:synthetic-secret@final.example.test/result?token=synthetic-key&region=test",
		ResponseURLSource: "http_response_request_url", ResponseURLStatus: "observed",
		UpstreamNextHopStatus: "not_reported", CacheReadReported: true,
		BillingMetadata: &coreusage.BillingMetadata{
			SchemaVersion: 1, ContextInputTokens: new(int64(15)), Region: "us", RegionSource: "usage.inference_geo",
			Modalities:      []coreusage.ModalityTokenUsage{{Category: "uncached_input", Modality: "text", Tokens: 1}},
			Charges:         []coreusage.BillingQuantity{{Kind: "web_search", Unit: "request", Quantity: 2}},
			ChargesReported: true,
		},
		UpstreamModel: "forwarded-model", ResponseModel: "returned-model", ResponseProvider: "reported-channel",
		ResponseProviderSource: "body.provider", RequestServiceTier: "priority", ResponseServiceTier: "default",
		RequestSpeed: "fast", ResponseSpeed: "standard", RequestID: "synthetic-request", Completed: true,
		Tokens: tokenStats{InputTokens: 1, OutputTokens: 2, TotalTokens: 3},
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := persistEvent(ctx, tx, store, &detail, nil); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	full, _, err := store.queryEventDetails(
		ctx, base.Add(-time.Minute), base.Add(time.Minute), EventFilters{}, 1, 50, "timestamp", false,
	)
	if err != nil || len(full) != 1 {
		t.Fatalf("full events: %+v, %v", full, err)
	}
	compact, _, err := store.queryCompactEventDetails(
		ctx, base.Add(-time.Minute), base.Add(time.Minute), EventFilters{}, 1, 50, "timestamp", false,
	)
	if err != nil || len(compact) != 1 {
		t.Fatalf("compact events: %+v, %v", compact, err)
	}
	detail.UpstreamURL = util.SanitizeUpstreamURL(detail.UpstreamURL)
	detail.ResponseURL = util.SanitizeUpstreamURL(detail.ResponseURL)
	detail.ServiceTier = detail.RequestServiceTier
	want := requestObservationFields(detail)
	for _, got := range []FlatDetail{full[0], compact[0]} {
		if !reflect.DeepEqual(requestObservationFields(got), want) {
			t.Fatalf("observations changed: got=%+v want=%+v", got, want)
		}
	}
	if compact[0].Cost != full[0].Cost || compact[0].CostStatus != full[0].CostStatus || !reflect.DeepEqual(
		compact[0].PricingReceipt, full[0].PricingReceipt,
	) {
		t.Fatal("compact view changed request pricing")
	}
	backup := append([]FlatDetail(nil), full...)
	if err := PrepareUsageEventExport(backup, "synthetic-export-secret"); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(backup)
	if err != nil {
		t.Fatal(err)
	}
	restored, _, err := parseAnyWithSecret(raw, "synthetic-export-secret")
	if err != nil || len(restored) != 1 {
		t.Fatalf("authenticated export/import: %+v, %v", restored, err)
	}
	if !reflect.DeepEqual(requestObservationFields(restored[0]), want) {
		t.Fatalf("authenticated backup lost observations: %+v", restored[0])
	}
	plain, err := json.Marshal(full)
	if err != nil {
		t.Fatal(err)
	}
	restored, _, err = parseAny(plain)
	if err != nil || len(restored) != 1 || !reflect.DeepEqual(requestObservationFields(restored[0]), want) {
		t.Fatalf("plain JSON lost observations: %+v, %v", restored, err)
	}
}

func TestRequestObservationsLegacyAndCSVCompatibility(t *testing.T) {
	ctx := context.Background()
	stats := NewRequestStatistics()
	record := coreusage.Record{
		Model: "routed-model", Provider: "claude", ClientModel: "client-alias", UpstreamModel: "forwarded-model",
		UpstreamURL: "https://upstream.example.test/v1/messages", ResponseModel: "returned-model",
		ResponseURL: "https://final.example.test/messages", ResponseURLSource: "http_response_request_url",
		ResponseURLStatus: "observed", UpstreamNextHopStatus: "not_reported", CacheReadReported: true,
		ResponseProvider: "reported-channel", ResponseProviderSource: "header.X-Upstream-Provider",
		RequestServiceTier: "priority", ResponseServiceTier: "standard", RequestSpeed: "fast",
		ResponseSpeed: "standard",
		RequestedAt:   time.Date(2026, 11, 18, 1, 0, 0, 0, time.UTC), APIKey: "synthetic-client-key", Completed: true,
	}
	record.BillingMetadata = &coreusage.BillingMetadata{
		SchemaVersion: 1, ContextInputTokens: new(int64(10)), Region: "us", RegionSource: "usage.inference_geo",
		Charges: []coreusage.BillingQuantity{{Kind: "web_search", Unit: "request", Quantity: 1}}, ChargesReported: true,
	}
	billingJSON, err := json.Marshal(record.BillingMetadata)
	if err != nil {
		t.Fatal(err)
	}
	stats.record(ctx, record)
	flat := flattenSnapshot(stats.snapshot())
	if len(flat) != 1 || flat[0].ClientModel != record.ClientModel || flat[0].UpstreamModel != record.UpstreamModel || flat[0].ResponseModel != record.ResponseModel ||
		flat[0].ResponseURL != record.ResponseURL || flat[0].ResponseURLSource != record.ResponseURLSource || flat[0].ResponseURLStatus != record.ResponseURLStatus || flat[0].UpstreamNextHopStatus != record.UpstreamNextHopStatus || !flat[0].CacheReadReported || flat[0].ResponseProviderSource != record.ResponseProviderSource || flat[0].RequestSpeed != "fast" || flat[0].ResponseSpeed != "standard" {
		t.Fatalf("legacy snapshot lost observations: %+v", flat)
	}
	var data bytes.Buffer
	writer := csv.NewWriter(&data)
	for _, row := range [][]string{
		{
			"timestamp", "model", "result", "input_tokens", "output_tokens", "total_tokens", "client_model",
			"upstream_url", "upstream_model", "response_model", "response_provider", "response_provider_source",
			"request_service_tier", "response_service_tier", "request_speed", "response_speed", "response_url",
			"response_url_source", "response_url_status", "upstream_next_hop_status", "cache_read_reported",
			"billing_metadata",
		},
		{
			"2026-11-18T01:00:00Z", "routed-model", "success", "1", "1", "2", "client-alias", record.UpstreamURL,
			"forwarded-model", "returned-model", "reported-channel", record.ResponseProviderSource, "priority",
			"standard", "fast", "standard", record.ResponseURL, record.ResponseURLSource, record.ResponseURLStatus,
			record.UpstreamNextHopStatus, "true", string(billingJSON),
		},
	} {
		if err := writer.Write(row); err != nil {
			t.Fatal(err)
		}
	}
	writer.Flush()
	if err := writer.Error(); err != nil {
		t.Fatal(err)
	}
	csvDetails, _, err := parseAny(data.Bytes())
	if err != nil || len(csvDetails) != 1 || csvDetails[0].UpstreamModel != "forwarded-model" || csvDetails[0].ResponseModel != "returned-model" || csvDetails[0].ResponseProviderSource != record.ResponseProviderSource ||
		!reflect.DeepEqual(
			csvDetails[0].BillingMetadata, record.BillingMetadata,
		) || csvDetails[0].ResponseURL != record.ResponseURL || csvDetails[0].ResponseURLSource != record.ResponseURLSource || csvDetails[0].ResponseURLStatus != record.ResponseURLStatus || csvDetails[0].UpstreamNextHopStatus != record.UpstreamNextHopStatus || !csvDetails[0].CacheReadReported || csvDetails[0].RequestServiceTier != "priority" || csvDetails[0].ResponseServiceTier != "standard" || csvDetails[0].RequestSpeed != "fast" || csvDetails[0].ResponseSpeed != "standard" {
		t.Fatalf("CSV lost observations: %+v, %v", csvDetails, err)
	}
	legacy, _, err := parseAny([]byte(`[{"timestamp":"2026-11-18T01:00:00Z","model":"old-model","service_tier":"priority","tokens":{"total_tokens":1}}]`))
	if err != nil || len(legacy) != 1 || legacy[0].RequestServiceTier != "priority" || legacy[0].ResponseServiceTier != "" || legacy[0].ResponseModel != "" || legacy[0].ResponseSpeed != "" || legacy[0].UpstreamURL != "" || legacy[0].ResponseURL != "" || legacy[0].ResponseURLStatus != "" || legacy[0].ResponseURLSource != "" || legacy[0].UpstreamNextHopStatus != "" || legacy[0].CacheReadReported {
		t.Fatalf("legacy request tier acquired unsupported response evidence: %+v, %v", legacy, err)
	}
	if strings.Contains(data.String(), "synthetic-secret") {
		t.Fatal("CSV disclosed URL credentials")
	}
}

func TestRequestObservationsResponseURLBoundsAndLegacy(t *testing.T) {
	for _, legacy := range []sql.NullString{
		{}, {Valid: true, String: `{"completed":true,"upstream_url":"https://initial.example.test"}`},
	} {
		var detail FlatDetail
		if err := decodeEventMetadata(&detail, legacy); err != nil {
			t.Fatal(err)
		}
		if detail.ResponseURL != "" || detail.ResponseURLSource != "" || detail.ResponseURLStatus != "" || detail.UpstreamNextHopStatus != "" || detail.CacheReadReported {
			t.Fatalf("old record acquired observations: %+v", detail)
		}
	}
	detail := FlatDetail{
		ResponseURL: "https://user:synthetic-secret@final.example.test/result?api_key=synthetic-secret&padding=" + strings.Repeat(
			"a", maxEventURLBytes,
		), ResponseURLSource: "http_response_request_url", ResponseURLStatus: "observed",
		UpstreamNextHopStatus: "not_reported", CacheReadReported: true,
	}
	raw, err := encodeEventMetadata(&detail)
	if err != nil {
		t.Fatal(err)
	}
	value, ok := raw.(string)
	if !ok || strings.Contains(value, "synthetic-secret") {
		t.Fatalf("unsafe encoded observation %v", raw)
	}
	var restored FlatDetail
	if err := decodeEventMetadata(&restored, sql.NullString{Valid: true, String: value}); err != nil {
		t.Fatal(err)
	}
	if len(restored.ResponseURL) > maxEventURLBytes || !restored.MetadataTruncated || !restored.CacheReadReported || restored.ResponseURLStatus != "observed" {
		t.Fatalf("bounded observation = %+v", restored)
	}
}

func TestRequestObservationsBillingMetadataValidation(t *testing.T) {
	for _, metadata := range []*coreusage.BillingMetadata{
		{SchemaVersion: 2}, {SchemaVersion: 1, ContextInputTokens: new(int64(-1))},
		{
			SchemaVersion: 1,
			Modalities:    []coreusage.ModalityTokenUsage{{Category: "output", Modality: "image", Tokens: -1}},
		},
		{
			SchemaVersion: 1,
			Charges:       []coreusage.BillingQuantity{{Kind: "generated_image", Unit: "image", Quantity: math.NaN()}},
		},
		{
			SchemaVersion: 1,
			Charges:       []coreusage.BillingQuantity{{Kind: "generated_image", Unit: "image", Quantity: math.Inf(1)}},
		},
	} {
		if _, err := encodeEventMetadata(&FlatDetail{BillingMetadata: metadata}); err == nil {
			t.Fatalf("invalid billing metadata accepted: %+v", metadata)
		}
	}
	metadata := &coreusage.BillingMetadata{SchemaVersion: 1, ContextInputTokens: new(int64(0)), ChargesReported: true}
	cloned := coreusage.CloneBillingMetadata(metadata)
	*cloned.ContextInputTokens = 1
	if *metadata.ContextInputTokens != 0 {
		t.Fatal("billing clone shared mutable context length")
	}
	for i := 0; i < 100; i++ {
		metadata.Charges = append(
			metadata.Charges, coreusage.BillingQuantity{
				Kind: "generated_image", Unit: "image", Quantity: 1, Resolution: strings.Repeat("x", 1000),
			},
		)
	}
	raw, err := encodeEventMetadata(&FlatDetail{BillingMetadata: metadata})
	if err != nil {
		t.Fatal(err)
	}
	var restored FlatDetail
	if err := decodeEventMetadata(&restored, sql.NullString{Valid: true, String: raw.(string)}); err != nil {
		t.Fatal(err)
	}
	if !restored.MetadataTruncated || restored.BillingMetadata == nil || restored.BillingMetadata.ChargesReported || len(restored.BillingMetadata.Charges) > maxEventHeaderNames {
		t.Fatalf("truncated charge metadata claimed completeness: %+v", restored.BillingMetadata)
	}
}
