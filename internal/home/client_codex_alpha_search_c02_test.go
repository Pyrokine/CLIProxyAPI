package home

import (
	"encoding/json"
	"testing"
)

func TestAuthDispatchRequestIncludesCredentialPolicy(t *testing.T) {
	request := newAuthDispatchRequest(
		"gpt-5.6-sol",
		"session-c02",
		nil,
		1,
		"codex_alpha_search_v1",
	)
	raw, errMarshal := json.Marshal(request)
	if errMarshal != nil {
		t.Fatalf("marshal auth dispatch request: %v", errMarshal)
	}
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(raw, &payload); errUnmarshal != nil {
		t.Fatalf("unmarshal auth dispatch request: %v", errUnmarshal)
	}
	if payload["credential_policy"] != "codex_alpha_search_v1" {
		t.Fatalf("credential_policy = %#v", payload["credential_policy"])
	}
}

func TestAuthDispatchRequestOmitsEmptyCredentialPolicy(t *testing.T) {
	request := newAuthDispatchRequest("gpt-5.6-sol", "", nil, 1)
	raw, errMarshal := json.Marshal(request)
	if errMarshal != nil {
		t.Fatalf("marshal auth dispatch request: %v", errMarshal)
	}
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(raw, &payload); errUnmarshal != nil {
		t.Fatalf("unmarshal auth dispatch request: %v", errUnmarshal)
	}
	if _, exists := payload["credential_policy"]; exists {
		t.Fatalf("empty credential_policy was serialized: %s", raw)
	}
}
