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

func TestBuildOpenAIResponsesStreamFailedChunkTimeoutIsRetryable(t *testing.T) {
	for _, tt := range []struct {
		status   int
		wantType string
		wantCode string
	}{
		{http.StatusRequestTimeout, "server_error", "request_timeout"},
		{http.StatusBadRequest, "invalid_request_error", "invalid_request_error"},
		{http.StatusBadGateway, "server_error", "internal_server_error"},
	} {
		t.Run(http.StatusText(tt.status), func(t *testing.T) {
			chunk := BuildOpenAIResponsesStreamFailedChunk(tt.status, "connection closed", 0)
			var payload struct {
				Response struct {
					Error struct {
						Type string `json:"type"`
						Code string `json:"code"`
					} `json:"error"`
				} `json:"response"`
			}
			if err := json.Unmarshal(chunk, &payload); err != nil {
				t.Fatal(err)
			}
			if payload.Response.Error.Type != tt.wantType || payload.Response.Error.Code != tt.wantCode {
				t.Fatalf("response.error = %+v, want type %q code %q", payload.Response.Error, tt.wantType, tt.wantCode)
			}
		})
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

func TestResponsesStreamErrorSequenceNumberPreservesIntegerMetadata(t *testing.T) {
	for _, builder := range []struct {
		name  string
		build func(int, string, int) []byte
	}{
		{"error", BuildOpenAIResponsesStreamErrorChunk},
		{"failed", BuildOpenAIResponsesStreamFailedChunk},
	} {
		t.Run(builder.name, func(t *testing.T) {
			for _, test := range []struct {
				name, body string
				caller     int
				want       int64
			}{
				{"nested HTTP", `{"error":{"code":"custom","message":"blocked"},"sequence_number":9}`, 0, 9},
				{"typed error", `{"type":"error","code":"custom","message":"blocked","sequence_number":17}`, 0, 17},
				{"failed response", `{"type":"response.failed","sequence_number":12,"response":{"error":{"message":"blocked"}}}`, 0, 12},
				{"large integer", `{"type":"error","message":"blocked","sequence_number":9007199254740993}`, 0, 9007199254740993},
				{"caller precedence", `{"error":{"message":"blocked"},"sequence_number":9}`, 7, 7},
				{"negative caller", `{"error":{"message":"blocked"},"sequence_number":9}`, -1, 9},
				{"negative upstream", `{"type":"error","message":"blocked","sequence_number":-2}`, 0, 0},
				{"fractional upstream", `{"type":"error","message":"blocked","sequence_number":1.5}`, 0, 0},
				{"out of range", `{"type":"error","message":"blocked","sequence_number":9223372036854775808}`, 0, 0},
				{"string upstream", `{"type":"error","message":"blocked","sequence_number":"9"}`, 0, 0},
			} {
				t.Run(test.name, func(t *testing.T) {
					want := test.want
					if int64(int(want)) != want {
						want = 0
					}
					body := builder.build(http.StatusBadRequest, test.body, test.caller)
					var payload struct {
						SequenceNumber int64 `json:"sequence_number"`
					}
					if err := json.Unmarshal(body, &payload); err != nil {
						t.Fatal(err)
					}
					if payload.SequenceNumber != want {
						t.Fatalf("sequence_number = %d, want %d; body=%s", payload.SequenceNumber, want, body)
					}
				})
			}
		})
	}
}

func TestResponsesStreamErrorPreservesIntegerDetails(t *testing.T) {
	body := BuildOpenAIResponsesStreamErrorChunk(http.StatusBadRequest,
		`{"type":"error","message":"limit","code":9007199254740993}`, 0)
	var ordinary struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(body, &ordinary); err != nil {
		t.Fatal(err)
	}
	if ordinary.Code != "9007199254740993" {
		t.Fatalf("error code = %q, want exact integer; body=%s", ordinary.Code, body)
	}
	body = BuildOpenAIResponsesStreamFailedChunk(http.StatusBadRequest,
		`{"error":{"type":"invalid_request_error","message":"limit","code":9007199254740993,"retry_after_ms":9007199254740993,"param":null}}`, 0)
	var failed struct {
		Response struct {
			Error map[string]json.RawMessage `json:"error"`
		} `json:"response"`
	}
	if err := json.Unmarshal(body, &failed); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"code", "retry_after_ms"} {
		if got := string(failed.Response.Error[key]); got != "9007199254740993" {
			t.Errorf("response.error.%s = %s, want exact integer; body=%s", key, got, body)
		}
	}
	if got, ok := failed.Response.Error["param"]; !ok || string(got) != "null" {
		t.Errorf("explicit null param = %s, present=%t; body=%s", got, ok, body)
	}
}
