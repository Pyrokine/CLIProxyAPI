package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestAttachResolvedAPIKeyModelInfoUsesSelectedCredential(t *testing.T) {
	const (
		highAuthID = "summary-model-info-high"
		maxAuthID  = "summary-model-info-max"
	)
	modelRegistry := registry.GetGlobalRegistry()
	modelRegistry.RegisterClient(
		highAuthID, "claude", []*registry.ModelInfo{
			{
				ID:          "shared-model",
				CatalogName: "claude-opus-5",
				Thinking:    &registry.ThinkingSupport{Levels: []string{"high"}},
				IsCompat:    true,
			},
		},
	)
	modelRegistry.RegisterClient(
		maxAuthID, "claude", []*registry.ModelInfo{
			{
				ID:          "tenant/shared-model",
				CatalogName: "claude-sonnet-5",
				Thinking:    &registry.ThinkingSupport{Levels: []string{"max"}},
			},
		},
	)
	defer modelRegistry.UnregisterClient(highAuthID)
	defer modelRegistry.UnregisterClient(maxAuthID)

	highAuth := &Auth{
		ID:         highAuthID,
		Provider:   "claude",
		Prefix:     "tenant",
		Attributes: map[string]string{"api_key": "high-key"},
	}
	maxAuth := &Auth{
		ID:         maxAuthID,
		Provider:   "claude",
		Prefix:     "tenant",
		Attributes: map[string]string{"api_key": "max-key"},
	}

	highReq, err := attachResolvedAPIKeyModelInfo(
		cliproxyexecutor.Request{}, highAuth, "tenant/shared-model(high)", "claude-opus-5(high)",
	)
	if err != nil {
		t.Fatalf("attach high model info: %v", err)
	}
	maxReq, err := attachResolvedAPIKeyModelInfo(
		cliproxyexecutor.Request{}, maxAuth, "tenant/shared-model(max)", "claude-sonnet-5(max)",
	)
	if err != nil {
		t.Fatalf("attach max model info: %v", err)
	}

	assertResolvedThinkingLevels(t, highReq, "high")
	assertResolvedThinkingLevels(t, maxReq, "max")
	highInfo, highOK := ResolvedAPIKeyModelInfo(highReq)
	if !highOK || highInfo == nil || !highInfo.IsCompat {
		t.Fatalf("high credential model info = (%+v, %t), want IsCompat=true", highInfo, highOK)
	}
	maxInfo, maxOK := ResolvedAPIKeyModelInfo(maxReq)
	if !maxOK || maxInfo == nil || maxInfo.IsCompat {
		t.Fatalf("max credential model info = (%+v, %t), want IsCompat=false", maxInfo, maxOK)
	}
}

func TestAttachResolvedAPIKeyModelInfoRejectsDifferentUpstream(t *testing.T) {
	const authID = "summary-model-info-mismatch"
	modelRegistry := registry.GetGlobalRegistry()
	modelRegistry.RegisterClient(
		authID, "claude", []*registry.ModelInfo{
			{
				ID:          "public-model",
				CatalogName: "configured-upstream",
				Thinking:    &registry.ThinkingSupport{Levels: []string{"high"}},
			},
		},
	)
	defer modelRegistry.UnregisterClient(authID)

	auth := &Auth{
		ID:         authID,
		Provider:   "claude",
		Attributes: map[string]string{"api_key": "key"},
	}
	req, err := attachResolvedAPIKeyModelInfo(
		cliproxyexecutor.Request{}, auth, "public-model", "different-upstream",
	)
	if err != nil {
		t.Fatalf("attach model info: %v", err)
	}
	if modelInfo, ok := ResolvedAPIKeyModelInfo(req); ok || modelInfo != nil {
		t.Fatalf("ResolvedAPIKeyModelInfo() = (%+v, %t), want unresolved", modelInfo, ok)
	}
}

