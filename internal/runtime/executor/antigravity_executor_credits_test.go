package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
)

func resetAntigravityCreditsRetryState() {
	antigravityCreditsHintRefreshByID.Range(func(key, _ any) bool {
		antigravityCreditsHintRefreshByID.Delete(key)
		return true
	})
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
				_, _ = w.Write([]byte(`{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}}`))
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
				_, _ = w.Write([]byte(`{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}}`))
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
	if !strings.Contains(gotUserAgent, "antigravity/cli/") || strings.Contains(
		gotUserAgent, antigravityLoadCodeAssistNodeClient,
	) {
		t.Fatalf("User-Agent = %q, want short loadCodeAssist client", gotUserAgent)
	}
	if gotAPIClient != antigravityLoadCodeAssistAPIClient {
		t.Fatalf("X-Goog-Api-Client = %q, want %q", gotAPIClient, antigravityLoadCodeAssistAPIClient)
	}
}

func TestAntigravityCreditsRejectReplacedCredentialResults(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		resetAntigravityCreditsRetryState()
		release := make(chan struct{})
		var once sync.Once
		defer once.Do(func() { close(release) })
		tokenCalls, oldCalls, newCalls := 0, 0, 0
		oldCtx, cancel := context.WithCancel(util.WithRoundTripper(context.Background(), groundingURLRoundTripper(func(req *http.Request) (*http.Response, error) {
			if req.URL.Path == "/token" {
				tokenCalls++
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Request: req, Body: io.NopCloser(strings.NewReader(`{"access_token":"old-access","expires_in":3600}`))}, nil
			}
			if req.URL.Path != antigravityLoadCodeAssistPath {
				return nil, fmt.Errorf("unexpected path %s", req.URL)
			}
			oldCalls++
			<-release
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Request: req, Body: io.NopCloser(strings.NewReader(`{"paidTier":{"id":"tier","availableCredits":[{"creditType":"GOOGLE_ONE_AI","creditAmount":"10","minimumCreditAmountForUsage":"50"}]}}`))}, nil
		})))
		defer cancel()
		cfg := &config.Config{QuotaExceeded: config.QuotaExceeded{AntigravityCredits: true}}
		exec := NewAntigravityExecutor(cfg)
		manager := cliproxyauth.NewManager(nil, nil, nil)
		manager.SetHomeClientProvider(func() cliproxyauth.HomeClient { return nil })
		manager.SetConfig(cfg)
		manager.RegisterExecutor(exec)
		auth := &cliproxyauth.Auth{ID: t.Name(), Provider: "antigravity", Attributes: map[string]string{"base_url": "https://synthetic.invalid"}, Metadata: map[string]any{"refresh_token": t.Name() + "-old-refresh", "project_id": "synthetic-project"}}
		if _, err := manager.Register(oldCtx, auth); err != nil {
			t.Fatal(err)
		}
		results := make(chan error, 1)
		go func() { _, err := manager.ForceRefreshAuth(oldCtx, auth.ID); results <- err }()
		synctest.Wait()
		select {
		case err := <-results:
			if err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatal("token refresh blocked")
		}
		if tokenCalls != 1 || oldCalls != 1 {
			t.Fatalf("old callbacks token=%d credits=%d", tokenCalls, oldCalls)
		}
		cancel()
		updated, _ := manager.GetByID(auth.ID)
		replacement := updated.Clone()
		replacement.Metadata["access_token"] = "replacement-access"
		replacement.Metadata["refresh_token"] = t.Name() + "-replacement-refresh"
		newCtx := util.WithRoundTripper(context.Background(), groundingURLRoundTripper(func(req *http.Request) (*http.Response, error) {
			if req.URL.Path != antigravityLoadCodeAssistPath {
				return nil, fmt.Errorf("unexpected new path %s", req.URL)
			}
			newCalls++
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Request: req, Body: io.NopCloser(strings.NewReader(`{"paidTier":{"id":"tier","availableCredits":[{"creditType":"GOOGLE_ONE_AI","creditAmount":"1500","minimumCreditAmountForUsage":"50"}]}}`))}, nil
		}))
		if _, err := manager.Update(newCtx, replacement); err != nil {
			t.Fatal(err)
		}
		exec.updateAntigravityCreditsBalance(newCtx, replacement, "replacement-access")
		hint, ok := cliproxyauth.GetAntigravityCreditsHint(auth.ID)
		if newCalls != 1 || !ok || hint.CreditAmount != 1500 {
			t.Fatalf("new balance not published: callbacks=%d hint=%+v", newCalls, hint)
		}
		once.Do(func() { close(release) })
		synctest.Wait()
		current, _ := manager.GetByID(auth.ID)
		hint, ok = cliproxyauth.GetAntigravityCreditsHint(auth.ID)
		if metaStringValue(current.Metadata, "access_token") != "replacement-access" {
			t.Fatal("manager lost replacement credential")
		}
		if !ok || hint.CreditAmount != 1500 || !hint.Available {
			t.Fatalf("old canceled refresh overwrote the current manager credential balance: %+v; want 1500 available=true", hint)
		}
	})
}
