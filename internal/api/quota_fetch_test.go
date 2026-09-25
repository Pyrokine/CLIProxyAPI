package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/quota"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestQuotaCredentialTypeForProviderAcceptsGeminiFileType(t *testing.T) {
	credType, ok := quotaCredentialTypeForProvider("gemini")
	if !ok {
		t.Fatalf("gemini provider was not accepted for quota registration")
	}
	if credType != quota.TypeGeminiCli {
		t.Fatalf("credential type = %q, want %q", credType, quota.TypeGeminiCli)
	}
}

func TestResponseServerTimeOffsetMilliseconds(t *testing.T) {
	observedAt := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	serverTime := observedAt.Add(2 * time.Minute)
	offset, ok := responseServerTimeOffsetMilliseconds(
		map[string][]string{"date": {serverTime.Format(http.TimeFormat)}},
		observedAt,
	)
	if !ok || offset != (2*time.Minute).Milliseconds() {
		t.Fatalf("server time offset = %d, %t, want %d, true", offset, ok, (2 * time.Minute).Milliseconds())
	}
}

func TestFetchAntigravityQuotaIncludesLoadCodeAssist(t *testing.T) {
	var requests []quota.APICallRequest
	body, err := fetchAntigravityQuota(
		func(req quota.APICallRequest) (*quota.APICallResponse, error) {
			requests = append(requests, req)
			switch req.URL {
			case geminiQuotaURL:
				return &quota.APICallResponse{
					StatusCode: 200,
					Header:     map[string][]string{"Date": {time.Now().UTC().Format(http.TimeFormat)}},
					Body:       json.RawMessage(`{"quota":"ok"}`),
				}, nil
			case antigravityLoadCodeAssistURL:
				return &quota.APICallResponse{
					StatusCode: 200, Body: json.RawMessage(`{"paidTier":{"id":"g1-ultra-tier","name":"Ultra"}}`),
				}, nil
			default:
				t.Fatalf("unexpected url: %s", req.URL)
				return nil, nil
			}
		},
		"antigravity-index",
	)
	if err != nil {
		t.Fatalf("fetchAntigravityQuota() error = %v", err)
	}
	if len(requests) != 2 {
		t.Fatalf("request count = %d, want 2", len(requests))
	}
	if requests[1].Data != `{"metadata":{"ideType":"ANTIGRAVITY"}}` {
		t.Fatalf("loadCodeAssist body = %q", requests[1].Data)
	}
	if requests[1].AuthIndex != "antigravity-index" {
		t.Fatalf("loadCodeAssist auth index = %q", requests[1].AuthIndex)
	}

	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(payload["quota"]) == 0 {
		t.Fatalf("missing quota payload")
	}
	if len(payload["codeAssist"]) == 0 {
		t.Fatalf("missing codeAssist payload")
	}
	if len(payload["server_time_offset_ms"]) == 0 {
		t.Fatal("missing server_time_offset_ms")
	}
}

func TestFetchAntigravityQuotaReportsPartialAuxiliaryFailure(t *testing.T) {
	result, err := fetchAntigravityQuotaResultContext(
		context.Background(),
		func(_ context.Context, req quota.APICallRequest) (*quota.APICallResponse, error) {
			switch req.URL {
			case geminiQuotaURL:
				return &quota.APICallResponse{
					StatusCode: http.StatusOK,
					Body:       json.RawMessage(`{"quota":"ok"}`),
				}, nil
			case antigravityLoadCodeAssistURL:
				return nil, errors.New("subscription unavailable")
			default:
				t.Fatalf("unexpected url: %s", req.URL)
				return nil, nil
			}
		},
		"antigravity-index",
	)
	if err != nil {
		t.Fatalf("fetchAntigravityQuotaResultContext() error = %v", err)
	}
	if result.Completeness != quota.FetchPartial || result.PartialError == "" ||
		result.Source != "antigravity_quota" || result.ObservedAt.IsZero() || len(result.Data) == 0 {
		t.Fatalf("partial quota result = %#v", result)
	}
}

func TestFetchCodexQuotaRejectsNullUsageWithoutFurtherRequests(t *testing.T) {
	calls := 0
	_, err := fetchCodexQuotaResultContext(
		context.Background(),
		func(_ context.Context, _ quota.APICallRequest) (*quota.APICallResponse, error) {
			calls++
			return &quota.APICallResponse{StatusCode: http.StatusOK, Body: json.RawMessage(`null`)}, nil
		},
		"mock-index",
	)
	if err == nil {
		t.Fatal("fetchCodexQuotaResultContext() accepted null usage data")
	}
	if calls != 1 {
		t.Fatalf("mock API calls = %d, want 1", calls)
	}
}