func TestPostAuthModelRewriteDoesNotBindPreviousModelCapabilities(t *testing.T) {
	const originalModel = "configured-upstream"
	for _, mode := range []string{"execute", "count", "stream"} {
		t.Run(mode, func(t *testing.T) {
			authID := "model-rewrite-" + mode
			models := registry.GetGlobalRegistry()
			models.RegisterClient(authID, "test", []*registry.ModelInfo{{
				ID: originalModel, Thinking: &registry.ThinkingSupport{Levels: []string{"high"}},
			}})
			t.Cleanup(func() { models.UnregisterClient(authID) })
			manager := NewManager(nil, nil, nil)
			executor := &homeCapabilityCaptureExecutor{}
			manager.RegisterExecutor(executor)
			if _, err := manager.Register(context.Background(), &Auth{
				ID: authID, Provider: "test", Status: StatusActive,
				Attributes: map[string]string{"api_key": "fixture-key"},
			}); err != nil {
				t.Fatal(err)
			}
			manager.RefreshSchedulerEntry(authID)
			execute := func(opts cliproxyexecutor.Options) error {
				request := cliproxyexecutor.Request{Model: originalModel, Payload: []byte(`{}`)}
				switch mode {
				case "execute":
					_, err := manager.Execute(context.Background(), []string{"test"}, request, opts)
					return err
				case "count":
					_, err := manager.ExecuteCount(context.Background(), []string{"test"}, request, opts)
					return err
				default:
					stream, err := manager.ExecuteStream(context.Background(), []string{"test"}, request, opts)
					if err != nil {
						return err
					}
					for chunk := range stream.Chunks {
						if chunk.Err != nil {
							return chunk.Err
						}
					}
					return nil
				}
			}
			if err := execute(cliproxyexecutor.Options{}); err != nil || len(executor.requests) != 1 {
				t.Fatalf("unmodified execution: requests=%d, error=%v", len(executor.requests), err)
			}
			if info, ok := ResolvedAPIKeyModelInfo(executor.requests[0]); !ok || info.ID != originalModel {
				t.Fatalf("original model capability = (%+v, %t)", info, ok)
			}
			executor.requests = nil
			opts := cliproxyexecutor.Options{
				RequestAfterAuthInterceptor: func(_ context.Context, in cliproxyexecutor.RequestAfterAuthInterceptRequest) cliproxyexecutor.RequestAfterAuthInterceptResponse {
					in.Request.Model = "different-upstream"
					return cliproxyexecutor.RequestAfterAuthInterceptResponse{Handled: true, Request: in.Request, Options: in.Options}
				},
			}
			if err := execute(opts); err != nil || len(executor.requests) != 1 {
				t.Fatalf("rewritten execution: requests=%d, error=%v", len(executor.requests), err)
			}
			if req := executor.requests[0]; req.Model != "different-upstream" {
				t.Fatalf("intercepted model = %q", req.Model)
			} else if info, ok := ResolvedAPIKeyModelInfo(req); ok || info != nil {
				t.Fatalf("previous model capability leaked to different upstream: %+v", info)
			}
		})
	}
}

func TestAttachResolvedAPIKeyModelInfoPropagatesIsCompat(t *testing.T) {
	const authID = "compat-model-info"
	modelRegistry := registry.GetGlobalRegistry()
	modelRegistry.RegisterClient(
		authID, "claude", []*registry.ModelInfo{
			{
				ID:          "tenant/deepseek-alias",
				CatalogName: "deepseek-upstream",
				IsCompat:    true,
				UserDefined: true,
			},
		},
	)
	defer modelRegistry.UnregisterClient(authID)

	auth := &Auth{
		ID:       authID,
		Provider: "claude",
		Prefix:   "tenant",
		Attributes: map[string]string{
			"api_key": "compat-key",
		},
	}
	req, err := attachResolvedAPIKeyModelInfo(
		cliproxyexecutor.Request{},
		auth,
		"tenant/deepseek-alias",
		"deepseek-upstream",
	)
	if err != nil {
		t.Fatalf("attach model info: %v", err)
	}
	modelInfo, ok := ResolvedAPIKeyModelInfo(req)
	if !ok || modelInfo == nil || !modelInfo.IsCompat {
		t.Fatalf("ResolvedAPIKeyModelInfo() = (%+v, %t), want IsCompat=true", modelInfo, ok)
	}
	if modelInfo.UserDefined {
		t.Fatalf("resolved configured model remained user-defined: %+v", modelInfo)
	}
	if modelInfo.Thinking != nil {
		t.Fatalf("unknown compatible model inferred thinking metadata: %+v", modelInfo.Thinking)
	}
}

