package registry

import (
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"testing"
)

const catalogLookupFixture = `{"claude":[{
	"id":"registry-lookup-fixture","object":"model","display_name":"first",
	"supported_parameters":["tools"],"supportedGenerationMethods":["generateContent"],
	"supportedInputModalities":["text","image"],"supportedOutputModalities":["text"],
	"thinking":{"min":1,"max":100,"levels":["high"]},
	"native_capabilities":{"web_search":true},"support_configuration_update":true,
	"config":{"override_header":{"X-Catalog":"original"}}
}]}`

func preserveLookupTestCatalog(t *testing.T) {
	t.Helper()
	original := getModels()
	t.Cleanup(
		func() {
			modelsCatalogStore.mu.Lock()
			modelsCatalogStore.data = original
			modelsCatalogStore.mu.Unlock()
		},
	)
}

func mutateLookupTestModel(model *ModelInfo) {
	model.ID = "mutated"
	model.SupportConfigurationUpdate = false
	for _, values := range [][]string{
		model.SupportedParameters, model.SupportedGenerationMethods,
		model.SupportedInputModalities, model.SupportedOutputModalities,
	} {
		if len(values) > 0 {
			values[0] = "mutated"
		}
	}
	if model.Thinking != nil {
		model.Thinking.Min = -1
		if len(model.Thinking.Levels) > 0 {
			model.Thinking.Levels[0] = "mutated"
		}
	}
	if model.NativeCapabilities != nil && model.NativeCapabilities.WebSearch != nil {
		*model.NativeCapabilities.WebSearch = false
	}
	if model.Config != nil && model.Config.OverrideHeader != nil {
		model.Config.OverrideHeader["X-Catalog"] = "mutated"
	}
}

func TestCatalogQueriesObserveRefreshAndDetachMetadata(t *testing.T) {
	preserveLookupTestCatalog(t)
	for _, version := range []string{"first", "second"} {
		data := []byte(strings.ReplaceAll(catalogLookupFixture, "first", version))
		var expected staticModelsJSON
		if err := json.Unmarshal(data, &expected); err != nil {
			t.Fatal(err)
		}
		if err := loadModelsFromBytes(data, "lookup test"); err != nil {
			t.Fatal(err)
		}
		for _, lookup := range []struct {
			name string
			get  func() *ModelInfo
		}{
			{"list", func() *ModelInfo { return GetClaudeModels()[0] }},
			{
				"channel", func() *ModelInfo {
					return LookupStaticModelInfoByChannel(" registry-lookup-fixture ", " CLAUDE ")
				},
			},
			{"global", func() *ModelInfo { return LookupStaticModelInfo(" registry-lookup-fixture ") }},
			{"registry", func() *ModelInfo { return LookupModelInfo("registry-lookup-fixture", "claude") }},
		} {
			t.Run(
				version+"/"+lookup.name, func(t *testing.T) {
					model := lookup.get()
					if !reflect.DeepEqual(model, expected.Claude[0]) {
						t.Fatalf("lookup = %+v, want %+v", model, expected.Claude[0])
					}
					mutateLookupTestModel(model)
					if next := lookup.get(); !reflect.DeepEqual(next, expected.Claude[0]) {
						t.Fatalf("returned metadata changed next lookup: %+v", next)
					}
					if !reflect.DeepEqual(getModels().Claude, expected.Claude) {
						t.Fatal("returned metadata changed the active catalog")
					}
				},
			)
		}
	}
	if err := loadModelsFromBytes([]byte(`{"claude":[{"id":"replacement-model"}]}`), "lookup test"); err != nil {
		t.Fatal(err)
	}
	if model := LookupStaticModelInfo("registry-lookup-fixture"); model != nil {
		t.Fatalf("removed dynamic model is still visible: %+v", model)
	}
}

