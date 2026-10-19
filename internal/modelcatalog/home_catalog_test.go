package modelcatalog_test

import (
	"testing"

	codexmodels "github.com/Pyrokine/CLIProxyAPI/v6/internal/client/codex/models"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/modelcatalog"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
)

func TestDecodeHomeCatalogRejectsTrailingJSON(t *testing.T) {
	if _, err := modelcatalog.DecodeHomeCatalog([]byte(`{"codex":[{"id":"m"}]} {}`)); err == nil {
		t.Fatal("DecodeHomeCatalog() error = nil, want trailing JSON rejected")
	}
}

func TestDecodeHomeCatalogRejectsInvalidNativeCapabilities(t *testing.T) {
	raw := []byte(`{"codex":[{"id":"m","native_capabilities":{"web_search":"yes"}}]}`)
	if _, err := modelcatalog.DecodeHomeCatalog(raw); err == nil {
		t.Fatal("DecodeHomeCatalog() error = nil, want invalid native capabilities rejected")
	}
}

func TestDecodeHomeCatalogKeepsSameIDRoutesSeparate(t *testing.T) {
	catalog, err := modelcatalog.DecodeHomeCatalog([]byte(`{
		"codex": [{
			"id": "shared-model",
			"context_length": 123456,
			"max_completion_tokens": 2345,
			"thinking": {"levels": ["low", "high"]},
			"native_capabilities": {"web_search": true}
		}],
		"gemini": [{
			"id": "shared-model",
			"context_length": 654321,
			"max_completion_tokens": 3456,
			"thinking": {"levels": ["low"]},
			"native_capabilities": {"web_search": true}
		}]
	}`))
	if err != nil {
		t.Fatalf("DecodeHomeCatalog() error = %v", err)
	}

	routes := catalog.Routes()
	if len(routes) != 2 {
		t.Fatalf("route count = %d, want 2", len(routes))
	}
	models := catalog.Models()
	if len(models) != 2 {
		t.Fatalf("model count = %d, want two route-bound entries", len(models))
	}
	byProvider := make(map[string]map[string]any, len(routes))
	for _, route := range routes {
		byProvider[route.Provider] = route.Model
	}
	assertHomeMetadata(t, byProvider["codex"], 123456, 2345, []string{"low", "high"})
	assertHomeMetadata(t, byProvider["gemini"], 654321, 3456, []string{"low"})

	capability := catalog.ResponsesWebSearchCapability("shared-model")
	if capability == nil || !*capability {
		t.Fatalf("web search capability = %v, want known supported", capability)
	}
	response := codexmodels.BuildResponseForClientWithCPACapabilities(
		catalog.Models(),
		nil,
		catalog.ResponsesWebSearchCapability,
		false,
		"cpa",
	)
	projected, ok := response["models"].([]map[string]any)
	if !ok || len(projected) != 1 {
		t.Fatalf("Codex projection = %#v, want one public same-ID model", response["models"])
	}
	capabilities, ok := projected[0]["cpa_capabilities"].(map[string]any)
	if !ok || capabilities["web_search"] != true {
		t.Fatalf("Codex capabilities = %#v, want web_search=true", projected[0]["cpa_capabilities"])
	}
}

