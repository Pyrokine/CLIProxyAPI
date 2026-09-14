package registry

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestEmbeddedCodexClientModelsCatalogIsValid(t *testing.T) {
	data, revision := GetCodexClientModelsSnapshot()
	if revision == 0 {
		t.Fatal("embedded Codex client model catalog revision = 0, want non-zero")
	}
	if err := validateCodexClientModelsJSON(data); err != nil {
		t.Fatalf("embedded Codex client model catalog is invalid: %v", err)
	}

	data[0] ^= 0xff
	second, secondRevision := GetCodexClientModelsSnapshot()
	if secondRevision != revision {
		t.Fatalf("snapshot revision = %d, want %d", secondRevision, revision)
	}
	if err := validateCodexClientModelsJSON(second); err != nil {
		t.Fatalf("mutating returned snapshot changed stored catalog: %v", err)
	}
}

func TestLoadCodexClientModelsRejectsInvalidWithoutReplacing(t *testing.T) {
	original, _ := GetCodexClientModelsSnapshot()
	t.Cleanup(
		func() {
			if _, err := loadCodexClientModelsFromBytes(original, "test cleanup"); err != nil {
				t.Fatalf("restore original catalog: %v", err)
			}
		},
	)

	valid := testCodexClientCatalog(t, testCodexClientModel("gpt-5.5", 1))
	changed, err := loadCodexClientModelsFromBytes(valid, "test")
	if err != nil {
		t.Fatalf("load valid catalog: %v", err)
	}
	if !changed {
		t.Fatal("load valid catalog changed = false, want true")
	}
	beforeInvalid, revision := GetCodexClientModelsSnapshot()

	if _, err = loadCodexClientModelsFromBytes([]byte(`{"models":[]}`), "test invalid"); err == nil {
		t.Fatal("load invalid catalog error = nil, want error")
	}
	afterInvalid, afterRevision := GetCodexClientModelsSnapshot()
	if string(afterInvalid) != string(beforeInvalid) {
		t.Fatal("invalid catalog replaced current snapshot")
	}
	if afterRevision != revision {
		t.Fatalf("revision after invalid catalog = %d, want %d", afterRevision, revision)
	}
}

func TestFetchCodexClientModelsFallsBackToNextURL(t *testing.T) {
	invalidServer := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"models":[{"slug":"gpt-5.6-sol"}]}`))
			},
		),
	)
	defer invalidServer.Close()

	validCatalog := testCodexClientCatalog(t, testCodexClientModel("gpt-5.5", 1))
	validServer := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Errorf("method = %s, want GET", r.Method)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(validCatalog)
			},
		),
	)
	defer validServer.Close()

	previousURLs := codexClientModelsURLs
	codexClientModelsURLs = []string{invalidServer.URL, validServer.URL}
	t.Cleanup(func() { codexClientModelsURLs = previousURLs })

	data, sourceURL := fetchCodexClientModelsFromRemote(context.Background())
	if sourceURL != validServer.URL {
		t.Fatalf("source URL = %q, want %q", sourceURL, validServer.URL)
	}
	if string(data) != string(validCatalog) {
		t.Fatalf("catalog = %s, want %s", data, validCatalog)
	}
}

func TestEmbeddedCodexClientModelsIncludeGPT6Astra(t *testing.T) {
	var payload codexClientModelsPayload
	if err := json.Unmarshal(GetCodexClientModelsJSON(), &payload); err != nil {
		t.Fatalf("decode embedded catalog: %v", err)
	}
	var astra map[string]any
	for _, model := range payload.Models {
		if slug, _ := model["slug"].(string); slug == "gpt-6-astra" {
			astra = model
			break
		}
	}
	if astra == nil {
		t.Fatal("gpt-6-astra is missing from the Codex client manifest")
	}
	if astra["display_name"] != "GPT-6-Astra" || astra["minimal_client_version"] != "0.153.0" {
		t.Fatalf("Astra client identity = (%v, %v)", astra["display_name"], astra["minimal_client_version"])
	}
	if astra["context_window"] != float64(272000) || astra["max_context_window"] != float64(872000) ||
		astra["priority"] != float64(1) {
		t.Fatalf(
			"Astra client limits = (%v, %v), priority=%v",
			astra["context_window"],
			astra["max_context_window"],
			astra["priority"],
		)
	}
	if astra["default_reasoning_level"] != "medium" || astra["supported_in_api"] != true {
		t.Fatalf(
			"Astra client defaults = reasoning %v, supported_in_api %v",
			astra["default_reasoning_level"],
			astra["supported_in_api"],
		)
	}
	levels, ok := astra["supported_reasoning_levels"].([]any)
	if !ok {
		t.Fatalf("Astra reasoning levels = %T", astra["supported_reasoning_levels"])
	}
	wantLevels := []string{"low", "medium", "high", "xhigh", "max", "ultra"}
	if len(levels) != len(wantLevels) {
		t.Fatalf("Astra reasoning level count = %d, want %d", len(levels), len(wantLevels))
	}
	for i, want := range wantLevels {
		level, okLevel := levels[i].(map[string]any)
		if !okLevel || level["effort"] != want {
			t.Fatalf("Astra reasoning level %d = %#v, want %q", i, levels[i], want)
		}
	}
	baseInstructions, ok := astra["base_instructions"].(string)
	modelMessages, okMessages := astra["model_messages"].(map[string]any)
	if !ok || baseInstructions == "" || !okMessages || modelMessages["instructions_template"] != baseInstructions {
		t.Fatal("Astra client instructions are incomplete or inconsistent")
	}
}

func TestEmbeddedCodexClientModelsExcludeObsoleteFields(t *testing.T) {
	var payload codexClientModelsPayload
	if err := json.Unmarshal(GetCodexClientModelsJSON(), &payload); err != nil {
		t.Fatalf("decode embedded catalog: %v", err)
	}
	for _, model := range payload.Models {
		for _, field := range []string{"instructions_variables", "approvals"} {
			if _, ok := model[field]; ok {
				t.Fatalf("model %q contains obsolete field %q", model["slug"], field)
			}
		}
	}
}

func testCodexClientModel(slug string, priority int) map[string]any {
	return map[string]any{
		"slug":                       slug,
		"display_name":               "Test " + slug,
		"description":                "Test model",
		"base_instructions":          "Test instructions",
		"minimal_client_version":     "0.144.0",
		"visibility":                 "list",
		"context_window":             372000,
		"max_context_window":         372000,
		"priority":                   priority,
		"default_reasoning_level":    "medium",
		"supported_reasoning_levels": []map[string]any{{"effort": "medium", "description": "Balanced"}},
	}
}

func testCodexClientCatalog(t *testing.T, models ...map[string]any) []byte {
	t.Helper()
	data, err := json.Marshal(map[string]any{"models": models})
	if err != nil {
		t.Fatalf("marshal test Codex client catalog: %v", err)
	}
	return data
}