func TestAttachResolvedAPIKeyModelInfoLeavesUnknownConfiguredModelUnresolved(t *testing.T) {
	const authID = "summary-model-info-unknown"
	modelRegistry := registry.GetGlobalRegistry()
	modelRegistry.RegisterClient(
		authID, "claude", []*registry.ModelInfo{
			{
				ID:          "custom-model",
				CatalogName: "custom-upstream-model",
				UserDefined: true,
			},
		},
	)
	defer modelRegistry.UnregisterClient(authID)

	auth := &Auth{
		ID:         authID,
		Provider:   "claude",
		Attributes: map[string]string{"api_key": "key"},
	}
	req, err := attachResolvedAPIKeyModelInfo(
		cliproxyexecutor.Request{}, auth, "custom-model", "custom-upstream-model",
	)
	if err != nil {
		t.Fatalf("attach model info: %v", err)
	}
	if modelInfo, ok := ResolvedAPIKeyModelInfo(req); ok || modelInfo != nil {
		t.Fatalf("ResolvedAPIKeyModelInfo() = (%+v, %t), want unresolved user-defined model", modelInfo, ok)
	}
}

func TestAttachResolvedAPIKeyModelInfoPreservesExplicitNativeFalse(t *testing.T) {
	const authID = "native-capability-false"
	webSearch := false
	modelRegistry := registry.GetGlobalRegistry()
	modelRegistry.RegisterClient(authID, "claude", []*registry.ModelInfo{{
		ID:          "native-alias",
		CatalogName: "native-upstream",
		NativeCapabilities: &registry.NativeCapabilities{
			WebSearch: &webSearch,
		},
		UserDefined: true,
	}})
	t.Cleanup(func() { modelRegistry.UnregisterClient(authID) })

	webSearch = true
	req, err := attachResolvedAPIKeyModelInfo(
		cliproxyexecutor.Request{},
		&Auth{ID: authID, Provider: "claude", Attributes: map[string]string{"api_key": "key"}},
		"native-alias",
		"native-upstream",
	)
	if err != nil {
		t.Fatalf("attach model info: %v", err)
	}
	modelInfo, ok := ResolvedAPIKeyModelInfo(req)
	if !ok || modelInfo == nil || modelInfo.NativeCapabilities == nil ||
		modelInfo.NativeCapabilities.WebSearch == nil || *modelInfo.NativeCapabilities.WebSearch {
		t.Fatalf("resolved native capability = (%+v, %t), want explicit false", modelInfo, ok)
	}

	*modelInfo.NativeCapabilities.WebSearch = true
	registered := modelRegistry.GetModelInfoForClient(authID, "native-alias")
	if registered == nil || registered.NativeCapabilities == nil || registered.NativeCapabilities.WebSearch == nil ||
		*registered.NativeCapabilities.WebSearch {
		t.Fatalf("resolved capability mutation leaked into registry: %+v", registered)
	}
}

