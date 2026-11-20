package registry

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCodexConfigurationUpdateCapability(t *testing.T) {
	for _, tier := range []struct {
		name    string
		models  func() []*ModelInfo
		capable []string
	}{
		{name: "free", models: GetCodexFreeModels, capable: []string{"gpt-6-luna"}},
		{name: "team", models: GetCodexTeamModels, capable: []string{"gpt-6-astra", "gpt-6-sol", "gpt-6-luna"}},
		{name: "plus", models: GetCodexPlusModels, capable: []string{"gpt-6-astra", "gpt-6-sol", "gpt-6-luna"}},
		{name: "pro", models: GetCodexProModels, capable: []string{"gpt-6-astra", "gpt-6-sol", "gpt-6-luna"}},
	} {
		t.Run(
			tier.name, func(t *testing.T) {
				byID := make(map[string]*ModelInfo)
				for _, info := range tier.models() {
					if info != nil {
						byID[info.ID] = info
					}
				}
				for _, id := range tier.capable {
					info := byID[id]
					if info == nil {
						t.Errorf("%s is missing", id)
						continue
					}
					want := true
					if info.SupportConfigurationUpdate != want {
						t.Errorf("%s configuration_update = %v, want %v", id, info.SupportConfigurationUpdate, want)
					}
					if cloned := cloneModelInfo(info); cloned.SupportConfigurationUpdate != want {
						t.Errorf(
							"%s clone configuration_update = %v, want %v", id, cloned.SupportConfigurationUpdate, want,
						)
					}
					raw, err := json.Marshal(info)
					if err != nil {
						t.Fatal(err)
					}
					if strings.Contains(string(raw), "support_configuration_update") ||
						strings.Contains(string(raw), "SupportConfigurationUpdate") {
						t.Errorf("%s exposed configuration_update capability: %s", id, raw)
					}
				}
			},
		)
	}
	if retired := LookupStaticModelInfoByChannel(
		"gpt-5.5", "codex",
	); retired == nil || retired.SupportConfigurationUpdate {
		t.Fatalf("retired GPT 5.5 exact lookup = %+v", retired)
	}
	for _, test := range []struct {
		name, raw string
		want      bool
	}{
		{name: "true", raw: `{"id":"test","support_configuration_update":true}`, want: true},
		{name: "false", raw: `{"id":"test","support_configuration_update":false}`},
		{name: "absent", raw: `{"id":"test"}`},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				info := ModelInfo{SupportConfigurationUpdate: true}
				if err := json.Unmarshal([]byte(test.raw), &info); err != nil {
					t.Fatal(err)
				}
				if info.SupportConfigurationUpdate != test.want {
					t.Errorf("capability = %v, want %v", info.SupportConfigurationUpdate, test.want)
				}
			},
		)
	}
}

func TestGetStaticModelDefinitionsByChannelSupportsGeminiInteractions(t *testing.T) {
	if models := GetStaticModelDefinitionsByChannel("gemini-interactions"); len(models) == 0 {
		t.Fatal("GetStaticModelDefinitionsByChannel(gemini-interactions) returned no models")
	}
}

func TestGetStaticModelDefinitionsByChannelSupportsKimiAndKimiAI(t *testing.T) {
	for _, channel := range []string{"kimi", "kimi-ai", "kimi.ai", "kimi.com"} {
		if models := GetStaticModelDefinitionsByChannel(channel); len(models) == 0 {
			t.Fatalf("GetStaticModelDefinitionsByChannel(%s) returned no models", channel)
		}
	}
}

func TestGeminiVertexModelsUseFlashLiteReleaseID(t *testing.T) {
	const releaseID = "gemini-3.1-flash-lite"
	const previewID = releaseID + "-preview"
	for _, model := range GetGeminiVertexModels() {
		if model == nil {
			continue
		}
		if model.ID == previewID {
			t.Fatalf("Vertex model ID = %q, want release ID %q", model.ID, releaseID)
		}
		if model.ID == releaseID {
			return
		}
	}
	t.Fatalf("Vertex models do not contain %q", releaseID)
}

func TestWithXAIBuiltinsIncludesImage20(t *testing.T) {
	for _, model := range withXAIBuiltins(nil) {
		if model != nil && model.ID == xaiBuiltinImage20ModelID {
			if model.Created != 1786060800 {
				t.Fatalf("created = %d, want 1786060800 (2026-08-07)", model.Created)
			}
			return
		}
	}
	t.Fatalf("expected xAI builtin model %s", xaiBuiltinImage20ModelID)
}

