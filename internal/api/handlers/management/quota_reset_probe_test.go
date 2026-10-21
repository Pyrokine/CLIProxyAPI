package management

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/quota"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
	"github.com/gin-gonic/gin"
)

const codexResetProbeAvailable = `{"rate_limit_reset_credits":{"available_count":1}}`

func attachCodexResetProbe(t *testing.T, h *Handler, auth *coreauth.Auth, fetch quota.ContextFetchFunc) *quota.Scheduler {
	t.Helper()
	current, ok := h.authManager.GetByID(auth.ID)
	if !ok {
		t.Fatal("synthetic credential not registered")
	}
	if fetch == nil {
		fetch = func(context.Context, *quota.Entry) ([]byte, error) {
			return []byte(codexResetProbeAvailable), nil
		}
	}
	scheduler := quota.NewSchedulerWithContext(quota.DefaultConfig(), fetch)
	fileName := current.FileName
	if fileName == "" {
		fileName = current.ID + ".json"
	}
	revision, equivalent, err := coreauth.QuotaCredentialRevisions(current)
	if err != nil {
		t.Fatal(err)
	}
	scheduler.RegisterBinding(fileName, quota.TypeCodex, current.EnsureIndex(), revision, equivalent...)
	h.SetQuotaScheduler(scheduler)
	return scheduler
}

type codexResetProbeFixture struct {
	handler      *Handler
	manager      *coreauth.Manager
	auth         *coreauth.Auth
	scheduler    *quota.Scheduler
	consumeCalls atomic.Int32
	probeCalls   atomic.Int32
	consumeHook  func(http.ResponseWriter, *http.Request)
}

func newCodexResetProbeFixture(t *testing.T, fetch quota.ContextFetchFunc) *codexResetProbeFixture {
	t.Helper()
	f := &codexResetProbeFixture{manager: coreauth.NewManager(nil, nil, nil)}
	next := time.Now().Add(time.Hour)
	auth := &coreauth.Auth{
		ID: "synthetic-codex-reset-probe", FileName: "synthetic-codex-reset-probe.json", Provider: "codex",
		Metadata: map[string]any{"access_token": "synthetic-probe-token", "account_id": "synthetic-account"},
		Status:   coreauth.StatusError, StatusMessage: "quota exhausted", Unavailable: true, NextRetryAfter: next,
		Quota: coreauth.QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: next, BackoffLevel: 2},
		ModelStates: map[string]*coreauth.ModelState{
			"synthetic-model": {
				Status: coreauth.StatusError, Unavailable: true, NextRetryAfter: next,
				Quota: coreauth.QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: next, BackoffLevel: 2},
			},
		},
	}
	var err error
	f.auth, err = f.manager.Register(context.Background(), auth)
	if err != nil {
		t.Fatal(err)
	}
	f.handler = NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, f.manager)
	t.Cleanup(func() { _ = f.handler.Close() })
	f.scheduler = attachCodexResetProbe(t, f.handler, f.auth, func(ctx context.Context, entry *quota.Entry) ([]byte, error) {
		f.probeCalls.Add(1)
		if entry.AuthIndex != f.auth.Index || entry.Type != quota.TypeCodex {
			return nil, errors.New("probe used a different synthetic credential")
		}
		if fetch != nil {
			return fetch(ctx, entry)
		}
		return []byte(codexResetProbeAvailable), nil
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.consumeCalls.Add(1)
		if r.Method != http.MethodPost {
			t.Errorf("consume method = %s, want POST", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer synthetic-probe-token" {
			t.Errorf("consume Authorization = %q", got)
		}
		if got := r.Header.Get("Chatgpt-Account-Id"); got != "synthetic-account" {
			t.Errorf("consume Chatgpt-Account-Id = %q, want synthetic-account", got)
		}
		if f.consumeHook != nil {
			f.consumeHook(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":"reset"}`))
	}))
	previousURL := codexRateLimitResetCreditsConsumeURL
	codexRateLimitResetCreditsConsumeURL = server.URL + "/backend-api/wham/rate-limit-reset-credits/consume"
	t.Cleanup(func() {
		server.Close()
		codexRateLimitResetCreditsConsumeURL = previousURL
	})
	return f
}

func callCodexResetProbeHandler(h *Handler, ctx context.Context, authIndex, purpose string, reset bool) *httptest.ResponseRecorder {
	body := map[string]string{"auth_index": authIndex, "provider": "codex"}
	if purpose != "" {
		body["purpose"] = purpose
	}
	encoded, _ := json.Marshal(body)
	path := "/v0/management/quota/fetch"
	if reset {
		path = "/v0/management/reset-quota"
	}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(encoded))).WithContext(ctx)
	c.Request.Header.Set("Content-Type", "application/json")
	if reset {
		h.ResetQuota(c)
	} else {
		h.FetchCredentialQuota(c)
	}
	return recorder
}

