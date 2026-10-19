package registry

import (
	"encoding/json"
	"testing"
	"time"
)

func TestGeminiInteractionsUsesGeminiStaticCatalog(t *testing.T) {
	for _, models := range [][]*ModelInfo{
		GetStaticModelDefinitionsByChannel("gemini-interactions"),
		GetEmbeddedStaticModelDefinitionsByChannel("gemini-interactions"),
	} {
		if len(models) == 0 || models[0] == nil {
			t.Fatal("gemini-interactions has no static Gemini catalog")
		}
	}
	if info := LookupStaticModelInfoByChannel(GetGeminiModels()[0].ID, "gemini-interactions"); info == nil {
		t.Fatal("Gemini interactions model lookup returned nil")
	}
}

func TestModelRefreshIntervalCanBeDisabled(t *testing.T) {
	t.Parallel()

	for _, hours := range []int{-1, 0} {
		if interval, enabled := modelRefreshInterval(hours); enabled || interval != 0 {
			t.Fatalf("modelRefreshInterval(%d) = (%s, %v), want disabled", hours, interval, enabled)
		}
	}
	if interval, enabled := modelRefreshInterval(6); !enabled || interval != 6*time.Hour {
		t.Fatalf("modelRefreshInterval(6) = (%s, %v), want (6h, true)", interval, enabled)
	}
}

func TestMergeRemoteModelsCatalogKeepsLastKnownGoodSections(t *testing.T) {
	t.Parallel()

	current := &staticModelsJSON{
		Gemini: []*ModelInfo{{ID: "gemini-old"}},
		Qwen:   []*ModelInfo{{ID: "qwen-current"}},
		IFlow:  []*ModelInfo{{ID: "iflow-current"}},
	}
	remote := map[string]any{
		"gemini": []map[string]any{{"id": "gemini-new"}},
		"qwen":   []any{},
		"iflow":  []map[string]any{{"id": ""}},
	}
	data, err := json.Marshal(remote)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}

	merged, err := mergeRemoteModelsCatalog(data, current)
	if err != nil {
		t.Fatalf("mergeRemoteModelsCatalog() error = %v", err)
	}
	assertOnlyModelID(t, merged.Gemini, "gemini-new")
	assertOnlyModelID(t, merged.Qwen, "qwen-current")
	assertOnlyModelID(t, merged.IFlow, "iflow-current")
	assertOnlyModelID(t, current.Gemini, "gemini-old")
}

func TestMergeRemoteModelsCatalogRejectsCatalogWithoutValidSections(t *testing.T) {
	t.Parallel()

	_, err := mergeRemoteModelsCatalog([]byte(`{"qwen":[],"iflow":[{"id":""}]}`), &staticModelsJSON{})
	if err == nil {
		t.Fatal("mergeRemoteModelsCatalog() error = nil, want rejected catalog")
	}
}

func TestDetectChangedProvidersIncludesGeminiInteractions(t *testing.T) {
	t.Parallel()

	oldData := &staticModelsJSON{Gemini: []*ModelInfo{{ID: "gemini-old"}}}
	newData := &staticModelsJSON{Gemini: []*ModelInfo{{ID: "gemini-new"}}}
	changed := detectChangedProviders(oldData, newData)
	want := []string{"gemini", "gemini-interactions"}
	if len(changed) != len(want) {
		t.Fatalf("detectChangedProviders() = %v, want %v", changed, want)
	}
	for index := range want {
		if changed[index] != want[index] {
			t.Fatalf("detectChangedProviders() = %v, want %v", changed, want)
		}
	}
}

func TestConfigurationUpdateModelCapabilityIsInternalAndRefreshable(t *testing.T) {
	model := LookupStaticModelInfoByChannel("gpt-6-luna", "codex")
	if model == nil || !model.SupportConfigurationUpdate {
		t.Fatalf("Codex configuration_update capability = %#v", model)
	}
	if !cloneModelInfo(model).SupportConfigurationUpdate {
		t.Fatal("cloning lost the configuration_update capability")
	}
	public, err := json.Marshal(model)
	if err != nil {
		t.Fatalf("marshal model: %v", err)
	}
	var fields map[string]any
	if err = json.Unmarshal(public, &fields); err != nil {
		t.Fatalf("unmarshal public model: %v", err)
	}
	if _, visible := fields["support_configuration_update"]; visible {
		t.Fatalf("internal capability appeared in public model JSON: %s", public)
	}
	var changed ModelInfo
	if err = json.Unmarshal([]byte(`{"id":"gpt-6-luna","support_configuration_update":true}`), &changed); err != nil {
		t.Fatalf("unmarshal configuration_update: %v", err)
	}
	base := &ModelInfo{ID: changed.ID}
	if !modelSectionChanged([]*ModelInfo{base}, []*ModelInfo{&changed}) {
		t.Fatal("configuration_update change did not trigger catalog refresh")
	}
}

