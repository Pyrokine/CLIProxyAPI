package registry

import (
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCatalogAvailableModelsReturnDetachedMutableFields(t *testing.T) {
	r := newTestModelRegistry()
	search := true
	r.RegisterClient(
		"detached", "codex", []*ModelInfo{
			{
				ID: "detached-model", SupportedParameters: []string{"temperature"},
				SupportedGenerationMethods: []string{"generateContent"},
				SupportedInputModalities:   []string{"text"}, SupportedOutputModalities: []string{"text"},
				Thinking:           &ThinkingSupport{Min: 1, Levels: []string{"high"}},
				NativeCapabilities: &NativeCapabilities{WebSearch: &search},
				Config:             &ModelConfig{OverrideHeader: map[string]string{"X-Model": "original"}},
			},
		},
	)
	openAI := r.GetAvailableModels("openai")[0]
	openAI["supported_parameters"].([]string)[0] = "mutated"
	openAI["thinking"].(*ThinkingSupport).Levels[0] = "mutated"
	gemini := r.GetAvailableModels("gemini")[0]
	for _, key := range []string{
		"supportedGenerationMethods", "supportedInputModalities", "supportedOutputModalities",
	} {
		gemini[key].([]string)[0] = "mutated"
	}
	provider := r.GetAvailableModelsByProvider("codex")[0]
	provider.ID = "mutated"
	provider.Config.OverrideHeader["X-Model"] = "mutated"
	*provider.NativeCapabilities.WebSearch = false
	model := r.GetModelsForClient("detached")[0]
	if model.ID != "detached-model" || model.SupportedParameters[0] != "temperature" ||
		model.Thinking.Levels[0] != "high" ||
		model.SupportedGenerationMethods[0] != "generateContent" ||
		model.SupportedInputModalities[0] != "text" ||
		model.SupportedOutputModalities[0] != "text" ||
		model.Config.OverrideHeader["X-Model"] != "original" ||
		!*model.NativeCapabilities.WebSearch {
		t.Fatalf("returned metadata modified registry: %+v", model)
	}
}

func TestCatalogFirstAvailableDoesNotNestReadLockUnderWriter(t *testing.T) {
	r := newTestModelRegistry()
	r.RegisterClient("reader-writer", "openai", []*ModelInfo{{ID: "available", Created: 1}})
	var workers sync.WaitGroup
	workers.Add(2)
	go func() {
		defer workers.Done()
		for range 300 {
			r.RegisterClient("reader-writer", "openai", []*ModelInfo{{ID: "available", Created: 1}})
		}
	}()
	go func() {
		defer workers.Done()
		for range 300 {
			if _, err := r.GetFirstAvailableModel("openai"); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	done := make(chan struct{})
	go func() { workers.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("model selection blocked with a queued registry writer")
	}
}

func TestCatalogPartialRefreshPreservesInternalCapabilities(t *testing.T) {
	search := true
	current := &staticModelsJSON{
		CodexPlus: []*ModelInfo{
			{
				ID: "retained-codex", NativeCapabilities: &NativeCapabilities{WebSearch: &search},
				SupportConfigurationUpdate: true, ExplicitThinking: true, ExplicitInputModalities: true,
				MaxContextLength: 500000, Thinking: &ThinkingSupport{Levels: []string{"high"}},
			},
		},
	}
	merged, err := mergeRemoteModelsCatalog([]byte(`{"gemini":[{"id":"refreshed-gemini"}]}`), current)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(current.CodexPlus, merged.CodexPlus) {
		t.Fatalf(
			"partial refresh dropped unrefreshed capabilities: before=%+v after=%+v", current.CodexPlus[0],
			merged.CodexPlus[0],
		)
	}
	for _, provider := range detectChangedProviders(current, merged) {
		if provider == "codex" {
			t.Fatal("unchanged Codex section triggered a refresh notification")
		}
	}
	merged.CodexPlus[0].Thinking.Levels[0] = "mutated"
	*merged.CodexPlus[0].NativeCapabilities.WebSearch = false
	if current.CodexPlus[0].Thinking.Levels[0] != "high" || !*current.CodexPlus[0].NativeCapabilities.WebSearch {
		t.Fatal("partial refresh reused mutable source model fields")
	}
}

func TestCatalogDynamicOverrideHeadersCanClearStaticDefaults(t *testing.T) {
	for _, raw := range []string{
		`{"id":"gpt-5.6-luna"}`,
		`{"id":"gpt-5.6-luna","config":{}}`,
		`{"id":"gpt-5.6-luna","config":{"override_header":{}}}`,
	} {
		t.Run(
			raw, func(t *testing.T) {
				var model ModelInfo
				if err := json.Unmarshal([]byte(raw), &model); err != nil {
					t.Fatal(err)
				}
				reg := GetGlobalRegistry()
				reg.RegisterClient("dynamic-header-clear", "codex", []*ModelInfo{&model})
				t.Cleanup(func() { reg.UnregisterClient("dynamic-header-clear") })
				if headers := ModelOverrideHeaders("gpt-5.6-luna", "codex"); len(headers) != 0 {
					t.Fatalf("dynamic configuration restored static defaults: %v", headers)
				}
			},
		)
	}
}

func TestCatalogOverrideHeadersUseSelectedProviderAndClone(t *testing.T) {
	r := GetGlobalRegistry()
	modelID := "catalog-header-contract"
	for _, provider := range []string{"codex", "gemini"} {
		clientID := "header-contract-" + provider
		r.RegisterClient(
			clientID, provider,
			[]*ModelInfo{{ID: modelID, Config: &ModelConfig{OverrideHeader: map[string]string{"X-Route": provider}}}},
		)
		t.Cleanup(func() { r.UnregisterClient(clientID) })
	}
	for _, provider := range []string{"codex", "gemini"} {
		got := ModelOverrideHeaders(modelID, provider)
		if got["X-Route"] != provider {
			t.Fatalf("provider=%s headers=%v", provider, got)
		}
		got["X-Route"] = "mutated"
		if ModelOverrideHeaders(modelID, provider)["X-Route"] != provider {
			t.Fatal("header mutation reached registry")
		}
	}
	if got := ModelOverrideHeaders(modelID, "claude"); len(got) != 0 {
		t.Fatalf("foreign provider headers leaked: %v", got)
	}
	if got := ModelOverrideHeaders(modelID); got["X-Route"] != "codex" {
		t.Fatalf("existing Codex consumer default headers = %v", got)
	}
}

func TestCatalogDevinLegacyOptionalLimitsAndGeminiMethods(t *testing.T) {
	for _, input := range []string{
		`{"devin":[{"id":"legacy"}]}`,
		`{"devin":[{"id":"missing-context","max_completion_tokens":1}]}`,
		`{"devin":[{"id":"missing-output","context_length":1}]}`,
	} {
		models, err := validateDevinModelsJSON([]byte(input))
		if err != nil {
			t.Fatalf("legacy optional fields rejected: %s: %v", input, err)
		}
		if got := strings.Join(models[0].SupportedGenerationMethods, ","); got != "generateContent,countTokens" {
			t.Fatalf("Gemini methods = %q", got)
		}
	}
}

func TestCatalogDevinDefaultCapsAndCaseInsensitiveLookup(t *testing.T) {
	for _, model := range staticDevinModels {
		if model.MaxCompletionTokens != 64000 {
			t.Fatalf("static Devin default %s completion = %d", model.ID, model.MaxCompletionTokens)
		}
	}
	if model := LookupDevinModel("DEVIN/SWE-1-6-SLOW"); model == nil || model.ID != "devin/swe-1-6-slow" {
		t.Fatalf("case-insensitive catalog lookup = %+v", model)
	}
	models, err := validateDevinModelsJSON(
		[]byte(`{"devin":[{"id":"explicit-limit","context_length":300000,"max_completion_tokens":128000}]}`),
	)
	if err != nil || models[0].MaxCompletionTokens != 128000 {
		t.Fatalf("explicit catalog completion cap changed: models=%+v err=%v", models, err)
	}
}