func TestFetchCodexQuotaIncludesResetCredits(t *testing.T) {
	var requests []quota.APICallRequest
	body, err := fetchCodexQuota(
		func(req quota.APICallRequest) (*quota.APICallResponse, error) {
			requests = append(requests, req)
			switch req.URL {
			case codexUsageURL:
				return &quota.APICallResponse{
					StatusCode: 200, Body: json.RawMessage(`{"rate_limit_reset_credits":{"available_count":1}}`),
				}, nil
			case codexRateLimitResetCreditsURL:
				return &quota.APICallResponse{
					StatusCode: 200,
					Body:       json.RawMessage(`{"available_count":1,"credits":[{"id":"credit-1","reset_type":"codex_rate_limits","status":"available","expires_at":"2026-07-06T08:00:00Z"}]}`),
				}, nil
			default:
				t.Fatalf("unexpected url: %s", req.URL)
				return nil, nil
			}
		},
		"codex-index",
	)
	if err != nil {
		t.Fatalf("fetchCodexQuota() error = %v", err)
	}
	if len(requests) != 2 {
		t.Fatalf("request count = %d, want 2", len(requests))
	}
	if requests[1].Header["OpenAI-Beta"] != "codex-1" {
		t.Fatalf("reset credits OpenAI-Beta header = %q", requests[1].Header["OpenAI-Beta"])
	}
	if requests[1].AuthIndex != "codex-index" {
		t.Fatalf("reset credits auth index = %q", requests[1].AuthIndex)
	}

	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(payload["rate_limit_reset_credits"]) == 0 {
		t.Fatalf("missing usage reset credit summary")
	}
	if len(payload["rate_limit_reset_credits_details"]) == 0 {
		t.Fatalf("missing reset credit details")
	}
}

func TestFetchCodexQuotaThroughLocalAPICallWire(t *testing.T) {
	tests := []struct {
		name             string
		detailsStatus    int
		wantCompleteness quota.FetchCompleteness
		wantPartial      bool
	}{
		{name: "complete", detailsStatus: http.StatusOK, wantCompleteness: quota.FetchComplete},
		{
			name: "partial details failure", detailsStatus: http.StatusServiceUnavailable,
			wantCompleteness: quota.FetchPartial, wantPartial: true,
		},
	}
	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				requests := make([]string, 0, 2)
				server := httptest.NewServer(
					http.HandlerFunc(
						func(w http.ResponseWriter, req *http.Request) {
							var call quota.APICallRequest
							if err := json.NewDecoder(req.Body).Decode(&call); err != nil {
								t.Errorf("decode api-call request: %v", err)
								return
							}
							if call.Method != http.MethodGet ||
								(call.URL != codexUsageURL && call.URL != codexRateLimitResetCreditsURL) {
								t.Errorf("quota refresh attempted a non-read-only request: %s %s", call.Method, call.URL)
								http.Error(w, "unexpected quota request", http.StatusBadRequest)
								return
							}
							if call.Header["User-Agent"] != "codex-tui/0.149.1 (Mac OS 26.5.2; arm64) iTerm.app/3.6.11 (codex-tui; 0.149.1)" {
								t.Errorf("quota User-Agent = %q", call.Header["User-Agent"])
							}
							if call.AuthIndex != "codex-index" {
								t.Errorf("quota auth index = %q", call.AuthIndex)
							}
							requests = append(requests, call.URL)
							status := http.StatusOK
							body := `{"rate_limit_reset_credits":{"available_count":2,"applicable_available_count":1}}`
							if call.URL == codexRateLimitResetCreditsURL {
								status = tt.detailsStatus
								body = `{"available_count":2,"applicable_available_count":1,"credits":[{"id":"credit-1","reset_type":"codex_rate_limits","status":"available","expires_at":"2026-10-01T00:00:00Z"}]}`
							}
							w.Header().Set("Content-Type", "application/json")
							_ = json.NewEncoder(w).Encode(
								map[string]any{
									"status_code": status,
									"header":      map[string][]string{"Content-Type": {"application/json"}},
									"body":        body,
								},
							)
						},
					),
				)
				t.Cleanup(server.Close)

				parsed, err := url.Parse(server.URL)
				if err != nil {
					t.Fatalf("parse test server URL: %v", err)
				}
				port, err := strconv.Atoi(parsed.Port())
				if err != nil {
					t.Fatalf("parse test server port: %v", err)
				}
				result, err := fetchCodexQuotaResultContext(
					context.Background(),
					quota.NewLocalAPICallContextFunc(port, ""),
					"codex-index",
				)
				if err != nil {
					t.Fatalf("fetch Codex quota: %v", err)
				}
				if result.Completeness != tt.wantCompleteness {
					t.Fatalf("completeness = %q, want %q", result.Completeness, tt.wantCompleteness)
				}
				if (result.PartialError != "") != tt.wantPartial {
					t.Fatalf("partial error = %q", result.PartialError)
				}
				if len(requests) != 2 || requests[0] != codexUsageURL || requests[1] != codexRateLimitResetCreditsURL {
					t.Fatalf("requests = %#v", requests)
				}
				var payload map[string]json.RawMessage
				if err = json.Unmarshal(result.Data, &payload); err != nil {
					t.Fatalf("decode result: %v", err)
				}
				if len(payload["rate_limit_reset_credits"]) == 0 {
					t.Fatal("missing usage reset credit summary")
				}
				if tt.wantPartial {
					if len(payload["rate_limit_reset_credits_error"]) == 0 {
						t.Fatal("missing reset credit details error")
					}
				} else if !strings.Contains(
					string(payload["rate_limit_reset_credits_details"]), "2026-10-01T00:00:00Z",
				) {
					t.Fatalf("reset credit details = %s", payload["rate_limit_reset_credits_details"])
				}
			},
		)
	}
}