func TestKimiAIHasSeparateProviderWithSharedCatalog(t *testing.T) {
	for _, provider := range []string{"kimi", "kimi-ai", "kimi.ai", "kimi.com"} {
		if got := GetStaticModelDefinitionsByChannel(provider); len(got) == 0 {
			t.Fatalf("Kimi catalog for %q is empty", provider)
		}
	}
	oldData := &staticModelsJSON{Kimi: []*ModelInfo{{ID: "kimi-old"}}}
	newData := &staticModelsJSON{Kimi: []*ModelInfo{{ID: "kimi-new"}}}
	changes := detectChangedProviders(oldData, newData)
	for _, provider := range []string{"kimi", "kimi-ai", "kimi.ai", "kimi.com"} {
		found := false
		for _, change := range changes {
			found = found || change == provider
		}
		if !found {
			t.Fatalf("catalog update did not notify %q: %#v", provider, changes)
		}
	}
	if supported := responsesWebSearchProviderPathSupport("kimi-ai"); supported == nil || *supported {
		t.Fatalf("Kimi.ai Responses web search support = %#v", supported)
	}
}

func TestMetaModelCatalogAndProviderLookup(t *testing.T) {
	models := GetMetaModels()
	if len(models) != 5 || models[0].ID != "muse-spark-1.3" || models[0].OwnedBy != "meta" {
		t.Fatalf("Meta model catalog = %#v", models)
	}
	models[0].ID = "mutated"
	if got := LookupStaticModelInfoByChannel("muse-spark-1.3", "meta"); got == nil || got.ID != "muse-spark-1.3" {
		t.Fatalf("Meta provider lookup = %#v", got)
	}
	if got := LookupStaticModelInfoByChannel("muse-spark-1.3", "muse"); got == nil || got.ID != "muse-spark-1.3" {
		t.Fatalf("Muse provider alias lookup = %#v", got)
	}
	if got := LookupStaticModelInfoByChannel("muse-spark-1.3", "kimi"); got != nil {
		t.Fatalf("Kimi provider returned Meta model = %#v", got)
	}
	if got := LookupStaticModelInfo("muse-spark-1.3-contributor"); got == nil || got.OwnedBy != "meta" {
		t.Fatalf("global Meta model lookup = %#v", got)
	}
}

func TestMetaCatalogRefreshAcceptsValidAndRejectsInvalid(t *testing.T) {
	current := &staticModelsJSON{Meta: []*ModelInfo{{ID: "muse-spark-old"}}}
	merged, err := mergeRemoteModelsCatalog([]byte(`{"meta":[{"id":"muse-spark-new"}]}`), current)
	if err != nil {
		t.Fatalf("merge valid Meta catalog: %v", err)
	}
	assertOnlyModelID(t, merged.Meta, "muse-spark-new")
	changed := detectChangedProviders(current, merged)
	if len(changed) != 1 || changed[0] != "meta" {
		t.Fatalf("changed providers = %#v, want meta", changed)
	}
	for _, invalid := range []string{`{"meta":[null]}`, `{"meta":[{"id":" "}]}`, `{"meta":[{"id":"x"},{"id":"x"}]}`} {
		if _, err = mergeRemoteModelsCatalog([]byte(invalid), &staticModelsJSON{}); err == nil {
			t.Fatalf("invalid Meta catalog %s accepted", invalid)
		}
	}
}

func assertOnlyModelID(t *testing.T, models []*ModelInfo, want string) {
	t.Helper()
	if len(models) != 1 || models[0] == nil || models[0].ID != want {
		t.Fatalf("models = %#v, want only %q", models, want)
	}
}
