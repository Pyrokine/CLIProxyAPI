package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/quota"
)

func TestNormalizeXAIBillingMonthly(t *testing.T) {
	syncedAt := time.Date(2026, 9, 5, 1, 2, 3, 0, time.UTC)
	payload, err := normalizeXAIBilling(
		[]byte(`{
		"config": {
			"monthlyLimit": {"val": 100},
			"used": {"val": 25},
			"onDemandCap": {"val": 0},
			"billingPeriodStart": "2026-07-01T00:00:00Z",
			"billingPeriodEnd": "2026-08-01T00:00:00Z"
		}
	}`), "legacy", syncedAt,
	)
	if err != nil {
		t.Fatal(err)
	}
	if payload.SchemaVersion != 1 || payload.Source != "xai_build_billing" {
		t.Fatalf("schema = %d source = %q", payload.SchemaVersion, payload.Source)
	}
	assertXAIFloat(t, "monthly_limit", payload.Normalized.MonthlyLimit, 100)
	assertXAIFloat(t, "included_used", payload.Normalized.IncludedUsed, 25)
	assertXAIFloat(t, "used", payload.Normalized.Used, 25)
	assertXAIFloat(t, "remaining", payload.Normalized.Remaining, 75)
	assertXAIFloat(t, "usage_percent", payload.Normalized.UsagePercent, 25)
	assertXAIFloat(t, "on_demand_cap", payload.Normalized.OnDemandCap, 0)
	if payload.Normalized.OnDemandUsed != nil {
		t.Fatalf("on_demand_used = %v", *payload.Normalized.OnDemandUsed)
	}
	if payload.Normalized.AmountUnit != "unknown" {
		t.Fatalf("amount_unit = %q", payload.Normalized.AmountUnit)
	}
	if payload.Normalized.SyncedAt != syncedAt.Format(time.RFC3339Nano) {
		t.Fatalf("synced_at = %q", payload.Normalized.SyncedAt)
	}
}

func TestNormalizeXAIBillingCreditsPreservesZeroValues(t *testing.T) {
	payload, err := normalizeXAIBilling(
		[]byte(`{
		"onDemandEnabled": false,
		"subscriptionTier": "SuperGrok Heavy",
		"config": {
			"creditUsagePercent": 42.5,
			"currentPeriod": {
				"type": "USAGE_PERIOD_TYPE_WEEKLY",
				"start": "2026-07-08T00:00:00Z",
				"end": "2026-07-15T00:00:00Z"
			},
			"onDemandCap": {"val": 0},
			"onDemandUsed": {"val": 0},
			"prepaidBalance": {"val": 0},
			"productUsage": [{"product": "Grok", "usagePercent": 0}]
		}
	}`), "credits", time.Unix(0, 0),
	)
	if err != nil {
		t.Fatal(err)
	}
	if payload.Normalized.PlanTier == nil || *payload.Normalized.PlanTier != "SuperGrok Heavy" {
		t.Fatalf("plan_tier = %v", payload.Normalized.PlanTier)
	}
	if payload.Normalized.OnDemandEnabled == nil || *payload.Normalized.OnDemandEnabled {
		t.Fatalf("on_demand_enabled = %v", payload.Normalized.OnDemandEnabled)
	}
	assertXAIFloat(t, "on_demand_used", payload.Normalized.OnDemandUsed, 0)
	assertXAIFloat(t, "prepaid_balance", payload.Normalized.PrepaidBalance, 0)
	assertXAIFloat(t, "usage_percent", payload.Normalized.UsagePercent, 42.5)
	if payload.Normalized.UsagePeriod.Type != "weekly" || payload.Normalized.AmountUnit != "credits" {
		t.Fatalf("period = %#v amount_unit = %q", payload.Normalized.UsagePeriod, payload.Normalized.AmountUnit)
	}
	if len(payload.Normalized.ProductUsage) != 1 || payload.Normalized.ProductUsage[0].UsagePercent == nil ||
		*payload.Normalized.ProductUsage[0].UsagePercent != 0 {
		t.Fatalf("product_usage = %#v", payload.Normalized.ProductUsage)
	}
}

