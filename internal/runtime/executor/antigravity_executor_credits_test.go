package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
)

func resetAntigravityCreditsRetryState() {
	antigravityCreditsHintRefreshByID = sync.Map{}
}

func TestInjectEnabledCreditTypes(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-6","request":{}}`)
	got := injectEnabledCreditTypes(body)
	if got == nil {
		t.Fatal("injectEnabledCreditTypes returned nil")
	}
	if !strings.Contains(string(got), `"enabledCreditTypes":["GOOGLE_ONE_AI"]`) {
		t.Fatalf("injectEnabledCreditTypes = %s, want enabledCreditTypes", string(got))
	}

	if got := injectEnabledCreditTypes([]byte(`not json`)); got != nil {
		t.Fatalf("injectEnabledCreditTypes for invalid json = %s, want nil", string(got))
	}
}

func TestAntigravityExecute_CreditsInjectedWhenConductorRequests(t *testing.T) {
	resetAntigravityCreditsRetryState()
	t.Cleanup(resetAntigravityCreditsRetryState)

	var requestBodies []string
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				_ = r.Body.Close()
				if r.URL.Path == antigravityLoadCodeAssistPath {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"paidTier":{"id":"tier-1","availableCredits":[{"creditType":"GOOGLE_ONE_AI","creditAmount":"25000","minimumCreditAmountForUsage":"50"}]}}`))
					return
				}
				requestBodies = append(requestBodies, string(body))
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]}}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}}`))
			},
		),
	)
	defer server.Close()

	exec := NewAntigravityExecutor(&config.Config{QuotaExceeded: config.QuotaExceeded{AntigravityCredits: true}})
	auth := &cliproxyauth.Auth{
		ID: "auth-credits-conductor",
		Attributes: map[string]string{
			"base_url": server.URL,
		},
		Metadata: map[string]any{
			"access_token": "token",
			"project_id":   "project-1",
			"expired":      time.Now().Add(time.Hour).Format(time.RFC3339),
		},
	}

	ctx := cliproxyauth.WithAntigravityCredits(context.Background())
	resp, err := exec.Execute(
		ctx, auth, cliproxyexecutor.Request{
			Model:   "claude-sonnet-4-6",
			Payload: []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}}`),
		}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatAntigravity},
	)
	if err != nil {
		t.Fatalf("Execute error = %v", err)
	}
	if len(resp.Payload) == 0 {
		t.Fatal("Execute returned empty payload")
	}
	if len(requestBodies) != 1 {
		t.Fatalf("request body count = %d, want 1", len(requestBodies))
	}
	if !strings.Contains(requestBodies[0], `"enabledCreditTypes":["GOOGLE_ONE_AI"]`) {
		t.Fatalf("request body missing enabledCreditTypes: %s", requestBodies[0])
	}
}

func TestAntigravityExecute_CreditsNotInjectedWithoutConductorFlag(t *testing.T) {
	resetAntigravityCreditsRetryState()
	t.Cleanup(resetAntigravityCreditsRetryState)

	var requestBodies []string
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				_ = r.Body.Close()
				if r.URL.Path == antigravityLoadCodeAssistPath {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"paidTier":{"id":"tier-1","availableCredits":[{"creditType":"GOOGLE_ONE_AI","creditAmount":"25000","minimumCreditAmountForUsage":"50"}]}}`))
					return
				}
				requestBodies = append(requestBodies, string(body))
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]}}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}}`))
			},
		),
	)
	defer server.Close()

	exec := NewAntigravityExecutor(&config.Config{QuotaExceeded: config.QuotaExceeded{AntigravityCredits: true}})
	auth := &cliproxyauth.Auth{
		ID: "auth-no-credits-flag",
		Attributes: map[string]string{
			"base_url": server.URL,
		},
		Metadata: map[string]any{
			"access_token": "token",
			"project_id":   "project-1",
			"expired":      time.Now().Add(time.Hour).Format(time.RFC3339),
		},
	}

	_, err := exec.Execute(
		context.Background(), auth, cliproxyexecutor.Request{
			Model:   "claude-sonnet-4-6",
			Payload: []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}}`),
		}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatAntigravity},
	)
	if err != nil {
		t.Fatalf("Execute error = %v", err)
	}
	if len(requestBodies) != 1 {
		t.Fatalf("request body count = %d, want 1", len(requestBodies))
	}
	if strings.Contains(requestBodies[0], `"enabledCreditTypes"`) {
		t.Fatalf("request should not contain enabledCreditTypes without conductor flag: %s", requestBodies[0])
	}
}

func TestUpdateAntigravityCreditsBalance_LoadCodeAssistHint(t *testing.T) {
	resetAntigravityCreditsRetryState()
	t.Cleanup(resetAntigravityCreditsRetryState)

	var gotUserAgent string
	var gotAPIClient string
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != antigravityLoadCodeAssistPath {
					t.Fatalf("path = %s, want %s", r.URL.Path, antigravityLoadCodeAssistPath)
				}
				gotUserAgent = r.Header.Get("User-Agent")
				gotAPIClient = r.Header.Get("X-Goog-Api-Client")
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"paidTier":{"id":"tier-1","availableCredits":[{"creditType":"GOOGLE_ONE_AI","creditAmount":"25000","minimumCreditAmountForUsage":"50"}]}}`))
			},
		),
	)
	defer server.Close()

	exec := NewAntigravityExecutor(&config.Config{QuotaExceeded: config.QuotaExceeded{AntigravityCredits: true}})
	auth := &cliproxyauth.Auth{
		ID: "auth-balance",
		Attributes: map[string]string{
			"base_url": server.URL,
		},
		Metadata: map[string]any{
			"access_token": "token",
		},
	}

	exec.updateAntigravityCreditsBalance(context.Background(), auth, "token")
	hint, ok := cliproxyauth.GetAntigravityCreditsHint(auth.ID)
	if !ok || !hint.Known || !hint.Available {
		t.Fatalf("credits hint = %+v, ok=%v, want known available", hint, ok)
	}
	if hint.CreditAmount != 25000 || hint.MinCreditAmount != 50 || hint.PaidTierID != "tier-1" {
		t.Fatalf("credits hint = %+v, want parsed balance", hint)
	}
	if !strings.Contains(gotUserAgent, antigravityLoadCodeAssistNodeClient) {
		t.Fatalf("User-Agent = %q, want loadCodeAssist client", gotUserAgent)
	}
	if gotAPIClient != antigravityLoadCodeAssistAPIClient {
		t.Fatalf("X-Goog-Api-Client = %q, want %q", gotAPIClient, antigravityLoadCodeAssistAPIClient)
	}
}
