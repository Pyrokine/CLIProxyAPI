package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	codexauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/codex"
	devinauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/devin"
	kimiauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/kimi"
	xaiauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/xai"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/misc"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/quota"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const (
	claudeUsageURL   = "https://api.anthropic.com/api/oauth/usage?cedar_ember=1&skip_spend=1"
	claudeProfileURL = "https://api.anthropic.com/api/oauth/profile"

	codexUsageURL                 = "https://chatgpt.com/backend-api/wham/usage"
	codexRateLimitResetCreditsURL = "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits"
	codexSubscriptionsURL         = "https://chatgpt.com/backend-api/subscriptions"

	geminiQuotaURL      = "https://cloudcode-pa.googleapis.com/v1internal:retrieveUserQuota"
	geminiCodeAssistURL = "https://cloudcode-pa.googleapis.com/v1internal:getCodeAssistSubscription"

	antigravityLoadCodeAssistURL = "https://daily-cloudcode-pa.googleapis.com/v1internal:loadCodeAssist"

	kimiUsageURL         = "https://api.kimi.com/coding/v1/usages"
	xaiBillingURL        = "https://cli-chat-proxy.grok.com/v1/billing"
	xaiBillingCreditsURL = xaiBillingURL + "?format=credits"
	xaiSubscriptionURL   = "https://cli-chat-proxy.grok.com/v1/user?include=subscription"
	xaiSettingsURL       = "https://cli-chat-proxy.grok.com/v1/settings"
)

var claudeHeaders = map[string]string{
	"Authorization":  "Bearer $TOKEN$",
	"Content-Type":   "application/json",
	"User-Agent":     "claude-cli/2.1.280 (external, cli)",
	"anthropic-beta": "oauth-2025-04-20",
}

var codexHeaders = map[string]string{
	"Authorization": "Bearer $TOKEN$",
	"Content-Type":  "application/json",
	"User-Agent":    "codex-tui/0.149.1 (Mac OS 26.5.2; arm64) iTerm.app/3.6.11 (codex-tui; 0.149.1)",
}

var codexResetCreditsHeaders = map[string]string{
	"Authorization": "Bearer $TOKEN$",
	"Accept":        "application/json",
	"OpenAI-Beta":   "codex-1",
	"Originator":    "Codex Desktop",
	"User-Agent":    "codex-tui/0.149.1 (Mac OS 26.5.2; arm64) iTerm.app/3.6.11 (codex-tui; 0.149.1)",
}

var geminiHeaders = map[string]string{
	"Authorization": "Bearer $TOKEN$",
	"Content-Type":  "application/json",
}

func antigravityHeaders() map[string]string {
	return map[string]string{
		"Authorization": "Bearer $TOKEN$",
		"Content-Type":  "application/json",
		"User-Agent":    misc.AntigravityUserAgent(),
	}
}

var kimiHeaders = map[string]string{
	"Authorization": "Bearer $TOKEN$",
	"Content-Type":  "application/json",
}

func xaiQuotaHeaders() map[string]string {
	headers := xaiauth.ShellIdentityHeaderValues()
	headers["Authorization"] = "Bearer $TOKEN$"
	headers["Accept"] = "application/json"
	return headers
}

func backgroundQuotaAPICall(apiCall quota.InternalAPICallFunc) quota.InternalAPICallContextFunc {
	return func(_ context.Context, req quota.APICallRequest) (*quota.APICallResponse, error) {
		return apiCall(req)
	}
}

func fetchClaudeQuotaResultContext(
	ctx context.Context,
	apiCall quota.InternalAPICallContextFunc,
	authIndex string,
) (quota.FetchResult, error) {
	usageResp, err := apiCall(
		ctx,
		quota.APICallRequest{
			URL:       claudeUsageURL,
			Method:    "GET",
			Header:    claudeHeaders,
			AuthIndex: authIndex,
		},
	)
	observedAt := time.Now().UTC()
	if err != nil {
		return quota.FetchResult{}, fmt.Errorf("claude usage request failed: %w", err)
	}
	if usageResp == nil {
		return quota.FetchResult{}, fmt.Errorf("claude usage returned empty response")
	}
	if usageResp.StatusCode < 200 || usageResp.StatusCode >= 300 {
		return quota.FetchResult{}, fmt.Errorf("claude usage returned status %d", usageResp.StatusCode)
	}

	profileResp, profileErr := apiCall(
		ctx,
		quota.APICallRequest{
			URL:       claudeProfileURL,
			Method:    "GET",
			Header:    claudeHeaders,
			AuthIndex: authIndex,
		},
	)

	payload := map[string]any{
		"usage": usageResp.Body,
	}
	partialError := ""
	if profileErr != nil {
		partialError = "claude profile request failed"
	} else if profileResp == nil {
		partialError = "claude profile returned empty response"
	} else if profileResp.StatusCode < 200 || profileResp.StatusCode >= 300 {
		partialError = fmt.Sprintf("claude profile returned status %d", profileResp.StatusCode)
	} else {
		payload["profile"] = profileResp.Body
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return quota.FetchResult{}, err
	}
	return newQuotaFetchResult(data, "claude_usage", observedAt, partialError), nil
}

