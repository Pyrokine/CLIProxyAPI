package api

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
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

func TestFetchClaudeQuotaUsesCLIUserAgent(t *testing.T) {
	calls := 0
	result, err := fetchClaudeQuotaResultContext(
		context.Background(),
		func(_ context.Context, req quota.APICallRequest) (*quota.APICallResponse, error) {
			calls++
			if req.Header["User-Agent"] != "claude-cli/2.1.280 (external, cli)" {
				t.Errorf("quota user agent = %q", req.Header["User-Agent"])
			}
			if req.AuthIndex != "claude-fixture" || req.Method != http.MethodGet ||
				(req.URL != "https://api.anthropic.com/api/oauth/usage?cedar_ember=1&skip_spend=1" && req.URL != claudeProfileURL) {
				t.Fatalf("unexpected Claude quota request: url=%q method=%q", req.URL, req.Method)
			}
			return &quota.APICallResponse{StatusCode: http.StatusOK, Body: json.RawMessage(`{}`)}, nil
		},
		"claude-fixture",
	)
	if err != nil || calls != 2 || result.Source != "claude_usage" {
		t.Fatalf("fetchClaudeQuotaResultContext() calls=%d source=%q error=%v", calls, result.Source, err)
	}
}

func TestFetchClaudeQuotaPreservesReadOnlyResetGrants(t *testing.T) {
	const usage = `{"five_hour":{"utilization":72},"cedar_ember":{"eligible":true,"grants":[{"id":"fixture","resets_total":4,"resets_left":2,"ends_at":"2026-11-01T00:00:00Z"}]}}`
	var requests []quota.APICallRequest
	result, err := fetchClaudeQuotaResultContext(context.Background(), func(_ context.Context, req quota.APICallRequest) (*quota.APICallResponse, error) {
		requests = append(requests, req)
		if req.Method != http.MethodGet || req.AuthIndex != "fixture-claude" || req.Data != "" {
			t.Fatalf("quota read changed credential or issued a mutation: %#v", req)
		}
		switch req.URL {
		case "https://api.anthropic.com/api/oauth/usage?cedar_ember=1&skip_spend=1":
			return &quota.APICallResponse{StatusCode: http.StatusOK, Body: json.RawMessage(usage)}, nil
		case claudeProfileURL:
			return nil, errors.New("fixture profile unavailable")
		default:
			t.Fatalf("unexpected Claude request: %s", req.URL)
			return nil, nil
		}
	}, "fixture-claude")
	if err != nil || len(requests) != 2 || result.Completeness != quota.FetchPartial {
		t.Fatalf("quota result = %#v, error = %v, requests = %d", result, err, len(requests))
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(result.Data, &payload); err != nil {
		t.Fatal(err)
	}
	if string(payload["usage"]) != usage {
		t.Fatalf("usage and grants changed: %s", payload["usage"])
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
		"",
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
		name               string
		detailsStatus      int
		subscriptionStatus int
		wantCompleteness   quota.FetchCompleteness
	}{
		{"complete", http.StatusOK, http.StatusOK, quota.FetchComplete},
		{"details unavailable", http.StatusServiceUnavailable, http.StatusOK, quota.FetchPartial},
		{"details forbidden", http.StatusForbidden, http.StatusOK, quota.FetchPartial},
		{"subscription forbidden", http.StatusOK, http.StatusForbidden, quota.FetchPartial},
		{"both forbidden", http.StatusForbidden, http.StatusForbidden, quota.FetchPartial},
	}
	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				requests := make([]string, 0, 3)
				accountID := "account/id + space"
				server := httptest.NewServer(
					http.HandlerFunc(
						func(w http.ResponseWriter, req *http.Request) {
							var call quota.APICallRequest
							if err := json.NewDecoder(req.Body).Decode(&call); err != nil {
								t.Errorf("decode api-call request: %v", err)
								return
							}
							if call.Method != http.MethodGet ||
								(call.URL != codexUsageURL && call.URL != codexRateLimitResetCreditsURL &&
									!strings.HasPrefix(call.URL, codexSubscriptionsURL+"?account_id=")) {
								t.Errorf("quota refresh attempted a non-read-only request: %s %s", call.Method, call.URL)
								http.Error(w, "unexpected quota request", http.StatusBadRequest)
								return
							}
							if call.Header["User-Agent"] != "codex-tui/0.149.1 (Mac OS 26.5.2; arm64) iTerm.app/3.6.11 (codex-tui; 0.149.1)" {
								t.Errorf("quota User-Agent = %q", call.Header["User-Agent"])
							}
							if call.AuthIndex != "codex-index" || call.Header["Chatgpt-Account-Id"] != accountID || call.Data != "" {
								t.Errorf("quota request changed account scope or body: auth=%q account=%q", call.AuthIndex, call.Header["Chatgpt-Account-Id"])
							}
							requests = append(requests, call.URL)
							status := http.StatusOK
							body := `{"rate_limit_reset_credits":{"available_count":2,"applicable_available_count":1}}`
							if strings.HasPrefix(call.URL, codexSubscriptionsURL+"?") {
								subscriptionURL, err := url.Parse(call.URL)
								if err != nil || subscriptionURL.Query().Get("account_id") != accountID ||
									call.Header["Chatgpt-Account-Id"] != accountID {
									t.Errorf("subscription request = %#v, url error = %v", call, err)
								}
								status = tt.subscriptionStatus
								body = `{"active_until":"2026-10-03T13:27:01Z"}`
							}
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
					accountID,
				)
				if err != nil {
					t.Fatalf("fetch Codex quota: %v", err)
				}
				if result.Completeness != tt.wantCompleteness {
					t.Fatalf("completeness = %q, want %q", result.Completeness, tt.wantCompleteness)
				}
				if (result.PartialError != "") != (tt.wantCompleteness == quota.FetchPartial) {
					t.Fatalf("partial error = %q", result.PartialError)
				}
				if len(requests) != 3 || requests[0] != codexUsageURL || requests[1] != codexRateLimitResetCreditsURL ||
					!strings.HasPrefix(requests[2], codexSubscriptionsURL+"?account_id=") {
					t.Fatalf("requests = %#v", requests)
				}
				var payload map[string]json.RawMessage
				if err = json.Unmarshal(result.Data, &payload); err != nil {
					t.Fatalf("decode result: %v", err)
				}
				if len(payload["rate_limit_reset_credits"]) == 0 {
					t.Fatal("missing usage reset credit summary")
				}
				if tt.subscriptionStatus == http.StatusOK {
					if string(payload["subscription_active_until"]) != `"2026-10-03T13:27:01Z"` ||
						string(payload["subscription_active_until_source"]) != `"codex_subscriptions"` {
						t.Fatalf("live subscription fields = %s, %s", payload["subscription_active_until"], payload["subscription_active_until_source"])
					}
				} else if !strings.Contains(string(payload["subscription_error"]), "codex subscription returned status 403") ||
					!strings.Contains(result.PartialError, "codex subscription returned status 403") ||
					len(payload["subscription_active_until"]) != 0 {
					t.Fatalf("subscription error lost stage or status: data=%s error=%q", result.Data, result.PartialError)
				}
				if tt.detailsStatus != http.StatusOK {
					want := "codex reset credits returned status " + strconv.Itoa(tt.detailsStatus)
					if !strings.Contains(string(payload["rate_limit_reset_credits_error"]), want) ||
						!strings.Contains(result.PartialError, want) || len(payload["rate_limit_reset_credits_details"]) != 0 {
						t.Fatalf("reset credit error lost stage or status: data=%s error=%q", result.Data, result.PartialError)
					}
				} else if !strings.Contains(string(payload["rate_limit_reset_credits_details"]), "2026-10-01T00:00:00Z") {
					t.Fatalf("reset credit details = %s", payload["rate_limit_reset_credits_details"])
				}
			},
		)
	}
}

