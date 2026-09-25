package openai

import (
	"reflect"
	"testing"

	codexmodels "github.com/Pyrokine/CLIProxyAPI/v6/internal/client/codex/models"
)

func TestCodexClientModelsResponseDelegatesToBuilder(t *testing.T) {
	models := []map[string]any{{"id": "gpt-5.6-sol"}}
	got := CodexClientModelsResponse(models)
	want := codexmodels.BuildResponse(models, nil, false)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("response = %#v, want %#v", got, want)
	}
}

func TestCodexClientModelsResponseForClientDelegatesVersion(t *testing.T) {
	models := []map[string]any{{"id": "gpt-5.6-sol"}}
	got := CodexClientModelsResponseForClient(models, "0.143.9", true)
	want := codexmodels.BuildResponseForClient(models, nil, true, "0.143.9")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("response = %#v, want %#v", got, want)
	}
}

func TestCodexClientModelsResponseWithCapabilitiesDelegates(t *testing.T) {
	models := []map[string]any{{"id": "gpt-5.6-sol"}}
	providersForModel := func(string) []string { return []string{"codex"} }
	webSearchCapabilityForModel := func(string) *bool {
		supported := true
		return &supported
	}
	got := codexClientModelsResponseWithCPACapabilities(
		models,
		providersForModel,
		webSearchCapabilityForModel,
		true,
		"cpa",
	)
	want := codexmodels.BuildResponseForClientWithCPACapabilities(
		models,
		providersForModel,
		webSearchCapabilityForModel,
		true,
		"cpa",
	)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("response = %#v, want %#v", got, want)
	}
}