func TestHomeCatalogRoutesAndModelsAreDeepCloned(t *testing.T) {
	catalog, err := modelcatalog.DecodeHomeCatalog([]byte(`{
		"codex": [{
			"id": "clone-model",
			"thinking": {"levels": ["low", "high"]},
			"native_capabilities": {"web_search": true}
		}]
	}`))
	if err != nil {
		t.Fatalf("DecodeHomeCatalog() error = %v", err)
	}

	routes := catalog.Routes()
	*routes[0].NativeCapabilities.WebSearch = false
	routes[0].Model["thinking"].(map[string]any)["levels"].([]any)[0] = "changed"
	models := catalog.Models()
	models[0]["id"] = "changed"

	freshRoute := catalog.Routes()[0]
	if freshRoute.NativeCapabilities == nil || freshRoute.NativeCapabilities.WebSearch == nil ||
		!*freshRoute.NativeCapabilities.WebSearch {
		t.Fatalf("native capabilities changed through returned route: %+v", freshRoute.NativeCapabilities)
	}
	levels := freshRoute.Model["thinking"].(map[string]any)["levels"].([]any)
	if levels[0] != "low" {
		t.Fatalf("thinking levels changed through returned route: %v", levels)
	}
	if id, _ := catalog.Models()[0]["id"].(string); id != "clone-model" {
		t.Fatalf("model ID changed through returned model: %q", id)
	}
}

func TestHomeCatalogWebSearchCapabilityUsesFalseUnknownTruePriority(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		present bool
		want    bool
	}{
		{
			name: "false wins",
			raw: `{
				"codex": [{"id":"m","native_capabilities":{"web_search":true}}],
				"kimi": [{"id":"m","native_capabilities":{"web_search":true}}]
			}`,
			present: true,
		},
		{
			name: "unknown wins over true",
			raw: `{
				"codex": [{"id":"m","native_capabilities":{"web_search":true}}],
				"xai": [{"id":"m"}]
			}`,
		},
		{
			name: "all routes explicitly support",
			raw: `{
				"codex": [{"id":"m","native_capabilities":{"web_search":true}}],
				"claude": [{"id":"m","native_capabilities":{"web_search":true}}]
			}`,
			present: true,
			want:    true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			catalog, err := modelcatalog.DecodeHomeCatalog([]byte(test.raw))
			if err != nil {
				t.Fatalf("DecodeHomeCatalog() error = %v", err)
			}
			got := catalog.ResponsesWebSearchCapability("m")
			if (got != nil) != test.present {
				t.Fatalf("capability presence = %v, want %v", got != nil, test.present)
			}
			if got != nil && *got != test.want {
				t.Fatalf("capability = %v, want %v", *got, test.want)
			}
		})
	}
}

func TestHomeCatalogResolvesKimiK3256KLegacyMetadata(t *testing.T) {
	catalog, err := modelcatalog.DecodeHomeCatalog([]byte(`{
		"kimi": [{"id":"kimi-k3-256k"}]
	}`))
	if err != nil {
		t.Fatalf("DecodeHomeCatalog() error = %v", err)
	}
	models := catalog.Models()
	if len(models) != 1 {
		t.Fatalf("models = %#v, want one model", models)
	}
	assertHomeMetadata(t, models[0], 262144, 65536, []string{"low", "high", "max"})
	if capability := catalog.ResponsesWebSearchCapability("kimi-k3-256k"); capability == nil || *capability {
		t.Fatalf("Kimi route web search capability = %v, want false", capability)
	}

	response := codexmodels.BuildResponseForClientWithCPACapabilities(
		models,
		nil,
		catalog.ResponsesWebSearchCapability,
		false,
		"cpa",
	)
	codexEntries, ok := response["models"].([]map[string]any)
	if !ok || len(codexEntries) != 1 {
		t.Fatalf("Codex models = %#v, want one Kimi legacy route", response["models"])
	}
	codexModel := codexEntries[0]
	if got := testModelInt(codexModel, "context_window"); got != 262144 {
		t.Fatalf("Kimi context_window = %d, want 262144", got)
	}
	if got := testModelInt(codexModel, "max_tokens"); got != 65536 {
		t.Fatalf("Kimi max_tokens = %d, want 65536", got)
	}
	assertCodexReasoningLevels(t, codexModel, []string{"low", "high", "max"})
	capabilities, ok := codexModel["cpa_capabilities"].(map[string]any)
	if !ok || capabilities["web_search"] != false {
		t.Fatalf("Kimi cpa_capabilities = %#v, want web_search=false", codexModel["cpa_capabilities"])
	}
}

