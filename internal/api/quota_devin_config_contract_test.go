package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	devinauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/devin"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/quota"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

func TestServerDevinQuotaUsesUpdatedProxyConfig(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	var directCalls, proxyACalls, proxyBCalls atomic.Int32
	newEndpoint := func(daily uint64, calls *atomic.Int32) *httptest.Server {
		endpoint := httptest.NewServer(
			http.HandlerFunc(
				func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					if r.Method != http.MethodPost || r.URL.Path != devinauth.GetUserStatusPath {
						t.Errorf("unexpected Devin status request: %s %s", r.Method, r.URL.Path)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					_, _ = w.Write(testAPIQuotaDevinStatus(daily, 90))
				},
			),
		)
		t.Cleanup(endpoint.Close)
		return endpoint
	}
	direct := newEndpoint(10, &directCalls)
	proxyA := newEndpoint(40, &proxyACalls)
	proxyB := newEndpoint(70, &proxyBCalls)
	server := newTestServer(t)
	manager := server.handlers.AuthManager
	auth, err := manager.Register(
		t.Context(), &coreauth.Auth{
			ID: "devin-config-quota", Provider: "devin", FileName: "devin-config.json", Status: coreauth.StatusActive,
			Attributes: map[string]string{
				"api_key": "devin-session-token$synthetic", "base_url": direct.URL, "device_seed": "synthetic-seed",
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if registered := registerAuthManagerForQuota(server.quotaScheduler, manager); registered != 1 {
		t.Fatalf("registered quota bindings=%d, want 1", registered)
	}
	for _, test := range []struct {
		proxy string
		daily int64
		calls [3]int32
	}{
		{"direct", 10, [3]int32{1, 0, 0}},
		{proxyA.URL, 40, [3]int32{1, 1, 0}},
		{proxyB.URL, 70, [3]int32{1, 1, 1}},
	} {
		cfg := *server.configSnapshot()
		cfg.ProxyURL = test.proxy
		server.UpdateClientsForConfigCommit(t.Context(), &cfg, 1)
		targets, unresolved := server.quotaScheduler.ResolveRefreshTargets([]string{auth.FileName})
		if len(targets) != 1 || len(unresolved) != 0 {
			t.Fatalf("quota targets=%+v unresolved=%v", targets, unresolved)
		}
		result := server.quotaScheduler.RefreshTarget(t.Context(), targets[0])
		if result.Outcome != quota.RefreshOutcomeSucceeded {
			t.Fatalf("Devin quota refresh failed: %+v", result)
		}
		entry := server.quotaScheduler.GetStatus().Credentials[targets[0].EntryKey]
		if entry == nil || gjson.GetBytes(entry.Data, "daily_quota_remaining_percent").Int() != test.daily {
			t.Fatalf("quota after proxy update=%+v, want daily %d", entry, test.daily)
		}
		if got := [3]int32{directCalls.Load(), proxyACalls.Load(), proxyBCalls.Load()}; got != test.calls {
			t.Fatalf("direct/proxy A/proxy B calls=%v, want %v", got, test.calls)
		}
	}
}

func TestServerQuotaBuiltInAPICallbacksPublishStatus(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	server := newTestServer(t)
	manager := server.handlers.AuthManager
	payloads := map[string]string{
		claudeUsageURL: `{"five_hour":{"utilization":25}}`, claudeProfileURL: `{}`,
		codexUsageURL: `{"rate_limit":{"allowed":true}}`, codexRateLimitResetCreditsURL: `{}`,
		codexSubscriptionsURL + "?account_id=synthetic-account": `{"active_until":"2026-12-01T00:00:00Z"}`,
		geminiQuotaURL: `{"buckets":[]}`, geminiCodeAssistURL: `{}`,
		antigravityQuotaURLs[0]: `{"groups":[]}`, antigravityLoadCodeAssistURL: `{}`,
		kimiUsageURL:         `{"usage":{"limit":100,"used":25}}`,
		xaiBillingCreditsURL: `{"config":{"creditUsagePercent":25,"currentPeriod":{"type":"weekly"}}}`,
		xaiBillingURL:        `{"config":{"monthlyLimit":{"val":100},"used":{"val":25}}}`,
		xaiSubscriptionURL:   `{"user":{"subscriptionTier":"SuperGrok"}}`, xaiSettingsURL: `{}`,
	}
	previous := http.DefaultTransport
	expectedIndex := ""
	calls := 0
	http.DefaultTransport = managementPanelRoundTripper(
		func(req *http.Request) (*http.Response, error) {
			if req.URL.Hostname() != "127.0.0.1" || req.URL.Path != "/v0/management/api-call" {
				return nil, fmt.Errorf("unexpected quota destination %s", req.URL)
			}
			var call quota.APICallRequest
			if err := json.NewDecoder(req.Body).Decode(&call); err != nil {
				return nil, err
			}
			if call.AuthIndex != expectedIndex {
				return nil, fmt.Errorf("quota identity=%s, want %s", call.AuthIndex, expectedIndex)
			}
			body, exists := payloads[call.URL]
			if !exists {
				return nil, fmt.Errorf("unexpected synthetic provider endpoint %s", call.URL)
			}
			if strings.Contains(call.URL, "/consume") {
				return nil, fmt.Errorf("ordinary quota refresh attempted credit consumption")
			}
			calls++
			wire := `{"status_code":200,"body":` + body + `}`
			return &http.Response{
				StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(wire)),
				Request: req,
			}, nil
		},
	)
	t.Cleanup(func() { http.DefaultTransport = previous })
	for _, test := range []struct {
		provider string
		source   string
		calls    int
	}{
		{"claude", "claude_usage", 2}, {"codex", "codex_usage", 3},
		{"gemini-cli", "gemini_quota", 2}, {"antigravity", "antigravity_quota", 2},
		{"kimi", "kimi_usage", 1}, {"xai", "xai_billing", 4},
	} {
		t.Run(
			test.provider, func(t *testing.T) {
				auth, err := manager.Register(
					t.Context(), &coreauth.Auth{
						ID: "quota-callback-" + test.provider, Provider: test.provider,
						FileName: test.provider + "-callback.json", Status: coreauth.StatusActive,
						Metadata: map[string]any{
							"access_token": "synthetic", "project_id": "synthetic-project",
							"account_id": "synthetic-account",
						},
					},
				)
				if err != nil {
					t.Fatal(err)
				}
				expectedIndex = auth.Index
				registerAuthManagerForQuota(server.quotaScheduler, manager)
				targets, unresolved := server.quotaScheduler.ResolveRefreshTargets([]string{auth.FileName})
				if len(targets) != 1 || len(unresolved) != 0 {
					t.Fatalf("targets=%+v unresolved=%v", targets, unresolved)
				}
				before := calls
				result := server.quotaScheduler.RefreshTarget(t.Context(), targets[0])
				if result.Outcome != quota.RefreshOutcomeSucceeded {
					t.Fatalf("quota refresh=%+v", result)
				}
				entry := server.quotaScheduler.GetStatus().Credentials[targets[0].EntryKey]
				if entry == nil || entry.Source != test.source || entry.Status != quota.StatusSuccess ||
					entry.ObservedAt == nil || !json.Valid(entry.Data) || calls-before != test.calls {
					t.Fatalf(
						"published status=%+v, calls=%d want source=%s calls=%d", entry, calls-before, test.source,
						test.calls,
					)
				}
			},
		)
	}
}