func assertCodexResetProbeCooldown(t *testing.T, f *codexResetProbeFixture) {
	t.Helper()
	current, ok := f.manager.GetByID(f.auth.ID)
	if !ok || current == nil {
		t.Error("synthetic credential disappeared")
		return
	}
	if current.Status != f.auth.Status || current.Unavailable != f.auth.Unavailable ||
		!current.NextRetryAfter.Equal(f.auth.NextRetryAfter) || !reflect.DeepEqual(current.Quota, f.auth.Quota) ||
		!reflect.DeepEqual(current.ModelStates, f.auth.ModelStates) {
		t.Error("request changed the existing local credential or model cooldown")
	}
}

func TestCodexResetProbeEligibilityContract(t *testing.T) {
	past := time.Now().Add(-24 * time.Hour).UTC().Format(time.RFC3339Nano)
	future := time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339Nano)
	credit := func(status, expiry string) string {
		return fmt.Sprintf(`{"reset_type":"codex_rate_limits","status":%q,"expires_at":%q}`, status, expiry)
	}
	for _, test := range []struct {
		name, data, availability string
	}{
		{"one", codexResetProbeAvailable, "available"},
		{"two", `{"rate_limit_reset_credits":{"available_count":2}}`, "available"},
		{"many", `{"rate_limit_reset_credits":{"available_count":12}}`, "available"},
		{"one applicable zero", `{"rate_limit_reset_credits":{"available_count":1,"applicable_available_count":0}}`, "available"},
		{"camel aliases", `{"rateLimitResetCredits":{"availableCount":"2","applicableAvailableCount":0}}`, "available"},
		{"matching count aliases", `{"rate_limit_reset_credits":{"available_count":1,"availableCount":"1"}}`, "available"},
		{"details only", `{"rate_limit_reset_credits_details":{"available_count":2}}`, "available"},
		{"quoted details", `{"rate_limit_reset_credits_details":"{\"available_count\":2}"}`, "available"},
		{"zero", `{"rate_limit_reset_credits":{"available_count":0}}`, "none"},
		{"missing", `{"rate_limit_reset_credits":{}}`, "missing"},
		{"details are not a count", `{"rate_limit_reset_credits_details":{"credits":[` + credit("available", future) + `]}}`, "missing"},
		{"negative", `{"rate_limit_reset_credits":{"available_count":-1}}`, "invalid"},
		{"fraction", `{"rate_limit_reset_credits":{"available_count":1.5}}`, "invalid"},
		{"boolean", `{"rate_limit_reset_credits":{"available_count":true}}`, "invalid"},
		{"null count", `{"rate_limit_reset_credits":{"available_count":null}}`, "invalid"},
		{"unsafe integer", `{"rate_limit_reset_credits":{"available_count":9007199254740992}}`, "invalid"},
		{"invalid numeric string", `{"rate_limit_reset_credits":{"available_count":"1e0"}}`, "invalid"},
		{"count aliases conflict", `{"rate_limit_reset_credits":{"available_count":1,"availableCount":2}}`, "conflict"},
		{"sources conflict", `{"rate_limit_reset_credits":{"available_count":1},"rate_limit_reset_credits_details":{"available_count":0}}`, "conflict"},
		{"invalid second source", `{"rate_limit_reset_credits":{"available_count":1},"rate_limit_reset_credits_details":{"available_count":"invalid"}}`, "invalid"},
		{"expired", `{"rate_limit_reset_credits":{"available_count":1},"rate_limit_reset_credits_details":{"credits":[` + credit("available", past) + `]}}`, "conflict"},
		{"consumed", `{"rate_limit_reset_credits":{"available_count":1},"rate_limit_reset_credits_details":{"credits":[` + credit("consumed", future) + `]}}`, "conflict"},
		{"partly expired", `{"rate_limit_reset_credits":{"available_count":2},"rate_limit_reset_credits_details":{"credits":[` + credit("available", past) + `,` + credit("available", future) + `]}}`, "available"},
		{"partly consumed", `{"rate_limit_reset_credits":{"available_count":2},"rate_limit_reset_credits_details":{"credits":[` + credit("consumed", future) + `,` + credit("available", future) + `]}}`, "available"},
		{"missing optional details", `{"rate_limit_reset_credits":{"available_count":1},"rate_limit":{"allowed":false,"secondary_window":{"used_percent":100}}}`, "available"},
		{"missing optional expiry", `{"rate_limit_reset_credits":{"available_count":1},"rate_limit_reset_credits_details":{"credits":[{"reset_type":"codex_rate_limits","status":"available"}]}}`, "available"},
		{"bad expiry", `{"rate_limit_reset_credits":{"available_count":1},"rate_limit_reset_credits_details":{"credits":[` + credit("available", "not-a-date") + `]}}`, "invalid"},
		{"expiry aliases conflict", `{"rate_limit_reset_credits":{"available_count":1},"rate_limit_reset_credits_details":{"credits":[{"reset_type":"codex_rate_limits","status":"available","expires_at":"` + future + `","expiresAt":"` + past + `"}]}}`, "invalid"},
		{"reset type aliases conflict", `{"rate_limit_reset_credits":{"available_count":1},"rate_limit_reset_credits_details":{"credits":[{"reset_type":"codex_rate_limits","resetType":"other","status":"available"}]}}`, "invalid"},
		{"subscription 403", `{"rate_limit_reset_credits":{"available_count":1},"subscription_error":"subscriptions returned status 403"}`, "available"},
		{"usage failed independent credits available", `{"usage_error":"usage returned status 503","rate_limit_reset_credits_details":{"available_count":1}}`, "available"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newCodexResetProbeFixture(t, func(context.Context, *quota.Entry) ([]byte, error) {
				return []byte(test.data), nil
			})
			before := time.Now()
			response := callCodexResetProbeHandler(f.handler, context.Background(), f.auth.Index, "codex_reset_credit", false)
			if response.Code != http.StatusOK {
				t.Errorf("fetch status = %d, want 200; body=%s", response.Code, response.Body.String())
			} else {
				var payload struct {
					Purpose, AuthIndex, Availability string
					Generation                       uint64
					CheckedAt                        string `json:"checked_at"`
					Data                             map[string]json.RawMessage
				}
				var wire map[string]json.RawMessage
				if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(response.Body.Bytes(), &wire); err != nil {
					t.Fatal(err)
				}
				var authIndex string
				_ = json.Unmarshal(wire["auth_index"], &authIndex)
				targets, _ := f.scheduler.ResolveRefreshTargets([]string{f.auth.FileName})
				if payload.Purpose != "codex_reset_credit" || authIndex != f.auth.Index || payload.Availability != test.availability ||
					len(targets) != 1 || payload.Generation != targets[0].Generation || payload.Data == nil {
					t.Errorf("invalid reset-purpose envelope: %s; want availability=%s", response.Body.String(), test.availability)
				}
				checkedAt, err := time.Parse(time.RFC3339Nano, payload.CheckedAt)
				if err != nil || checkedAt.Before(before) || checkedAt.After(time.Now()) {
					t.Errorf("checked_at = %q, want time of this request", payload.CheckedAt)
				}
				if strings.Contains(response.Body.String(), "synthetic-probe-token") || strings.Contains(response.Body.String(), "access_token") {
					t.Error("probe response included authentication material")
				}
			}
			if f.consumeCalls.Load() != 0 {
				t.Error("read-only eligibility check consumed a reset credit")
			}
			assertCodexResetProbeCooldown(t, f)
			response = callCodexResetProbeHandler(f.handler, context.Background(), f.auth.Index, "", true)
			if test.availability == "available" {
				if response.Code != http.StatusOK || f.consumeCalls.Load() != 1 {
					t.Errorf("eligible reset status=%d consume=%d, want 200/1; body=%s", response.Code, f.consumeCalls.Load(), response.Body.String())
				}
				current, _ := f.manager.GetByID(f.auth.ID)
				if current == nil || current.Quota.Exceeded || current.Unavailable || current.ModelStates["synthetic-model"].Quota.Exceeded {
					t.Error("confirmed successful reset did not clear local quota cooldown")
				}
			} else {
				if response.Code < 400 || f.consumeCalls.Load() != 0 {
					t.Errorf("ineligible reset status=%d consume=%d, want rejection/0; body=%s", response.Code, f.consumeCalls.Load(), response.Body.String())
				}
				assertCodexResetProbeCooldown(t, f)
			}
			if f.probeCalls.Load() != 2 {
				t.Errorf("native probes=%d, want a fresh probe for both fetch and reset", f.probeCalls.Load())
			}
		})
	}
}