func TestHomeCatalogCodexProjectionUsesRequestMetadata(t *testing.T) {
	catalog, err := modelcatalog.DecodeHomeCatalog([]byte(`{
		"codex": [{
			"id": "gpt-5.6-sol",
			"context_length": 0,
			"inputTokenLimit": 123456,
			"max_completion_tokens": 0,
			"outputTokenLimit": 4321,
			"thinking": {"levels": ["low", "high"]},
			"native_capabilities": {"web_search": true}
		}]
	}`))
	if err != nil {
		t.Fatalf("DecodeHomeCatalog() error = %v", err)
	}

	response := codexmodels.BuildResponseForClientWithCPACapabilities(
		catalog.Models(),
		nil,
		catalog.ResponsesWebSearchCapability,
		false,
		"cpa",
	)
	models, ok := response["models"].([]map[string]any)
	if !ok || len(models) != 1 {
		t.Fatalf("models = %#v, want one model", response["models"])
	}
	model := models[0]
	if got := testModelInt(model, "context_window"); got != 123456 {
		t.Fatalf("context_window = %d, want 123456", got)
	}
	if got := testModelInt(model, "max_context_window"); got != 123456 {
		t.Fatalf("max_context_window = %d, want 123456", got)
	}
	if got := testModelInt(model, "max_tokens"); got != 4321 {
		t.Fatalf("max_tokens = %d, want 4321", got)
	}
	assertCodexReasoningLevels(t, model, []string{"low", "high"})
	capabilities, ok := model["cpa_capabilities"].(map[string]any)
	if !ok || capabilities["web_search"] != true {
		t.Fatalf("cpa_capabilities = %#v, want web_search=true", model["cpa_capabilities"])
	}
}

func assertHomeMetadata(t *testing.T, model map[string]any, contextLength, completionTokens int, levels []string) {
	t.Helper()
	if got := testModelInt(model, "context_length"); got != contextLength {
		t.Fatalf("context_length = %d, want %d", got, contextLength)
	}
	if got := testModelInt(model, "max_completion_tokens"); got != completionTokens {
		t.Fatalf("max_completion_tokens = %d, want %d", got, completionTokens)
	}
	thinking, ok := model["thinking"].(map[string]any)
	if !ok {
		if typed, typedOK := model["thinking"].(*registry.ThinkingSupport); typedOK {
			if !sameStrings(typed.Levels, levels) {
				t.Fatalf("thinking levels = %v, want %v", typed.Levels, levels)
			}
			return
		}
		t.Fatalf("thinking = %#v, want levels %v", model["thinking"], levels)
	}
	rawLevels, ok := thinking["levels"].([]any)
	if !ok {
		t.Fatalf("thinking levels = %#v", thinking["levels"])
	}
	got := make([]string, 0, len(rawLevels))
	for _, raw := range rawLevels {
		level, _ := raw.(string)
		got = append(got, level)
	}
	if !sameStrings(got, levels) {
		t.Fatalf("thinking levels = %v, want %v", got, levels)
	}
}

func assertCodexReasoningLevels(t *testing.T, model map[string]any, want []string) {
	t.Helper()
	rawLevels, ok := model["supported_reasoning_levels"].([]any)
	if !ok {
		t.Fatalf("supported_reasoning_levels = %#v", model["supported_reasoning_levels"])
	}
	got := make([]string, 0, len(rawLevels))
	for _, raw := range rawLevels {
		level, _ := raw.(map[string]any)
		effort, _ := level["effort"].(string)
		got = append(got, effort)
	}
	if !sameStrings(got, want) {
		t.Fatalf("reasoning levels = %v, want %v", got, want)
	}
}

func testModelInt(model map[string]any, key string) int {
	switch value := model[key].(type) {
	case int:
		return value
	case int64:
		return int(value)
	case float64:
		return int(value)
	default:
		return 0
	}
}

func sameStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