func TestCatalogFallbackAndEmbeddedQueriesDetachMetadata(t *testing.T) {
	preserveLookupTestCatalog(t)
	var expected staticModelsJSON
	if err := json.Unmarshal(embeddedModelsJSON, &expected); err != nil {
		t.Fatal(err)
	}
	for _, current := range []*staticModelsJSON{nil, {}} {
		modelsCatalogStore.mu.Lock()
		modelsCatalogStore.data = current
		modelsCatalogStore.mu.Unlock()
		models := GetClaudeModels()
		if !reflect.DeepEqual(models, expected.Claude) {
			t.Fatal("missing dynamic section did not use the embedded catalog")
		}
		mutateLookupTestModel(models[0])
		if !reflect.DeepEqual(GetClaudeModels(), expected.Claude) {
			t.Fatal("fallback metadata mutation changed the embedded catalog")
		}
		if got := getCatalogModels(func(*staticModelsJSON) []*ModelInfo { return nil }); got != nil {
			t.Fatalf("empty section = %+v, want nil", got)
		}
	}
	for _, section := range []struct {
		channel string
		models  []*ModelInfo
	}{
		{"claude", expected.Claude}, {"gemini", expected.Gemini},
		{"gemini-interactions", expected.Gemini}, {"vertex", expected.Vertex},
		{"gemini-cli", expected.GeminiCLI}, {"aistudio", expected.AIStudio},
		{"codex", expected.CodexPro}, {"qwen", expected.Qwen}, {"iflow", expected.IFlow},
		{"kimi", expected.Kimi}, {"kimi-ai", expected.Kimi},
		{"kimi.ai", expected.Kimi}, {"kimi.com", expected.Kimi},
		{"antigravity", expected.Antigravity}, {"xai", expected.XAI},
		{"x-ai", expected.XAI}, {"grok", expected.XAI},
		{"devin", expected.Devin}, {"meta", expected.Meta}, {"muse", expected.Meta},
	} {
		t.Run(
			section.channel, func(t *testing.T) {
				if len(section.models) == 0 {
					if got := GetEmbeddedStaticModelDefinitionsByChannel(section.channel); got != nil {
						t.Fatalf("empty embedded section = %+v, want nil", got)
					}
					return
				}
				want := section.models[0]
				for range 2 {
					got := findModelByID(GetEmbeddedStaticModelDefinitionsByChannel(section.channel), want.ID)
					if !reflect.DeepEqual(got, want) {
						t.Fatalf("embedded model = %+v, want %+v", got, want)
					}
					mutateLookupTestModel(got)
				}
			},
		)
	}
	for channel, models := range expected.Legacy {
		want := models[0]
		for range 2 {
			got := findModelByID(legacyStaticLookupModelsByChannel(channel), want.ID)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("legacy %s = %+v, want %+v", channel, got, want)
			}
			mutateLookupTestModel(got)
		}
	}
	for _, modelID := range []string{"", " \t", "unknown-registry-lookup", "CLAUDE-OPUS-5-5"} {
		if got := LookupStaticModelInfo(modelID); got != nil {
			t.Fatalf("lookup %q = %+v, want nil", modelID, got)
		}
	}
	if got := GetStaticModelDefinitionsByChannel("unknown-registry-channel"); got != nil {
		t.Fatalf("unknown channel = %+v, want nil", got)
	}
	if got := GetEmbeddedStaticModelDefinitionsByChannel("unknown-registry-channel"); got != nil {
		t.Fatalf("unknown embedded channel = %+v, want nil", got)
	}
}

func TestStaticLookupPreservesProviderPrecedence(t *testing.T) {
	preserveLookupTestCatalog(t)
	catalog := &staticModelsJSON{}
	sections := []struct {
		name   string
		models *[]*ModelInfo
	}{
		{"claude", &catalog.Claude}, {"gemini", &catalog.Gemini},
		{"vertex", &catalog.Vertex}, {"gemini-cli", &catalog.GeminiCLI},
		{"aistudio", &catalog.AIStudio}, {"codex", &catalog.CodexPro},
		{"qwen", &catalog.Qwen}, {"kimi", &catalog.Kimi},
		{"xai", &catalog.XAI}, {"meta", &catalog.Meta},
	}
	for _, section := range sections {
		*section.models = []*ModelInfo{nil, {ID: "registry-priority-fixture", DisplayName: section.name}}
	}
	modelsCatalogStore.mu.Lock()
	modelsCatalogStore.data = catalog
	modelsCatalogStore.mu.Unlock()
	for _, section := range sections {
		got := LookupStaticModelInfo("registry-priority-fixture")
		if got == nil || got.DisplayName != section.name {
			t.Fatalf("next provider = %+v, want %s", got, section.name)
		}
		if got := LookupStaticModelInfoByChannel("registry-priority-fixture", section.name); got == nil ||
			got.DisplayName != section.name {
			t.Fatalf("provider-specific lookup = %+v, want %s", got, section.name)
		}
		*section.models = nil
	}
	if got := LookupStaticModelInfo("registry-priority-fixture"); got != nil {
		t.Fatalf("removed model = %+v, want nil", got)
	}
	catalog.Claude = []*ModelInfo{nil}
	if got := GetClaudeModels(); len(got) != 1 || got[0] != nil {
		t.Fatalf("populated nil model section = %+v, want [nil]", got)
	}
	for _, test := range []struct{ id, channel string }{
		{"gpt-5.3-codex-spark", "codex"}, {"gpt-5.5", "codex"},
		{"kimi-k3-256k", "kimi"}, {"gemini-3.1-flash-lite-preview", "gemini"},
	} {
		got := LookupStaticModelInfo(test.id)
		byChannel := LookupStaticModelInfoByChannel(test.id, test.channel)
		if got == nil || got.ID != test.id || byChannel == nil || byChannel.ID != test.id {
			t.Fatalf("historical %s: global=%+v channel=%+v", test.id, got, byChannel)
		}
	}
}

