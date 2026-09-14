package auth

import (
	"errors"
	"net/http"
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
