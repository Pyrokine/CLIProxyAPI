package handlers

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	internalconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	sdkconfig "github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
)

func TestHTTPAPIKeyPolicyRejectsBeforeOperation(t *testing.T) {
	for _, test := range []struct {
		name  string
		rule  internalconfig.APIKeyRule
		model string
		store *apiKeyPolicyStoreStub
		want  int
	}{
		{
			name: "blocked model", model: "restricted",
			rule: internalconfig.APIKeyRule{BlockedModels: []string{"restricted"}}, want: http.StatusForbidden,
		},
		{
			name: "provider restriction", model: "model",
			rule: internalconfig.APIKeyRule{Providers: []string{"claude"}}, want: http.StatusForbidden,
		},
		{
			name: "missing restricted model", rule: internalconfig.APIKeyRule{CanonicalModels: []string{"allowed"}},
			want: http.StatusForbidden,
		},
		{
			name: "missing store", model: "model",
			rule: internalconfig.APIKeyRule{Limits: []internalconfig.APIKeyPolicyLimit{{Window: "1h", Requests: 1}}},
			want: http.StatusServiceUnavailable,
		},
		{
			name: "failed store", model: "model",
			rule:  internalconfig.APIKeyRule{Limits: []internalconfig.APIKeyPolicyLimit{{Window: "1h", Requests: 1}}},
			store: &apiKeyPolicyStoreStub{reserveErr: errors.New("unavailable")}, want: http.StatusServiceUnavailable,
		},
		{
			name: "exhausted limit", model: "model",
			rule: internalconfig.APIKeyRule{Limits: []internalconfig.APIKeyPolicyLimit{{Window: "1h", Requests: 1}}},
			store: &apiKeyPolicyStoreStub{
				reserveErr: &coreusage.APIKeyLimitExceededError{
					RetryAt: time.Now().Add(time.Minute),
				},
			},
			want: http.StatusTooManyRequests,
		},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				h := NewBaseAPIHandlers(
					&sdkconfig.SDKConfig{
						APIKeyRules: map[string]internalconfig.APIKeyRule{"key": test.rule},
					},
					nil,
				)
				if test.store != nil {
					h.SetAPIKeyPolicyStore(test.store)
				}
				called := false
				errMsg := h.ExecuteHTTPWithAPIKeyPolicy(
					apiKeyPolicyContext("key", t.Name()), "openai", "codex", test.model, test.model,
					func(context.Context, string, func()) *ErrorMessage { called = true; return nil },
				)
				if errMsg == nil || errMsg.StatusCode != test.want || called {
					t.Fatalf(
						"result=%+v operation called=%v, want status %d with no operation",
						errMsg,
						called,
						test.want,
					)
				}
			},
		)
	}
}

func TestHTTPAPIKeyPolicyMappingAndReservationLifecycle(t *testing.T) {
	for _, test := range []struct {
		name       string
		dispatch   bool
		fail       bool
		cancel     bool
		tokens     int64
		cost       int64
		transition string
	}{
		{name: "request only success", dispatch: true, transition: "commit:"},
		{name: "before dispatch failure", fail: true, transition: "release:"},
		{name: "after dispatch failure", dispatch: true, fail: true, transition: "unknown:"},
		{name: "after dispatch canceled", dispatch: true, cancel: true, transition: "unknown:"},
		{name: "unreported tokens", dispatch: true, tokens: 100, transition: "unknown:"},
		{name: "unreported cost", dispatch: true, cost: 100, transition: "unknown:"},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				store := &apiKeyPolicyStoreStub{}
				h := NewBaseAPIHandlers(
					&sdkconfig.SDKConfig{
						APIKeyRules: map[string]internalconfig.APIKeyRule{
							"key": {
								Providers: []string{
									"codex",
								}, ModelMappings: map[string]string{"alias": "actual"},
								CanonicalModels: []string{"actual"}, Limits: []internalconfig.APIKeyPolicyLimit{
									{
										Window: "1h", Requests: 10, Tokens: test.tokens, CostMicro: test.cost,
									},
								},
							},
						},
					}, nil,
				)
				h.SetAPIKeyPolicyStore(store)
				ctx, cancel := context.WithCancel(apiKeyPolicyContext("key", t.Name()))
				defer cancel()
				errMsg := h.ExecuteHTTPWithAPIKeyPolicy(
					ctx, "openai", "codex", "alias", "alias",
					func(ctx context.Context, model string, dispatch func()) *ErrorMessage {
						if model != "actual" || coreusage.APIKeyReservationFromContext(ctx) == "" {
							t.Fatalf(
								"model=%q reservation=%q",
								model,
								coreusage.APIKeyReservationFromContext(ctx),
							)
						}
						if test.dispatch {
							dispatch()
						}
						if test.cancel {
							cancel()
						}
						if test.fail {
							return &ErrorMessage{
								StatusCode: http.StatusBadGateway, Error: errors.New("synthetic failure"),
							}
						}
						return nil
					},
				)
				if (errMsg != nil) != test.fail {
					t.Fatalf("result=%+v", errMsg)
				}
				request, calls, transitions := store.snapshot()
				if calls != 1 || request.CanonicalModel != "actual" || len(transitions) != 1 ||
					!strings.HasPrefix(
						transitions[0], test.transition,
					) {
					t.Fatalf("request=%+v calls=%d transitions=%v", request, calls, transitions)
				}
			},
		)
	}
}

func TestHTTPAPIKeyPolicyAllowsUnrestrictedMissingModel(t *testing.T) {
	h := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil)
	called := false
	errMsg := h.ExecuteHTTPWithAPIKeyPolicy(
		apiKeyPolicyContext("key", t.Name()), "openai", "codex", "", "",
		func(_ context.Context, model string, dispatch func()) *ErrorMessage {
			called = true
			if model != "" {
				t.Fatalf("model=%q", model)
			}
			dispatch()
			return nil
		},
	)
	if errMsg != nil || !called {
		t.Fatalf("result=%+v called=%v", errMsg, called)
	}
}
