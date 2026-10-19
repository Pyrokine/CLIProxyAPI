package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	codexauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/codex"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/quota"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestCodexQuotaSubscriptionUsesBoundAccountID(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	registered, err := manager.Register(context.Background(), &coreauth.Auth{
		ID: "codex-test", Provider: "codex", FileName: "codex-test.json",
		Storage:  &codexauth.TokenStorage{AccountID: "account/id + space"},
		Metadata: map[string]any{"account_id": "stale-account", "chatgpt_subscription_active_until": "2026-01-01T00:00:00Z"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := codexQuotaAccountID(manager, registered.Index); got != "account/id + space" {
		t.Fatalf("bound account ID = %q", got)
	}
	if got := codexQuotaAccountID(manager, "other-index"); got != "" {
		t.Fatalf("unbound account ID = %q", got)
	}
	fileAuth, err := manager.Register(context.Background(), &coreauth.Auth{
		ID: "codex-file", Provider: "codex", FileName: "codex-file.json",
		Metadata: map[string]any{"account_id": "file-account"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := codexQuotaAccountID(manager, fileAuth.Index); got != "file-account" {
		t.Fatalf("file credential account ID = %q", got)
	}
}

func TestParseCodexSubscriptionActiveUntil(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
	}{
		{"snake case date", `{"active_until":"2026-10-03T13:27:01Z"}`, "2026-10-03T13:27:01Z"},
		{"camel case milliseconds", `{"activeUntil":1791034021000}`, "2026-10-03T13:27:01Z"},
		{"encoded body", `"{\"active_until\":\"2026-10-03T13:27:01Z\"}"`, "2026-10-03T13:27:01Z"},
		{"empty", `{}`, ""},
		{"null", `{"active_until":null}`, ""},
		{"malformed", `{"active_until":"yesterday"}`, ""},
		{"zero", `{"active_until":0}`, ""},
		{"nested cannot be used", `{"subscription":{"active_until":"2026-10-03T13:27:01Z"}}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseCodexSubscriptionActiveUntil([]byte(tc.body))
			if got != tc.want || (err != nil) != (tc.want == "") {
				t.Fatalf("date=%q, error=%v; want %q", got, err, tc.want)
			}
		})
	}
}

func TestCodexSubscriptionFailureKeepsUsageWithoutInventingLiveDate(t *testing.T) {
	for _, tc := range []struct {
		name, accountID string
		status          int
		body            string
		requestErr      error
		wantCalls       int
	}{
		{name: "missing account", wantCalls: 0},
		{name: "invalid account header", accountID: "account\x01bad", wantCalls: 0},
		{name: "request failed", accountID: "account-1", requestErr: errors.New("Bearer secret"), wantCalls: 1},
		{name: "HTTP failure", accountID: "account-1", status: http.StatusServiceUnavailable, wantCalls: 1},
		{name: "missing date", accountID: "account-1", status: http.StatusOK, body: `{}`, wantCalls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			result, err := fetchCodexQuotaResultContext(context.Background(), func(_ context.Context, request quota.APICallRequest) (*quota.APICallResponse, error) {
				switch {
				case request.URL == codexUsageURL:
					return &quota.APICallResponse{StatusCode: http.StatusOK, Body: json.RawMessage(`{"rate_limit":{"allowed":true}}`)}, nil
				case request.URL == codexRateLimitResetCreditsURL:
					return &quota.APICallResponse{StatusCode: http.StatusOK, Body: json.RawMessage(`{"available_count":0}`)}, nil
				case strings.HasPrefix(request.URL, codexSubscriptionsURL+"?"):
					calls++
					if request.Method != http.MethodGet || request.AuthIndex != "auth-index" ||
						request.Header["Chatgpt-Account-Id"] != tc.accountID || request.Header["Authorization"] != "Bearer $TOKEN$" {
						t.Errorf("subscription request = %+v", request)
					}
					return &quota.APICallResponse{StatusCode: tc.status, Body: json.RawMessage(tc.body)}, tc.requestErr
				default:
					t.Errorf("unexpected request: %+v", request)
					return nil, errors.New("unexpected request")
				}
			}, "auth-index", tc.accountID)
			if err != nil || result.Completeness != quota.FetchPartial || result.PartialError == "" || calls != tc.wantCalls {
				t.Fatalf("result=%+v error=%v subscription calls=%d", result, err, calls)
			}
			var data map[string]json.RawMessage
			if err := json.Unmarshal(result.Data, &data); err != nil {
				t.Fatal(err)
			}
			if data["subscription_active_until"] != nil || data["subscription_active_until_source"] != nil ||
				data["subscription_error"] == nil || data["rate_limit"] == nil ||
				strings.Contains(string(result.Data), "secret") {
				t.Fatalf("partial response = %s", result.Data)
			}
		})
	}
}