// fetchCodexQuota queries Codex/Copilot quota via api-call proxy.
func fetchCodexQuota(apiCall quota.InternalAPICallFunc, authIndex string) ([]byte, error) {
	return fetchCodexQuotaContext(context.Background(), backgroundQuotaAPICall(apiCall), authIndex)
}

func fetchCodexQuotaContext(
	ctx context.Context,
	apiCall quota.InternalAPICallContextFunc,
	authIndex string,
) ([]byte, error) {
	result, err := fetchCodexQuotaResultContext(ctx, apiCall, authIndex, "")
	if err != nil {
		return nil, err
	}
	return result.Data, nil
}

func fetchCodexQuotaResultContext(
	ctx context.Context,
	apiCall quota.InternalAPICallContextFunc,
	authIndex, accountID string,
) (quota.FetchResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	accountID = strings.TrimSpace(accountID)
	hasAccountID := accountID != "" && len(accountID) <= 256 && strings.IndexFunc(accountID, unicode.IsControl) < 0
	usageHeaders := maps.Clone(codexHeaders)
	resetCreditsHeaders := maps.Clone(codexResetCreditsHeaders)
	if hasAccountID {
		usageHeaders["Chatgpt-Account-Id"] = accountID
		resetCreditsHeaders["Chatgpt-Account-Id"] = accountID
	}
	resp, err := apiCall(
		ctx,
		quota.APICallRequest{
			URL:       codexUsageURL,
			Method:    http.MethodGet,
			Header:    usageHeaders,
			AuthIndex: authIndex,
		},
	)
	observedAt := time.Now().UTC()
	if err != nil {
		return quota.FetchResult{}, fmt.Errorf("codex usage request failed: %w", err)
	}
	if resp == nil {
		return quota.FetchResult{}, errors.New("codex usage returned empty response")
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return quota.FetchResult{}, fmt.Errorf("codex usage returned status %d", resp.StatusCode)
	}

	var payload map[string]json.RawMessage
	if err := json.Unmarshal(resp.Body, &payload); err != nil {
		return quota.FetchResult{}, fmt.Errorf("codex usage payload is invalid: %w", err)
	}
	if payload == nil {
		return quota.FetchResult{}, errors.New("codex usage returned null payload")
	}

	partialErrors := make([]string, 0, 2)
	resetCreditsResp, resetCreditsErr := apiCall(
		ctx,
		quota.APICallRequest{
			URL:       codexRateLimitResetCreditsURL,
			Method:    http.MethodGet,
			Header:    resetCreditsHeaders,
			AuthIndex: authIndex,
		},
	)
	resetCreditsError := ""
	switch {
	case resetCreditsErr != nil:
		resetCreditsError = "codex reset credits request failed"
	case resetCreditsResp == nil:
		resetCreditsError = "codex reset credits returned empty response"
	case resetCreditsResp.StatusCode < http.StatusOK || resetCreditsResp.StatusCode >= http.StatusMultipleChoices:
		resetCreditsError = fmt.Sprintf("codex reset credits returned status %d", resetCreditsResp.StatusCode)
	default:
		payload["rate_limit_reset_credits_details"] = resetCreditsResp.Body
	}
	if resetCreditsError != "" {
		partialErrors = append(partialErrors, resetCreditsError)
		payload["rate_limit_reset_credits_error"] = json.RawMessage(strconv.Quote(resetCreditsError))
	}

	subscriptionError := ""
	if !hasAccountID {
		subscriptionError = "codex subscription account is unavailable"
	} else {
		subscriptionCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
		subscriptionResp, subscriptionErr := apiCall(subscriptionCtx, quota.APICallRequest{
			URL:       codexSubscriptionsURL + "?account_id=" + url.QueryEscape(accountID),
			Method:    http.MethodGet,
			AuthIndex: authIndex,
			Header: map[string]string{
				"Authorization":      codexHeaders["Authorization"],
				"Content-Type":       codexHeaders["Content-Type"],
				"User-Agent":         codexHeaders["User-Agent"],
				"Chatgpt-Account-Id": accountID,
			},
		})
		cancel()
		switch {
		case subscriptionErr != nil:
			subscriptionError = "codex subscription request failed"
		case subscriptionResp == nil:
			subscriptionError = "codex subscription returned empty response"
		case subscriptionResp.StatusCode < http.StatusOK || subscriptionResp.StatusCode >= http.StatusMultipleChoices:
			subscriptionError = fmt.Sprintf("codex subscription returned status %d", subscriptionResp.StatusCode)
		default:
			activeUntil, errParse := parseCodexSubscriptionActiveUntil(subscriptionResp.Body)
			if errParse != nil {
				subscriptionError = "codex subscription did not provide a valid active_until"
			} else {
				payload["subscription_active_until"] = json.RawMessage(strconv.Quote(activeUntil))
				payload["subscription_active_until_source"] = json.RawMessage(`"codex_subscriptions"`)
			}
		}
	}
	if subscriptionError != "" {
		partialErrors = append(partialErrors, subscriptionError)
		payload["subscription_error"] = json.RawMessage(strconv.Quote(subscriptionError))
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return quota.FetchResult{}, err
	}
	return newQuotaFetchResult(data, "codex_usage", observedAt, strings.Join(partialErrors, "; ")), nil
}

