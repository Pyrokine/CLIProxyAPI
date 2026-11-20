package handlers_test

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
)

func TestPublishedProtocolExecutionModeValidation(t *testing.T) {
	handler := handlers.NewBaseAPIHandlers(nil, nil)
	req := handlers.ProtocolExecutionRequest{
		EntryProtocol: "openai", ExitProtocol: "openai-response", ForcedProvider: "codex",
		AuthSelectionModel: "synthetic-selection", Model: "synthetic-model", Stream: true,
		Body: []byte(`{"model":"synthetic-model"}`), Headers: http.Header{"X-Fixture": {"value"}},
		Query: url.Values{"fixture": {"value"}}, Alt: "sse", Path: "/v1/responses",
	}
	if _, err := handler.ExecuteProtocolWithAuthManager(
		context.Background(), req,
	); err == nil || err.StatusCode != http.StatusBadRequest {
		t.Fatalf("non-stream mode validation: %v", err)
	}
	req.Stream = false
	if _, err := handler.ExecuteProtocolStreamWithAuthManager(
		context.Background(), req,
	); err == nil || err.StatusCode != http.StatusBadRequest {
		t.Fatalf("stream mode validation: %v", err)
	}
	var streamError *handlers.ModelExecutionStreamError
	if streamError.Error() != "" {
		t.Fatal("nil public stream error changed")
	}
}