func TestRegisterAuthManagerForQuotaUsesPersistedCredentialRevision(t *testing.T) {
	authManager := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{
		ID:       "auth-claude-revision",
		Provider: "claude",
		FileName: "claude.json",
		Attributes: map[string]string{
			coreauth.PersistedSHA256Attribute: strings.Repeat("a", 64),
		},
	}
	registered, err := authManager.Register(context.Background(), auth)
	if err != nil {
		t.Fatalf("register auth: %v", err)
	}
	scheduler := quota.NewScheduler(quota.DefaultConfig(), nil)
	registerAuthManagerForQuota(scheduler, authManager)
	key := quota.EntryKey(quota.TypeClaude, "claude.json")
	before := scheduler.GetStatus().Credentials[key]

	updated := registered.Clone()
	updated.Attributes[coreauth.PersistedSHA256Attribute] = strings.Repeat("b", 64)
	if _, err = authManager.Update(coreauth.WithSkipPersist(context.Background()), updated); err != nil {
		t.Fatalf("update auth digest: %v", err)
	}
	registerAuthManagerForQuota(scheduler, authManager)
	after := scheduler.GetStatus().Credentials[key]
	if before == nil || after == nil || after.Generation <= before.Generation ||
		after.Freshness != quota.FreshnessOutdated || len(after.Data) != 0 {
		t.Fatalf("quota binding before=%#v after=%#v", before, after)
	}
}

func TestRegisterAuthManagerForQuotaUsesRuntimeAuthIndex(t *testing.T) {
	authManager := coreauth.NewManager(nil, nil, nil)
	authPath := filepath.Join(t.TempDir(), "claude.json")
	auth := &coreauth.Auth{
		ID:       "auth-claude",
		Provider: "claude",
		FileName: filepath.Base(authPath),
		Attributes: map[string]string{
			"path":   authPath,
			"source": authPath,
		},
		Metadata: map[string]any{
			"type": "claude",
		},
	}
	registeredAuth, err := authManager.Register(context.Background(), auth)
	if err != nil {
		t.Fatalf("Register auth: %v", err)
	}
	legacyIndex := computeAuthIndex(filepath.Base(authPath))
	if registeredAuth.Index == legacyIndex {
		t.Fatalf("runtime auth index unexpectedly matches legacy filename hash: %s", registeredAuth.Index)
	}

	scheduler := quota.NewScheduler(quota.DefaultConfig(), nil)
	if count := registerAuthManagerForQuota(scheduler, authManager); count != 1 {
		t.Fatalf("registered quota auth count = %d, want 1", count)
	}

	entry := scheduler.GetStatus().Credentials[quota.EntryKey(quota.TypeClaude, filepath.Base(authPath))]
	if entry == nil {
		t.Fatalf("quota entry for %q was not registered", filepath.Base(authPath))
	}
	if entry.AuthIndex != registeredAuth.Index {
		t.Fatalf("auth index = %q, want runtime auth index %q", entry.AuthIndex, registeredAuth.Index)
	}
}