func parseCodexSubscriptionActiveUntil(body []byte) (string, error) {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		var encoded string
		if json.Unmarshal(body, &encoded) != nil || json.Unmarshal([]byte(encoded), &payload) != nil {
			return "", errors.New("invalid subscription payload")
		}
	}
	if payload == nil {
		return "", errors.New("missing subscription payload")
	}
	value := payload["active_until"]
	if len(value) == 0 || string(value) == "null" {
		value = payload["activeUntil"]
	}
	if len(value) == 0 || string(value) == "null" {
		return "", errors.New("missing active_until")
	}
	var raw string
	if err := json.Unmarshal(value, &raw); err != nil {
		raw = string(value)
	}
	raw = strings.TrimSpace(raw)
	if parsed, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return parsed.UTC().Format(time.RFC3339Nano), nil
	}
	unixValue, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || unixValue <= 0 {
		return "", errors.New("invalid active_until")
	}
	var date time.Time
	if unixValue >= 1_000_000_000_000 {
		date = time.UnixMilli(unixValue)
	} else {
		date = time.Unix(unixValue, 0)
	}
	if date.Year() < 2000 || date.Year() > 9999 {
		return "", errors.New("active_until is outside supported dates")
	}
	return date.UTC().Format(time.RFC3339Nano), nil
}

func fetchGeminiQuotaResultContext(
	ctx context.Context,
	apiCall quota.InternalAPICallContextFunc,
	authIndex string,
) (quota.FetchResult, error) {
	quotaResp, err := apiCall(
		ctx,
		quota.APICallRequest{
			URL:       geminiQuotaURL,
			Method:    "POST",
			Header:    geminiHeaders,
			AuthIndex: authIndex,
			Data:      "{}",
		},
	)
	observedAt := time.Now().UTC()
	if err != nil {
		return quota.FetchResult{}, fmt.Errorf("gemini quota request failed: %w", err)
	}
	if quotaResp == nil {
		return quota.FetchResult{}, fmt.Errorf("gemini quota returned empty response")
	}
	if quotaResp.StatusCode < 200 || quotaResp.StatusCode >= 300 {
		return quota.FetchResult{}, fmt.Errorf("gemini quota returned status %d", quotaResp.StatusCode)
	}

	codeAssistResp, codeAssistErr := apiCall(
		ctx,
		quota.APICallRequest{
			URL:       geminiCodeAssistURL,
			Method:    "POST",
			Header:    geminiHeaders,
			AuthIndex: authIndex,
			Data:      "{}",
		},
	)

	payload := map[string]any{
		"quota": quotaResp.Body,
	}
	partialError := ""
	if codeAssistErr != nil {
		partialError = "gemini code assist request failed"
	} else if codeAssistResp == nil {
		partialError = "gemini code assist returned empty response"
	} else if codeAssistResp.StatusCode < 200 || codeAssistResp.StatusCode >= 300 {
		partialError = fmt.Sprintf("gemini code assist returned status %d", codeAssistResp.StatusCode)
	} else {
		payload["codeAssist"] = codeAssistResp.Body
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return quota.FetchResult{}, err
	}
	return newQuotaFetchResult(data, "gemini_quota", observedAt, partialError), nil
}