func TestMergedCatalogDoesNotMutateEmbeddedMetadata(t *testing.T) {
	var expected staticModelsJSON
	if err := json.Unmarshal(embeddedModelsJSON, &expected); err != nil {
		t.Fatal(err)
	}
	merged, err := mergeRemoteModelsCatalog([]byte(`{"qwen":[{"id":"merged-qwen"}]}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	mutateLookupTestModel(merged.Claude[0])
	mutateLookupTestModel(merged.Legacy["claude"][0])
	delete(merged.Legacy, "gemini")
	if !reflect.DeepEqual(embeddedModelsCatalog(), &expected) {
		t.Fatal("merged catalog mutation changed shared embedded metadata")
	}
}

func TestCatalogQueriesDuringConcurrentRefresh(t *testing.T) {
	preserveLookupTestCatalog(t)
	first := []byte(catalogLookupFixture)
	second := []byte(strings.ReplaceAll(catalogLookupFixture, "first", "second"))
	if err := loadModelsFromBytes(first, "concurrent lookup test"); err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	workers.Go(
		func() {
			for range 20 {
				for _, data := range [][]byte{first, second} {
					if err := loadModelsFromBytes(data, "concurrent lookup test"); err != nil {
						t.Error(err)
						return
					}
				}
			}
		},
	)
	for range 4 {
		workers.Go(
			func() {
				for range 10 {
					model := LookupStaticModelInfo("registry-lookup-fixture")
					if model == nil || model.ID != "registry-lookup-fixture" ||
						(model.DisplayName != "first" && model.DisplayName != "second") ||
						model.Thinking == nil || model.Thinking.Levels[0] != "high" {
						t.Errorf("concurrent model = %+v", model)
						return
					}
					mutateLookupTestModel(model)
					embedded := GetEmbeddedStaticModelDefinitionsByChannel("claude")
					if len(embedded) == 0 || embedded[0] == nil || embedded[0].ID == "mutated" {
						t.Error("concurrent embedded metadata changed")
						return
					}
					mutateLookupTestModel(embedded[0])
				}
			},
		)
	}
	workers.Wait()
}

func BenchmarkCatalogModelLists(b *testing.B) {
	for _, test := range []struct {
		name string
		get  func() []*ModelInfo
	}{
		{"dynamic", GetClaudeModels},
		{"embedded", func() []*ModelInfo { return GetEmbeddedStaticModelDefinitionsByChannel("claude") }},
	} {
		b.Run(
			test.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					if models := test.get(); len(models) == 0 {
						b.Fatal("model list is empty")
					}
				}
			},
		)
	}
}

func BenchmarkStaticModelLookup(b *testing.B) {
	for _, id := range []string{
		"claude-opus-5-5", "gpt-6.1-sol", "gpt-5.3-codex-spark", "muse-spark-1.3", "unknown-registry-lookup",
	} {
		b.Run(
			id, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					got := LookupStaticModelInfo(id)
					if (got != nil) != (id != "unknown-registry-lookup") {
						b.Fatalf("lookup %s = %+v", id, got)
					}
				}
			},
		)
	}
}
