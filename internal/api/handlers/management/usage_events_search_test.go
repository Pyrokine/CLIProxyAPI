package management

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/usage"
	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	"github.com/gin-gonic/gin"
)

func TestGetUsageEventsSearchSQLiteCompactAndFull(t *testing.T) {
	gin.SetMode(gin.TestMode)
	transport := http.DefaultTransport
	http.DefaultTransport = roundTripFunc(
		func(request *http.Request) (*http.Response, error) {
			t.Errorf("unexpected outbound request: %s %s", request.Method, request.URL.Redacted())
			return nil, fmt.Errorf("usage search test has no outbound transport")
		},
	)
	t.Cleanup(func() { http.DefaultTransport = transport })

	const keyA, keyB = "ab-synthetic-search-client-xy", "zz-synthetic-search-client-uv"
	const managementKey = "synthetic-usage-search-management-key"
	base := time.Date(2026, 11, 9, 1, 2, 3, 123456789, time.UTC)
	identityA := coreusage.HashAPIKeyIdentity(keyA)
	identitySource := usage.TagIdentityCredentialSource
	seeds := []usage.FlatDetail{
		{
			Model: "north blue team v1", Source: identitySource("account-a@example.test"), AuthIndex: "auth-a",
			APIKey: keyA,
		},
		{
			Model: "plain beta", Source: identitySource("source needle@example.test"),
			AuthIndex: "auth-b", APIKey: keyB, Failed: true,
		},
		{
			Model: "plain gamma", Source: identitySource("account-c@example.test"), AuthIndex: "index-needle",
			APIKey: keyA,
		},
		{Model: "plain delta", AuthIndex: "credential fallback needle", APIKey: keyB},
		{
			Model: "模型 含中文 needle", Source: identitySource("account-e@example.test"),
			AuthIndex: "auth-e", APIKey: keyA, Failed: true,
		},
		{
			Model: "north blue team v2", Source: identitySource("account-a@example.test"), AuthIndex: "auth-a",
			APIKey: keyA,
		},
		{
			Model: "north blue team excluded", Source: identitySource("account-a@example.test"),
			AuthIndex: "auth-f", APIKey: keyB,
		},
		{Model: "north blue", Source: identitySource("team"), AuthIndex: "auth-g"},
		{
			Model: "north blue team v1", Source: identitySource("account-a@example.test"),
			AuthIndex: "auth-a", APIKey: keyA, Failed: true,
		},
		{
			Model: "north blue team v1", Source: identitySource("account-a@example.test"), AuthIndex: "auth-a",
			APIKey: keyA,
		},
	}
	dir := t.TempDir()
	persister := usage.NewPersister(dir, config.UsageRetention{Days: -1})
	persister.Start(t.Context())
	t.Cleanup(persister.Stop)
	if !persister.IsReady() {
		t.Fatal("temporary SQLite unavailable")
	}
	for i := range seeds {
		event := &seeds[i]
		event.Timestamp = base.Add(time.Duration(i) * time.Second)
		if i == len(seeds)-1 {
			event.Timestamp = base.Add(-time.Minute)
		}
		event.Provider = "synthetic-provider"
		event.RequestID = fmt.Sprintf("search-request-%d", i)
		event.LatencyMs = int64(100 + i)
		event.TimeToFirstByteMs = int64(20 + i)
		event.TotalDurationMs = int64(200 + i)
		event.Completed = true
		event.Thinking = &usage.Thinking{Mode: "enabled", Intensity: "high"}
		event.UserAgent = "search-full-view-metadata"
		event.Tokens.InputTokens = 1<<53 + 1 + int64(i)
		event.Tokens.OutputTokens = 7
		event.Tokens.ReasoningTokens = 2
		event.Tokens.CachedTokens = 4
		event.Tokens.CacheReadTokens = 3
		event.Tokens.CacheWriteTokens = 1
		event.Tokens.TotalTokens = event.Tokens.InputTokens + 7
		if !persister.Record(*event) {
			t.Fatalf("record %d refused", i)
		}
	}
	// Drain and reopen so every assertion reads persisted SQLite events.
	persister.Stop()
	persister = usage.NewPersister(dir, config.UsageRetention{Days: -1})
	persister.Start(context.Background())
	t.Cleanup(persister.Stop)
	if !persister.IsReady() {
		t.Fatal("temporary SQLite did not reopen")
	}
	h := &Handler{
		cfg: &config.Config{
			APIKeys: []string{keyA, keyB},
			APIKeyAliases: map[string]string{
				keyA: "Search alias A", keyB: "Search alias B",
			},
		},
		usagePersister: persister,
		envSecret:      managementKey,
		failedAttempts: make(map[string]*attemptInfo),
	}
	router := gin.New()
	router.Use(h.MiddlewareWithConfig(nil))
	router.GET("/v0/management/usage/events", h.GetUsageEvents)

	intersection := url.Values{
		"model":   {"north blue team v1,north blue team v2"},
		"source":  {"synthetic-provider:account-a@example.test"},
		"api_key": {keyA},
		"status":  {"success"},
	}
	all := []int{0, 1, 2, 3, 4, 5, 6, 7, 8}
	phrase := []int{0, 5, 6, 8}
	cases := []struct {
		name     string
		search   string
		filters  url.Values
		want     []int
		page     int
		pageSize int
		desc     bool
	}{
		{name: "model_substring_case_insensitive", search: "BLUE TEAM", want: phrase},
		{name: "source_and_credential", search: "source needle", want: []int{1}},
		{name: "auth_index_with_distinct_source", search: "index-needle", want: []int{2}},
		{name: "credential_auth_index_fallback", search: "fallback needle", want: []int{3}},
		{name: "persisted_api_key_identity", search: identityA, want: []int{0, 2, 4, 5, 8}},
		{name: "unicode_substring", search: "含中文", want: []int{4}},
		{name: "no_match", search: "absent-search-marker"},
		{name: "empty_search", want: all},
		{name: "whitespace_search", search: " \t\n ", want: all},
		{name: "trimmed_phrase", search: " \tblue team\n ", want: phrase},
		{name: "phrase_does_not_join_columns", search: "north blue team", want: phrase},
		{name: "reversed_words_do_not_match", search: "team blue"},
		{name: "internal_whitespace_is_preserved", search: "blue  team"},
		{name: "regex_is_not_interpreted", search: "^north.*"},
		{name: "sql_text_is_bound", search: "' OR 1=1 --"},
		{name: "existing_like_percent", search: "blue%team", want: phrase},
		{name: "existing_like_underscore", search: "blue_team", want: phrase},
		{
			name: "filters_intersect_before_page_one", search: "blue team", filters: intersection,
			want: []int{0, 5}, pageSize: 1,
		},
		{
			name: "filters_intersect_before_page_two", search: "blue team", filters: intersection,
			want: []int{0, 5}, page: 2, pageSize: 1,
		},
		{
			name: "page_past_filtered_results", search: "blue team", filters: intersection,
			want: []int{0, 5}, page: 3, pageSize: 1,
		},
		{
			name: "descending_filtered_page", search: "blue team", filters: intersection,
			want: []int{5, 0}, pageSize: 1, desc: true,
		},
		{
			name:    "status_intersection",
			search:  "blue team",
			filters: url.Values{"status": {"failure"}},
			want:    []int{8},
		},
		{
			name:    "matching_search_unknown_model",
			search:  "blue team",
			filters: url.Values{"model": {"absent-model"}},
		},
		{
			name: "matching_search_other_credential", search: "blue team",
			filters: url.Values{"source": {"synthetic-provider:account-c@example.test"}},
		},
		{
			name: "matching_search_repeated_api_keys", search: "blue team",
			filters: url.Values{"api_key": {keyA, keyB}}, want: phrase,
		},
		{name: "matching_search_wrong_key", search: "index-needle", filters: url.Values{"api_key": {keyB}}},
		{
			name: "inclusive_nanosecond_time_intersection", search: "blue team", filters: url.Values{
				"from": {
					seeds[5].Timestamp.Format(time.RFC3339Nano),
				}, "to": {seeds[6].Timestamp.Format(time.RFC3339Nano)},
			}, want: []int{5, 6},
		},
		{
			name:    "wider_range_includes_persisted_earlier_event",
			filters: url.Values{"from": {seeds[9].Timestamp.Format(time.RFC3339Nano)}},
			want:    []int{9, 0, 1, 2, 3, 4, 5, 6, 7, 8},
		},
	}
	for _, tc := range cases {
		t.Run(
			tc.name, func(t *testing.T) {
				page, pageSize := max(tc.page, 1), tc.pageSize
				if pageSize == 0 {
					pageSize = 50
				}
				values := url.Values{
					"from": {
						base.Format(time.RFC3339Nano),
					}, "to": {base.Add(time.Minute).Format(time.RFC3339Nano)},
					"search": {tc.search}, "page": {strconv.Itoa(page)}, "page_size": {strconv.Itoa(pageSize)},
					"sort": {"timestamp"}, "order": {"asc"},
				}
				if tc.desc {
					values.Set("order", "desc")
				}
				maps.Copy(values, tc.filters)
				start := min((page-1)*pageSize, len(tc.want))
				wantPage := tc.want[start:min(start+pageSize, len(tc.want))]
				var full compactEventsResponse
				for _, view := range []string{"full", "compact"} {
					t.Run(
						view, func(t *testing.T) {
							if view == "compact" {
								values.Set("view", "compact")
							}
							request := httptest.NewRequest(
								http.MethodGet, "/v0/management/usage/events?"+values.Encode(), nil,
							)
							request.RemoteAddr = "127.0.0.1:12345"
							request.Header.Set("Authorization", "Bearer "+managementKey)
							recorder := httptest.NewRecorder()
							router.ServeHTTP(recorder, request)
							if recorder.Code != http.StatusOK {
								t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
							}
							var response compactEventsResponse
							if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
								t.Fatal(err)
							}
							if response.Total !=
								len(tc.want) ||
								response.Page !=
									page ||
								response.PageSize !=
									pageSize ||
								response.TotalPages != (len(tc.want)+pageSize-1)/pageSize ||
								len(response.Events) != len(wantPage) ||
								response.Events == nil {
								t.Fatalf(
									"pagination=%+v want total=%d page=%d page_size=%d indices=%v",
									response, len(tc.want), page, pageSize, wantPage,
								)
							}
							for i, seedIndex := range wantPage {
								actual, want := response.Events[i], seeds[seedIndex]
								source, _, _ := usage.DecodeCredentialSource(want.Source)
								display, alias := "", ""
								switch want.APIKey {
								case keyA:
									display, alias = "ab...xy", "Search alias A"
								case keyB:
									display, alias = "zz...uv", "Search alias B"
								}
								if !actual.Timestamp.Equal(want.Timestamp) ||
									actual.Model !=
										want.Model ||
									actual.Source !=
										source ||
									actual.AuthIndex !=
										want.AuthIndex ||
									actual.APIKey !=
										display ||
									actual.APIKeyAlias !=
										alias ||
									actual.Provider !=
										want.Provider ||
									actual.RequestID !=
										want.RequestID ||
									actual.Failed !=
										want.Failed ||
									!reflect.DeepEqual(actual.LatencyMs, want.RecordedLatencyMs()) ||
									actual.TimeToFirstByteMs !=
										want.TimeToFirstByteMs ||
									actual.TotalDurationMs != want.TotalDurationMs || !actual.Completed ||
									!actual.MetadataRecorded ||
									!reflect.DeepEqual(actual.Thinking, want.Thinking) {
									t.Fatalf(
										"event[%d]=%+v does not match seed[%d]=%+v",
										i,
										actual,
										seedIndex,
										want,
									)
								}
								if actual.Tokens != (compactUsageEventTokens{
									Input: want.Tokens.InputTokens, Output: 7, Reasoning: 2, Cached: 4, CacheRead: 3,
									CacheWrite: 1,
									Total:      want.Tokens.TotalTokens,
								}) {
									t.Fatalf("event[%d] token precision changed: %+v", i, actual.Tokens)
								}
							}
							var wire struct {
								Events []map[string]json.RawMessage `json:"events"`
							}
							if err := json.Unmarshal(recorder.Body.Bytes(), &wire); err != nil {
								t.Fatal(err)
							}
							for _, event := range wire.Events {
								if view == "compact" && event["user_agent"] != nil {
									t.Fatal("compact response included full-only metadata")
								}
								if view == "full" && !bytes.Equal(
									event["user_agent"], []byte(`"search-full-view-metadata"`),
								) {
									t.Fatal("full response lost full-only metadata")
								}
								for _, name := range []string{"raw_api_key", "import_data"} {
									if event[name] != nil {
										t.Fatalf("ordinary search response included export-only %s", name)
									}
								}
							}
							for _, secret := range []string{keyA, keyB, identityA} {
								if strings.Contains(recorder.Body.String(), secret) {
									t.Fatal("search response disclosed canonical client key identity")
								}
							}
							if view == "full" {
								full = response
							} else if !reflect.DeepEqual(response, full) {
								t.Fatalf("compact/full mapping diverged\ncompact=%+v\nfull=%+v", response, full)
							}
						},
					)
				}
			},
		)
	}
}
