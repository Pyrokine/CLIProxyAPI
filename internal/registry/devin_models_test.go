package registry

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestEmbeddedDevinModelsAreNamespacedAndUsable(t *testing.T) {
	models := GetDevinModels()
	if len(models) != 57 {
		t.Fatalf("GetDevinModels() count = %d, want 57", len(models))
	}
	for _, model := range models {
		if model == nil || !strings.HasPrefix(model.ID, "devin/") {
			t.Fatalf("model is not namespaced: %+v", model)
		}
		if model.Type != "devin" || model.ContextLength <= 0 || model.MaxCompletionTokens <= 0 {
			t.Fatalf("invalid model metadata: %+v", model)
		}
	}
	for _, id := range []string{
		"gpt-5-6-sol", "claude-opus-5-5", "gpt-6-sol", "gpt-6-luna", "swe-2-lightning",
		"glm-5-2-1m", "grok-4-7", "claude-opus-4-6-1m", "claude-sonnet-4-6-1m",
		"gemini-4-carbon", "gemini-4-argon", "claude-fable-5-2", "claude-haiku-5-5", "claude-sonnet-5-5",
		"gpt-6-1-sol", "gpt-6-1-terra", "kimi-k3-1-pro", "muse-wonder-1-0", "swe-2-flash",
		"deepseek-v4-1-flash-1017", "nemotron-3-5-super", "swe-1-6-slow",
	} {
		if model := LookupDevinModel(id); model == nil || model.ID != "devin/"+id {
			t.Fatalf("LookupDevinModel(%q) = %+v", id, model)
		}
	}
	if model := LookupStaticModelInfo(devinBuiltinSWE16SlowID); model == nil || model.ID != devinBuiltinSWE16SlowID {
		t.Fatalf("LookupStaticModelInfo(%q) = %+v, want built-in", devinBuiltinSWE16SlowID, model)
	}
	if model := LookupStaticModelInfo("gpt-5-6-sol"); model != nil {
		t.Fatalf("bare Devin model unexpectedly leaked into global static lookup: %+v", model)
	}
	if model := LookupStaticModelInfo("devin/gpt-5-6-sol"); model == nil || model.Type != "devin" {
		t.Fatalf("namespaced Devin lookup = %+v", model)
	}
}

func TestEmbeddedDevinPublicModelsUseUpstreamCompletionCap(t *testing.T) {
	models, err := validateDevinModelsJSON(embeddedDevinModelsJSON)
	if err != nil {
		t.Fatal(err)
	}
	legacyAliases := map[string]bool{
		"devin/claude-haiku-4-5": true, "devin/claude-sonnet-4-5": true,
		"devin/gpt-4-1": true, "devin/gemini-3-flash": true,
	}
	checked := 0
	for _, model := range models {
		if legacyAliases[model.ID] {
			continue
		}
		checked++
		if model.MaxCompletionTokens != 64000 {
			t.Errorf("%s max completion tokens = %d, want 64000", model.ID, model.MaxCompletionTokens)
		}
	}
	if checked != 52 {
		t.Fatalf("checked %d upstream public models, want 52", checked)
	}
}

func TestValidateDevinModelsJSONNormalizesWithoutMutatingInput(t *testing.T) {
	input := `{"models":[{"id":"TEST.MODEL","type":"openai","context_length":4096,"m` +
		`ax_completion_tokens":1024,"thinking":{"levels":[" HIGH ","low"]}}]}`
	models, err := validateDevinModelsJSON([]byte(input))
	if err != nil {
		t.Fatalf("ValidateDevinModelsJSON(): %v", err)
	}
	if len(models) != 1 {
		t.Fatalf("model count = %d, want 1", len(models))
	}
	model := models[0]
	if model.ID != "devin/test.model" || model.Type != "devin" || model.Object != "model" {
		t.Fatalf("normalized identity = %+v", model)
	}
	if model.InputTokenLimit != 4096 || model.OutputTokenLimit != 1024 ||
		len(model.Thinking.Levels) != 2 || model.Thinking.Levels[0] != "low" || model.Thinking.Levels[1] != "high" {
		t.Fatalf("normalized metadata = %+v", model)
	}

	candidate := &ModelInfo{
		ID: "Original", Type: "openai", ContextLength: 10, MaxCompletionTokens: 5,
		Thinking: &ThinkingSupport{Levels: []string{"HIGH"}},
	}
	if _, err = sanitizeAndValidateDevinModels([]*ModelInfo{candidate}); err != nil {
		t.Fatalf("sanitizeAndValidateDevinModels(): %v", err)
	}
	if candidate.ID != "Original" || candidate.Type != "openai" || candidate.Thinking.Levels[0] != "HIGH" {
		t.Fatalf("source candidate mutated: %+v", candidate)
	}
}