func TestFetchCodexQuotaAccountHeadersDoNotLeak(t *testing.T) {
	usageBefore := maps.Clone(codexHeaders)
	resetBefore := maps.Clone(codexResetCreditsHeaders)
	var captured []quota.APICallRequest
	for _, accountID := range []string{"synthetic-a", "synthetic-b", "", "invalid\naccount", strings.Repeat("x", 257)} {
		wantAccount := accountID
		if accountID == "" || strings.Contains(accountID, "\n") || len(accountID) > 256 {
			wantAccount = ""
		}
		calls := 0
		_, err := fetchCodexQuotaResultContext(context.Background(), func(_ context.Context, req quota.APICallRequest) (*quota.APICallResponse, error) {
			calls++
			if req.Method != http.MethodGet || req.Data != "" || req.Header["Chatgpt-Account-Id"] != wantAccount {
				t.Fatalf("invalid account scope or mutation: method=%q account=%q", req.Method, req.Header["Chatgpt-Account-Id"])
			}
			captured = append(captured, req)
			return &quota.APICallResponse{StatusCode: http.StatusOK, Body: json.RawMessage(`{"active_until":"2026-11-01T00:00:00Z"}`)}, nil
		}, "synthetic-index", accountID)
		wantCalls := 2
		if wantAccount != "" {
			wantCalls = 3
		}
		if err != nil || calls != wantCalls {
			t.Fatalf("calls=%d want=%d error=%v", calls, wantCalls, err)
		}
	}
	for i, req := range captured[:6] {
		want := "synthetic-a"
		if i >= 3 {
			want = "synthetic-b"
		}
		if req.Header["Chatgpt-Account-Id"] != want {
			t.Fatalf("later query changed an earlier account header: %q", req.Header["Chatgpt-Account-Id"])
		}
	}
	if !maps.Equal(usageBefore, codexHeaders) || !maps.Equal(resetBefore, codexResetCreditsHeaders) {
		t.Fatal("account headers mutated shared defaults")
	}
}

