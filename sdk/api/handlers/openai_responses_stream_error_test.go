package handlers

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestBuildOpenAIResponsesStreamErrorChunk(t *testing.T) {
	chunk := BuildOpenAIResponsesStreamErrorChunk(http.StatusInternalServerError, "unexpected EOF", 0)
	var payload map[string]any
	if err := json.Unmarshal(chunk, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload["type"] != "error" {
		t.Fatalf("type = %v, want %q", payload["type"], "error")
	}
	if payload["code"] != "internal_server_error" {
		t.Fatalf("code = %v, want %q", payload["code"], "internal_server_error")
	}
	if payload["message"] != "unexpected EOF" {
		t.Fatalf("message = %v, want %q", payload["message"], "unexpected EOF")
	}
	if payload["sequence_number"] != float64(0) {
		t.Fatalf("sequence_number = %v, want %v", payload["sequence_number"], 0)
	}
}

func TestBuildOpenAIResponsesStreamErrorChunkExtractsHTTPErrorBody(t *testing.T) {
	chunk := BuildOpenAIResponsesStreamErrorChunk(
		http.StatusInternalServerError,
		`{"error":{"message":"oops","type":"server_error","code":"internal_server_error"}}`,
		0,
	)
	var payload map[string]any
	if err := json.Unmarshal(chunk, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload["type"] != "error" {
		t.Fatalf("type = %v, want %q", payload["type"], "error")
	}
	if payload["code"] != "internal_server_error" {
		t.Fatalf("code = %v, want %q", payload["code"], "internal_server_error")
	}
	if payload["message"] != "oops" {
		t.Fatalf("message = %v, want %q", payload["message"], "oops")
	}
}

func TestBuildOpenAIResponsesStreamFailedChunkPreservesNestedError(t *testing.T) {
	chunk := BuildOpenAIResponsesStreamFailedChunk(
		http.StatusBadRequest,
		`{"error":{"type":"invalid_request","code":"cyber_policy","message":"blocked","param":null}}`,
		0,
	)

	var payload struct {
		Type           string `json:"type"`
		SequenceNumber int    `json:"sequence_number"`
		Response       struct {
			Status string         `json:"status"`
			Error  map[string]any `json:"error"`
		} `json:"response"`
	}
	if err := json.Unmarshal(chunk, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.Type != "response.failed" {
		t.Fatalf("type = %q, want response.failed", payload.Type)
	}
	if payload.SequenceNumber != 0 {
		t.Fatalf("sequence_number = %d, want 0", payload.SequenceNumber)
	}
	if payload.Response.Status != "failed" {
		t.Fatalf("response.status = %q, want failed", payload.Response.Status)
	}
	if payload.Response.Error["type"] != "invalid_request" ||
		payload.Response.Error["code"] != "cyber_policy" ||
		payload.Response.Error["message"] != "blocked" {
		t.Fatalf("response.error = %#v, want preserved nested error", payload.Response.Error)
	}
	if value, exists := payload.Response.Error["param"]; !exists || value != nil {
		t.Fatalf("response.error.param = %#v, exists=%v; want explicit null", value, exists)
	}
}