func responseServerTimeOffsetMilliseconds(header map[string][]string, observedAt time.Time) (int64, bool) {
	for name, values := range header {
		if !strings.EqualFold(strings.TrimSpace(name), "Date") {
			continue
		}
		for _, value := range values {
			serverTime, err := http.ParseTime(strings.TrimSpace(value))
			if err == nil {
				return serverTime.Sub(observedAt).Milliseconds(), true
			}
		}
	}
	return 0, false
}

// fetchAntigravityQuota queries Antigravity quota and subscription tier via api-call proxy.
func fetchAntigravityQuota(apiCall quota.InternalAPICallFunc, authIndex string) ([]byte, error) {
	return fetchAntigravityQuotaContext(context.Background(), backgroundQuotaAPICall(apiCall), authIndex)
}

func fetchAntigravityQuotaContext(
	ctx context.Context,
	apiCall quota.InternalAPICallContextFunc,
	authIndex string,
) ([]byte, error) {
	result, err := fetchAntigravityQuotaResultContext(ctx, apiCall, authIndex)
	if err != nil {
		return nil, err
	}
	return result.Data, nil
}

func fetchAntigravityQuotaResultContext(
	ctx context.Context,
	apiCall quota.InternalAPICallContextFunc,
	authIndex string,
) (quota.FetchResult, error) {
	quotaResp, err := apiCall(
		ctx,
		quota.APICallRequest{
			URL:       geminiQuotaURL,
			Method:    "POST",
			Header:    antigravityHeaders(),
			AuthIndex: authIndex,
			Data:      "{}",
		},
	)
	observedAt := time.Now().UTC()
	if err != nil {
		return quota.FetchResult{}, fmt.Errorf("antigravity quota request failed: %w", err)
	}
	if quotaResp == nil {
		return quota.FetchResult{}, fmt.Errorf("antigravity quota returned empty response")
	}
	if quotaResp.StatusCode < 200 || quotaResp.StatusCode >= 300 {
		return quota.FetchResult{}, fmt.Errorf("antigravity quota returned status %d", quotaResp.StatusCode)
	}
	serverTimeOffsetMillis, hasServerTimeOffset := responseServerTimeOffsetMilliseconds(quotaResp.Header, observedAt)

	codeAssistResp, codeAssistErr := apiCall(
		ctx,
		quota.APICallRequest{
			URL:       antigravityLoadCodeAssistURL,
			Method:    "POST",
			Header:    antigravityHeaders(),
			AuthIndex: authIndex,
			Data:      `{"metadata":{"ideType":"ANTIGRAVITY"}}`,
		},
	)

	payload := map[string]any{
		"quota": quotaResp.Body,
	}
	if hasServerTimeOffset {
		payload["server_time_offset_ms"] = serverTimeOffsetMillis
	}
	partialError := ""
	if codeAssistErr != nil {
		partialError = "antigravity code assist request failed"
	} else if codeAssistResp == nil {
		partialError = "antigravity code assist returned empty response"
	} else if codeAssistResp.StatusCode < 200 || codeAssistResp.StatusCode >= 300 {
		partialError = fmt.Sprintf("antigravity code assist returned status %d", codeAssistResp.StatusCode)
	} else {
		payload["codeAssist"] = codeAssistResp.Body
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return quota.FetchResult{}, err
	}
	return newQuotaFetchResult(data, "antigravity_quota", observedAt, partialError), nil
}

func fetchKimiQuotaResultContext(
	ctx context.Context,
	apiCall quota.InternalAPICallContextFunc,
	authManager *coreauth.Manager,
	authIndex string,
) (quota.FetchResult, error) {
	auth := quotaAuthByIndex(authManager, authIndex)
	if auth == nil {
		return quota.FetchResult{}, fmt.Errorf("kimi credential %q is unavailable", authIndex)
	}
	credentialType, recognized := quotaCredentialTypeForProvider(auth.Provider)
	if !recognized || credentialType != quota.TypeKimi {
		return quota.FetchResult{}, fmt.Errorf("credential %q is not a Kimi account", authIndex)
	}
	if kimiauth.IsKimiAIAuth(auth) {
		return quota.FetchResult{}, fmt.Errorf("kimi.ai usage endpoint is not verified for credential %q", authIndex)
	}
	resp, err := apiCall(
		ctx,
		quota.APICallRequest{
			URL:       kimiUsageURL,
			Method:    "GET",
			Header:    kimiHeaders,
			AuthIndex: authIndex,
		},
	)
	observedAt := time.Now().UTC()
	if err != nil {
		return quota.FetchResult{}, fmt.Errorf("kimi usage request failed: %w", err)
	}
	if resp == nil {
		return quota.FetchResult{}, fmt.Errorf("kimi usage returned empty response")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return quota.FetchResult{}, fmt.Errorf("kimi usage returned status %d", resp.StatusCode)
	}
	return newQuotaFetchResult(resp.Body, "kimi_usage", observedAt, ""), nil
}