func TestCodexResetProbeIsReadOnlyAndBypassesPluginAndDeclarative(t *testing.T) {
	f := newCodexResetProbeFixture(t, nil)
	var declarativeCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		declarativeCalls.Add(1)
		_, _ = w.Write([]byte(`{"remaining":99}`))
	}))
	defer server.Close()
	updated := f.auth.Clone()
	updated.Metadata[pluginapi.QuotaProbeMetadataKey] = map[string]any{"url": server.URL, "method": "GET"}
	if _, err := f.manager.Update(context.Background(), updated); err != nil {
		t.Fatal(err)
	}
	f.auth, _ = f.manager.GetByID(f.auth.ID)
	f.scheduler = attachCodexResetProbe(t, f.handler, f.auth, nil)
	host := &managementQuotaHost{pluginID: "synthetic-plugin", provider: "codex", fetchHandled: true,
		fetchResponse: pluginapi.QuotaFetchResponse{Summary: []pluginapi.QuotaMetric{{Key: "credits", Label: "Credits", Value: 99}}}}
	f.handler.pluginHost = host
	var finalizers atomic.Int32
	f.scheduler.SetRefreshContextFinalizer(func(context.Context, quota.RefreshAttempt) error {
		finalizers.Add(1)
		return nil
	})
	before, _ := json.Marshal(f.scheduler.GetStatus())
	response := callCodexResetProbeHandler(f.handler, context.Background(), f.auth.Index, "codex_reset_credit", false)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"purpose":"codex_reset_credit"`) {
		t.Errorf("native purpose response=%d %s", response.Code, response.Body.String())
	}
	after, _ := json.Marshal(f.scheduler.GetStatus())
	if string(before) != string(after) || finalizers.Load() != 0 || host.fetchCalls != 0 || declarativeCalls.Load() != 0 || f.consumeCalls.Load() != 0 {
		t.Errorf("read-only probe side effects: cacheChanged=%v finalizer=%d plugin=%d declarative=%d consume=%d",
			string(before) != string(after), finalizers.Load(), host.fetchCalls, declarativeCalls.Load(), f.consumeCalls.Load())
	}
	assertCodexResetProbeCooldown(t, f)
}

func TestCodexResetProbePurposeValidationAndLegacyFetch(t *testing.T) {
	f := newCodexResetProbeFixture(t, nil)
	host := &managementQuotaHost{pluginID: "synthetic-plugin", provider: "codex", fetchHandled: true,
		fetchResponse: pluginapi.QuotaFetchResponse{Summary: []pluginapi.QuotaMetric{{Key: "credits", Label: "Credits", Value: 7}}}}
	f.handler.pluginHost = host
	response := callCodexResetProbeHandler(f.handler, context.Background(), f.auth.Index, "", false)
	if response.Code != http.StatusOK || host.fetchCalls != 1 || f.probeCalls.Load() != 0 {
		t.Errorf("legacy fetch changed: status=%d plugin=%d native=%d", response.Code, host.fetchCalls, f.probeCalls.Load())
	}
	response = callCodexResetProbeHandler(f.handler, context.Background(), f.auth.Index, "unsupported", false)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "error") || host.fetchCalls != 1 {
		t.Errorf("invalid purpose must fail before plugin lookup: status=%d plugin=%d body=%s", response.Code, host.fetchCalls, response.Body.String())
	}
	updated := f.auth.Clone()
	updated.Provider = "claude"
	if _, err := f.manager.Update(context.Background(), updated); err != nil {
		t.Fatal(err)
	}
	response = callCodexResetProbeHandler(f.handler, context.Background(), f.auth.Index, "codex_reset_credit", false)
	if response.Code < 400 || host.fetchCalls != 1 || f.probeCalls.Load() != 0 || f.consumeCalls.Load() != 0 {
		t.Errorf("spoofed request provider accepted: status=%d plugin=%d native=%d consume=%d", response.Code, host.fetchCalls, f.probeCalls.Load(), f.consumeCalls.Load())
	}
	assertCodexResetProbeCooldown(t, f)
}

func TestCodexResetProbeRejectsFailedReads(t *testing.T) {
	for _, test := range []struct {
		name  string
		fetch quota.ContextFetchFunc
	}{
		{"network error", func(context.Context, *quota.Entry) ([]byte, error) {
			return nil, errors.New("synthetic credit GET failed")
		}},
		{"bad JSON", func(context.Context, *quota.Entry) ([]byte, error) { return []byte(`{`), nil }},
		{"null JSON", func(context.Context, *quota.Entry) ([]byte, error) { return []byte(`null`), nil }},
		{"credit read error", func(context.Context, *quota.Entry) ([]byte, error) {
			return []byte(`{"rate_limit_reset_credits_error":"credit GET returned 503"}`), nil
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newCodexResetProbeFixture(t, test.fetch)
			for _, reset := range []bool{false, true} {
				response := callCodexResetProbeHandler(f.handler, context.Background(), f.auth.Index, "codex_reset_credit", reset)
				if response.Code < 400 || !strings.Contains(response.Body.String(), "error") || f.consumeCalls.Load() != 0 {
					t.Errorf("failed read reset=%v status=%d consume=%d body=%s", reset, response.Code, f.consumeCalls.Load(), response.Body.String())
				}
				assertCodexResetProbeCooldown(t, f)
			}
		})
	}
}

func TestCodexResetProbeRejectsCancelledAndTimedOutChecks(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		t.Run(fmt.Sprintf("timeout=%v", timeout), func(t *testing.T) {
			f := newCodexResetProbeFixture(t, func(ctx context.Context, _ *quota.Entry) ([]byte, error) {
				<-ctx.Done()
				return nil, ctx.Err()
			})
			for _, reset := range []bool{false, true} {
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
				if !timeout {
					cancel()
				}
				response := callCodexResetProbeHandler(f.handler, ctx, f.auth.Index, "codex_reset_credit", reset)
				cancel()
				if response.Code < 400 || f.consumeCalls.Load() != 0 {
					t.Errorf("cancelled check reset=%v status=%d consume=%d", reset, response.Code, f.consumeCalls.Load())
				}
				assertCodexResetProbeCooldown(t, f)
			}
		})
	}
}

func TestCodexResetProbeDoesNotTrustYesterdayCacheOrEarlierCheck(t *testing.T) {
	for _, data := range []string{
		`{"rate_limit_reset_credits":{"available_count":0}}`,
		`{"rate_limit_reset_credits":{"available_count":1},"rate_limit_reset_credits_details":{"credits":[{"reset_type":"codex_rate_limits","status":"available","expires_at":"2000-01-01T00:00:00Z"}]}}`,
		`{"rate_limit_reset_credits":{"available_count":1},"rate_limit_reset_credits_details":{"credits":[{"reset_type":"codex_rate_limits","status":"consumed"}]}}`,
	} {
		t.Run(data, func(t *testing.T) {
			var calls atomic.Int32
			f := newCodexResetProbeFixture(t, func(context.Context, *quota.Entry) ([]byte, error) {
				if calls.Add(1) == 1 {
					return []byte(codexResetProbeAvailable), nil
				}
				return []byte(data), nil
			})
			revision, _, err := coreauth.QuotaCredentialRevisions(f.auth)
			if err != nil {
				t.Fatal(err)
			}
			targets, _ := f.scheduler.ResolveRefreshTargets([]string{f.auth.FileName})
			yesterday := time.Now().Add(-24 * time.Hour)
			f.scheduler.Restore(map[string]quota.EntryState{targets[0].EntryKey: {
				FileName: f.auth.FileName, Type: quota.TypeCodex, AuthIndex: f.auth.Index,
				CredentialRevision: revision, Generation: targets[0].Generation, StateRevision: 100,
				Status: quota.StatusSuccess, Freshness: quota.FreshnessFresh,
				ObservedAt: &yesterday, FetchedAt: &yesterday, LastRefresh: &yesterday,
				Data: json.RawMessage(codexResetProbeAvailable),
			}})
			response := callCodexResetProbeHandler(f.handler, context.Background(), f.auth.Index, "codex_reset_credit", false)
			if response.Code != http.StatusOK {
				t.Errorf("initial check status=%d body=%s", response.Code, response.Body.String())
			}
			response = callCodexResetProbeHandler(f.handler, context.Background(), f.auth.Index, "", true)
			if response.Code < 400 || f.consumeCalls.Load() != 0 || calls.Load() != 2 {
				t.Errorf("reset trusted stale evidence: status=%d consume=%d freshReads=%d", response.Code, f.consumeCalls.Load(), calls.Load())
			}
			assertCodexResetProbeCooldown(t, f)
		})
	}
}

func TestCodexResetProbeRejectsIdentityAndProxyChanges(t *testing.T) {
	for _, change := range []string{"token", "account", "disabled", "deleted", "auth proxy", "global proxy", "scheduler generation"} {
		t.Run(change, func(t *testing.T) {
			var f *codexResetProbeFixture
			f = newCodexResetProbeFixture(t, func(context.Context, *quota.Entry) ([]byte, error) {
				updated := f.auth.Clone()
				switch change {
				case "token":
					updated.Metadata["access_token"] = "synthetic-replacement"
				case "account":
					updated.Metadata["account_id"] = "synthetic-other-account"
				case "disabled":
					updated.Disabled = true
				case "deleted":
					if err := f.manager.Remove(context.Background(), f.auth.ID); err != nil {
						return nil, err
					}
					return []byte(codexResetProbeAvailable), nil
				case "auth proxy":
					updated.ProxyURL = "direct"
				case "global proxy":
					cfg := &config.Config{AuthDir: t.TempDir()}
					cfg.ProxyURL = "direct"
					f.handler.SetConfig(cfg)
					return []byte(codexResetProbeAvailable), nil
				case "scheduler generation":
					f.scheduler.RegisterBinding(f.auth.FileName, quota.TypeCodex, f.auth.Index, "synthetic-replacement-revision")
					return []byte(codexResetProbeAvailable), nil
				}
				committed, err := f.manager.Update(context.Background(), updated)
				if change == "disabled" && err == nil && committed != nil {
					// 禁用动作会清理冷却，核查失败应保留该动作完成后的状态
					f.auth = committed.Clone()
				}
				return []byte(codexResetProbeAvailable), err
			})
			response := callCodexResetProbeHandler(f.handler, context.Background(), f.auth.Index, "", true)
			if response.Code < 400 || f.consumeCalls.Load() != 0 || f.probeCalls.Load() != 1 {
				t.Errorf("changed identity/config allowed consumption: status=%d consume=%d probe=%d", response.Code, f.consumeCalls.Load(), f.probeCalls.Load())
			}
			if change == "deleted" {
				if current, _ := f.manager.GetByID(f.auth.ID); current != nil {
					t.Error("deleted credential reappeared")
				}
			} else {
				assertCodexResetProbeCooldown(t, f)
			}
		})
	}
}

func TestCodexResetProbeConcurrentResetRejectsBusyWithoutQueueing(t *testing.T) {
	f := newCodexResetProbeFixture(t, nil)
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	f.consumeHook = func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		select {
		case <-release:
			_, _ = w.Write([]byte(`{"code":"reset"}`))
		case <-r.Context().Done():
		}
	}
	first := make(chan *httptest.ResponseRecorder, 1)
	go func() { first <- callCodexResetProbeHandler(f.handler, context.Background(), f.auth.Index, "", true) }()
	select {
	case <-entered:
	case response := <-first:
		t.Fatalf("first reset did not reach synthetic consume: status=%d body=%s", response.Code, response.Body.String())
	case <-time.After(2 * time.Second):
		t.Fatal("first reset did not start")
	}
	second := make(chan *httptest.ResponseRecorder, 1)
	go func() { second <- callCodexResetProbeHandler(f.handler, context.Background(), f.auth.Index, "", true) }()
	secondDone := false
	select {
	case response := <-second:
		secondDone = true
		if response.Code != http.StatusConflict && response.Code != http.StatusTooManyRequests {
			t.Errorf("concurrent reset status=%d, want explicit busy response", response.Code)
		}
	case <-entered:
		t.Error("concurrent reset performed a second consume")
	case <-time.After(300 * time.Millisecond):
		t.Error("concurrent reset queued instead of returning busy")
	}
	assertCodexResetProbeCooldown(t, f)
	releaseOnce.Do(func() { close(release) })
	select {
	case response := <-first:
		if response.Code != http.StatusOK {
			t.Errorf("first reset status=%d body=%s", response.Code, response.Body.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first reset did not finish")
	}
	if !secondDone {
		select {
		case <-second:
		case <-time.After(2 * time.Second):
			t.Fatal("concurrent reset did not finish after release")
		}
	}
	if f.consumeCalls.Load() != 1 {
		t.Errorf("consume calls=%d, want exactly 1", f.consumeCalls.Load())
	}
	// A later explicit request must work after the first request releases its in-flight slot.
	response := callCodexResetProbeHandler(f.handler, context.Background(), f.auth.Index, "", true)
	if response.Code != http.StatusOK || f.consumeCalls.Load() != 2 {
		t.Errorf("in-flight slot not cleared: status=%d consume=%d", response.Code, f.consumeCalls.Load())
	}
}

func TestCodexResetProbeFailedCheckReleasesInFlightSlot(t *testing.T) {
	var reads atomic.Int32
	f := newCodexResetProbeFixture(t, func(context.Context, *quota.Entry) ([]byte, error) {
		if reads.Add(1) == 1 {
			return nil, errors.New("synthetic first credit read failed")
		}
		return []byte(codexResetProbeAvailable), nil
	})
	response := callCodexResetProbeHandler(f.handler, context.Background(), f.auth.Index, "", true)
	if response.Code < 400 || f.consumeCalls.Load() != 0 {
		t.Errorf("failed check status=%d consume=%d", response.Code, f.consumeCalls.Load())
	}
	assertCodexResetProbeCooldown(t, f)
	response = callCodexResetProbeHandler(f.handler, context.Background(), f.auth.Index, "", true)
	if response.Code != http.StatusOK || f.consumeCalls.Load() != 1 || reads.Load() != 2 {
		t.Errorf("failed check retained in-flight slot: status=%d consume=%d reads=%d", response.Code, f.consumeCalls.Load(), reads.Load())
	}
}

func TestCodexResetProbeConfirmedConsumeDoesNotClearReplacement(t *testing.T) {
	f := newCodexResetProbeFixture(t, nil)
	f.consumeHook = func(w http.ResponseWriter, _ *http.Request) {
		updated := f.auth.Clone()
		updated.Metadata["access_token"] = "synthetic-replacement-during-consume"
		if _, err := f.manager.Update(context.Background(), updated); err != nil {
			t.Errorf("replace synthetic credential during consume: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"code":"reset"}`))
	}
	response := callCodexResetProbeHandler(f.handler, context.Background(), f.auth.Index, "", true)
	if response.Code < 400 || f.consumeCalls.Load() != 1 {
		t.Errorf("reset of replaced credential status=%d consume=%d, want rejection after exactly one confirmed upstream call", response.Code, f.consumeCalls.Load())
	}
	assertCodexResetProbeCooldown(t, f)
}

