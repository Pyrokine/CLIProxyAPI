package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/usage"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

func TestGetUsageSummaryCanonicalAPIKeyAliases(t *testing.T) {
	const rawA, rawB = "abcd-first-wxyz", "abcd-second-wxyz"
	const shortMask = "ab...yz"
	identityA, identityB := coreusage.HashAPIKeyIdentity(rawA), coreusage.HashAPIKeyIdentity(rawB)
	for _, storage := range []string{"memory", "sqlite"} {
		t.Run(
			storage, func(t *testing.T) {
				historicalIdentity, historicalDisplay := "(none)", "(none)"
				if storage == "memory" {
					historicalIdentity = coreusage.HashAPIKeyIdentity("codex")
					historicalDisplay = maskUsageAPIKey(historicalIdentity)
				}
				stats := usage.NewRequestStatistics()
				var persister *usage.Persister
				dir := t.TempDir()
				if storage == "sqlite" {
					persister = usage.NewPersister(dir, config.UsageRetention{Days: -1})
					persister.Start(context.Background())
					if !persister.IsReady() {
						t.Fatal("synthetic SQLite unavailable")
					}
				}
				logger := usage.NewLoggerPlugin(persister, stats, func() bool { return true })
				base := time.Date(2026, 11, 20, 1, 0, 0, 0, time.UTC)
				for index, key := range []string{rawA, rawB, rawA, ""} {
					at := base.Add(time.Duration(index) * time.Minute)
					if key == "" {
						at = base.Add(time.Hour)
					}
					logger.HandleUsage(
						context.Background(), coreusage.Record{
							APIKey: key, Model: "alias-model", Provider: "codex", Completed: true,
							RequestedAt: at, Detail: coreusage.Detail{InputTokens: 1, OutputTokens: 1, TotalTokens: 2},
						},
					)
				}
				if persister != nil {
					persister.Stop()
					persister = usage.NewPersister(dir, config.UsageRetention{Days: -1})
					persister.Start(context.Background())
					t.Cleanup(persister.Stop)
					if !persister.IsReady() {
						t.Fatal("synthetic SQLite did not reopen")
					}
				}
				makeHandler := func(cfg *config.Config) *Handler {
					h := NewHandler(cfg, "", nil, withEnvSecret(""), withUsagePersister(persister))
					h.SetUsageStatistics(stats)
					return h
				}
				h := makeHandler(
					&config.Config{
						APIKeys: []string{rawB, rawA}, APIKeyAliases: map[string]string{rawA: "First", rawB: "Second"},
					},
				)
				basePath := "/usage/summary?from=2026-11-20T01:00:00Z&to=2026-11-20T02:59:00Z"
				request := func(handler *Handler, path string) (summaryResponse, map[string]map[string]any, string) {
					t.Helper()
					ctx, rec := newArtifactCacheTestContext(http.MethodGet, path, "")
					handler.GetUsageSummary(ctx)
					if rec.Code != http.StatusOK {
						t.Fatalf("summary status=%d body=%s", rec.Code, rec.Body.String())
					}
					var result summaryResponse
					var fields struct {
						ByAPIKey map[string]map[string]any `json:"by_api_key"`
					}
					if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
						t.Fatal(err)
					}
					if err := json.Unmarshal(rec.Body.Bytes(), &fields); err != nil {
						t.Fatal(err)
					}
					for _, secret := range []string{rawA, rawB} {
						if strings.Contains(rec.Body.String(), secret) {
							t.Fatalf("summary exposed raw key %q", secret)
						}
					}
					return result, fields.ByAPIKey, rec.Body.String()
				}
				legacy, legacyFields, legacyBody := request(h, basePath)
				if len(legacy.ByAPIKey) != 2 || legacy.ByAPIKey[shortMask] == nil || legacy.ByAPIKey[shortMask].Requests != 3 {
					t.Fatalf("default masked contract changed: %+v", legacy.ByAPIKey)
				}
				for _, fields := range legacyFields {
					for _, field := range []string{"alias", "display_name"} {
						if _, exists := fields[field]; exists {
							t.Fatalf("default response acquired %s", field)
						}
					}
				}

				canonicalPath := basePath + "&api_key_identity=canonical"
				canonical, fields, _ := request(h, canonicalPath)
				if len(canonical.ByAPIKey) != 3 || !reflect.DeepEqual(canonical.Totals, legacy.Totals) ||
					!reflect.DeepEqual(canonical.ByModel, legacy.ByModel) || !reflect.DeepEqual(
					canonical.ByCredential, legacy.ByCredential,
				) {
					t.Fatalf(
						"canonical view buckets=%d, totals=%d/%d", len(canonical.ByAPIKey), canonical.Totals.Requests,
						legacy.Totals.Requests,
					)
				}
				for identity, want := range map[string]struct {
					count int64
					alias string
				}{identityA: {2, "First"}, identityB: {1, "Second"}, historicalIdentity: {1, ""}} {
					keyStats := canonical.ByAPIKey[identity]
					if keyStats == nil || keyStats.Requests != want.count {
						t.Fatalf("canonical identity %q lost distinct counts: %+v", identity, canonical.ByAPIKey)
					}
					if fields[identity]["alias"] != want.alias && !(want.alias == "" && fields[identity]["alias"] == nil) {
						t.Fatalf("identity %q alias=%v want %q", identity, fields[identity]["alias"], want.alias)
					}
					wantDisplay := shortMask
					if identity == historicalIdentity {
						wantDisplay = historicalDisplay
					}
					if fields[identity]["display_name"] != wantDisplay {
						t.Fatalf(
							"identity %q display=%v want %q", identity, fields[identity]["display_name"], wantDisplay,
						)
					}
					var count int64
					for _, point := range canonical.TimeSeriesByAPIKey[identity] {
						count += point.Requests
					}
					if count != want.count {
						t.Fatalf("time series identity %q count=%d want %d", identity, count, want.count)
					}
				}
				if len(canonical.TimeSeriesByAPIKey) != 3 {
					t.Fatalf("canonical time series still merged: %+v", canonical.TimeSeriesByAPIKey)
				}
				_, _, afterCanonical := request(h, basePath)
				if legacyBody != afterCanonical {
					t.Fatal("canonical request changed the following default response")
				}

				for _, filter := range []struct {
					value string
					count int64
					keys  int
				}{
					{identityA, 2, 1}, {identityB, 1, 1}, {rawA, 2, 1}, {shortMask, 3, 2},
					{util.HideAPIKey(rawA), 3, 2}, {util.HideAPIKey(rawA) + "#1", 2, 1},
					{util.HideAPIKey(rawA) + "#2", 1, 1}, {historicalIdentity, 1, 1},
				} {
					filtered, _, _ := request(h, canonicalPath+"&api_key="+url.QueryEscape(filter.value))
					if filtered.Totals.Requests != filter.count || len(filtered.ByAPIKey) != filter.keys {
						t.Fatalf(
							"filter %q changed: count=%d keys=%d", filter.value, filtered.Totals.Requests,
							len(filtered.ByAPIKey),
						)
					}
				}
				combined, _, _ := request(
					h, canonicalPath+"&api_key="+url.QueryEscape(identityA)+"&api_key="+url.QueryEscape(identityB),
				)
				if combined.Totals.Requests != 3 || len(combined.ByAPIKey) != 2 {
					t.Fatalf("repeated canonical filters changed: %+v", combined)
				}

				renamedHandler := makeHandler(
					&config.Config{
						APIKeys: []string{rawA}, APIKeyAliases: map[string]string{rawA: "Renamed"},
					},
				)
				renamed, renamedFields, _ := request(renamedHandler, canonicalPath)
				if !reflect.DeepEqual(renamed.TimeSeriesByAPIKey, canonical.TimeSeriesByAPIKey) ||
					renamed.ByAPIKey[identityB] == nil || renamedFields[identityA]["alias"] != "Renamed" ||
					renamedFields[identityB]["alias"] != nil {
					t.Fatalf("alias/config changes reassigned history: %+v", renamedFields)
				}
				retired, _, _ := request(renamedHandler, canonicalPath+"&api_key="+url.QueryEscape(identityB))
				if retired.Totals.Requests != 1 || retired.ByAPIKey[identityB] == nil {
					t.Fatalf("retired key identity stopped filtering history: %+v", retired)
				}
			},
		)
	}
}