func TestRegisterAuthManagerForQuotaUsesCredentialMaterialRevision(t *testing.T) {
	authManager := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{
		ID: "auth-claude-revision", Provider: "claude", FileName: "claude.json",
		Metadata:   map[string]any{"type": "claude", "access_token": "synthetic-before"},
		Attributes: map[string]string{coreauth.PersistedSHA256Attribute: strings.Repeat("a", 64)},
	}
	registered, err := authManager.Register(context.Background(), auth)
	if err != nil {
		t.Fatalf("register auth: %v", err)
	}
	scheduler := quota.NewScheduler(quota.DefaultConfig(), func(*quota.Entry) ([]byte, error) {
		return []byte(`{"remaining":5}`), nil
	})
	registerAuthManagerForQuota(scheduler, authManager)
	scheduler.RefreshNow([]string{"claude.json"})
	key := quota.EntryKey(quota.TypeClaude, "claude.json")
	before := scheduler.GetStatus().Credentials[key]
	if before == nil || before.ObservedAt == nil || len(before.Data) == 0 {
		t.Fatalf("missing initial quota: %#v", before)
	}
	for _, disabled := range []bool{true, false, true, false} {
		updated := registered.Clone()
		updated.Disabled = disabled
		updated.Metadata["disabled"] = disabled
		updated.Attributes[coreauth.PersistedSHA256Attribute] = strings.Repeat("b", 64)
		registered, err = authManager.Update(coreauth.WithSkipPersist(context.Background()), updated)
		if err != nil {
			t.Fatalf("update auth status: %v", err)
		}
		registerAuthManagerForQuota(scheduler, authManager)
		after := scheduler.GetStatus().Credentials[key]
		if after == nil || after.Generation != before.Generation || after.Disabled != disabled ||
			string(after.Data) != string(before.Data) || after.ObservedAt == nil || !after.ObservedAt.Equal(*before.ObservedAt) {
			t.Fatalf("status=%v discarded quota: before=%#v after=%#v", disabled, before, after)
		}
	}
	updated := registered.Clone()
	updated.Metadata["access_token"] = "synthetic-after"
	if _, err = authManager.Update(coreauth.WithSkipPersist(context.Background()), updated); err != nil {
		t.Fatal(err)
	}
	registerAuthManagerForQuota(scheduler, authManager)
	after := scheduler.GetStatus().Credentials[key]
	if after == nil || after.Generation <= before.Generation || after.Freshness != quota.FreshnessOutdated || len(after.Data) != 0 {
		t.Fatalf("replaced credential retained quota: %#v", after)
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
