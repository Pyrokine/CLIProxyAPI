package management

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/pluginhost"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
	"github.com/gin-gonic/gin"
)

type managementQuotaHost struct {
	pluginID      string
	provider      string
	supportsReset bool
	fetchResponse pluginapi.QuotaFetchResponse
	fetchHandled  bool
	fetchErr      error
	resetResponse pluginapi.QuotaResetResponse
	resetHandled  bool
	resetErr      error
	fetchCalls    int
	resetCalls    int
	lastFetch     pluginapi.QuotaFetchRequest
	lastReset     pluginapi.QuotaResetRequest
}

func (h *managementQuotaHost) RegisteredPlugins() []pluginhost.RegisteredPluginInfo {
	return []pluginhost.RegisteredPluginInfo{{
		ID:       h.pluginID,
		Metadata: pluginapi.Metadata{Name: "Quota test plugin"},
	}}
}

func (*managementQuotaHost) PluginBusy(string) bool { return false }

func (*managementQuotaHost) PluginRuntimeIdentity(string) pluginhost.PluginRuntimeIdentity {
	return pluginhost.PluginRuntimeIdentity{}
}

func (*managementQuotaHost) UnloadPluginWithError(string) (bool, error) { return false, nil }

func (*managementQuotaHost) UnloadPluginIfCurrentWithError(pluginhost.PluginRuntimeIdentity) (bool, error) {
	return false, nil
}

func (h *managementQuotaHost) QuotaProviders(context.Context) []pluginhost.RegisteredQuotaProviderInfo {
	if h.pluginID == "" || h.provider == "" {
		return nil
	}
	return []pluginhost.RegisteredQuotaProviderInfo{{
		PluginID:           h.pluginID,
		Provider:           h.provider,
		DisplayName:        "Quota test plugin",
		SupportedProviders: []string{h.provider},
		SupportsReset:      h.supportsReset,
	}}
}

func (h *managementQuotaHost) HasQuotaProviderContext(_ context.Context, provider string) bool {
	return strings.EqualFold(strings.TrimSpace(provider), h.provider)
}

func (h *managementQuotaHost) HasQuotaProviderForPlugin(pluginID string) bool {
	return strings.EqualFold(strings.TrimSpace(pluginID), h.pluginID)
}

func (h *managementQuotaHost) DescribeQuota(
	_ context.Context,
	pluginID string,
) (pluginapi.QuotaDescribeResponse, bool, error) {
	if !h.HasQuotaProviderForPlugin(pluginID) && !strings.EqualFold(pluginID, h.provider) {
		return pluginapi.QuotaDescribeResponse{}, false, nil
	}
	return pluginapi.QuotaDescribeResponse{
		SupportedProviders: []string{h.provider},
		SupportsReset:      h.supportsReset,
	}, true, nil
}

func (h *managementQuotaHost) FetchQuota(
	_ context.Context,
	req pluginapi.QuotaFetchRequest,
) (pluginapi.QuotaFetchResponse, bool, error) {
	h.fetchCalls++
	h.lastFetch = req
	return h.fetchResponse, h.fetchHandled, h.fetchErr
}

func (h *managementQuotaHost) FetchQuotaByPlugin(
	ctx context.Context,
	pluginID string,
	req pluginapi.QuotaFetchRequest,
) (pluginapi.QuotaFetchResponse, bool, error) {
	if !h.HasQuotaProviderForPlugin(pluginID) {
		return pluginapi.QuotaFetchResponse{}, false, nil
	}
	return h.FetchQuota(ctx, req)
}

func (h *managementQuotaHost) ResetQuota(
	_ context.Context,
	req pluginapi.QuotaResetRequest,
) (pluginapi.QuotaResetResponse, bool, error) {
	h.resetCalls++
	h.lastReset = req
	return h.resetResponse, h.resetHandled, h.resetErr
}

func (h *managementQuotaHost) ResetQuotaByPlugin(
	ctx context.Context,
	pluginID string,
	req pluginapi.QuotaResetRequest,
) (pluginapi.QuotaResetResponse, bool, error) {
	if !h.HasQuotaProviderForPlugin(pluginID) {
		return pluginapi.QuotaResetResponse{}, false, nil
	}
	return h.ResetQuota(ctx, req)
}

