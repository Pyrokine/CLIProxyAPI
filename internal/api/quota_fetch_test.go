package api

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/quota"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestQuotaCredentialTypeForProviderAcceptsGeminiFileType(t *testing.T) {
	credType, ok := quotaCredentialTypeForProvider("gemini")
	if !ok {
		t.Fatalf("gemini provider was not accepted for quota registration")
	}
	if credType != quota.TypeGeminiCli {
		t.Fatalf("credential type = %q, want %q", credType, quota.TypeGeminiCli)
	}
}

func TestFetchAntigravityQuotaIncludesLoadCodeAssist(t *testing.T) {
	var requests []quota.APICallRequest
	body, err := fetchAntigravityQuota(
		func(req quota.APICallRequest) (*quota.APICallResponse, error) {
			requests = append(requests, req)
			switch req.URL {
			case geminiQuotaURL:
				return &quota.APICallResponse{StatusCode: 200, Body: json.RawMessage(`{"quota":"ok"}`)}, nil
			case antigravityLoadCodeAssistURL:
				return &quota.APICallResponse{
					StatusCode: 200, Body: json.RawMessage(`{"paidTier":{"id":"g1-ultra-tier","name":"Ultra"}}`),
				}, nil
			default:
				t.Fatalf("unexpected url: %s", req.URL)
				return nil, nil
			}
		},
		"antigravity-index",
	)
	if err != nil {
		t.Fatalf("fetchAntigravityQuota() error = %v", err)
	}
	if len(requests) != 2 {
		t.Fatalf("request count = %d, want 2", len(requests))
	}
	if requests[1].Data != `{"metadata":{"ideType":"ANTIGRAVITY"}}` {
		t.Fatalf("loadCodeAssist body = %q", requests[1].Data)
	}
	if requests[1].AuthIndex != "antigravity-index" {
		t.Fatalf("loadCodeAssist auth index = %q", requests[1].AuthIndex)
	}

	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(payload["quota"]) == 0 {
		t.Fatalf("missing quota payload")
	}
	if len(payload["codeAssist"]) == 0 {
		t.Fatalf("missing codeAssist payload")
	}
}

func TestFetchCodexQuotaIncludesResetCredits(t *testing.T) {
	var requests []quota.APICallRequest
	body, err := fetchCodexQuota(
		func(req quota.APICallRequest) (*quota.APICallResponse, error) {
			requests = append(requests, req)
			switch req.URL {
			case codexUsageURL:
				return &quota.APICallResponse{
					StatusCode: 200, Body: json.RawMessage(`{"rate_limit_reset_credits":{"available_count":1}}`),
				}, nil
			case codexRateLimitResetCreditsURL:
				return &quota.APICallResponse{
					StatusCode: 200,
					Body:       json.RawMessage(`{"available_count":1,"credits":[{"id":"credit-1","reset_type":"codex_rate_limits","status":"available","expires_at":"2026-07-06T08:00:00Z"}]}`),
				}, nil
			default:
				t.Fatalf("unexpected url: %s", req.URL)
				return nil, nil
			}
		},
		"codex-index",
	)
	if err != nil {
		t.Fatalf("fetchCodexQuota() error = %v", err)
	}
	if len(requests) != 2 {
		t.Fatalf("request count = %d, want 2", len(requests))
	}
	if requests[1].Header["OpenAI-Beta"] != "codex-1" {
		t.Fatalf("reset credits OpenAI-Beta header = %q", requests[1].Header["OpenAI-Beta"])
	}
	if requests[1].AuthIndex != "codex-index" {
		t.Fatalf("reset credits auth index = %q", requests[1].AuthIndex)
	}

	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(payload["rate_limit_reset_credits"]) == 0 {
		t.Fatalf("missing usage reset credit summary")
	}
	if len(payload["rate_limit_reset_credits_details"]) == 0 {
		t.Fatalf("missing reset credit details")
	}
}

func TestRegisterAuthManagerForQuotaUsesRuntimeAuthIndex(t *testing.T) {
	authManager := coreauth.NewManager(nil, nil, nil)
	authPath := filepath.Join(t.TempDir(), "claude.json")
	auth := &coreauth.Auth{
		ID:       "auth-claude",
		Provider: "claude",
		FileName: filepath.Base(authPath),
		Attributes: map[string]string{
			"path":   authPath,
			"source": authPath,
		},
		Metadata: map[string]any{
			"type": "claude",
		},
	}
	registeredAuth, err := authManager.Register(context.Background(), auth)
	if err != nil {
		t.Fatalf("Register auth: %v", err)
	}
	legacyIndex := computeAuthIndex(filepath.Base(authPath))
	if registeredAuth.Index == legacyIndex {
		t.Fatalf("runtime auth index unexpectedly matches legacy filename hash: %s", registeredAuth.Index)
	}

	scheduler := quota.NewScheduler(quota.DefaultConfig(), nil)
	if count := registerAuthManagerForQuota(scheduler, authManager); count != 1 {
		t.Fatalf("registered quota auth count = %d, want 1", count)
	}

	entry := scheduler.GetStatus().Credentials[quota.EntryKey(quota.TypeClaude, filepath.Base(authPath))]
	if entry == nil {
		t.Fatalf("quota entry for %q was not registered", filepath.Base(authPath))
	}
	if entry.AuthIndex != registeredAuth.Index {
		t.Fatalf("auth index = %q, want runtime auth index %q", entry.AuthIndex, registeredAuth.Index)
	}
}