func TestAttachResolvedAPIKeyModelInfoKeepsCodexConfigurationUpdatePerCredential(t *testing.T) {
	const model = "codex-capability-upstream"
	registryClient := registry.GetGlobalRegistry()
	for _, tc := range []struct {
		id      string
		support bool
	}{
		{id: "codex-support-update", support: true},
		{id: "codex-no-update", support: false},
	} {
		registryClient.RegisterClient(tc.id, "codex", []*registry.ModelInfo{{
			ID:                         "tenant/public-model",
			CatalogName:                model,
			SupportConfigurationUpdate: tc.support,
			Thinking:                   &registry.ThinkingSupport{Levels: []string{"high"}},
			UserDefined:                true,
		}})
		t.Cleanup(func() { registryClient.UnregisterClient(tc.id) })
		auth := &Auth{ID: tc.id, Provider: "codex", Prefix: "tenant", Attributes: map[string]string{"api_key": "test-key"}}
		req, err := attachResolvedAPIKeyModelInfo(cliproxyexecutor.Request{}, auth, "tenant/public-model", model)
		if err != nil {
			t.Fatalf("attach %s model: %v", tc.id, err)
		}
		info, ok := ResolvedAPIKeyModelInfo(req)
		if !ok || info == nil || info.SupportConfigurationUpdate != tc.support || info.ID != model {
			t.Fatalf("%s model capability = (%+v, %v), want support=%v", tc.id, info, ok, tc.support)
		}
		mismatched, err := attachResolvedAPIKeyModelInfo(cliproxyexecutor.Request{}, auth, "tenant/public-model", "other-upstream")
		if err != nil {
			t.Fatalf("attach mismatched %s model: %v", tc.id, err)
		}
		if info, ok := ResolvedAPIKeyModelInfo(mismatched); ok || info != nil {
			t.Fatalf("%s capability leaked to a different upstream: %+v", tc.id, info)
		}
	}
}

func TestCodexOAuthModelInfoUsesSelectedPlanAndUpstream(t *testing.T) {
	for _, tc := range []struct {
		name, plan, upstream string
		wantBound            bool
	}{
		{name: "free", plan: "free", upstream: "gpt-6-luna", wantBound: true},
		{name: "free excludes pro model", plan: "free", upstream: "gpt-6-astra"},
		{name: "pro", plan: "pro", upstream: "gpt-6-astra(high)", wantBound: true},
		{name: "default is free", upstream: "gpt-6-astra"},
		{name: "unknown upstream", plan: "pro", upstream: "unlisted-model"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := cliproxyexecutor.Request{Metadata: map[string]any{"request": "kept"}}
			auth := &Auth{
				Provider:   "codex",
				Attributes: map[string]string{"auth_kind": "oauth", "plan_type": tc.plan},
			}
			req, err := attachResolvedAPIKeyModelInfo(original, auth, "tenant/public-alias", tc.upstream)
			if err != nil {
				t.Fatal(err)
			}
			info, ok := ResolvedModelInfo(req)
			if ok != tc.wantBound || (ok && (info == nil || info.ID != strings.TrimSuffix(tc.upstream, "(high)") || !info.SupportConfigurationUpdate)) {
				t.Fatalf("selected model info = (%+v, %t), want bound=%t", info, ok, tc.wantBound)
			}
			if keyInfo, keyOK := ResolvedAPIKeyModelInfo(req); keyOK || keyInfo != nil {
				t.Fatalf("OAuth obtained API-key model info: %+v", keyInfo)
			}
			if req.Metadata["request"] != "kept" {
				t.Fatal("unrelated request metadata lost")
			}
			if _, leaked := original.Metadata[resolvedCodexOAuthModelInfoMetadataKey]; leaked {
				t.Fatal("selected capability mutated original request metadata")
			}
		})
	}
}

func TestCodexOAuthModelInfoRejectsUnsupportedContext1M(t *testing.T) {
	const authID = "codex-oauth-context-1m"
	models := registry.GetGlobalRegistry()
	models.RegisterClient(authID, "codex", []*registry.ModelInfo{
		{ID: "gpt-6-luna", ContextLength: context1MMinimumTokens},
		{ID: "gpt-6-astra", ContextLength: context1MMinimumTokens},
	})
	t.Cleanup(func() { models.UnregisterClient(authID) })

	for _, tc := range []struct {
		name, plan, route, upstream string
	}{
		{name: "free model", plan: "free", route: "gpt-6-luna[1m]", upstream: "gpt-6-luna[1m]"},
		{name: "pro model", plan: "pro", route: "gpt-6-astra[1m]", upstream: "gpt-6-astra[1m]"},
		{name: "intercepted model", plan: "free", route: "gpt-6-luna[1m]", upstream: "unlisted-model[1m]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := attachResolvedAPIKeyModelInfo(
				cliproxyexecutor.Request{},
				&Auth{ID: authID, Provider: "codex", Attributes: map[string]string{"auth_kind": "oauth", "plan_type": tc.plan}},
				tc.route, tc.upstream,
			)
			requireContext1MModelSupportError(t, err)
		})
	}
}