func newManagementQuotaHandler(
	t *testing.T,
	host *managementQuotaHost,
) (*Handler, *coreauth.Manager, *coreauth.Auth) {
	t.Helper()
	manager := coreauth.NewManager(nil, nil, nil)
	nextRetry := time.Now().Add(time.Hour)
	auth := &coreauth.Auth{
		ID:             "quota-auth",
		FileName:       "quota.json",
		Provider:       host.provider,
		Status:         coreauth.StatusError,
		StatusMessage:  "quota exhausted",
		Unavailable:    true,
		NextRetryAfter: nextRetry,
		Quota: coreauth.QuotaState{
			Exceeded: true, Reason: "quota", NextRecoverAt: nextRetry, BackoffLevel: 2,
		},
		Metadata:   map[string]any{"access_token": "secret-token", "account": "one"},
		Attributes: map[string]string{"account_id": "account-1"},
	}
	auth.EnsureIndex()
	registered, errRegister := manager.Register(context.Background(), auth)
	if errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}
	handler := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	handler.pluginHost = host
	return handler, manager, registered
}

func validQuotaResponse() pluginapi.QuotaFetchResponse {
	return pluginapi.QuotaFetchResponse{
		Subscription: &pluginapi.QuotaSubscription{Plan: "Pro", TierName: "Tier Pro"},
		Groups: []pluginapi.QuotaGroup{{
			DisplayName: "Daily",
			Buckets: []pluginapi.QuotaBucket{{
				Window:            "day",
				RemainingFraction: 0.25,
			}},
		}},
	}
}

func quotaTestContext(method, path, body string) (*gin.Context, *httptest.ResponseRecorder) {
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		ctx.Request.Header.Set("Content-Type", "application/json")
	}
	return ctx, recorder
}

func TestFetchCredentialQuotaAcceptsAuthIndexAliasesAndUsesPluginFirst(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, field := range []string{"auth_index", "authIndex", "AuthIndex"} {
		t.Run(field, func(t *testing.T) {
			host := &managementQuotaHost{
				pluginID:      "quota-plugin",
				provider:      "quota-provider",
				fetchResponse: validQuotaResponse(),
				fetchHandled:  true,
			}
			handler, _, auth := newManagementQuotaHandler(t, host)
			auth.Metadata[pluginapi.QuotaProbeMetadataKey] = map[string]any{
				"url": "http://127.0.0.1/probe-must-not-run",
			}
			body := `{"` + field + `":"` + auth.Index + `"}`
			ctx, recorder := quotaTestContext(http.MethodPost, "/quota/fetch", body)
			handler.FetchCredentialQuota(ctx)
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
			}
			if host.fetchCalls != 1 {
				t.Fatalf("fetch calls = %d, want 1", host.fetchCalls)
			}
			if host.lastFetch.AuthIndex != auth.Index || host.lastFetch.AuthID != auth.ID ||
				host.lastFetch.Provider != auth.Provider {
				t.Fatalf("fetch request = %#v", host.lastFetch)
			}
			if !strings.Contains(string(host.lastFetch.StorageJSON), `"account":"one"`) {
				t.Fatalf("storage JSON = %s", host.lastFetch.StorageJSON)
			}
		})
	}
}

