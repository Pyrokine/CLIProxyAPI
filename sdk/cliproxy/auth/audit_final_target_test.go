package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executionregistry"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestAuditHomeContext1MRejectsRewrittenFinalTarget(t *testing.T) {
	for _, mode := range []string{"execute", "count", "stream"} {
		t.Run(
			mode, func(t *testing.T) {
				manager := NewManager(nil, nil, nil)
				manager.SetConfig(&config.Config{Home: config.HomeConfig{Enabled: true}})
				capture := &homeCapabilityCaptureExecutor{}
				manager.RegisterExecutor(capture)
				credentialID := t.Name()
				reg := registry.GetGlobalRegistry()
				reg.RegisterClient(
					credentialID, "test", []*registry.ModelInfo{
						{
							ID: "public-model", CatalogName: "high-model", ContextLength: context1MMinimumTokens,
						},
					},
				)
				t.Cleanup(func() { reg.UnregisterClient(credentialID) })
				raw, err := json.Marshal(
					homeAuthDispatchResponse{
						Model: "high-model[1m]", Auth: Auth{
							ID: credentialID, Provider: "test",
							Attributes: map[string]string{"api_key": "synthetic", "auth_kind": "apikey"},
						}, ModelInfo: &homeDispatchModelInfo{ID: "high-model", ContextLength: context1MMinimumTokens},
					},
				)
				if err != nil {
					t.Fatal(err)
				}
				manager.PublishHomeDispatch(
					&testHomeDispatcher{heartbeat: true, payload: raw}, executionregistry.New(), 1,
				)
				opts := executor.Options{
					RequestAfterAuthInterceptor: func(
						_ context.Context,
						input executor.RequestAfterAuthInterceptRequest,
					) executor.RequestAfterAuthInterceptResponse {
						changed := input.Request
						changed.Model = "low-model[1m]"
						return executor.RequestAfterAuthInterceptResponse{
							Request: changed, Options: input.Options, Handled: true,
						}
					},
				}
				req := executor.Request{Model: "public-model[1m]"}
				switch mode {
				case "execute":
					_, err = manager.Execute(t.Context(), []string{"test"}, req, opts)
				case "count":
					_, err = manager.ExecuteCount(t.Context(), []string{"test"}, req, opts)
				case "stream":
					_, err = manager.ExecuteStream(t.Context(), []string{"test"}, req, opts)
				}
				requireContext1MModelSupportError(t, err)
				if len(capture.requests) != 0 {
					t.Fatalf("untrusted final target reached executor: %+v", capture.requests)
				}
			},
		)
	}
}

func TestAuditForceCooldownSetsEffectiveUnavailableState(t *testing.T) {
	previous := transientErrorCooldownSeconds.Load()
	transientErrorCooldownSeconds.Store(-1)
	t.Cleanup(func() { transientErrorCooldownSeconds.Store(previous) })
	now := time.Now()
	forced := &Error{
		Code: ErrorCodeForceCooldown, HTTPStatus: http.StatusBadGateway, Message: "synthetic forced cooldown",
	}
	credential := &Auth{ID: "credential", Provider: "test"}
	applyAuthFailureState(credential, forced, nil, now)
	if !credential.Unavailable || !credential.NextRetryAfter.After(now) {
		t.Fatalf("credential=%+v", credential)
	}
	if blocked, _, _ := isAuthBlockedForModel(credential, "model", now); !blocked {
		t.Fatal("forced credential remained selectable")
	}
	state := &ModelState{}
	applyExactModelFailure(state, "model", Result{AuthID: "model", Model: "model", Error: forced}, now, false)
	modeled := &Auth{ID: "modeled", Provider: "test", ModelStates: map[string]*ModelState{"model": state}}
	if !state.Unavailable || !state.NextRetryAfter.After(now) {
		t.Fatalf("model=%+v", state)
	}
	if blocked, _, _ := isAuthBlockedForModel(modeled, "model", now); !blocked {
		t.Fatal("forced model remained selectable")
	}
}

func TestAuditMissingModelFieldDoesNotPoisonModelAvailability(t *testing.T) {
	for _, message := range []string{
		"field model not found in request body", "requested field model not found", "model not found field in payload",
	} {
		if isExplicitModelNotFoundMessage(message, "synthetic-model") {
			t.Fatalf("field failure classified as missing upstream model: %s", message)
		}
		if containsStructuredModelNotFound(
			map[string]any{"error": map[string]any{"message": message}}, "synthetic-model",
		) {
			t.Fatalf("structured field failure classified as missing model: %s", message)
		}
	}
	if !containsStructuredModelNotFound(
		map[string]any{"error": map[string]any{"code": "model_not_found"}}, "synthetic-model",
	) {
		t.Fatal("explicit missing model code was lost")
	}
}