func TestCodexOAuthModelInfoFollowsExecutionModel(t *testing.T) {
	for _, mode := range []string{"execute", "count", "stream"} {
		t.Run(mode, func(t *testing.T) {
			authID := "codex-oauth-selected-" + mode
			models := registry.GetGlobalRegistry()
			models.RegisterClient(authID, "codex", registry.GetCodexFreeModels())
			t.Cleanup(func() { models.UnregisterClient(authID) })
			manager := NewManager(nil, nil, nil)
			executor := &homeCapabilityCaptureExecutor{roundExecutor: roundExecutor{provider: "codex"}}
			manager.RegisterExecutor(executor)
			if _, err := manager.Register(context.Background(), &Auth{
				ID: authID, Provider: "codex", Status: StatusActive,
				Attributes: map[string]string{"auth_kind": "oauth", "plan_type": "free"},
				Metadata:   map[string]any{"access_token": "fixture-token"},
			}); err != nil {
				t.Fatal(err)
			}
			manager.RefreshSchedulerEntry(authID)
			execute := func(opts cliproxyexecutor.Options) error {
				request := cliproxyexecutor.Request{Model: "gpt-6-luna", Payload: []byte(`{}`)}
				switch mode {
				case "execute":
					_, err := manager.Execute(context.Background(), []string{"codex"}, request, opts)
					return err
				case "count":
					_, err := manager.ExecuteCount(context.Background(), []string{"codex"}, request, opts)
					return err
				default:
					stream, err := manager.ExecuteStream(context.Background(), []string{"codex"}, request, opts)
					if err != nil {
						return err
					}
					for chunk := range stream.Chunks {
						if chunk.Err != nil {
							return chunk.Err
						}
					}
					return nil
				}
			}
			if err := execute(cliproxyexecutor.Options{}); err != nil || len(executor.requests) != 1 {
				t.Fatalf("original execution: requests=%d, error=%v", len(executor.requests), err)
			}
			if info, ok := ResolvedModelInfo(executor.requests[0]); !ok || info == nil || info.ID != "gpt-6-luna" || !info.SupportConfigurationUpdate {
				t.Fatalf("selected OAuth model = (%+v, %t)", info, ok)
			}
			executor.requests = nil
			opts := cliproxyexecutor.Options{
				RequestAfterAuthInterceptor: func(_ context.Context, in cliproxyexecutor.RequestAfterAuthInterceptRequest) cliproxyexecutor.RequestAfterAuthInterceptResponse {
					in.Request.Model = "gpt-6-astra"
					return cliproxyexecutor.RequestAfterAuthInterceptResponse{Handled: true, Request: in.Request, Options: in.Options}
				},
			}
			if err := execute(opts); err != nil || len(executor.requests) != 1 {
				t.Fatalf("rewritten execution: requests=%d, error=%v", len(executor.requests), err)
			}
			if req := executor.requests[0]; req.Model != "gpt-6-astra" {
				t.Fatalf("intercepted model = %q", req.Model)
			} else if info, ok := ResolvedModelInfo(req); ok || info != nil {
				t.Fatalf("free credential inherited original model capability: %+v", info)
			}
		})
	}
}