func TestAntigravityWebSearchModelForRequiresRequestedModelCapability(t *testing.T) {
	r := GetGlobalRegistry()
	r.RegisterClient(
		"test-antigravity-websearch-route", "antigravity", []*ModelInfo{
			{ID: "gemini-route-test"},
			{ID: "gemini-web-search-test", SupportsWebSearch: true},
		},
	)
	r.RegisterClient(
		"test-gemini-websearch-route", "gemini", []*ModelInfo{
			{ID: "gemini-cross-provider-route"},
			{ID: "gemini-cross-provider-search", SupportsWebSearch: true},
		},
	)
	t.Cleanup(
		func() {
			r.UnregisterClient("test-antigravity-websearch-route")
			r.UnregisterClient("test-gemini-websearch-route")
		},
	)
	if got := AntigravityWebSearchModelFor("gemini-route-test"); got != "" {
		t.Fatalf("route model without web search support got fallback model %q", got)
	}
	if got := AntigravityWebSearchModelFor("gemini-route-test(high)"); got != "" {
		t.Fatalf("suffix route model without web search support got fallback model %q", got)
	}
	if got := AntigravityWebSearchModelFor("gemini-web-search-test"); got != "gemini-web-search-test" {
		t.Fatalf("capable model = %q, want itself", got)
	}
	if got := AntigravityWebSearchModelFor("gemini-cross-provider-route"); got != "" {
		t.Fatalf("cross-provider model got Antigravity web search model %q", got)
	}
	if got := AntigravityWebSearchModelFor("unknown-model"); got != "" {
		t.Fatalf("unknown model got Antigravity web search model %q", got)
	}
}

func TestValidateModelsCatalog_Meta(t *testing.T) {
	valid := &staticModelsJSON{Meta: []*ModelInfo{{ID: "muse-spark-1.3"}}}
	if err := validateModelsCatalog(valid); err != nil {
		t.Fatalf("expected valid Meta catalog to pass, got: %v", err)
	}
	withNull := &staticModelsJSON{Meta: []*ModelInfo{nil}}
	if err := validateModelsCatalog(withNull); err == nil {
		t.Fatal("expected error for Meta section with null model")
	}
	withEmptyID := &staticModelsJSON{Meta: []*ModelInfo{{ID: " "}}}
	if err := validateModelsCatalog(withEmptyID); err == nil {
		t.Fatal("expected error for Meta section with empty model id")
	}
	withDuplicate := &staticModelsJSON{Meta: []*ModelInfo{{ID: "muse-spark-1.3"}, {ID: "muse-spark-1.3"}}}
	if err := validateModelsCatalog(withDuplicate); err == nil {
		t.Fatal("expected error for Meta section with duplicate model id")
	}
}

func TestWithCodexBuiltinsIncludesAdoptedImage25Metadata(t *testing.T) {
	models := withCodexBuiltins(nil)
	for id, displayName := range map[string]string{
		"gpt-image-2.5-flare":               "GPT Image 2.5 Flare",
		"gpt-image-2.5-flare-2026-09-08":    "GPT Image 2.5 Flare",
		"gpt-image-2.5-sunburst":            "GPT Image 2.5 Sunburst",
		"gpt-image-2.5-sunburst-2026-09-08": "GPT Image 2.5 Sunburst",
	} {
		model := findModelByID(models, id)
		if model == nil {
			t.Fatalf("expected builtin model %s", id)
		}
		if model.DisplayName != displayName || model.Object != "model" || model.OwnedBy != "openai" ||
			model.Type != "openai" || model.Version != id || model.Created != 1788825600 {
			t.Fatalf("model %s metadata = %#v", id, model)
		}
	}
}

func TestGetDevinModelsFallback(t *testing.T) {
	models := GetDevinModels()
	if len(models) == 0 {
		t.Fatal("GetDevinModels() returned empty list")
	}
	found := make(map[string]*ModelInfo)
	for _, model := range models {
		if model != nil {
			found[model.ID] = model
		}
	}
	for _, test := range []struct {
		id, owner string
	}{
		{id: "devin/swe-2"},
		{id: "devin/claude-fable-5-1"},
		{id: "devin/gemini-3-8-flash", owner: "google"},
		{id: "devin/grok-4-6", owner: "xai"},
		{id: "devin/deepseek-v4-flash", owner: "deepseek"},
		{id: "devin/deepseek-v4-1-flash-1017", owner: "deepseek"},
	} {
		model := found[test.id]
		if model == nil {
			t.Errorf("expected %s in GetDevinModels()", test.id)
			continue
		}
		if test.id == "devin/swe-2" && model.Type != "devin" {
			t.Errorf("devin/swe-2 Type = %q, want devin", model.Type)
		}
		if test.owner != "" && model.OwnedBy != test.owner {
			t.Errorf("%s OwnedBy = %q, want %q", test.id, model.OwnedBy, test.owner)
		}
	}
	if models := GetStaticModelDefinitionsByChannel("devin"); len(models) == 0 {
		t.Fatal("GetStaticModelDefinitionsByChannel(devin) returned empty list")
	}
	info := LookupStaticModelInfo("devin/swe-2")
	if info == nil {
		t.Fatal("LookupStaticModelInfo(devin/swe-2) = nil")
	}
	if info.DisplayName != "SWE-2" {
		t.Errorf("info.DisplayName = %q, want SWE-2", info.DisplayName)
	}
}
