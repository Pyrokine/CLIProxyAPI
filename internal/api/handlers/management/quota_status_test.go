package management

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/quota"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/gin-gonic/gin"
)

func TestGetQuotaStatusReturnsFreshnessEnvelope(t *testing.T) {
	observedAt := time.Date(2026, 9, 9, 3, 4, 5, 0, time.UTC)
	scheduler := quota.NewSchedulerWithResult(
		quota.DefaultConfig(),
		func(context.Context, *quota.Entry) (quota.FetchResult, error) {
			return quota.FetchResult{
				Data:         []byte(`{"remaining":5}`),
				Source:       "claude_usage",
				ObservedAt:   observedAt,
				Completeness: quota.FetchComplete,
			}, nil
		},
	)
	scheduler.Register("claude.json", quota.TypeClaude, "auth-1")
	scheduler.RefreshNow([]string{"claude.json"})
	h := &Handler{quotaScheduler: scheduler}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/quota/status", nil)
	h.GetQuotaStatus(c)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GetQuotaStatus() status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Credentials map[string]struct {
			Generation      uint64          `json:"generation"`
			Freshness       quota.Freshness `json:"freshness"`
			Source          string          `json:"source"`
			ObservedAt      *time.Time      `json:"observed_at"`
			FetchedAt       *time.Time      `json:"fetched_at"`
			LastSuccessAt   *time.Time      `json:"last_success_at"`
			SuccessfulEmpty bool            `json:"successful_empty"`
		} `json:"credentials"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode quota status: %v", err)
	}
	entry := response.Credentials[quota.EntryKey(quota.TypeClaude, "claude.json")]
	if entry.Generation == 0 || entry.Freshness != quota.FreshnessFresh ||
		entry.Source != "claude_usage" || entry.ObservedAt == nil || !entry.ObservedAt.Equal(observedAt) ||
		entry.FetchedAt == nil || entry.LastSuccessAt == nil || entry.SuccessfulEmpty {
		t.Fatalf("quota freshness envelope = %#v", entry)
	}
}

func TestCodexQuotaAvailability(t *testing.T) {
	tests := []struct {
		name      string
		data      string
		available bool
		known     bool
	}{
		{
			name: "available", data: `{"rate_limit":{"allowed":true,"limit_reached":false}}`, available: true,
			known: true,
		},
		{
			name: "available production windows",
			data: `{
				"rate_limit":{
					"allowed":true,
					"limit_reached":false,
					"primary_window":{"used_percent":0},
					"secondary_window":null
				},
				"additional_rate_limits":[{
					"rate_limit":{
						"allowed":true,
						"limit_reached":false,
						"primary_window":{"used_percent":0},
						"secondary_window":{"used_percent":0}
					}
				}]
			}`,
			available: true,
			known:     true,
		},
		{
			name:      "available quoted production payload",
			data:      `"{\"rate_limit\":{\"allowed\":true,\"limit_reached\":false,\"primary_window\":{\"used_percent\":0}},\"additional_rate_limits\":[{\"rate_limit\":{\"allowed\":true,\"primary_window\":{\"used_percent\":0},\"secondary_window\":{\"used_percent\":0}}}]}"`,
			available: true,
			known:     true,
		},
		{
			name:      "available camel case strings",
			data:      `{"rateLimit":{"allowed":"true","limitReached":"false","primaryWindow":{"usedPercent":"7.5"}}}`,
			available: true, known: true,
		},
		{name: "exhausted by allowed", data: `{"rate_limit":{"allowed":false}}`, known: true},
		{name: "exhausted by limit", data: `{"rateLimit":{"allowed":true,"limitReached":true}}`, known: true},
		{
			name: "exhausted primary window",
			data: `{"rate_limit":{"allowed":true,"primary_window":{"used_percent":100}}}`, known: true,
		},
		{
			name: "exhausted secondary window", data: `{"rate_limit":{"secondary_window":{"used_percent":"100"}}}`,
			known: true,
		},
		{
			name: "exhausted code review",
			data: `{"rate_limit":{"allowed":true},"code_review_rate_limit":{"limit_reached":true}}`, known: true,
		},
		{
			name:  "exhausted additional limit",
			data:  `{"rate_limit":{"allowed":true},"additional_rate_limits":[{"rate_limit":{"primary_window":{"used_percent":100}}}]}`,
			known: true,
		},
		{name: "unknown", data: `{"rate_limit":{}}`},
		{name: "unknown non numeric percent", data: `{"rate_limit":{"primary_window":{"used_percent":"unknown"}}}`},
		{name: "malformed", data: `{`},
	}
	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				available, known := codexQuotaAvailability([]byte(tt.data))
				if available != tt.available || known != tt.known {
					t.Fatalf("availability = (%v, %v), want (%v, %v)", available, known, tt.available, tt.known)
				}
			},
		)
	}
}

func TestCodexCredentialQuotaAvailabilityUsesRootLimitOnly(t *testing.T) {
	tests := []struct {
		name      string
		data      string
		available bool
		known     bool
	}{
		{
			name: "root available while additional exhausted",
			data: `{
				"rate_limit":{"allowed":true,"primary_window":{"used_percent":32}},
				"additional_rate_limits":[{"rate_limit":{"allowed":false,"primary_window":{"used_percent":100}}}]
			}`,
			available: true,
			known:     true,
		},
		{
			name: "root exhausted while additional available",
			data: `{
				"rate_limit":{"allowed":false,"primary_window":{"used_percent":100}},
				"additional_rate_limits":[{"rate_limit":{"allowed":true,"primary_window":{"used_percent":0}}}]
			}`,
			known: true,
		},
		{
			name: "additional limit cannot supply root authority",
			data: `{
				"rate_limit":{},
				"additional_rate_limits":[{"rate_limit":{"allowed":true,"primary_window":{"used_percent":0}}}]
			}`,
		},
	}
	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				available, known := codexCredentialQuotaAvailability([]byte(tt.data))
				if available != tt.available || known != tt.known {
					t.Fatalf("availability = (%v, %v), want (%v, %v)", available, known, tt.available, tt.known)
				}
			},
		)
	}
}

func TestCodexQuotaFamilyAvailabilityUsesMatchingMeters(t *testing.T) {
	candidate := coreauth.QuotaFamilyRecoveryCandidate{
		Family: "codex-sol",
		Models: []string{"gpt-5.6-sol"},
	}
	tests := []struct {
		name      string
		data      string
		available bool
		known     bool
	}{
		{
			name: "root authority without matching meter",
			data: `{"rate_limit":{"allowed":true},"additional_rate_limits":[{
				"limit_name":"other-model","rate_limit":{"allowed":false}
			}]}`,
			available: true,
			known:     true,
		},
		{
			name: "matching model meter available",
			data: `{"rate_limit":{"allowed":true},"additional_rate_limits":[{
				"limit_name":"models/GPT-5.6-SOL","rate_limit":{"allowed":true,"primary_window":{"used_percent":2}}
			}]}`,
			available: true,
			known:     true,
		},
		{
			name: "matching family feature available",
			data: `{"rate_limit":{"allowed":true},"additional_rate_limits":[{
				"metered_feature":"codex-sol","rate_limit":{"allowed":true}
			}]}`,
			available: true,
			known:     true,
		},
		{
			name: "matching meter exhausted",
			data: `{"rate_limit":{"allowed":true},"additional_rate_limits":[{
				"limit_name":"gpt-5.6-sol","rate_limit":{"limit_reached":true}
			}]}`,
			known: true,
		},
		{
			name: "matching meter unknown",
			data: `{"rate_limit":{"allowed":true},"additional_rate_limits":[{
				"limit_name":"gpt-5.6-sol","rate_limit":{}
			}]}`,
		},
		{
			name: "unnamed exhausted meter ignored",
			data: `{"rate_limit":{"allowed":true},"additional_rate_limits":[{
				"rate_limit":{"allowed":false}
			}]}`,
			available: true,
			known:     true,
		},
		{
			name:      "code review limit ignored",
			data:      `{"rate_limit":{"allowed":true},"code_review_rate_limit":{"allowed":false}}`,
			available: true,
			known:     true,
		},
		{
			name: "unknown root remains unknown",
			data: `{"rate_limit":{},"additional_rate_limits":[{
				"limit_name":"gpt-5.6-sol","rate_limit":{"allowed":true}
			}]}`,
		},
		{
			name: "exhausted root remains exhausted",
			data: `{"rate_limit":{"allowed":false},"additional_rate_limits":[{
				"limit_name":"gpt-5.6-sol","rate_limit":{"allowed":true}
			}]}`,
			known: true,
		},
	}
	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				root, ok := parseCodexQuotaPayload([]byte(test.data))
				if !ok {
					t.Fatal("quota payload did not parse")
				}
				available, known := inspectCodexQuotaFamilyAvailability(root, candidate)
				if available != test.available || known != test.known {
					t.Fatalf(
						"availability = (%v, %v), want (%v, %v)",
						available,
						known,
						test.available,
						test.known,
					)
				}
			},
		)
	}
}

func TestCodexQuotaRefreshRestoresCredentialAndFamilyQuota(t *testing.T) {
	var resetCreditCalls atomic.Int32
	resetCreditServer := httptest.NewServer(
		http.HandlerFunc(
			func(http.ResponseWriter, *http.Request) {
				resetCreditCalls.Add(1)
			},
		),
	)
	t.Cleanup(resetCreditServer.Close)
	previousResetCreditURL := codexRateLimitResetCreditsConsumeURL
	codexRateLimitResetCreditsConsumeURL = resetCreditServer.URL
	t.Cleanup(func() { codexRateLimitResetCreditsConsumeURL = previousResetCreditURL })

	manager := coreauth.NewManager(nil, nil, nil)
	next := time.Now().Add(time.Hour)
	registered, errRegister := manager.Register(
		context.Background(), &coreauth.Auth{
			ID:               "codex-recovered",
			Provider:         "codex",
			Status:           coreauth.StatusError,
			StatusMessage:    "credential quota",
			Unavailable:      true,
			NextRetryAfter:   next,
			Quota:            coreauth.QuotaState{Exceeded: true, Reason: "credential quota", NextRecoverAt: next},
			LastError:        &coreauth.Error{HTTPStatus: http.StatusTooManyRequests, Message: "credential quota"},
			StateEffectScope: cliproxyexecutor.StateEffectScopeCredential,
			ModelStates: map[string]*coreauth.ModelState{
				"gpt-5.6-sol": {
					Status:         coreauth.StatusError,
					StatusMessage:  "usage_limit_reached",
					Unavailable:    true,
					NextRetryAfter: next,
					LastError: &coreauth.Error{
						HTTPStatus: http.StatusTooManyRequests, Message: "usage_limit_reached",
					},
					Quota: coreauth.QuotaState{
						Exceeded: true, Reason: "quota", Family: "codex-sol", NextRecoverAt: next,
					},
					EffectScope: cliproxyexecutor.StateEffectScopeQuotaFamily,
					EffectKey:   "codex-sol",
					QuotaEpoch:  2,
				},
			},
		},
	)
	if errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(registered.ID, "codex", []*registry.ModelInfo{{ID: "gpt-5.6-sol"}})
	reg.SetModelQuotaExceeded(registered.ID, "gpt-5.6-sol")
	reg.SuspendClientModel(registered.ID, "gpt-5.6-sol", "usage_limit_reached")
	t.Cleanup(func() { reg.UnregisterClient(registered.ID) })
	scheduler := quota.NewScheduler(
		quota.DefaultConfig(), func(*quota.Entry) ([]byte, error) {
			return []byte(`{
			"rate_limit":{
				"allowed":true,
				"limit_reached":false,
				"primary_window":{"used_percent":0}
			},
			"additional_rate_limits":[{
				"rate_limit":{
					"allowed":true,
					"limit_reached":false,
					"primary_window":{"used_percent":0},
					"secondary_window":{"used_percent":0}
				}
			}]
		}`), nil
		},
	)
	scheduler.Register("codex.json", quota.TypeCodex, registered.Index)
	NewHandler(&config.Config{}, "", manager, withQuotaScheduler(scheduler))

	scheduler.RefreshNow([]string{"codex.json"})
	current, ok := manager.GetByID(registered.ID)
	if !ok {
		t.Fatal("restored auth missing")
	}
	state := current.ModelStates["gpt-5.6-sol"]
	if state == nil || state.Quota.Exceeded || state.Unavailable || state.LastError != nil ||
		state.QuotaEpoch != 3 || state.EffectScope != "" || state.Quota.Generation != 2 {
		t.Fatalf("model quota state after refresh recovery = %#v", state)
	}
	if current.Status != coreauth.StatusActive || current.Unavailable || current.LastError != nil {
		t.Fatalf("aggregate auth state after refresh recovery = %#v", current)
	}
	available := false
	for _, model := range reg.GetAvailableModelsByProvider("codex") {
		if model != nil && model.ID == "gpt-5.6-sol" {
			available = true
		}
	}
	if !available {
		t.Fatal("family recovery did not resume the model registry entry")
	}
	if current.Generation == registered.Generation {
		t.Fatalf("auth generation did not advance: %d", current.Generation)
	}
	entry := scheduler.GetStatus().Credentials[quota.EntryKey(quota.TypeCodex, "codex.json")]
	if entry == nil || entry.Status != quota.StatusSuccess || entry.Error != "" {
		t.Fatalf("quota entry after recovery = %#v", entry)
	}
	if calls := resetCreditCalls.Load(); calls != 0 {
		t.Fatalf("ordinary quota refresh consumed reset credit %d times", calls)
	}
}

func TestCodexQuotaRefreshRestoresLegacyAuthOnly429Cooldown(t *testing.T) {
	var resetCreditCalls atomic.Int32
	resetCreditServer := httptest.NewServer(
		http.HandlerFunc(
			func(http.ResponseWriter, *http.Request) {
				resetCreditCalls.Add(1)
			},
		),
	)
	t.Cleanup(resetCreditServer.Close)
	previousResetCreditURL := codexRateLimitResetCreditsConsumeURL
	codexRateLimitResetCreditsConsumeURL = resetCreditServer.URL
	t.Cleanup(func() { codexRateLimitResetCreditsConsumeURL = previousResetCreditURL })

	manager := coreauth.NewManager(nil, nil, nil)
	next := time.Now().Add(time.Hour)
	registered, errRegister := manager.Register(
		context.Background(), &coreauth.Auth{
			ID:             "codex-legacy-429",
			Provider:       "codex",
			Status:         coreauth.StatusError,
			StatusMessage:  "rate limited",
			Unavailable:    true,
			NextRetryAfter: next,
			LastError:      &coreauth.Error{HTTPStatus: http.StatusTooManyRequests, Message: "rate limited"},
		},
	)
	if errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	scheduler := quota.NewScheduler(
		quota.DefaultConfig(), func(*quota.Entry) ([]byte, error) {
			return []byte(`{"rate_limit":{"allowed":true,"limit_reached":false}}`), nil
		},
	)
	scheduler.Register("codex.json", quota.TypeCodex, registered.Index)
	NewHandler(&config.Config{}, "", manager, withQuotaScheduler(scheduler))

	scheduler.RefreshNow([]string{"codex.json"})
	current, ok := manager.GetByID(registered.ID)
	if !ok {
		t.Fatal("restored auth missing")
	}
	if current.Status != coreauth.StatusActive || current.Unavailable || current.LastError != nil ||
		current.Generation == registered.Generation {
		t.Fatalf("restored legacy auth state = %#v", current)
	}
	if calls := resetCreditCalls.Load(); calls != 0 {
		t.Fatalf("ordinary quota refresh consumed reset credit %d times", calls)
	}
}

func TestCodexQuotaRefreshPreservesLegacyModel429Cooldown(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	next := time.Now().Add(time.Hour)
	registered, errRegister := manager.Register(
		context.Background(), &coreauth.Auth{
			ID:             "codex-legacy-model-429",
			Provider:       "codex",
			Status:         coreauth.StatusError,
			StatusMessage:  "rate limited",
			Unavailable:    true,
			NextRetryAfter: next,
			LastError:      &coreauth.Error{HTTPStatus: http.StatusTooManyRequests, Message: "rate limited"},
			ModelStates: map[string]*coreauth.ModelState{
				"gpt-5": {
					Status:         coreauth.StatusError,
					StatusMessage:  "rate limited",
					Unavailable:    true,
					NextRetryAfter: next,
					LastError:      &coreauth.Error{HTTPStatus: http.StatusTooManyRequests, Message: "rate limited"},
					QuotaEpoch:     2,
				},
			},
		},
	)
	if errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	h := &Handler{authManager: manager}
	if err := h.finalizeQuotaRefresh(
		quota.RefreshAttempt{
			Type: quota.TypeCodex, Data: []byte(`{"rate_limit":{"allowed":true}}`), AuthIndex: registered.Index,
		},
	); err != nil {
		t.Fatalf("finalizeQuotaRefresh() error = %v", err)
	}
	current, ok := manager.GetByID(registered.ID)
	if !ok {
		t.Fatal("auth missing after quota refresh")
	}
	state := current.ModelStates["gpt-5"]
	if state == nil || !state.Unavailable || state.LastError == nil || state.QuotaEpoch != 2 {
		t.Fatalf("legacy model cooldown changed = %#v", state)
	}
	if current.Generation != registered.Generation || !current.Unavailable || current.LastError == nil {
		t.Fatalf("legacy model aggregate changed = %#v", current)
	}
}

func TestCodexQuotaRefreshRestoresLegacyModel429Cooldown(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	startedAt := time.Now().UTC()
	next := startedAt.Add(time.Hour)
	registered, errRegister := manager.Register(
		context.Background(), &coreauth.Auth{
			ID:          "codex-legacy-model-recovery",
			Provider:    "codex",
			Status:      coreauth.StatusError,
			Unavailable: true,
			LastError:   &coreauth.Error{HTTPStatus: http.StatusTooManyRequests, Message: "rate limited"},
			ModelStates: map[string]*coreauth.ModelState{
				"gpt-5": {
					Status:         coreauth.StatusError,
					StatusMessage:  "rate limited",
					Unavailable:    true,
					NextRetryAfter: next,
					LastError:      &coreauth.Error{HTTPStatus: http.StatusTooManyRequests, Message: "rate limited"},
					UpdatedAt:      startedAt.Add(-time.Minute),
					QuotaEpoch:     2,
				},
			},
		},
	)
	if errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	h := &Handler{authManager: manager}
	if err := h.finalizeQuotaRefresh(
		quota.RefreshAttempt{
			Type:      quota.TypeCodex,
			Data:      []byte(`{"rate_limit":{"allowed":true}}`),
			AuthIndex: registered.Index,
			StartedAt: startedAt,
		},
	); err != nil {
		t.Fatalf("finalizeQuotaRefresh() error = %v", err)
	}
	current, ok := manager.GetByID(registered.ID)
	if !ok {
		t.Fatal("auth missing after quota refresh")
	}
	state := current.ModelStates["gpt-5"]
	if state == nil || state.Unavailable || state.LastError != nil || state.QuotaEpoch != 3 ||
		current.Unavailable || current.LastError != nil || current.Generation == registered.Generation {
		t.Fatalf("legacy model cooldown was not recovered = %#v", current)
	}
}

func TestCodexQuotaRefreshFamilyRecoveryConditions(t *testing.T) {
	startedAt := time.Now().UTC()
	tests := []struct {
		name      string
		data      string
		mutate    func(*coreauth.ModelState)
		disabled  bool
		wantClear bool
	}{
		{
			name:      "root authority without matching meter",
			data:      `{"rate_limit":{"allowed":true}}`,
			wantClear: true,
		},
		{
			name: "matching meter available",
			data: `{"rate_limit":{"allowed":true},"additional_rate_limits":[{
				"limit_name":"gpt-5.6-sol","rate_limit":{"allowed":true}
			}]}`,
			wantClear: true,
		},
		{
			name: "matching meter exhausted",
			data: `{"rate_limit":{"allowed":true},"additional_rate_limits":[{
				"limit_name":"gpt-5.6-sol","rate_limit":{"allowed":false}
			}]}`,
		},
		{
			name: "matching meter unknown",
			data: `{"rate_limit":{"allowed":true},"additional_rate_limits":[{
				"metered_feature":"sol-family","rate_limit":{}
			}]}`,
		},
		{
			name: "unrelated exhausted meter",
			data: `{"rate_limit":{"allowed":true},"additional_rate_limits":[{
				"limit_name":"other-model","rate_limit":{"allowed":false}
			}]}`,
			wantClear: true,
		},
		{
			name: "exact model state",
			data: `{"rate_limit":{"allowed":true}}`,
			mutate: func(state *coreauth.ModelState) {
				state.EffectScope = cliproxyexecutor.StateEffectScopeExactModel
				state.EffectKey = "gpt-5.6-sol"
			},
		},
		{
			name: "newer family state",
			data: `{"rate_limit":{"allowed":true}}`,
			mutate: func(state *coreauth.ModelState) {
				state.UpdatedAt = startedAt.Add(time.Second)
			},
		},
		{
			name: "operator reset authority",
			data: `{"rate_limit":{"allowed":true}}`,
			mutate: func(state *coreauth.ModelState) {
				state.ResetAuthority = coreauth.StateResetAuthorityOperator
			},
		},
		{
			name:     "disabled credential",
			data:     `{"rate_limit":{"allowed":true}}`,
			disabled: true,
		},
	}
	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				manager := coreauth.NewManager(nil, nil, nil)
				next := startedAt.Add(time.Hour)
				state := &coreauth.ModelState{
					Status:         coreauth.StatusError,
					StatusMessage:  "quota exhausted",
					Unavailable:    true,
					NextRetryAfter: next,
					LastError: &coreauth.Error{
						HTTPStatus: http.StatusTooManyRequests,
						Message:    "quota exhausted",
					},
					Quota: coreauth.QuotaState{
						Exceeded:   true,
						Reason:     "quota",
						Family:     "sol-family",
						Generation: 5,
					},
					EffectScope: cliproxyexecutor.StateEffectScopeQuotaFamily,
					EffectKey:   "sol-family",
					Generation:  5,
					UpdatedAt:   startedAt.Add(-time.Minute),
					QuotaEpoch:  4,
				}
				if test.mutate != nil {
					test.mutate(state)
				}
				auth := &coreauth.Auth{
					ID:          "family-condition-" + test.name,
					Provider:    "codex",
					Status:      coreauth.StatusError,
					Unavailable: true,
					LastError: &coreauth.Error{
						HTTPStatus: http.StatusTooManyRequests,
						Message:    "quota exhausted",
					},
					StateEffectScope: cliproxyexecutor.StateEffectScopeQuotaFamily,
					ModelStates:      map[string]*coreauth.ModelState{"gpt-5.6-sol": state},
				}
				if test.disabled {
					auth.Disabled = true
					auth.Status = coreauth.StatusDisabled
				}
				registered, errRegister := manager.Register(context.Background(), auth)
				if errRegister != nil {
					t.Fatalf("Register() error = %v", errRegister)
				}
				h := &Handler{authManager: manager}
				if err := h.finalizeQuotaRefresh(
					quota.RefreshAttempt{
						Type:      quota.TypeCodex,
						Data:      []byte(test.data),
						AuthIndex: registered.Index,
						StartedAt: startedAt,
					},
				); err != nil {
					t.Fatalf("finalizeQuotaRefresh() error = %v", err)
				}
				current, _ := manager.GetByID(registered.ID)
				currentState := current.ModelStates["gpt-5.6-sol"]
				cleared := currentState != nil && !currentState.Quota.Exceeded &&
					!currentState.Unavailable && currentState.LastError == nil
				if cleared != test.wantClear {
					t.Fatalf("family cleared = %v, want %v; state = %#v", cleared, test.wantClear, currentState)
				}
				wantEpoch := uint64(4)
				if test.wantClear {
					wantEpoch = 5
				}
				if currentState == nil || currentState.QuotaEpoch != wantEpoch {
					t.Fatalf("quota epoch = %#v, want %d", currentState, wantEpoch)
				}
				if test.disabled && (!current.Disabled || current.Status != coreauth.StatusDisabled) {
					t.Fatalf("disabled credential was re-enabled = %#v", current)
				}
			},
		)
	}
}

func TestCodexQuotaRefreshKeepsExhaustedRuntimeState(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	next := time.Now().Add(time.Hour)
	registered, errRegister := manager.Register(
		context.Background(), &coreauth.Auth{
			ID:          "codex-exhausted",
			Provider:    "codex",
			Status:      coreauth.StatusError,
			Unavailable: true,
			Quota:       coreauth.QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: next},
			ModelStates: map[string]*coreauth.ModelState{
				"gpt-5": {
					Status:         coreauth.StatusError,
					Unavailable:    true,
					NextRetryAfter: next,
					Quota:          coreauth.QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: next},
					QuotaEpoch:     2,
				},
			},
		},
	)
	if errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	scheduler := quota.NewScheduler(
		quota.DefaultConfig(), func(*quota.Entry) ([]byte, error) {
			return []byte(`{"rate_limit":{"allowed":false,"limit_reached":true}}`), nil
		},
	)
	scheduler.Register("codex.json", quota.TypeCodex, registered.Index)
	NewHandler(&config.Config{}, "", manager, withQuotaScheduler(scheduler))

	scheduler.RefreshNow([]string{"codex.json"})
	current, ok := manager.GetByID(registered.ID)
	if !ok {
		t.Fatal("exhausted auth missing")
	}
	state := current.ModelStates["gpt-5"]
	if state == nil || !state.Quota.Exceeded || !state.Unavailable || state.QuotaEpoch != 2 {
		t.Fatalf("exhausted model state changed: %#v", state)
	}
	if current.Generation != registered.Generation {
		t.Fatalf("exhausted auth generation changed: %d", current.Generation)
	}
}

func TestQuotaRefreshRecoveryConditions(t *testing.T) {
	tests := []struct {
		name                string
		entryType           quota.CredentialType
		provider            string
		data                string
		credentialQuota     bool
		modelQuota          bool
		effectScope         cliproxyexecutor.StateEffectScope
		wantCredentialClear bool
	}{
		{
			name: "allowed explicit credential quota", entryType: quota.TypeCodex, provider: "codex",
			data: `{"rate_limit":{"allowed":true}}`, credentialQuota: true,
			effectScope: cliproxyexecutor.StateEffectScopeCredential, wantCredentialClear: true,
		},
		{
			name: "available root with exhausted additional meter", entryType: quota.TypeCodex, provider: "codex",
			data:            `{"rate_limit":{"allowed":true},"additional_rate_limits":[{"rate_limit":{"allowed":false}}]}`,
			credentialQuota: true, effectScope: cliproxyexecutor.StateEffectScopeCredential, wantCredentialClear: true,
		},
		{
			name: "allowed legacy auth-only quota", entryType: quota.TypeCodex, provider: "codex",
			data: `{"rate_limit":{"allowed":true}}`, credentialQuota: true, wantCredentialClear: true,
		},
		{
			name: "allowed model quota", entryType: quota.TypeCodex, provider: "codex",
			data: `{"rate_limit":{"allowed":true}}`, credentialQuota: true, modelQuota: true,
			effectScope: cliproxyexecutor.StateEffectScopeQuotaFamily,
		},
		{
			name: "disallowed credential quota", entryType: quota.TypeCodex, provider: "codex",
			data: `{"rate_limit":{"allowed":false}}`, credentialQuota: true,
			effectScope: cliproxyexecutor.StateEffectScopeCredential,
		},
		{
			name: "unknown root with available additional meter", entryType: quota.TypeCodex, provider: "codex",
			data:            `{"rate_limit":{},"additional_rate_limits":[{"rate_limit":{"allowed":true}}]}`,
			credentialQuota: true, effectScope: cliproxyexecutor.StateEffectScopeCredential,
		},
		{
			name: "non Codex entry", entryType: quota.TypeClaude, provider: "claude",
			data: `{"rate_limit":{"allowed":true}}`, credentialQuota: true,
			effectScope: cliproxyexecutor.StateEffectScopeCredential,
		},
		{
			name: "Codex auth without quota state", entryType: quota.TypeCodex, provider: "codex",
			data: `{"rate_limit":{"allowed":true}}`,
		},
	}
	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				manager := coreauth.NewManager(nil, nil, nil)
				next := time.Now().Add(time.Hour)
				auth := &coreauth.Auth{
					ID:               "quota-condition-auth",
					Provider:         tt.provider,
					Status:           coreauth.StatusActive,
					StateEffectScope: tt.effectScope,
					ModelStates: map[string]*coreauth.ModelState{
						"model": {Status: coreauth.StatusActive, QuotaEpoch: 4},
					},
				}
				if tt.credentialQuota {
					auth.Status = coreauth.StatusError
					auth.Unavailable = true
					auth.LastError = &coreauth.Error{HTTPStatus: http.StatusTooManyRequests, Message: "quota"}
					auth.Quota = coreauth.QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: next}
				}
				if tt.modelQuota {
					auth.ModelStates["model"] = &coreauth.ModelState{
						Status: coreauth.StatusError, Unavailable: true, NextRetryAfter: next,
						LastError:   &coreauth.Error{HTTPStatus: http.StatusTooManyRequests, Message: "model quota"},
						Quota:       coreauth.QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: next},
						EffectScope: cliproxyexecutor.StateEffectScopeQuotaFamily,
						EffectKey:   "family",
						QuotaEpoch:  4,
					}
				}
				registered, errRegister := manager.Register(context.Background(), auth)
				if errRegister != nil {
					t.Fatalf("Register() error = %v", errRegister)
				}
				h := &Handler{authManager: manager}

				err := h.finalizeQuotaRefresh(
					quota.RefreshAttempt{
						Type: tt.entryType, Data: []byte(tt.data), AuthIndex: registered.Index,
					},
				)
				if err != nil {
					t.Fatalf("finalizeQuotaRefresh() error = %v", err)
				}
				current, ok := manager.GetByID(registered.ID)
				if !ok {
					t.Fatal("auth missing after quota finalizer")
				}
				cleared := current.Generation != registered.Generation
				if cleared != tt.wantCredentialClear {
					t.Fatalf("credential cleared = %v, want %v; auth = %#v", cleared, tt.wantCredentialClear, current)
				}
				state := current.ModelStates["model"]
				if state == nil || state.QuotaEpoch != 4 {
					t.Fatalf("model state changed = %#v", state)
				}
				if tt.wantCredentialClear {
					if current.Status != coreauth.StatusActive || current.Unavailable || current.LastError != nil ||
						current.Quota.Exceeded {
						t.Fatalf("credential quota was not cleared = %#v", current)
					}
				} else if tt.credentialQuota && !current.Quota.Exceeded {
					t.Fatalf("credential quota was cleared without authority = %#v", current)
				}
			},
		)
	}
}

func TestQuotaRefreshRecoveryIgnoresProviderMismatch(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	next := time.Now().Add(time.Hour)
	registered, errRegister := manager.Register(
		context.Background(), &coreauth.Auth{
			ID: "mismatched-provider", Provider: "claude", Status: coreauth.StatusError, Unavailable: true,
			Quota: coreauth.QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: next},
		},
	)
	if errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	h := &Handler{authManager: manager}

	err := h.finalizeQuotaRefresh(
		quota.RefreshAttempt{
			Type: quota.TypeCodex, Data: []byte(`{"rate_limit":{"allowed":true}}`), AuthIndex: registered.Index,
		},
	)
	if err != nil {
		t.Fatalf("finalizeQuotaRefresh() error = %v", err)
	}
	current, _ := manager.GetByID(registered.ID)
	if current.Generation != registered.Generation || !current.Quota.Exceeded {
		t.Fatalf("provider mismatch changed auth = %#v", current)
	}
}

func TestAuthFilesAggregateReflectsAutomaticFamilyRecovery(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	next := time.Now().Add(time.Hour)
	registered, errRegister := manager.Register(
		context.Background(), &coreauth.Auth{
			ID: "codex-aggregate", Provider: "codex", FileName: "codex.json", Status: coreauth.StatusError,
			StatusMessage: "credential quota", Unavailable: true,
			Attributes:       map[string]string{"runtime_only": "true"},
			LastError:        &coreauth.Error{HTTPStatus: http.StatusTooManyRequests, Message: "credential quota"},
			Quota:            coreauth.QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: next},
			StateEffectScope: cliproxyexecutor.StateEffectScopeCredential,
			ModelStates: map[string]*coreauth.ModelState{
				"gpt-5.6-sol": {
					Status: coreauth.StatusError, StatusMessage: "usage_limit_reached", Unavailable: true,
					NextRetryAfter: next,
					LastError: &coreauth.Error{
						HTTPStatus: http.StatusTooManyRequests, Message: "usage_limit_reached",
					},
					Quota: coreauth.QuotaState{
						Exceeded: true, Reason: "quota", Family: "codex-sol", NextRecoverAt: next,
					},
					EffectScope: cliproxyexecutor.StateEffectScopeQuotaFamily,
					EffectKey:   "codex-sol",
					QuotaEpoch:  6,
				},
			},
		},
	)
	if errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	scheduler := quota.NewScheduler(
		quota.DefaultConfig(), func(*quota.Entry) ([]byte, error) {
			return []byte(`{"rate_limit":{"allowed":true}}`), nil
		},
	)
	scheduler.Register("codex.json", quota.TypeCodex, registered.Index)
	h := NewHandler(&config.Config{}, "", manager, withQuotaScheduler(scheduler))

	scheduler.RefreshNow(nil)

	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/auth-files", nil)
	h.ListAuthFiles(ginCtx)
	if recorder.Code != http.StatusOK {
		t.Fatalf("ListAuthFiles() status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Files []map[string]any `json:"files"`
	}
	if errDecode := json.Unmarshal(recorder.Body.Bytes(), &response); errDecode != nil {
		t.Fatalf("decode auth files: %v", errDecode)
	}
	if len(response.Files) != 1 {
		t.Fatalf("auth files = %#v, want one", response.Files)
	}
	file := response.Files[0]
	if file["status"] != string(coreauth.StatusActive) || file["unavailable"] != false ||
		file["status_message"] != "" {
		t.Fatalf("aggregate auth file state = %#v", file)
	}
	current, _ := manager.GetByID(registered.ID)
	state := current.ModelStates["gpt-5.6-sol"]
	if state == nil || state.Quota.Exceeded || state.Unavailable || state.LastError != nil ||
		state.QuotaEpoch != 7 {
		t.Fatalf("runtime state = %#v", state)
	}
	if current.Generation == registered.Generation {
		t.Fatalf("credential recovery did not advance auth generation: %#v", current)
	}
}

func TestPostQuotaRefreshRejectsMalformedBody(t *testing.T) {
	scheduler := quota.NewScheduler(
		quota.DefaultConfig(), func(*quota.Entry) ([]byte, error) {
			return []byte(`{}`), nil
		},
	)
	h := NewHandler(&config.Config{}, "", nil, withQuotaScheduler(scheduler))
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(
		http.MethodPost,
		"/v0/management/quota/refresh",
		bytes.NewBufferString(`{`),
	)
	ginCtx.Request.Header.Set("Content-Type", "application/json")

	h.PostQuotaRefresh(ginCtx)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d body=%s", recorder.Code, http.StatusBadRequest, recorder.Body.String())
	}
}

func TestPutQuotaConfigSaveFailureKeepsMemoryConfig(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("quota-refresh: [\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	previousConfig := config.QuotaRefresh{Enabled: false, Interval: 600, MaxInterval: 1800}
	cfg := &config.Config{QuotaRefresh: previousConfig}
	schedulerConfig := quota.Config{
		Enabled: previousConfig.Enabled, Interval: previousConfig.Interval, MaxInterval: previousConfig.MaxInterval,
	}
	scheduler := quota.NewScheduler(schedulerConfig, nil)
	h := NewHandler(cfg, configPath, nil, withQuotaScheduler(scheduler))

	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(
		http.MethodPut,
		"/v0/management/quota/config",
		bytes.NewBufferString(`{"enabled":true,"interval":120,"max-interval":600}`),
	)
	ginCtx.Request.Header.Set("Content-Type", "application/json")

	h.PutQuotaConfig(ginCtx)

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d body=%s", recorder.Code, http.StatusInternalServerError, recorder.Body.String())
	}
	if cfg.QuotaRefresh != previousConfig {
		t.Fatalf("cfg.QuotaRefresh = %+v, want %+v", cfg.QuotaRefresh, previousConfig)
	}
	if got := scheduler.GetConfig(); got != schedulerConfig {
		t.Fatalf("scheduler config = %+v, want %+v", got, schedulerConfig)
	}
}