func TestGetUsageSummaryRejectsUnknownAPIKeyIdentity(t *testing.T) {
	h := NewHandler(&config.Config{}, "", nil, withEnvSecret(""))
	ctx, rec := newArtifactCacheTestContext(
		http.MethodGet, "/usage/summary?from=2026-11-20T01:00:00Z&to=2026-11-20T02:59:00Z&api_key_identity=raw", "",
	)
	h.GetUsageSummary(ctx)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid identity mode status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestUsageCanonicalNoneIdentityFiltersAllViews(t *testing.T) {
	const rawOther, backupKey = "synthetic-other-client", "synthetic-none-backup"
	literalIdentity := coreusage.HashAPIKeyIdentity("(none)")
	otherIdentity := coreusage.HashAPIKeyIdentity(rawOther)
	dir := t.TempDir()
	persister := usage.NewPersister(dir, config.UsageRetention{Days: -1})
	persister.Start(context.Background())
	t.Cleanup(func() { persister.Stop() })
	if !persister.IsReady() {
		t.Fatal("synthetic SQLite unavailable")
	}
	for index, key := range []string{"", "(none)", rawOther} {
		persister.Record(
			usage.FlatDetail{
				Timestamp: time.Date(2026, 11, 20, 1, index, 0, 0, time.UTC),
				APIKey:    key, RequestID: []string{"no-key", "literal-none", "other-key"}[index],
				Model: "none-model", Completed: true,
			},
		)
	}
	persister.Stop()
	persister = usage.NewPersister(dir, config.UsageRetention{Days: -1})
	persister.Start(context.Background())
	if !persister.IsReady() {
		t.Fatal("synthetic SQLite did not reopen")
	}
	h := NewHandler(
		&config.Config{
			APIKeys: []string{"(none)", rawOther}, APIKeyAliases: map[string]string{"(none)": "Literal none"},
		}, "", nil, withEnvSecret(""), withUsagePersister(persister),
	)
	request := func(endpoint, suffix string) *httptest.ResponseRecorder {
		t.Helper()
		ctx, response := newArtifactCacheTestContext(
			http.MethodGet,
			endpoint+"?from=2026-11-20T01:00:00Z&to=2026-11-20T02:00:00Z"+suffix, "",
		)
		ctx.Request.Header.Set(usageBackupKeyHeader, backupKey)
		switch endpoint {
		case "/usage/summary":
			h.GetUsageSummary(ctx)
		case "/usage/events":
			h.GetUsageEvents(ctx)
		case "/usage/export":
			h.ExportUsageStatistics(ctx)
		default:
			t.Fatalf("unexpected endpoint %q", endpoint)
		}
		if strings.Contains(response.Body.String(), rawOther) {
			t.Fatal("response exposed a synthetic raw key")
		}
		return response
	}
	for _, tc := range []struct {
		name, mode string
		keys       []string
		identities []string
		requestIDs []string
	}{
		{"canonical empty", "canonical", []string{"(none)"}, []string{"(none)"}, []string{"no-key"}},
		{
			"canonical literal", "canonical", []string{literalIdentity}, []string{literalIdentity},
			[]string{"literal-none"},
		},
		{
			"canonical repeated", "canonical", []string{"(none)", otherIdentity}, []string{"(none)", otherIdentity},
			[]string{"no-key", "other-key"},
		},
		{
			"canonical empty and literal", "canonical", []string{"(none)", literalIdentity},
			[]string{"(none)", literalIdentity}, []string{"no-key", "literal-none"},
		},
		{
			"canonical CSV", "canonical", []string{"(none)," + otherIdentity}, []string{"(none)", otherIdentity},
			[]string{"no-key", "other-key"},
		},
		{"legacy empty and literal", "", []string{"(none)"}, nil, []string{"no-key", "literal-none"}},
	} {
		t.Run(
			tc.name, func(t *testing.T) {
				query := url.Values{"api_key": tc.keys}
				if tc.mode != "" {
					query.Set("api_key_identity", tc.mode)
				}
				suffix := "&" + query.Encode()
				summary := request("/usage/summary", suffix)
				var payload summaryResponse
				if err := json.Unmarshal(summary.Body.Bytes(), &payload); err != nil || summary.Code != http.StatusOK {
					t.Fatalf("summary status=%d error=%v body=%s", summary.Code, err, summary.Body.String())
				}
				if payload.Totals.Requests != int64(len(tc.requestIDs)) {
					t.Fatalf("summary count=%d want %d", payload.Totals.Requests, len(tc.requestIDs))
				}
				if tc.mode == "canonical" {
					if len(payload.ByAPIKey) != len(tc.identities) || len(payload.TimeSeriesByAPIKey) != len(tc.identities) {
						t.Fatalf(
							"canonical groups=%d series=%d want %d", len(payload.ByAPIKey),
							len(payload.TimeSeriesByAPIKey), len(tc.identities),
						)
					}
					for _, identity := range tc.identities {
						var count int64
						for _, point := range payload.TimeSeriesByAPIKey[identity] {
							count += point.Requests
						}
						if payload.ByAPIKey[identity] == nil || payload.ByAPIKey[identity].Requests != 1 || count != 1 {
							t.Fatalf("identity %q did not retain its own request", identity)
						}
					}
				}
				for _, view := range []string{"", "&view=compact", "&export=true"} {
					response := request("/usage/events", suffix+view)
					var events struct {
						Total  int `json:"total"`
						Events []struct {
							RequestID  string `json:"request_id"`
							ImportData string `json:"import_data"`
						} `json:"events"`
					}
					if err := json.Unmarshal(
						response.Body.Bytes(), &events,
					); err != nil || response.Code != http.StatusOK {
						t.Fatalf(
							"events %q status=%d error=%v body=%s", view, response.Code, err, response.Body.String(),
						)
					}
					seen := make(map[string]bool)
					for _, event := range events.Events {
						seen[event.RequestID] = true
						if view == "&export=true" && !strings.HasPrefix(event.ImportData, "usage-event-v1:") {
							t.Fatal("event export omitted its sealed identity")
						}
					}
					if events.Total != len(tc.requestIDs) || len(events.Events) != len(tc.requestIDs) {
						t.Fatalf(
							"events %q count=%d/%d want %d", view, events.Total, len(events.Events), len(tc.requestIDs),
						)
					}
					for _, id := range tc.requestIDs {
						if !seen[id] {
							t.Fatalf("events %q missing request %q", view, id)
						}
					}
				}
				exported := request("/usage/export", suffix)
				events, err := usage.ParseUsageBackup(exported.Body.Bytes(), backupKey)
				if err != nil || exported.Code != http.StatusOK || len(events) != len(tc.requestIDs) {
					t.Fatalf("backup status=%d events=%d error=%v", exported.Code, len(events), err)
				}
				for index, event := range events {
					if event.RequestID != tc.requestIDs[index] {
						t.Fatalf("backup request=%q want %q", event.RequestID, tc.requestIDs[index])
					}
				}
			},
		)
	}
	for _, endpoint := range []string{"/usage/summary", "/usage/events", "/usage/export"} {
		response := request(endpoint, "&api_key_identity=raw")
		if response.Code != http.StatusBadRequest || !strings.Contains(
			response.Body.String(), "invalid api_key_identity",
		) {
			t.Fatalf("%s accepted unknown mode: status=%d body=%s", endpoint, response.Code, response.Body.String())
		}
	}
}
