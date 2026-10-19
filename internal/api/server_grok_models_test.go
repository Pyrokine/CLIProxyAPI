package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/client/grokbuild"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/modelcatalog"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
)

func TestModelsDispatchByGrokShellUserAgent(t *testing.T) {
	modelRegistry := registry.GetGlobalRegistry()
	clientID := "test-grok-shell-model-list"
	modelRegistry.RegisterClient(
		clientID, "openai", []*registry.ModelInfo{
			{
				ID:            "grok-shell-model",
				DisplayName:   "Grok Shell Model",
				ContextLength: 256000,
				Thinking:      &registry.ThinkingSupport{Levels: []string{"high"}},
			},
		},
	)
	t.Cleanup(func() { modelRegistry.UnregisterClient(clientID) })

	server := newTestServer(t)
	request := httptest.NewRequest(http.MethodGet, "/v1/models?client_version=0.2.120", nil)
	request.Header.Set("Authorization", "Bearer test-key")
	request.Header.Set("User-Agent", "grok-shell/0.2.120 (linux; amd64)")
	recorder := httptest.NewRecorder()
	server.engine.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response grokbuild.Response
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v; body=%s", err, recorder.Body.String())
	}
	if response.Object != "list" {
		t.Fatalf("object = %q, want list", response.Object)
	}
	for _, model := range response.Data {
		if model.ID != "grok-shell-model" {
			continue
		}
		if model.Model != model.ID || model.Name != "Grok Shell Model" || model.ContextWindow != 256000 {
			t.Fatalf("model mapping = %#v", model)
		}
		if model.APIBackend != "responses" || !model.SupportedInAPI {
			t.Fatalf("routing metadata = %#v", model)
		}
		if len(model.ReasoningEfforts) != 1 || model.ReasoningEfforts[0].Value != "high" {
			t.Fatalf("reasoning efforts = %#v", model.ReasoningEfforts)
		}
		return
	}
	t.Fatalf("registered Grok Shell model missing: %s", recorder.Body.String())
}

func TestModelsDispatchKeepsOrdinaryOpenAIResponse(t *testing.T) {
	modelRegistry := registry.GetGlobalRegistry()
	clientID := "test-ordinary-model-list-after-grok"
	modelRegistry.RegisterClient(clientID, "openai", []*registry.ModelInfo{{ID: "ordinary-model"}})
	t.Cleanup(func() { modelRegistry.UnregisterClient(clientID) })

	server := newTestServer(t)
	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	request.Header.Set("Authorization", "Bearer test-key")
	request.Header.Set("User-Agent", "curl/8.7.1")
	recorder := httptest.NewRecorder()
	server.engine.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	found := false
	for _, model := range response.Data {
		if _, exists := model["api_backend"]; exists {
			t.Fatalf("ordinary response contains Grok field: %#v", model)
		}
		if model["id"] == "ordinary-model" {
			found = true
		}
	}
	if !found {
		t.Fatalf("registered ordinary model missing: %s", recorder.Body.String())
	}
}

func TestHomeGrokCatalogPreservesReasoningEfforts(t *testing.T) {
	catalog, err := modelcatalog.DecodeHomeCatalog([]byte(`{"x-ai":[{"id":"grok-4.7"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	entries := homeModelEntries(catalog)
	models := grokModelsFromHomeEntries(entries)
	response := grokbuild.BuildResponse(models)
	want := []string{"low", "medium", "high", "xhigh"}
	if len(response.Data) != 1 || response.Data[0].ID != "grok-4.7" ||
		len(response.Data[0].ReasoningEfforts) != len(want) {
		t.Fatalf("Home Grok response = %#v", response)
	}
	for index, effort := range response.Data[0].ReasoningEfforts {
		if effort.Value != want[index] {
			t.Fatalf("effort %d = %q, want %q", index, effort.Value, want[index])
		}
	}
	models[0].ReasoningLevels[0] = "changed"
	if entries[0].reasoningLevels[0] != want[0] ||
		homeModelEntries(catalog)[0].reasoningLevels[0] != want[0] {
		t.Fatal("Grok response mutated Home catalog reasoning levels")
	}
}

func TestGrokModelsFromHomeEntriesPreservesContextWindow(t *testing.T) {
	models := grokModelsFromHomeEntries(
		[]homeModelEntry{
			{
				id:            "home-grok",
				displayName:   "Home Grok",
				contextLength: 123456,
			},
		},
	)
	if len(models) != 1 {
		t.Fatalf("model count = %d, want 1", len(models))
	}
	if models[0].ID != "home-grok" || models[0].DisplayName != "Home Grok" || models[0].ContextLength != 123456 {
		t.Fatalf("home model mapping = %#v", models[0])
	}
}