func TestCodexOAuthModelInfoClearsStaleSelection(t *testing.T) {
	const authID = "codex-model-info-isolation"
	models := registry.GetGlobalRegistry()
	models.RegisterClient(authID, "codex", []*registry.ModelInfo{{
		ID: "tenant/public-alias", CatalogName: "gpt-6-luna", Thinking: &registry.ThinkingSupport{Levels: []string{"low"}},
	}})
	t.Cleanup(func() { models.UnregisterClient(authID) })

	oauth := &Auth{Provider: "codex", Attributes: map[string]string{"auth_kind": "oauth", "plan_type": "free"}}
	key := &Auth{ID: authID, Provider: "codex", Prefix: "tenant", Attributes: map[string]string{"api_key": "fixture-key"}}
	req, err := attachResolvedAPIKeyModelInfo(cliproxyexecutor.Request{}, oauth, "tenant/public-alias", "gpt-6-luna")
	if err != nil {
		t.Fatal(err)
	}
	req, err = attachResolvedAPIKeyModelInfo(req, key, "tenant/public-alias", "gpt-6-luna")
	if err != nil {
		t.Fatal(err)
	}
	if info, ok := ResolvedModelInfo(req); !ok || info == nil || info.SupportConfigurationUpdate || info.Thinking == nil {
		t.Fatalf("API key inherited OAuth capability: %+v", info)
	}
	if _, leaked := req.Metadata[resolvedCodexOAuthModelInfoMetadataKey]; leaked {
		t.Fatal("OAuth capability remained after API-key selection")
	}
	req, err = attachResolvedAPIKeyModelInfo(req, oauth, "tenant/public-alias", "gpt-6-luna")
	if err != nil {
		t.Fatal(err)
	}
	if info, ok := ResolvedModelInfo(req); !ok || info == nil || !info.SupportConfigurationUpdate {
		t.Fatalf("OAuth capability after API-key selection: %+v", info)
	}
	if _, leaked := req.Metadata[resolvedAPIKeyModelInfoMetadataKey]; leaked {
		t.Fatal("API-key capability remained after OAuth selection")
	}
	req, err = attachResolvedAPIKeyModelInfo(req, oauth, "tenant/public-alias", "gpt-6-astra")
	if err != nil {
		t.Fatal(err)
	}
	if info, ok := ResolvedModelInfo(req); ok || info != nil {
		t.Fatalf("unlisted free-plan model inherited previous capability: %+v", info)
	}
}

func TestHomeModelInfoUsesCodexOAuthOnlyForMatchingUpstream(t *testing.T) {
	oauth := &Auth{Provider: "codex", Attributes: map[string]string{"auth_kind": "oauth", "plan_type": "free"}}
	original, err := attachResolvedAPIKeyModelInfo(
		cliproxyexecutor.Request{}, oauth, "tenant/gpt-6-luna", "gpt-6-luna",
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, upstream string
		explicit       *bool
		wantSupport    bool
	}{
		{name: "matching upstream", upstream: "gpt-6-luna(high)", wantSupport: true},
		{name: "different upstream", upstream: "gpt-6-astra"},
		{name: "Home disables support", upstream: "gpt-6-luna", explicit: new(bool)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info := &registry.ModelInfo{ID: tc.upstream}
			request := attachResolvedHomeModelInfo(original, nil, tc.upstream, info, tc.explicit)
			selected, ok := ResolvedModelInfo(request)
			if !ok || selected == nil || selected.SupportConfigurationUpdate != tc.wantSupport || info.SupportConfigurationUpdate {
				t.Fatalf("Home selected model = (%+v, %t), source=%+v", selected, ok, info)
			}
			if oauthInfo, ok := ResolvedModelInfo(original); !ok || oauthInfo == nil || !oauthInfo.SupportConfigurationUpdate {
				t.Fatalf("source OAuth capability was changed: %+v", oauthInfo)
			}
		})
	}
}

func TestConfiguredModelHasCapabilityAcceptsConfigurationUpdateOverride(t *testing.T) {
	if !configuredModelHasCapability(&registry.ModelInfo{SupportConfigurationUpdate: true}) {
		t.Fatal("configuration-update-only model was not treated as capability metadata")
	}
}

func TestConfiguredModelHasCapabilityAcceptsContextOverride(t *testing.T) {
	if !configuredModelHasCapability(&registry.ModelInfo{MaxContextLength: context1MMinimumTokens}) {
		t.Fatal("context-only configured model was not treated as explicit capability metadata")
	}
}