func fetchDevinQuotaResultContext(
	ctx context.Context,
	cfg *config.Config,
	authManager *coreauth.Manager,
	authIndex string,
) (quota.FetchResult, error) {
	auth := quotaAuthByIndex(authManager, authIndex)
	if auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), "devin") {
		return quota.FetchResult{}, fmt.Errorf("devin credential %q is unavailable", authIndex)
	}
	token, baseURL, deviceSeed := devinauth.CredentialsFromAuth(auth)
	if token == "" {
		return quota.FetchResult{}, fmt.Errorf("devin credential %q is missing a session token", authIndex)
	}
	client := executor.NewProxyAwareHTTPClient(ctx, cfg, auth, 30*time.Second)
	service := devinauth.NewDevinAuthService(client)
	service.SetServerBaseURL(baseURL)
	status, err := service.FetchUserStatus(ctx, token, deviceSeed)
	observedAt := time.Now().UTC()
	if err != nil {
		return quota.FetchResult{}, fmt.Errorf("devin user status request failed: %w", err)
	}
	data, err := json.Marshal(status)
	if err != nil {
		return quota.FetchResult{}, fmt.Errorf("encode Devin user status: %w", err)
	}
	return newQuotaFetchResult(data, "devin_user_status", observedAt, ""), nil
}

func quotaAuthByIndex(authManager *coreauth.Manager, authIndex string) *coreauth.Auth {
	if authManager == nil || strings.TrimSpace(authIndex) == "" {
		return nil
	}
	for _, auth := range authManager.List() {
		if auth == nil {
			continue
		}
		auth.EnsureIndex()
		if auth.Index == authIndex {
			return auth
		}
	}
	return nil
}

func codexQuotaAccountID(authManager *coreauth.Manager, authIndex string) string {
	auth := quotaAuthByIndex(authManager, authIndex)
	if auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return ""
	}
	if storage, ok := auth.Storage.(*codexauth.TokenStorage); ok {
		if accountID := strings.TrimSpace(storage.AccountID); accountID != "" {
			return accountID
		}
	}
	if accountID, ok := auth.Metadata["account_id"].(string); ok {
		return strings.TrimSpace(accountID)
	}
	return ""
}

func fetchXAIQuota(apiCall quota.InternalAPICallFunc, authIndex string) ([]byte, error) {
	return fetchXAIQuotaContext(context.Background(), backgroundQuotaAPICall(apiCall), authIndex)
}

func fetchXAIQuotaContext(
	ctx context.Context,
	apiCall quota.InternalAPICallContextFunc,
	authIndex string,
) ([]byte, error) {
	result, err := fetchXAIQuotaResultContext(ctx, apiCall, authIndex)
	if err != nil {
		return nil, err
	}
	if result.Enrich != nil {
		result, err = result.Enrich(ctx)
		if err != nil {
			return nil, err
		}
	}
	return result.Data, nil
}

func fetchXAIQuotaResultContext(
	ctx context.Context,
	apiCall quota.InternalAPICallContextFunc,
	authIndex string,
) (quota.FetchResult, error) {
	observedAt := time.Now().UTC()
	payload, partialErrors, err := fetchXAIBillingSnapshotsContext(ctx, apiCall, authIndex, observedAt)
	if err != nil {
		return quota.FetchResult{}, err
	}

	encoded, err := json.Marshal(payload)
	if err != nil {
		return quota.FetchResult{}, fmt.Errorf("marshal xai billing payload: %w", err)
	}
	result := newQuotaFetchResult(encoded, "xai_billing", observedAt, strings.Join(partialErrors, "; "))
	result.Enrich = func(ctx context.Context) (quota.FetchResult, error) {
		planCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
		defer cancel()
		for _, source := range []struct {
			name string
			url  string
		}{
			{name: "subscription", url: xaiSubscriptionURL},
			{name: "settings", url: xaiSettingsURL},
		} {
			response, requestErr := apiCall(planCtx, quota.APICallRequest{
				URL: source.url, Method: http.MethodGet, Header: xaiQuotaHeaders(), AuthIndex: authIndex,
			})
			switch {
			case requestErr != nil:
				partialErrors = append(partialErrors, "xai "+source.name+" request failed")
				payload.addAuxiliaryError(source.name, "request_failed", 0)
			case response == nil:
				partialErrors = append(partialErrors, "xai "+source.name+" returned empty response")
				payload.addAuxiliaryError(source.name, "empty_response", 0)
			case response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices:
				partialErrors = append(partialErrors, fmt.Sprintf("xai %s returned status %d", source.name, response.StatusCode))
				payload.addAuxiliaryError(source.name, fmt.Sprintf("status_%d", response.StatusCode), response.StatusCode)
			case payload.applySubscription(response.Body, source.name) != nil:
				partialErrors = append(partialErrors, "xai "+source.name+" payload is invalid")
				payload.addAuxiliaryError(source.name, "invalid_payload", response.StatusCode)
			}
		}
		encoded, err := json.Marshal(payload)
		if err != nil {
			return quota.FetchResult{}, fmt.Errorf("marshal xai billing payload: %w", err)
		}
		return newQuotaFetchResult(encoded, "xai_billing", observedAt, strings.Join(partialErrors, "; ")), nil
	}
	return result, nil
}

