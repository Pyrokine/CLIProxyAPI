package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/api/handlers/management"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/quota"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/gin-gonic/gin"
)

func TestCodexResetCreditProbeUsesNativeReadOnlyWire(t *testing.T) {
	for _, item := range []struct {
		name             string
		usageStatus      int
		creditStatus     int
		creditBody       string
		wantStatus       int
		wantAvailability string
	}{
		{"available despite subscription failure", http.StatusOK, http.StatusOK, `{"available_count":2,"applicable_available_count":0}`, http.StatusOK, "available"},
		{"independent credit after usage failure", http.StatusServiceUnavailable, http.StatusOK, `{"available_count":2,"applicable_available_count":0}`, http.StatusOK, "available"},
		{"credit query rejected", http.StatusOK, http.StatusForbidden, `{}`, http.StatusBadGateway, ""},
		{"bad credit JSON", http.StatusOK, http.StatusOK, `{`, http.StatusBadGateway, ""},
		{"zero current credits", http.StatusServiceUnavailable, http.StatusOK, `{"available_count":0}`, http.StatusOK, "none"},
	} {
		t.Run(item.name, func(t *testing.T) {
			var mu sync.Mutex
			var requests []string
			stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var call quota.APICallRequest
				if err := json.NewDecoder(r.Body).Decode(&call); err != nil {
					t.Errorf("decode internal API call: %v", err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				mu.Lock()
				requests = append(requests, call.URL)
				mu.Unlock()
				if call.Method != http.MethodGet || (call.URL != codexUsageURL && call.URL != codexRateLimitResetCreditsURL) {
					t.Errorf("eligibility queried an unrelated or consuming endpoint: %s %s", call.Method, call.URL)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if call.Header["Chatgpt-Account-Id"] != "synthetic-account" || call.Data != "" {
					t.Errorf("native read lost account scope or introduced a request body")
				}
				status := item.usageStatus
				body := `{"rate_limit_reset_credits":{"available_count":2,"applicable_available_count":0},"unrelated_field":"not-for-probe-output"}`
				if call.URL == codexRateLimitResetCreditsURL {
					status, body = item.creditStatus, item.creditBody
					if call.Header["OpenAI-Beta"] != "codex-1" || call.Header["Originator"] != "Codex Desktop" {
						t.Error("reset-credit read lost the existing provider headers")
					}
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"status_code": status, "body": body})
			}))
			defer stub.Close()
			address, err := url.Parse(stub.URL)
			if err != nil {
				t.Fatal(err)
			}
			port, err := strconv.Atoi(address.Port())
			if err != nil {
				t.Fatal(err)
			}
			manager := coreauth.NewManager(nil, nil, nil)
			auth, err := manager.Register(context.Background(), &coreauth.Auth{
				ID: "synthetic-native-probe", FileName: "synthetic-native-probe.json", Provider: "codex",
				Metadata: map[string]any{"access_token": "synthetic-token", "account_id": "synthetic-account"},
				Quota:    coreauth.QuotaState{Exceeded: true}, Unavailable: true, Status: coreauth.StatusError,
			})
			if err != nil {
				t.Fatal(err)
			}
			localCall := quota.NewLocalAPICallContextFunc(port, "")
			scheduler := quota.NewSchedulerWithResult(quota.DefaultConfig(), func(ctx context.Context, entry *quota.Entry) (quota.FetchResult, error) {
				return fetchCodexQuotaResultContext(ctx, localCall, entry.AuthIndex, codexQuotaAccountID(manager, entry.AuthIndex))
			})
			revision, equivalent, err := coreauth.QuotaCredentialRevisions(auth)
			if err != nil {
				t.Fatal(err)
			}
			scheduler.RegisterBinding(auth.FileName, quota.TypeCodex, auth.Index, revision, equivalent...)
			handler := management.NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
			defer func() { _ = handler.Close() }()
			handler.SetQuotaScheduler(scheduler)
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v0/management/quota/fetch", strings.NewReader(fmt.Sprintf(
				`{"auth_index":%q,"provider":"codex","purpose":"codex_reset_credit"}`, auth.Index)))
			ctx.Request.Header.Set("Content-Type", "application/json")
			handler.FetchCredentialQuota(ctx)
			if recorder.Code != item.wantStatus {
				t.Fatalf("HTTP %d, want %d; body=%s", recorder.Code, item.wantStatus, recorder.Body.String())
			}
			if item.wantStatus == http.StatusOK {
				var response struct {
					Availability string `json:"availability"`
				}
				if err = json.Unmarshal(recorder.Body.Bytes(), &response); err != nil || response.Availability != item.wantAvailability {
					t.Fatalf("invalid qualification: %s; error=%v", recorder.Body.String(), err)
				}
			}
			if strings.Contains(recorder.Body.String(), "not-for-probe-output") || strings.Contains(recorder.Body.String(), "synthetic-token") {
				t.Error("probe returned unrelated data or authentication material")
			}
			mu.Lock()
			if len(requests) != 2 || requests[0] != codexUsageURL || requests[1] != codexRateLimitResetCreditsURL {
				t.Errorf("read-only query chain=%v, want usage and reset-credit GETs only", requests)
			}
			mu.Unlock()
			current, _ := manager.GetByID(auth.ID)
			if current == nil || !current.Quota.Exceeded || !current.Unavailable {
				t.Error("read-only native query cleared local cooldown")
			}
		})
	}
}