func TestFetchCredentialQuotaFallsBackTo501WithoutPluginOrProbe(t *testing.T) {
	host := &managementQuotaHost{pluginID: "quota-plugin", provider: "quota-provider"}
	handler, _, auth := newManagementQuotaHandler(t, host)
	host.provider = "other-provider"
	delete(auth.Metadata, pluginapi.QuotaProbeMetadataKey)
	ctx, recorder := quotaTestContext(
		http.MethodPost,
		"/quota/fetch",
		`{"auth_index":"`+auth.Index+`"}`,
	)
	handler.FetchCredentialQuota(ctx)
	if recorder.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestResetCredentialQuotaRequiresDeclaredResetAndUsesOnlyFakeProvider(t *testing.T) {
	host := &managementQuotaHost{
		pluginID:      "quota-plugin",
		provider:      "quota-provider",
		fetchResponse: validQuotaResponse(),
		fetchHandled:  true,
		resetResponse: pluginapi.QuotaResetResponse{Success: true, Message: "reset"},
		resetHandled:  true,
	}
	handler, manager, auth := newManagementQuotaHandler(t, host)
	body := `{"auth_index":"` + auth.Index + `"}`
	ctxRejected, rejected := quotaTestContext(http.MethodPost, "/quota/reset", body)
	handler.ResetCredentialQuota(ctxRejected)
	if rejected.Code != http.StatusMethodNotAllowed || host.resetCalls != 0 {
		t.Fatalf("unsupported reset status=%d calls=%d body=%s", rejected.Code, host.resetCalls, rejected.Body.String())
	}
	stillUnavailable, _ := manager.GetByID(auth.ID)
	if !stillUnavailable.Unavailable || !stillUnavailable.Quota.Exceeded {
		t.Fatalf("unsupported reset changed local quota: %#v", stillUnavailable)
	}

	host.supportsReset = true
	ctxAccepted, accepted := quotaTestContext(http.MethodPost, "/quota/reset", body)
	handler.ResetCredentialQuota(ctxAccepted)
	if accepted.Code != http.StatusOK || host.resetCalls != 1 {
		t.Fatalf("accepted reset status=%d calls=%d body=%s", accepted.Code, host.resetCalls, accepted.Body.String())
	}
	if host.lastReset.AuthIndex != auth.Index || host.lastReset.Provider != auth.Provider {
		t.Fatalf("reset request = %#v", host.lastReset)
	}
	updated, _ := manager.GetByID(auth.ID)
	if updated.Unavailable || updated.Quota.Exceeded {
		t.Fatalf("successful reset did not clear local quota: %#v", updated)
	}
}

func TestResetCredentialQuotaProviderFailureDoesNotClearLocalQuota(t *testing.T) {
	host := &managementQuotaHost{
		pluginID:      "quota-plugin",
		provider:      "quota-provider",
		supportsReset: true,
		resetHandled:  true,
		resetErr:      errors.New("provider unavailable"),
	}
	handler, manager, auth := newManagementQuotaHandler(t, host)
	unavailable := auth.Clone()
	unavailable.Unavailable = true
	unavailable.Quota = coreauth.QuotaState{Exceeded: true, Reason: "quota"}
	if _, errUpdate := manager.Update(coreauth.WithSkipPersist(context.Background()), unavailable); errUpdate != nil {
		t.Fatalf("mark auth unavailable: %v", errUpdate)
	}
	ctx, recorder := quotaTestContext(
		http.MethodPost,
		"/quota/reset",
		`{"auth_index":"`+auth.Index+`"}`,
	)
	handler.ResetCredentialQuota(ctx)
	if recorder.Code != http.StatusBadGateway || host.resetCalls != 1 {
		t.Fatalf("status=%d calls=%d body=%s", recorder.Code, host.resetCalls, recorder.Body.String())
	}
	updated, _ := manager.GetByID(auth.ID)
	if !updated.Unavailable || !updated.Quota.Exceeded {
		t.Fatalf("failed provider reset changed local quota: %#v", updated)
	}
}

func TestPluginQuotaEndpointsAndPluginListExposeCapability(t *testing.T) {
	host := &managementQuotaHost{
		pluginID:      "quota-plugin",
		provider:      "quota-provider",
		supportsReset: true,
		fetchResponse: validQuotaResponse(),
		fetchHandled:  true,
		resetResponse: pluginapi.QuotaResetResponse{Success: true},
		resetHandled:  true,
	}
	handler, _, auth := newManagementQuotaHandler(t, host)

	getCtx, getRecorder := quotaTestContext(
		http.MethodGet,
		"/plugins/quota-plugin/quota?auth_index="+auth.Index,
		"",
	)
	getCtx.Params = gin.Params{{Key: "id", Value: "quota-plugin"}}
	handler.GetPluginQuota(getCtx)
	if getRecorder.Code != http.StatusOK {
		t.Fatalf("GET status=%d body=%s", getRecorder.Code, getRecorder.Body.String())
	}

	postCtx, postRecorder := quotaTestContext(
		http.MethodPost,
		"/plugins/quota-plugin/quota",
		`{"authIndex":"`+auth.Index+`"}`,
	)
	postCtx.Params = gin.Params{{Key: "id", Value: "quota-plugin"}}
	handler.FetchPluginQuota(postCtx)
	if postRecorder.Code != http.StatusOK {
		t.Fatalf("POST status=%d body=%s", postRecorder.Code, postRecorder.Body.String())
	}

	deleteCtx, deleteRecorder := quotaTestContext(
		http.MethodDelete,
		"/plugins/quota-plugin/quota?auth_index="+auth.Index,
		"",
	)
	deleteCtx.Params = gin.Params{{Key: "id", Value: "quota-plugin"}}
	handler.ResetPluginQuota(deleteCtx)
	if deleteRecorder.Code != http.StatusOK || host.resetCalls != 1 {
		t.Fatalf("DELETE status=%d calls=%d body=%s", deleteRecorder.Code, host.resetCalls, deleteRecorder.Body.String())
	}

	providersCtx, providersRecorder := quotaTestContext(http.MethodGet, "/quota/providers", "")
	handler.GetQuotaProviders(providersCtx)
	if providersRecorder.Code != http.StatusOK || !strings.Contains(providersRecorder.Body.String(), `"supports_reset":true`) {
		t.Fatalf("providers status=%d body=%s", providersRecorder.Code, providersRecorder.Body.String())
	}

	pluginsCtx, pluginsRecorder := quotaTestContext(http.MethodGet, "/plugins", "")
	handler.ListPlugins(pluginsCtx)
	if pluginsRecorder.Code != http.StatusOK {
		t.Fatalf("plugins status=%d body=%s", pluginsRecorder.Code, pluginsRecorder.Body.String())
	}
	var pluginList struct {
		Plugins []pluginListEntry `json:"plugins"`
	}
	if errDecode := json.Unmarshal(pluginsRecorder.Body.Bytes(), &pluginList); errDecode != nil {
		t.Fatalf("decode plugins: %v", errDecode)
	}
	if len(pluginList.Plugins) != 1 || !pluginList.Plugins[0].SupportsQuota ||
		pluginList.Plugins[0].QuotaProvider != "quota-provider" {
		t.Fatalf("plugins = %#v", pluginList.Plugins)
	}
}

func TestNormalizeQuotaProbeResponsePreservesUnknownNumericFields(t *testing.T) {
	response, errNormalize := normalizeQuotaProbeResponse([]byte(`{
		"subscription":{"tier_name":"Team"},
		"groups":[{"display_name":"Limits","buckets":[
			{"window":"missing"},
			{"window":"invalid","remaining_fraction":"unknown"},
			{"window":"zero","remaining_fraction":0},
			{"window":"valid","remainingFraction":"0.5"}
		]}]
	}`))
	if errNormalize != nil {
		t.Fatalf("normalize response: %v", errNormalize)
	}
	if response.Subscription == nil || response.Subscription.TierName != "Team" {
		t.Fatalf("subscription = %#v", response.Subscription)
	}
	if len(response.Groups) != 1 || len(response.Groups[0].Buckets) != 2 {
		t.Fatalf("groups = %#v", response.Groups)
	}
	got := response.Groups[0].Buckets
	if got[0].Window != "zero" || got[0].RemainingFraction != 0 ||
		got[1].Window != "valid" || got[1].RemainingFraction != 0.5 {
		t.Fatalf("buckets = %#v", got)
	}
}

func TestMapQuotaProbeResponseSupportsCamelSnakeAndAmountRatios(t *testing.T) {
	data := []byte(`{"account":{"plan":"Pro","tier":"Team"},"limits":[
		{"name":"daily","remaining":25,"total":100,"reset":"2026-09-24T00:00:00Z"},
		{"name":"unknown","remaining":"n/a","total":100}
	]}`)
	mapping := map[string]any{
		"plan":     "account.plan",
		"tierName": "account.tier",
		"groups": []any{map[string]any{
			"displayName":          "Daily limits",
			"buckets_path":         "limits",
			"window_key":           "name",
			"remaining_amount_key": "remaining",
			"total_amount_key":     "total",
			"reset_time_key":       "reset",
		}},
	}
	response, errMap := mapQuotaProbeResponse(data, mapping)
	if errMap != nil {
		t.Fatalf("map response: %v", errMap)
	}
	if response.Subscription == nil || response.Subscription.Plan != "Pro" || response.Subscription.TierName != "Team" {
		t.Fatalf("subscription = %#v", response.Subscription)
	}
	if len(response.Groups) != 1 || len(response.Groups[0].Buckets) != 1 {
		t.Fatalf("groups = %#v", response.Groups)
	}
	bucket := response.Groups[0].Buckets[0]
	if bucket.Window != "daily" || bucket.RemainingFraction != 0.25 || bucket.ResetTime == "" {
		t.Fatalf("bucket = %#v", bucket)
	}
}

func TestDeclarativeQuotaProbeUsesInjectedHTTPAndMapsDateOffset(t *testing.T) {
	host := &managementQuotaHost{pluginID: "quota-plugin", provider: "quota-provider"}
	handler, _, auth := newManagementQuotaHandler(t, host)
	probe := map[string]any{
		"url":    "https://8.8.8.8/quota",
		"method": http.MethodPost,
		"headers": map[string]any{
			"Content-Type": "application/json",
		},
		"data": map[string]any{"scope": "account"},
		"mapping": map[string]any{
			"plan": "account.plan",
			"groups": []any{map[string]any{
				"display_name":           "Daily",
				"buckets_path":           "limits",
				"window_key":             "name",
				"remaining_fraction_key": "remaining",
			}},
		},
	}
	serverTime := time.Now().UTC().Add(time.Minute).Truncate(time.Second)
	response, handled, errProbe := handler.executeQuotaProbeWithHTTPDo(
		context.Background(),
		auth,
		probe,
		func(request *http.Request) (*http.Response, error) {
			if request.Method != http.MethodPost || request.URL.String() != "https://8.8.8.8/quota" {
				t.Fatalf("request = %s %s", request.Method, request.URL)
			}
			requestBody, errRead := io.ReadAll(request.Body)
			if errRead != nil {
				t.Fatalf("read request body: %v", errRead)
			}
			if string(requestBody) != `{"scope":"account"}` {
				t.Fatalf("request body = %s", requestBody)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Date": {serverTime.Format(http.TimeFormat)}},
				Body: io.NopCloser(strings.NewReader(
					`{"account":{"plan":"Pro"},"limits":[{"name":"daily","remaining":0.4}]}`,
				)),
			}, nil
		},
	)
	if errProbe != nil || !handled {
		t.Fatalf("handled=%t error=%v", handled, errProbe)
	}
	if response.Subscription == nil || response.Subscription.Plan != "Pro" ||
		len(response.Groups) != 1 || len(response.Groups[0].Buckets) != 1 ||
		response.Groups[0].Buckets[0].RemainingFraction != 0.4 {
		t.Fatalf("response = %#v", response)
	}
	if response.ServerTimeOffsetMs < 58_000 || response.ServerTimeOffsetMs > 61_000 {
		t.Fatalf("server time offset = %d", response.ServerTimeOffsetMs)
	}
}

func TestDeclarativeQuotaProbeRejectsPrivateAndUntrustedTokenTargetsBeforeNetwork(t *testing.T) {
	host := &managementQuotaHost{pluginID: "quota-plugin", provider: "quota-provider"}
	handler, _, auth := newManagementQuotaHandler(t, host)
	for _, test := range []struct {
		name  string
		probe map[string]any
	}{
		{
			name:  "private target",
			probe: map[string]any{"url": "http://127.0.0.1/quota"},
		},
		{
			name: "untrusted token target",
			probe: map[string]any{
				"url":     "https://8.8.8.8/quota",
				"headers": map[string]any{"Authorization": "Bearer $TOKEN$"},
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, handled, errProbe := handler.executeQuotaProbe(context.Background(), auth, test.probe)
			if !handled || errProbe == nil {
				t.Fatalf("handled=%t error=%v", handled, errProbe)
			}
		})
	}
}

func TestQuotaProbeServerTimeOffsetMilliseconds(t *testing.T) {
	observedAt := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	header := http.Header{"date": {observedAt.Add(90 * time.Second).Format(http.TimeFormat)}}
	offset, okOffset := quotaProbeServerTimeOffsetMilliseconds(header, observedAt)
	if !okOffset || offset != (90*time.Second).Milliseconds() {
		t.Fatalf("offset=%d ok=%t", offset, okOffset)
	}
}

func TestCredentialQuotaRequestAliasPrecedence(t *testing.T) {
	snake, camel, pascal := "snake", "camel", "pascal"
	request := credentialQuotaRequest{
		AuthIndexSnake:  &snake,
		AuthIndexCamel:  &camel,
		AuthIndexPascal: &pascal,
	}
	if got := request.resolveAuthIndex(); got != "snake" {
		t.Fatalf("resolveAuthIndex() = %q", got)
	}
	if got := (credentialQuotaRequest{AuthIndexCamel: &camel, AuthIndexPascal: &pascal}).resolveAuthIndex(); got != "camel" {
		t.Fatalf("camel resolveAuthIndex() = %q", got)
	}
	if got := (credentialQuotaRequest{AuthIndexPascal: &pascal}).resolveAuthIndex(); got != "pascal" {
		t.Fatalf("pascal resolveAuthIndex() = %q", got)
	}
}

func TestQuotaProbeHeadersCopyAndValidateStringValues(t *testing.T) {
	headers, errHeaders := quotaProbeHeaders(map[string]any{
		"header": map[string]any{"Authorization": "Bearer $TOKEN$", "Accept": "application/json"},
	})
	if errHeaders != nil {
		t.Fatalf("headers error: %v", errHeaders)
	}
	if !reflect.DeepEqual(headers, map[string]string{
		"Authorization": "Bearer $TOKEN$",
		"Accept":        "application/json",
	}) {
		t.Fatalf("headers = %#v", headers)
	}
	if _, errInvalid := quotaProbeHeaders(map[string]any{"headers": map[string]any{"X": 1}}); errInvalid == nil {
		t.Fatal("non-string header value was accepted")
	}
}