func fetchXAIBillingSnapshotsContext(
	ctx context.Context,
	apiCall quota.InternalAPICallContextFunc,
	authIndex string,
	observedAt time.Time,
) (*xaiQuotaPayload, []string, error) {
	creditsResp, creditsErr := apiCall(ctx, quota.APICallRequest{
		URL: xaiBillingCreditsURL, Method: http.MethodGet, Header: xaiQuotaHeaders(), AuthIndex: authIndex,
	})
	var weekly *xaiQuotaPayload
	weeklyError, weeklyCode, weeklyStatus := "", "", 0
	switch {
	case creditsErr != nil:
		weeklyError, weeklyCode = "xai weekly credits request failed", "request_failed"
	case creditsResp == nil:
		weeklyError, weeklyCode = "xai weekly credits returned empty response", "empty_response"
	case creditsResp.StatusCode == http.StatusUnauthorized || creditsResp.StatusCode == http.StatusForbidden ||
		creditsResp.StatusCode == http.StatusTooManyRequests:
		return nil, nil, fmt.Errorf("xai weekly credits returned status %d", creditsResp.StatusCode)
	case creditsResp.StatusCode < http.StatusOK || creditsResp.StatusCode >= http.StatusMultipleChoices:
		weeklyStatus = creditsResp.StatusCode
		if xaiCreditsFormatUnsupported(creditsResp) {
			weeklyError, weeklyCode = "xai weekly credits format unsupported", "credits_format_unsupported"
		} else if creditsResp.StatusCode >= http.StatusInternalServerError {
			weeklyError, weeklyCode = fmt.Sprintf("xai weekly credits returned status %d", weeklyStatus), fmt.Sprintf("status_%d", weeklyStatus)
		} else {
			return nil, nil, fmt.Errorf("xai weekly credits returned status %d", weeklyStatus)
		}
	default:
		var err error
		weekly, err = normalizeXAIBilling(creditsResp.Body, "credits", observedAt)
		if err != nil {
			weeklyError, weeklyCode = "xai weekly credits payload is invalid", "invalid_payload"
		} else if weekly.Normalized.Weekly.PeriodType != "weekly" {
			weeklyError, weeklyCode = "xai weekly billing period unavailable", "period_unavailable"
			if weekly.Normalized.Weekly.UsagePercent == nil {
				weekly = nil
			}
		}
	}

	monthlyResp, monthlyErr := apiCall(ctx, quota.APICallRequest{
		URL: xaiBillingURL, Method: http.MethodGet, Header: xaiQuotaHeaders(), AuthIndex: authIndex,
	})
	monthlyError, monthlyCode, monthlyStatus := "", "", 0
	var payload *xaiQuotaPayload
	if weekly != nil {
		payload = weekly
		payload.retainWeeklyBilling()
	}
	switch {
	case monthlyErr != nil:
		monthlyError, monthlyCode = "xai monthly billing request failed", "request_failed"
	case monthlyResp == nil:
		monthlyError, monthlyCode = "xai monthly billing returned empty response", "empty_response"
	case monthlyResp.StatusCode < http.StatusOK || monthlyResp.StatusCode >= http.StatusMultipleChoices:
		monthlyStatus = monthlyResp.StatusCode
		monthlyError = fmt.Sprintf("xai monthly billing returned status %d", monthlyStatus)
		monthlyCode = fmt.Sprintf("status_%d", monthlyStatus)
	case payload != nil:
		if err := payload.applyMonthlyBilling(monthlyResp.Body, observedAt); err != nil {
			monthlyError, monthlyCode = "xai monthly billing payload is invalid", "invalid_payload"
		}
	default:
		var err error
		payload, err = normalizeXAIBilling(monthlyResp.Body, "legacy", observedAt)
		if err != nil || payload.Normalized.Monthly.PeriodType != "monthly" {
			payload = nil
			monthlyError, monthlyCode = "xai monthly billing payload is invalid", "invalid_payload"
		} else {
			payload.Normalized.UsagePercent = payload.Normalized.Monthly.UsagePercent
			delete(payload.ValueSource, "usage_percent")
			if source := payload.ValueSource["monthly.usage_percent"]; source != "" {
				payload.ValueSource["usage_percent"] = source
			}
		}
	}
	if payload == nil {
		return nil, nil, fmt.Errorf("xai billing has no usable window: %s; %s", weeklyError, monthlyError)
	}
	partialErrors := make([]string, 0, 2)
	if weeklyError != "" {
		partialErrors = append(partialErrors, weeklyError)
		payload.addAuxiliaryError("weekly_billing", weeklyCode, weeklyStatus)
	}
	if monthlyError != "" {
		partialErrors = append(partialErrors, monthlyError)
		payload.addAuxiliaryError("monthly_billing", monthlyCode, monthlyStatus)
	}
	return payload, partialErrors, nil
}