func TestXAIBillingSubscriptionOverridesTier(t *testing.T) {
	payload, err := normalizeXAIBilling(
		[]byte(`{"subscriptionTier":"Billing Tier","config":{"monthlyLimit":{"val":100}}}`),
		"credits",
		time.Unix(0, 0),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err = payload.applySubscription([]byte(`{"user":{"subscriptionTier":"Subscription Tier"}}`)); err != nil {
		t.Fatal(err)
	}
	if payload.Normalized.PlanTier == nil || *payload.Normalized.PlanTier != "Subscription Tier" {
		t.Fatalf("plan_tier = %v", payload.Normalized.PlanTier)
	}
	if source := payload.ValueSource["plan_tier"]; source != "subscription.user.subscriptionTier" {
		t.Fatalf("plan_tier source = %q", source)
	}
}

func TestNormalizeXAIBillingRejectsInvalidPayload(t *testing.T) {
	for _, body := range []string{`[]`, `{}`} {
		if _, err := normalizeXAIBilling([]byte(body), "credits", time.Now()); err == nil {
			t.Fatalf("expected invalid payload error for %s", body)
		}
	}
}

func TestFetchXAIQuotaUsesCreditsAndSubscription(t *testing.T) {
	var requests []quota.APICallRequest
	apiCall := func(req quota.APICallRequest) (*quota.APICallResponse, error) {
		requests = append(requests, req)
		switch len(requests) {
		case 1:
			return &quota.APICallResponse{
				StatusCode: http.StatusOK,
				Body:       json.RawMessage(`{"config":{"monthlyLimit":{"val":100},"used":{"val":25}}}`),
			}, nil
		case 2:
			return &quota.APICallResponse{
				StatusCode: http.StatusOK,
				Body:       json.RawMessage(`{"user":{"subscriptionTier":"SuperGrokPlus"}}`),
			}, nil
		default:
			t.Fatalf("unexpected request %d", len(requests))
			return nil, nil
		}
	}

	encoded, err := fetchXAIQuota(apiCall, "auth-index")
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 2 || requests[0].URL != xaiBillingCreditsURL || requests[1].URL != xaiSubscriptionURL {
		t.Fatalf("requests = %#v", requests)
	}
	for _, req := range requests {
		headers := make(http.Header)
		for name, value := range req.Header {
			headers.Set(name, value)
		}
		if headers.Get("Authorization") != "Bearer $TOKEN$" ||
			headers.Get("X-XAI-Token-Auth") != "xai-grok-cli" ||
			headers.Get("x-grok-client-identifier") != "grok-shell" {
			t.Fatalf("headers = %#v", req.Header)
		}
	}
	var payload xaiQuotaPayload
	if err = json.Unmarshal(encoded, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Normalized.PlanTier == nil || *payload.Normalized.PlanTier != "SuperGrokPlus" {
		t.Fatalf("plan_tier = %v", payload.Normalized.PlanTier)
	}
	if payload.BillingFormat != "credits" || len(payload.AuxiliaryErrors) != 0 {
		t.Fatalf("payload = %#v", payload)
	}
}

func TestFetchXAIQuotaFallsBackOnlyForUnsupportedCreditsFormat(t *testing.T) {
	var requests []quota.APICallRequest
	apiCall := func(req quota.APICallRequest) (*quota.APICallResponse, error) {
		requests = append(requests, req)
		switch len(requests) {
		case 1:
			return &quota.APICallResponse{
				StatusCode: http.StatusBadRequest,
				Body:       json.RawMessage(`{"error":"unsupported format credits"}`),
			}, nil
		case 2:
			return &quota.APICallResponse{
				StatusCode: http.StatusOK,
				Body:       json.RawMessage(`{"config":{"monthlyLimit":{"val":100},"used":{"val":25}}}`),
			}, nil
		case 3:
			return &quota.APICallResponse{StatusCode: http.StatusServiceUnavailable}, nil
		default:
			t.Fatalf("unexpected request %d", len(requests))
			return nil, nil
		}
	}

	encoded, err := fetchXAIQuota(apiCall, "auth-index")
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 3 || requests[1].URL != xaiBillingURL {
		t.Fatalf("requests = %#v", requests)
	}
	var payload xaiQuotaPayload
	if err = json.Unmarshal(encoded, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.BillingFormat != "legacy" || payload.Normalized.AmountUnit != "unknown" {
		t.Fatalf("payload = %#v", payload)
	}
	if len(payload.AuxiliaryErrors) != 1 || payload.AuxiliaryErrors[0].Code != "status_503" {
		t.Fatalf("auxiliary_errors = %#v", payload.AuxiliaryErrors)
	}
}

func TestFetchXAIBillingDoesNotRetryAuthenticationOrRateLimitFailures(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests} {
		t.Run(
			http.StatusText(status), func(t *testing.T) {
				calls := 0
				_, _, err := fetchXAIBilling(
					func(quota.APICallRequest) (*quota.APICallResponse, error) {
						calls++
						return &quota.APICallResponse{StatusCode: status}, nil
					}, "auth-index",
				)
				if err == nil || calls != 1 {
					t.Fatalf("err = %v calls = %d", err, calls)
				}
			},
		)
	}
}

func TestFetchXAIBillingDoesNotRetryAmbiguousBadRequest(t *testing.T) {
	calls := 0
	_, _, err := fetchXAIBilling(
		func(quota.APICallRequest) (*quota.APICallResponse, error) {
			calls++
			return &quota.APICallResponse{
				StatusCode: http.StatusBadRequest,
				Body:       json.RawMessage(`{"error":"invalid account"}`),
			}, nil
		}, "auth-index",
	)
	if err == nil || calls != 1 {
		t.Fatalf("err = %v calls = %d", err, calls)
	}
}

func TestFetchXAIBillingDoesNotRetryTransportFailure(t *testing.T) {
	calls := 0
	_, _, err := fetchXAIBilling(
		func(quota.APICallRequest) (*quota.APICallResponse, error) {
			calls++
			return nil, errors.New("network unavailable")
		}, "auth-index",
	)
	if err == nil || calls != 1 {
		t.Fatalf("err = %v calls = %d", err, calls)
	}
}

func assertXAIFloat(t *testing.T, name string, value *float64, want float64) {
	t.Helper()
	if value == nil || *value != want {
		t.Fatalf("%s = %v, want %v", name, value, want)
	}
}