func TestValidateDevinModelsJSONRejectsInvalidCatalogs(t *testing.T) {
	tests := []string{
		``,
		`{"devin":[]}`,
		`{"devin":[null]}`,
		`{"devin":[{"id":"openai/model","context_length":1,"max_completion_tokens":1}]}`,
		`{"devin":[{"id":"duplicate","context_length":1,"max_completion_tokens":1},{"id":"DEVIN/DUPLICATE","` +
			`context_length":1,"max_completion_tokens":1}]}`,
		`{"devin":[{"id":"negative-context","context_length":-1,"max_completion_tokens":1}]}`,
		`{"devin":[{"id":"negative-output","context_length":1,"max_completion_tokens":-1}]}`,
		`{"devin":[{"id":"bad-level","context_length":1,"max_completion_tokens":1,"thinking":{"levels":["turbo"]}}]}`,
		`{"devin":[{"id":"duplicate-level","context_length":1,"max_completion_tokens":1,"thinking":{"levels":` +
			`["high","HIGH"]}}]}`,
	}
	for _, input := range tests {
		if _, err := validateDevinModelsJSON([]byte(input)); err == nil {
			t.Fatalf("ValidateDevinModelsJSON(%q) error = nil", input)
		}
	}
}

func TestLoadDevinModelsUpdatesRevisionAndReturnsCopies(t *testing.T) {
	devinCatalogStore.mu.Lock()
	originalModels := cloneModelInfos(devinCatalogStore.models)
	originalRaw := append([]byte(nil), devinCatalogStore.rawJSON...)
	originalRevision := devinCatalogStore.revision
	devinCatalogStore.mu.Unlock()
	t.Cleanup(
		func() {
			devinCatalogStore.mu.Lock()
			devinCatalogStore.models = originalModels
			devinCatalogStore.rawJSON = originalRaw
			devinCatalogStore.revision = originalRevision
			devinCatalogStore.mu.Unlock()
		},
	)

	data := []byte(`{"devin":[{"id":"revision-test","context_length":100,"max_completion_tokens":20}]}`)
	changed, err := loadDevinModelsFromBytes(data, "test")
	if err != nil || !changed {
		t.Fatalf("loadDevinModelsFromBytes() = %t/%v, want true/nil", changed, err)
	}
	_, revision := getDevinModelsSnapshot()
	if revision != originalRevision+1 {
		t.Fatalf("revision = %d, want %d", revision, originalRevision+1)
	}
	changed, err = loadDevinModelsFromBytes(data, "test")
	if err != nil || changed {
		t.Fatalf("second load = %t/%v, want false/nil", changed, err)
	}

	models := GetDevinModels()
	models[0].ID = "mutated"
	if got := LookupDevinModel("revision-test"); got == nil || got.ID != "devin/revision-test" {
		t.Fatalf("catalog copy mutation changed store: %+v", got)
	}
	if got := LookupDevinModel("swe-1-6-slow"); got == nil || got.ContextLength != 200000 {
		t.Fatalf("built-in model missing after dynamic update: %+v", got)
	}
}