func TestAttachResolvedAPIKeyModelInfoUsesValidatedContext1MSnapshot(t *testing.T) {
	const authID = "context-1m-snapshot"
	modelRegistry := registry.GetGlobalRegistry()
	modelRegistry.RegisterClient(
		authID, "claude", []*registry.ModelInfo{
			{
				ID:               "context-model",
				CatalogName:      "context-upstream",
				ContextLength:    context1MMinimumTokens,
				MaxContextLength: context1MMinimumTokens,
				Thinking:         &registry.ThinkingSupport{Levels: []string{"high"}},
			},
		},
	)
	defer modelRegistry.UnregisterClient(authID)

	req, err := attachResolvedAPIKeyModelInfo(
		cliproxyexecutor.Request{},
		&Auth{ID: authID, Provider: "claude", Attributes: map[string]string{"api_key": "key"}},
		"context-model[1m]",
		"context-upstream[1m]",
	)
	if err != nil {
		t.Fatalf("attach context snapshot: %v", err)
	}
	modelInfo, ok := ResolvedAPIKeyModelInfo(req)
	if !ok || !modelInfoSupportsContext1M(modelInfo) || modelInfo.ID != "context-upstream" {
		t.Fatalf("resolved model info = (%+v, %t), want validated 1M upstream snapshot", modelInfo, ok)
	}
}

func TestAttachResolvedAPIKeyModelInfoRejectsContext1MUpstreamMismatch(t *testing.T) {
	const authID = "context-1m-upstream-mismatch"
	modelRegistry := registry.GetGlobalRegistry()
	modelRegistry.RegisterClient(
		authID, "claude", []*registry.ModelInfo{
			{
				ID:               "context-model",
				CatalogName:      "configured-upstream",
				ContextLength:    context1MMinimumTokens,
				MaxContextLength: context1MMinimumTokens,
			},
		},
	)
	defer modelRegistry.UnregisterClient(authID)

	_, err := attachResolvedAPIKeyModelInfo(
		cliproxyexecutor.Request{},
		&Auth{ID: authID, Provider: "claude", Attributes: map[string]string{"api_key": "key"}},
		"context-model[1m]",
		"different-upstream[1m]",
	)
	requireContext1MModelSupportError(t, err)
}

func TestAttachResolvedAPIKeyModelInfoRejectsContext1MRegistryRace(t *testing.T) {
	const authID = "context-1m-final-guard"
	modelRegistry := registry.GetGlobalRegistry()
	modelRegistry.RegisterClient(
		authID, "test", []*registry.ModelInfo{
			{
				ID: "context-model", ContextLength: 200_000,
			},
		},
	)
	defer modelRegistry.UnregisterClient(authID)

	_, err := attachResolvedAPIKeyModelInfo(
		cliproxyexecutor.Request{},
		&Auth{ID: authID, Provider: "test"},
		"context-model[1m]",
		"context-model[1m]",
	)
	var authErr *Error
	if !errors.As(err, &authErr) || authErr.Code != "model_not_supported" ||
		authErr.HTTPStatus != http.StatusBadRequest || authErr.Retryable {
		t.Fatalf("final guard error = %#v, want non-retryable 400 model_not_supported", err)
	}
}

func assertResolvedThinkingLevels(t *testing.T, req cliproxyexecutor.Request, want string) {
	t.Helper()
	modelInfo, ok := ResolvedAPIKeyModelInfo(req)
	if !ok || modelInfo == nil || modelInfo.Thinking == nil {
		t.Fatalf("ResolvedAPIKeyModelInfo() = (%+v, %t), want thinking metadata", modelInfo, ok)
	}
	if modelInfo.UserDefined {
		t.Fatalf("resolved configured model remained user-defined: %+v", modelInfo)
	}
	if len(modelInfo.Thinking.Levels) != 1 || modelInfo.Thinking.Levels[0] != want {
		t.Fatalf("thinking levels = %v, want [%s]", modelInfo.Thinking.Levels, want)
	}
}