func TestCodexResetProbeQueryRejectsReplacedIdentity(t *testing.T) {
	var f *codexResetProbeFixture
	f = newCodexResetProbeFixture(t, func(context.Context, *quota.Entry) ([]byte, error) {
		updated := f.auth.Clone()
		updated.Metadata["access_token"] = "synthetic-replacement-during-query"
		_, err := f.manager.Update(context.Background(), updated)
		return []byte(codexResetProbeAvailable), err
	})
	response := callCodexResetProbeHandler(f.handler, context.Background(), f.auth.Index, "codex_reset_credit", false)
	if response.Code < 400 || f.consumeCalls.Load() != 0 || f.probeCalls.Load() != 1 {
		t.Errorf("query published outdated identity: status=%d consume=%d reads=%d", response.Code, f.consumeCalls.Load(), f.probeCalls.Load())
	}
	assertCodexResetProbeCooldown(t, f)
}

func TestCodexResetProbeRechecksExpiryImmediatelyBeforeConsume(t *testing.T) {
	f := newCodexResetProbeFixture(t, nil)
	data := json.RawMessage(`{"rate_limit_reset_credits":{"available_count":1},"rate_limit_reset_credits_details":{"credits":[{"reset_type":"codex_rate_limits","status":"available","expires_at":"2000-01-01T00:00:00Z"}]}}`)
	err := f.handler.consumeCodexResetCredit(context.Background(), f.handler.configSnapshot(), f.auth, data)
	if err == nil || f.consumeCalls.Load() != 0 {
		t.Fatalf("expired evidence submitted a consuming request: error=%v calls=%d", err, f.consumeCalls.Load())
	}
	assertCodexResetProbeCooldown(t, f)
}
