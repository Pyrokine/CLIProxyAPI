package api

import (
	"context"
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
	if err = payload.applySubscription([]byte(`{"user":{"subscriptionTier":"Subscription Tier"}}`), "subscription"); err != nil {
		t.Fatal(err)
	}
	if payload.Normalized.PlanTier == nil || *payload.Normalized.PlanTier != "Subscription Tier" {
		t.Fatalf("plan_tier = %v", payload.Normalized.PlanTier)
	}
	if source := payload.ValueSource["plan_tier"]; source != "subscription.user.subscriptionTier" {
		t.Fatalf("plan_tier source = %q", source)
	}
}

func TestXAIBillingSettingsPlanTier(t *testing.T) {
	for _, tc := range []struct {
		name, subscription, settings, want, source string
	}{
		{name: "snake display", subscription: `{"subscriptionTier":"User Tier"}`, settings: `{"subscription_tier_display":"Display Tier"}`, want: "Display Tier", source: "settings.subscription_tier_display"},
		{name: "camel display", subscription: `{"subscriptionTier":"User Tier"}`, settings: `{"subscriptionTierDisplay":"Display Tier"}`, want: "Display Tier", source: "settings.subscriptionTierDisplay"},
		{name: "nested settings", subscription: `{"subscriptionTier":"User Tier"}`, settings: `{"settings":{"subscriptionTierDisplay":"Nested Display"},"subscriptionTierDisplay":"Root Display"}`, want: "Nested Display", source: "settings.settings.subscriptionTierDisplay"},
		{name: "user tier retained", subscription: `{"user":{"subscriptionTier":"User Tier"}}`, settings: `{"subscriptionTier":"Settings Tier"}`, want: "User Tier", source: "subscription.user.subscriptionTier"},
		{name: "blank display retains user", subscription: `{"subscriptionTier":"User Tier"}`, settings: `{"subscription_tier_display":"  "}`, want: "User Tier", source: "subscription.subscriptionTier"},
		{name: "missing display retains user", subscription: `{"subscriptionTier":"User Tier"}`, settings: `{}`, want: "User Tier", source: "subscription.subscriptionTier"},
		{name: "settings tier when user missing", subscription: `{}`, settings: `{"subscription_tier":"Settings Tier"}`, want: "Settings Tier", source: "settings.subscription_tier"},
		{name: "settings display when user missing", subscription: `{}`, settings: `{"subscription_tier_display":"Display Tier"}`, want: "Display Tier", source: "settings.subscription_tier_display"},
		{name: "no tier", subscription: `{}`, settings: `{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := &xaiQuotaPayload{ValueSource: make(map[string]string)}
			if err := payload.applySubscription([]byte(tc.subscription), "subscription"); err != nil {
				t.Fatal(err)
			}
			if err := payload.applySubscription([]byte(tc.settings), "settings"); err != nil {
				t.Fatal(err)
			}
			if tc.want == "" {
				if payload.Normalized.PlanTier != nil {
					t.Fatalf("unexpected tier = %v", payload.Normalized.PlanTier)
				}
			} else if payload.Normalized.PlanTier == nil || *payload.Normalized.PlanTier != tc.want {
				t.Fatalf("tier = %v, want %q", payload.Normalized.PlanTier, tc.want)
			}
			if payload.ValueSource["plan_tier"] != tc.source {
				t.Fatalf("source = %q, want %q", payload.ValueSource["plan_tier"], tc.source)
			}
		})
	}
}

func TestFetchXAIQuotaSettingsFailureRetainsVerifiedBilling(t *testing.T) {
	for _, tc := range []struct {
		name       string
		response   *quota.APICallResponse
		requestErr error
		wantCode   string
	}{
		{name: "transport failure", requestErr: errors.New("fixture transport error"), wantCode: "request_failed"},
		{name: "empty response", wantCode: "empty_response"},
		{name: "unavailable", response: &quota.APICallResponse{StatusCode: http.StatusServiceUnavailable}, wantCode: "status_503"},
		{name: "invalid payload", response: &quota.APICallResponse{StatusCode: http.StatusOK, Body: json.RawMessage(`[]`)}, wantCode: "invalid_payload"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests []quota.APICallRequest
			result, err := fetchXAIQuotaResultContext(context.Background(), func(_ context.Context, req quota.APICallRequest) (*quota.APICallResponse, error) {
				requests = append(requests, req)
				switch req.URL {
				case xaiBillingCreditsURL:
					return &quota.APICallResponse{StatusCode: http.StatusOK, Body: json.RawMessage(`{"config":{"creditUsagePercent":42,"currentPeriod":{"type":"weekly","end":"2026-07-15T00:00:00Z"}}}`)}, nil
				case xaiBillingURL:
					return &quota.APICallResponse{StatusCode: http.StatusOK, Body: json.RawMessage(`{"config":{"monthlyLimit":{"val":100},"used":{"val":20}}}`)}, nil
				case xaiSubscriptionURL:
					return &quota.APICallResponse{StatusCode: http.StatusOK, Body: json.RawMessage(`{"user":{"subscriptionTier":"User Tier"}}`)}, nil
				case xaiSettingsURL:
					return tc.response, tc.requestErr
				default:
					t.Fatalf("unexpected URL %s", req.URL)
					return nil, nil
				}
			}, "fixture-xai-index")
			if err != nil || result.Enrich == nil {
				t.Fatalf("initial billing result = %#v, error = %v", result, err)
			}
			result, err = result.Enrich(context.Background())
			if err != nil || result.Completeness != quota.FetchPartial || result.PartialError == "" || len(requests) != 4 {
				t.Fatalf("result = %#v, error = %v, requests = %#v", result, err, requests)
			}
			for _, req := range requests {
				if req.Method != http.MethodGet || req.AuthIndex != "fixture-xai-index" {
					t.Fatalf("request identity or method changed: %#v", req)
				}
			}
			var payload xaiQuotaPayload
			if err := json.Unmarshal(result.Data, &payload); err != nil {
				t.Fatal(err)
			}
			if payload.Normalized.PlanTier == nil || *payload.Normalized.PlanTier != "User Tier" ||
				payload.ValueSource["plan_tier"] != "subscription.user.subscriptionTier" ||
				len(payload.AuxiliaryErrors) != 1 || payload.AuxiliaryErrors[0].Source != "settings" ||
				payload.AuxiliaryErrors[0].Code != tc.wantCode {
				t.Fatalf("settings failure changed subscription or hid error: %#v", payload)
			}
			assertXAIFloat(t, "weekly usage", payload.Normalized.Weekly.UsagePercent, 42)
			assertXAIFloat(t, "monthly usage", payload.Normalized.Monthly.UsagePercent, 20)
		})
	}
}

func TestXAIBillingMonthlyPreservesWeeklyPlanIdentity(t *testing.T) {
	payload, err := normalizeXAIBilling(
		[]byte(`{"planCode":"weekly","planName":"Weekly","subscriptionTier":"Weekly Tier","config":{"creditUsagePercent":5,"currentPeriod":{"type":"weekly"}}}`),
		"credits", time.Unix(0, 0),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err = payload.applyMonthlyBilling(
		[]byte(`{"planCode":"monthly","planName":"Monthly","subscriptionTier":"Monthly Tier","config":{"monthlyLimit":{"val":100},"used":{"val":30}}}`),
		time.Unix(0, 0),
	); err != nil {
		t.Fatal(err)
	}
	if payload.Normalized.PlanCode == nil || *payload.Normalized.PlanCode != "weekly" ||
		payload.Normalized.PlanName == nil || *payload.Normalized.PlanName != "Weekly" ||
		payload.Normalized.PlanTier == nil || *payload.Normalized.PlanTier != "Weekly Tier" {
		t.Fatalf("weekly plan identity changed: %#v", payload.Normalized)
	}
	for field, source := range map[string]string{
		"plan_code": "billing_credits.planCode", "plan_name": "billing_credits.planName",
		"plan_tier": "billing_credits.subscriptionTier",
	} {
		if payload.ValueSource[field] != source {
			t.Fatalf("%s source = %q, want %q", field, payload.ValueSource[field], source)
		}
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
				Body:       json.RawMessage(`{"config":{"creditUsagePercent":25,"currentPeriod":{"type":"weekly","start":"2026-07-08T00:00:00Z","end":"2026-07-15T00:00:00Z"}}}`),
			}, nil
		case 2:
			return &quota.APICallResponse{
				StatusCode: http.StatusOK,
				Body:       json.RawMessage(`{"config":{"planCode":"super-grok","planName":"SuperGrok","monthlyLimit":{"val":100},"used":{"val":25},"billingPeriodStart":"2026-07-01T00:00:00Z","billingPeriodEnd":"2026-08-01T00:00:00Z","amountUnit":"currency_minor","currency":"USD"}}`),
			}, nil
		case 3:
			return &quota.APICallResponse{
				StatusCode: http.StatusOK,
				Body:       json.RawMessage(`{"user":{"subscriptionTier":"SuperGrokPlus"}}`),
			}, nil
		case 4:
			return &quota.APICallResponse{
				StatusCode: http.StatusOK,
				Body:       json.RawMessage(`{"subscription_tier_display":"SuperGrok Plus"}`),
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
	if len(requests) != 4 || requests[0].URL != xaiBillingCreditsURL ||
		requests[1].URL != xaiBillingURL || requests[2].URL != xaiSubscriptionURL || requests[3].URL != xaiSettingsURL {
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
	if payload.Normalized.PlanTier == nil || *payload.Normalized.PlanTier != "SuperGrok Plus" {
		t.Fatalf("plan_tier = %v", payload.Normalized.PlanTier)
	}
	if payload.Normalized.PlanCode == nil || *payload.Normalized.PlanCode != "super-grok" ||
		payload.Normalized.PlanName == nil || *payload.Normalized.PlanName != "SuperGrok" {
		t.Fatalf("monthly plan identity = code %v, name %v", payload.Normalized.PlanCode, payload.Normalized.PlanName)
	}
	if payload.ValueSource["plan_code"] != "billing_legacy.config.planCode" ||
		payload.ValueSource["plan_name"] != "billing_legacy.config.planName" {
		t.Fatalf("monthly plan sources = %#v", payload.ValueSource)
	}
	if payload.BillingFormat != "credits" || len(payload.AuxiliaryErrors) != 0 {
		t.Fatalf("payload = %#v", payload)
	}
	assertXAIFloat(t, "weekly usage", payload.Normalized.Weekly.UsagePercent, 25)
	assertXAIFloat(t, "monthly usage", payload.Normalized.Monthly.UsagePercent, 25)
	if payload.Normalized.Weekly.PeriodType != "weekly" || payload.Normalized.Monthly.PeriodType != "monthly" ||
		payload.Normalized.Weekly.PeriodStart == nil || *payload.Normalized.Weekly.PeriodStart != "2026-07-08T00:00:00Z" ||
		payload.Normalized.Monthly.PeriodStart == nil || *payload.Normalized.Monthly.PeriodStart != "2026-07-01T00:00:00Z" ||
		payload.Normalized.AmountUnit != "currency_minor" || payload.Normalized.Currency == nil || *payload.Normalized.Currency != "USD" ||
		payload.ValueSource["monthly.usage_percent"] != "derived:billing_legacy.config.used/billing_legacy.config.monthlyLimit" ||
		payload.ValueSource["amount_unit"] != "billing_legacy.config.amountUnit" {
		t.Fatalf("mixed billing source or missing periods: %#v", payload)
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
		case 4:
			return &quota.APICallResponse{StatusCode: http.StatusOK, Body: json.RawMessage(`{}`)}, nil
		default:
			t.Fatalf("unexpected request %d", len(requests))
			return nil, nil
		}
	}

	encoded, err := fetchXAIQuota(apiCall, "auth-index")
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 4 || requests[1].URL != xaiBillingURL || requests[3].URL != xaiSettingsURL {
		t.Fatalf("requests = %#v", requests)
	}
	var payload xaiQuotaPayload
	if err = json.Unmarshal(encoded, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.BillingFormat != "legacy" || payload.Normalized.AmountUnit != "unknown" {
		t.Fatalf("payload = %#v", payload)
	}
	if len(payload.AuxiliaryErrors) != 2 || payload.AuxiliaryErrors[0].Code != "credits_format_unsupported" ||
		payload.AuxiliaryErrors[1].Code != "status_503" || payload.Normalized.Weekly.UsagePercent != nil ||
		payload.Normalized.Monthly.PeriodType != "monthly" {
		t.Fatalf("partial legacy billing = %#v", payload)
	}
}

func TestFetchXAIBillingDoesNotRetryAuthenticationOrRateLimitFailures(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests} {
		t.Run(
			http.StatusText(status), func(t *testing.T) {
				calls := 0
				_, err := fetchXAIQuota(
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
	_, err := fetchXAIQuota(
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

func TestFetchXAIBillingBothTransportFailuresRemainErrors(t *testing.T) {
	calls := 0
	_, err := fetchXAIQuota(
		func(quota.APICallRequest) (*quota.APICallResponse, error) {
			calls++
			return nil, errors.New("network unavailable")
		}, "auth-index",
	)
	if err == nil || calls != 2 {
		t.Fatalf("err = %v calls = %d", err, calls)
	}
}

func TestFetchXAIQuotaCreditsFailurePublishesOnlyVerifiedMonthlyWindow(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		requestErr error
		wantCode   string
	}{
		{name: "credits 503", status: http.StatusServiceUnavailable, wantCode: "status_503"},
		{name: "credits transport failed", requestErr: errors.New("network unavailable"), wantCode: "request_failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := make([]string, 0, 4)
			result, err := fetchXAIQuotaResultContext(context.Background(), func(_ context.Context, req quota.APICallRequest) (*quota.APICallResponse, error) {
				requests = append(requests, req.URL)
				switch req.URL {
				case xaiBillingCreditsURL:
					return &quota.APICallResponse{StatusCode: tc.status}, tc.requestErr
				case xaiBillingURL:
					return &quota.APICallResponse{StatusCode: http.StatusOK, Body: json.RawMessage(`{"config":{"monthlyLimit":{"val":100},"used":{"val":20},"billingPeriodStart":"2026-07-01T00:00:00Z","billingPeriodEnd":"2026-08-01T00:00:00Z"}}`)}, nil
				case xaiSubscriptionURL:
					return &quota.APICallResponse{StatusCode: http.StatusOK, Body: json.RawMessage(`{"user":{"subscriptionTier":"Paid"}}`)}, nil
				case xaiSettingsURL:
					return &quota.APICallResponse{StatusCode: http.StatusOK, Body: json.RawMessage(`{}`)}, nil
				default:
					t.Fatalf("unexpected URL %s", req.URL)
					return nil, nil
				}
			}, "xai-index")
			if err != nil || result.Completeness != quota.FetchPartial || len(requests) != 2 {
				t.Fatalf("quota result=%#v, error=%v, requests=%v", result, err, requests)
			}
			var payload xaiQuotaPayload
			if err := json.Unmarshal(result.Data, &payload); err != nil {
				t.Fatal(err)
			}
			if payload.Normalized.Weekly.PeriodType != "unknown" || payload.Normalized.Weekly.UsagePercent != nil ||
				payload.Normalized.Monthly.PeriodType != "monthly" || payload.Normalized.Monthly.PeriodEnd == nil ||
				*payload.Normalized.Monthly.PeriodEnd != "2026-08-01T00:00:00Z" ||
				len(payload.AuxiliaryErrors) != 1 || payload.AuxiliaryErrors[0].Code != tc.wantCode {
				t.Fatalf("monthly-only quota = %#v", payload)
			}
			assertXAIFloat(t, "verified monthly usage", payload.Normalized.Monthly.UsagePercent, 20)
		})
	}
}

func TestFetchXAIQuotaKeepsWeeklyAndMonthlyMeasurementsIndependent(t *testing.T) {
	result, err := fetchXAIQuotaResultContext(context.Background(), func(_ context.Context, req quota.APICallRequest) (*quota.APICallResponse, error) {
		switch req.URL {
		case xaiBillingCreditsURL:
			return &quota.APICallResponse{StatusCode: http.StatusOK, Body: json.RawMessage(`{"config":{"currentPeriod":{"type":"weekly","start":"2026-07-08T00:00:00Z","end":"2026-07-15T00:00:00Z"},"monthlyLimit":{"val":999},"used":{"val":999}}}`)}, nil
		case xaiBillingURL:
			return &quota.APICallResponse{StatusCode: http.StatusOK, Body: json.RawMessage(`{"config":{"monthlyLimit":{"val":100},"used":{"val":20},"billingPeriodStart":"2026-07-01T00:00:00Z","billingPeriodEnd":"2026-08-01T00:00:00Z"}}`)}, nil
		case xaiSubscriptionURL:
			return &quota.APICallResponse{StatusCode: http.StatusOK, Body: json.RawMessage(`{"user":{"subscriptionTier":"Paid"}}`)}, nil
		case xaiSettingsURL:
			return &quota.APICallResponse{StatusCode: http.StatusOK, Body: json.RawMessage(`{}`)}, nil
		default:
			t.Fatalf("unexpected URL %s", req.URL)
			return nil, nil
		}
	}, "xai-index")
	if err != nil || result.Completeness != quota.FetchComplete {
		t.Fatalf("quota result = %#v, error = %v", result, err)
	}
	var payload xaiQuotaPayload
	if err := json.Unmarshal(result.Data, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Normalized.Weekly.UsagePercent != nil || payload.Normalized.UsagePercent != nil ||
		payload.Normalized.MonthlyLimit == nil || *payload.Normalized.MonthlyLimit != 100 ||
		payload.Normalized.IncludedUsed == nil || *payload.Normalized.IncludedUsed != 20 ||
		payload.Normalized.AmountUnit != "unknown" || payload.Normalized.Currency != nil ||
		payload.ValueSource["monthly_limit"] != "billing_legacy.config.monthlyLimit" ||
		payload.ValueSource["weekly.usage_percent"] != "" ||
		payload.Normalized.Weekly.PeriodEnd == nil || *payload.Normalized.Weekly.PeriodEnd != "2026-07-15T00:00:00Z" ||
		payload.Normalized.Monthly.PeriodEnd == nil || *payload.Normalized.Monthly.PeriodEnd != "2026-08-01T00:00:00Z" {
		t.Fatalf("quota sources mixed: %#v", payload)
	}
	assertXAIFloat(t, "monthly usage", payload.Normalized.Monthly.UsagePercent, 20)
}

func TestFetchXAIQuotaMonthlyFailurePublishesPartialWeeklyWithoutBorrowingValues(t *testing.T) {
	result, err := fetchXAIQuotaResultContext(context.Background(), func(_ context.Context, req quota.APICallRequest) (*quota.APICallResponse, error) {
		switch req.URL {
		case xaiBillingCreditsURL:
			return &quota.APICallResponse{StatusCode: http.StatusOK, Body: json.RawMessage(`{"config":{"creditUsagePercent":42,"currentPeriod":{"type":"weekly","start":"2026-07-08T00:00:00Z","end":"2026-07-15T00:00:00Z"},"monthlyLimit":{"val":999},"used":{"val":999}}}`)}, nil
		case xaiBillingURL:
			return &quota.APICallResponse{StatusCode: http.StatusServiceUnavailable}, nil
		case xaiSubscriptionURL:
			return &quota.APICallResponse{StatusCode: http.StatusOK, Body: json.RawMessage(`{"user":{"subscriptionTier":"Paid"}}`)}, nil
		case xaiSettingsURL:
			return &quota.APICallResponse{StatusCode: http.StatusOK, Body: json.RawMessage(`{}`)}, nil
		default:
			t.Fatalf("unexpected URL %s", req.URL)
			return nil, nil
		}
	}, "xai-index")
	if err != nil || result.Completeness != quota.FetchPartial || result.PartialError == "" {
		t.Fatalf("quota result = %#v, error = %v", result, err)
	}
	var payload xaiQuotaPayload
	if err := json.Unmarshal(result.Data, &payload); err != nil {
		t.Fatal(err)
	}
	assertXAIFloat(t, "weekly usage", payload.Normalized.Weekly.UsagePercent, 42)
	if payload.Normalized.Monthly.PeriodType != "unknown" || payload.Normalized.Monthly.UsagePercent != nil ||
		payload.Normalized.MonthlyLimit != nil || payload.Normalized.IncludedUsed != nil ||
		payload.Normalized.AmountUnit != "unknown" || payload.Normalized.Currency != nil ||
		payload.ValueSource["monthly_limit"] != "" || payload.ValueSource["amount_unit"] != "" ||
		len(payload.AuxiliaryErrors) != 1 || payload.AuxiliaryErrors[0].Source != "monthly_billing" {
		t.Fatalf("monthly failure leaked weekly values: %#v", payload)
	}
}

func TestNormalizeXAIBillingRetainsMonthlyDatesWithoutUsablePercentage(t *testing.T) {
	for _, tc := range []struct {
		name, usage string
	}{
		{name: "missing used", usage: `"monthlyLimit":{"val":100}`},
		{name: "zero limit", usage: `"monthlyLimit":{"val":0},"used":{"val":25}`},
		{name: "overflowing ratio", usage: `"monthlyLimit":{"val":1e-300},"used":{"val":1e300}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"config":{` + tc.usage + `,"billingPeriodStart":"2026-07-01T00:00:00Z","billingPeriodEnd":"2026-08-01T00:00:00Z"}}`
			payload, err := normalizeXAIBilling([]byte(body), "legacy", time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if payload.Normalized.Monthly.UsagePercent != nil || payload.ValueSource["monthly.usage_percent"] != "" ||
				payload.Normalized.Monthly.PeriodType != "monthly" || payload.Normalized.Monthly.PeriodEnd == nil ||
				*payload.Normalized.Monthly.PeriodEnd != "2026-08-01T00:00:00Z" {
				t.Fatalf("monthly period with unavailable percentage = %#v", payload.Normalized.Monthly)
			}
			if _, err := json.Marshal(payload); err != nil {
				t.Fatalf("monthly quota payload cannot be published: %v", err)
			}
		})
	}
}

func TestFetchXAIQuotaPublishesBillingBeforeOptionalRequests(t *testing.T) {
	const fileName = "fixture-xai-enrichment.json"
	key := quota.EntryKey(quota.TypeXAI, fileName)
	var scheduler *quota.Scheduler
	var planDeadline time.Time
	var snapshots []quota.EntryState
	scheduler = quota.NewSchedulerWithResult(quota.DefaultConfig(), func(ctx context.Context, entry *quota.Entry) (quota.FetchResult, error) {
		return fetchXAIQuotaResultContext(ctx, func(requestCtx context.Context, req quota.APICallRequest) (*quota.APICallResponse, error) {
			body := `{}`
			switch req.URL {
			case xaiBillingCreditsURL:
				body = `{"config":{"creditUsagePercent":42,"currentPeriod":{"type":"weekly"}}}`
			case xaiBillingURL:
				body = `{"config":{"monthlyLimit":{"val":100},"used":{"val":20}}}`
			case xaiSubscriptionURL, xaiSettingsURL:
				published := scheduler.GetStatus().Credentials[key]
				if published == nil || published.Status != quota.StatusSuccess || len(published.Data) == 0 {
					t.Errorf("billing is not published before optional request %s: %#v", req.URL, published)
				} else {
					var payload xaiQuotaPayload
					if err := json.Unmarshal(published.Data, &payload); err != nil {
						t.Errorf("decode published billing: %v", err)
					} else {
						if payload.Normalized.Weekly.UsagePercent == nil || *payload.Normalized.Weekly.UsagePercent != 42 ||
							payload.Normalized.Monthly.UsagePercent == nil || *payload.Normalized.Monthly.UsagePercent != 20 {
							t.Errorf("published billing changed: weekly=%v monthly=%v", payload.Normalized.Weekly.UsagePercent, payload.Normalized.Monthly.UsagePercent)
						}
					}
				}
				deadline, ok := requestCtx.Deadline()
				if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > 8*time.Second {
					t.Errorf("optional request deadline = %v, exists = %t; want at most 8s", deadline, ok)
				}
				if req.URL == xaiSubscriptionURL {
					planDeadline = deadline
					body = `{"user":{"subscriptionTier":"User Tier"}}`
				} else {
					if deadline != planDeadline {
						t.Errorf("optional requests do not share one deadline: %v != %v", deadline, planDeadline)
					}
					body = `{"subscriptionTierDisplay":"Display Tier"}`
				}
			default:
				return nil, errors.New("unexpected fixture URL")
			}
			return &quota.APICallResponse{StatusCode: http.StatusOK, Body: json.RawMessage(body)}, nil
		}, entry.AuthIndex)
	})
	scheduler.SetPersistence(func(state quota.EntryState) { snapshots = append(snapshots, state) }, nil)
	scheduler.Register(fileName, quota.TypeXAI, "fixture-xai-index")
	snapshots = nil
	scheduler.RefreshNow([]string{key})
	published := scheduler.GetStatus().Credentials[key]
	var payload xaiQuotaPayload
	if err := json.Unmarshal(published.Data, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Normalized.PlanTier == nil || *payload.Normalized.PlanTier != "Display Tier" {
		t.Fatalf("final plan tier = %v, want Display Tier", payload.Normalized.PlanTier)
	}
	if len(snapshots) != 3 || snapshots[1].Status != quota.StatusSuccess || snapshots[2].StateRevision <= snapshots[1].StateRevision {
		t.Fatalf("want loading, billing and enriched snapshots: %#v", snapshots)
	}
}

func assertXAIFloat(t *testing.T, name string, value *float64, want float64) {
	t.Helper()
	if value == nil || *value != want {
		t.Fatalf("%s = %v, want %v", name, value, want)
	}
}