func xaiCreditsFormatUnsupported(resp *quota.APICallResponse) bool {
	if resp == nil {
		return false
	}
	switch resp.StatusCode {
	case http.StatusNotFound, http.StatusNotAcceptable, http.StatusUnsupportedMediaType, http.StatusNotImplemented:
		return true
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		message := strings.ToLower(strings.TrimSpace(string(resp.Body) + " " + resp.BodyText))
		if !strings.Contains(message, "format") || !strings.Contains(message, "credit") {
			return false
		}
		for _, marker := range []string{"unsupported", "not supported", "invalid", "unknown", "unrecognized"} {
			if strings.Contains(message, marker) {
				return true
			}
		}
	}
	return false
}

func newQuotaFetchResult(
	data []byte,
	source string,
	observedAt time.Time,
	partialError string,
) quota.FetchResult {
	completeness := quota.FetchComplete
	if strings.TrimSpace(partialError) != "" {
		completeness = quota.FetchPartial
	}
	return quota.FetchResult{
		Data:         append([]byte(nil), data...),
		Source:       source,
		ObservedAt:   observedAt,
		Completeness: completeness,
		PartialError: partialError,
	}
}

type authQuotaSchedulingProvider struct {
	scheduler *quota.Scheduler
	now       func() time.Time
}

func (p authQuotaSchedulingProvider) Snapshots(
	provider,
	model string,
	authIndexes []string,
) map[string]coreauth.QuotaSchedulingSnapshot {
	if p.scheduler == nil {
		return nil
	}
	credType, ok := quotaCredentialTypeForProvider(provider)
	if !ok {
		return nil
	}
	now := time.Now()
	if p.now != nil {
		now = p.now()
	}
	snapshots := p.scheduler.SchedulingSnapshots(credType, authIndexes, model, now)
	result := make(map[string]coreauth.QuotaSchedulingSnapshot, len(snapshots))
	for authIndex, snapshot := range snapshots {
		status := coreauth.QuotaSchedulingStatusStale
		switch snapshot.Status {
		case quota.SchedulingStatusFresh:
			status = coreauth.QuotaSchedulingStatusFresh
		case quota.SchedulingStatusUnknown:
			status = coreauth.QuotaSchedulingStatusUnknown
		case quota.SchedulingStatusStale:
			status = coreauth.QuotaSchedulingStatusStale
		}
		result[authIndex] = coreauth.QuotaSchedulingSnapshot{
			Status:   status,
			Headroom: snapshot.Headroom,
			ResetAt:  snapshot.ResetAt,
		}
	}
	return result
}

func registerAuthManagerForQuota(scheduler *quota.Scheduler, authManager *coreauth.Manager) int {
	if scheduler == nil || authManager == nil {
		return 0
	}
	registered := 0
	for _, auth := range authManager.List() {
		if auth == nil {
			continue
		}
		fileName := strings.TrimSpace(auth.FileName)
		if fileName == "" {
			continue
		}
		credType, ok := quotaCredentialTypeForProvider(auth.Provider)
		if !ok {
			continue
		}
		auth.EnsureIndex()
		if auth.Index == "" {
			continue
		}
		revision, equivalent, err := coreauth.QuotaCredentialRevisions(auth)
		if err != nil {
			scheduler.UnregisterBinding(fileName, credType, auth.Index)
			log.WithError(err).Warnf("quota: cannot fingerprint credential %s", fileName)
			continue
		}
		scheduler.RegisterBinding(fileName, credType, auth.Index, revision, equivalent...)
		scheduler.SetBindingDisabled(fileName, credType, auth.Index, auth.Disabled)
		registered++
	}
	return registered
}