func TestValidateDevinModelsJSONAggregatesThinkingVariants(t *testing.T) {
	payload := []byte(("{\"devin\":[\n\t\t{\"id\":\"claude-opus-5\",\"display_name\":\"Claude Opus 5\",\"" +
		"context_length\":200000,\"max_completion_tokens\":64000,\"thinking\":{\"levels\":[\"medium\"]}},\n\t" +
		"\t{\"id\":\"claude-opus-5-low-fast\",\"display_name\":\"Claude Opus 5 Low Fast\",\"context_length\":" +
		"1000000,\"max_completion_tokens\":64000,\"supportedInputModalities\":[\"text\",\"image\"]},\n\t\t{\"" +
		"id\":\"claude-opus-5-high-fast\",\"display_name\":\"Claude Opus 5 High Fast\",\"context_length\":" +
		"1000000,\"max_completion_tokens\":64000}\n\t]}"))
	models, err := validateDevinModelsJSON(payload)
	if err != nil {
		t.Fatalf("ValidateDevinModelsJSON() error = %v", err)
	}
	if len(models) != 1 || models[0].ID != "devin/claude-opus-5" || models[0].DisplayName != "Claude Opus 5" ||
		models[0].ContextLength != 1000000 || models[0].Thinking == nil ||
		strings.Join(models[0].Thinking.Levels, ",") != "low,medium,high" ||
		strings.Join(models[0].SupportedInputModalities, ",") != "text,image" {
		t.Fatalf("aggregated model = %+v", models)
	}
	if got := LookupDevinModel("claude-opus-5-low-fast"); got == nil || got.ID != "devin/claude-opus-5" {
		t.Fatalf("thinking variant lookup did not resolve base model: %+v", got)
	}
}

func TestDevinBuiltinsReplaceCatalogDuplicates(t *testing.T) {
	models := withDevinBuiltins([]*ModelInfo{{ID: devinBuiltinSWE16SlowID, DisplayName: "obsolete"}})
	if len(models) != 1 || models[0].ID != devinBuiltinSWE16SlowID ||
		models[0].DisplayName != "SWE-1.6 Slow" || models[0].MaxCompletionTokens != 64000 {
		t.Fatalf("built-in replacement = %+v", models)
	}
}

func TestFetchDevinModelsFromRemoteFallsBackAndBoundsPayload(t *testing.T) {
	validBody := `{"devin":[{"id":"remote-test","context_length":100,"max_completion_tokens":20}]}`
	failed := newCatalogTestServer(
		t, http.HandlerFunc(
			func(response http.ResponseWriter, _ *http.Request) {
				http.Error(response, "failed", http.StatusBadGateway)
			},
		),
	)
	defer failed.Close()
	valid := newCatalogTestServer(
		t, http.HandlerFunc(
			func(response http.ResponseWriter, _ *http.Request) {
				_, _ = response.Write([]byte(validBody))
			},
		),
	)
	defer valid.Close()

	originalURLs := devinModelsURLs
	devinModelsURLs = []string{failed.URL, valid.URL}
	t.Cleanup(func() { devinModelsURLs = originalURLs })
	data, source, err := fetchDevinModelsFromRemote(context.Background())
	if err != nil || string(data) != validBody || source != valid.URL {
		t.Fatalf("fetchDevinModelsFromRemote() = %q/%q/%v", data, source, err)
	}

	oversized := newCatalogTestServer(
		t, http.HandlerFunc(
			func(response http.ResponseWriter, _ *http.Request) {
				response.Header().Set("Content-Length", fmt.Sprintf("%d", maxDevinModelsSize+1))
				response.WriteHeader(http.StatusOK)
			},
		),
	)
	defer oversized.Close()
	devinModelsURLs = []string{oversized.URL}
	if _, _, err = fetchDevinModelsFromRemote(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "payload exceeds") {
		t.Fatalf("oversized fetch error = %v", err)
	}
}

func TestEmbeddedDevinPublishedAliasesRemainPublicOnly(t *testing.T) {
	model := LookupDevinModel("claude-sonnet-4-5")
	if model == nil || model.Thinking == nil || strings.Join(model.Thinking.Levels, ",") != "none,high" {
		t.Fatalf("Sonnet binary thinking capability = %#v", model)
	}
	for _, model := range GetDevinModels() {
		if strings.HasPrefix(strings.TrimPrefix(model.ID, "devin/"), "model_") {
			t.Errorf("internal UID leaked into public catalog: %s", model.ID)
		}
	}
}