func registerAuthFilesForQuota(scheduler *quota.Scheduler, authDir string) {
	entries, err := os.ReadDir(authDir)
	if err != nil {
		log.Warnf("quota: cannot read auth dir %s: %v", authDir, err)
		return
	}

	registered := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}

		name := entry.Name()
		nameLower := strings.ToLower(name)

		credType, ok := quotaCredentialTypeForFileName(nameLower)
		if !ok {
			continue
		}

		// Compute auth_index the same way as sdk/cliproxy/auth: sha256("file:" + name)[:8]
		authIndex := computeAuthIndex(name)
		data, errRead := os.ReadFile(filepath.Join(authDir, name))
		if errRead != nil {
			scheduler.UnregisterBinding(name, credType, authIndex)
			log.WithError(errRead).Warnf("quota: cannot fingerprint auth file %s", name)
			continue
		}
		metadata, errMetadata := coreauth.DecodeCredentialMetadata(data)
		if errMetadata != nil {
			scheduler.UnregisterBinding(name, credType, authIndex)
			log.WithError(errMetadata).Warnf("quota: auth file %s is not a valid JSON object", name)
			continue
		}
		provider, _ := metadata["type"].(string)
		if strings.TrimSpace(provider) == "" {
			provider = string(credType)
		}
		disabled, _ := metadata["disabled"].(bool)
		coreauth.NormalizeCredentialMetadata(metadata)
		auth := &coreauth.Auth{Provider: provider, Metadata: metadata, Disabled: disabled}
		persistedDigest, errDigest := coreauth.DigestPersistedJSON(data)
		if errDigest == nil {
			errDigest = coreauth.BindPersistedAuthDigest(auth, persistedDigest)
		}
		if errDigest != nil {
			scheduler.UnregisterBinding(name, credType, authIndex)
			log.WithError(errDigest).Warnf("quota: cannot bind persisted auth file %s", name)
			continue
		}
		revision, equivalent, err := coreauth.QuotaCredentialRevisions(auth)
		if err != nil {
			scheduler.UnregisterBinding(name, credType, authIndex)
			log.WithError(err).Warnf("quota: cannot fingerprint auth file %s", name)
			continue
		}
		digest := sha256.Sum256(data)
		equivalent = append(equivalent, hex.EncodeToString(digest[:]))
		scheduler.RegisterBinding(name, credType, authIndex, revision, equivalent...)
		scheduler.SetBindingDisabled(name, credType, authIndex, disabled)
		registered++
	}

	if registered > 0 {
		log.Infof("quota: registered %d auth files from %s", registered, authDir)
	}
}

func quotaCredentialTypeForProvider(provider string) (quota.CredentialType, bool) {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "claude":
		return quota.TypeClaude, true
	case "codex":
		return quota.TypeCodex, true
	case "gemini", "gemini-cli":
		return quota.TypeGeminiCli, true
	case "antigravity":
		return quota.TypeAntigravity, true
	case "kimi", "kimi-ai", "kimi.ai", "kimi.com":
		return quota.TypeKimi, true
	case "xai", "x-ai", "grok":
		return quota.TypeXAI, true
	case "devin", "cognition":
		return quota.TypeDevin, true
	default:
		return "", false
	}
}

func quotaCredentialTypeForFileName(nameLower string) (quota.CredentialType, bool) {
	switch {
	case strings.HasPrefix(nameLower, "claude"):
		return quota.TypeClaude, true
	case strings.HasPrefix(nameLower, "codex"):
		return quota.TypeCodex, true
	case strings.HasPrefix(nameLower, "antigravity"):
		return quota.TypeAntigravity, true
	case strings.HasPrefix(nameLower, "gemini"):
		return quota.TypeGeminiCli, true
	case strings.HasPrefix(nameLower, "kimi"):
		return quota.TypeKimi, true
	case strings.HasPrefix(nameLower, "xai"), strings.HasPrefix(nameLower, "x-ai"), strings.HasPrefix(
		nameLower, "grok",
	):
		return quota.TypeXAI, true
	case strings.HasPrefix(nameLower, "devin"), strings.HasPrefix(nameLower, "cognition"):
		return quota.TypeDevin, true
	default:
		return "", false
	}
}

func computeAuthIndex(fileName string) string {
	seed := "file:" + strings.TrimSpace(fileName)
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:8])
}
