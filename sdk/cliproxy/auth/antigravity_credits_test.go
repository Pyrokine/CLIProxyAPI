package auth

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	internalconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type antigravityCreditsFallbackExecutor struct {
	executeAuthIDs          []string
	executeCreditsRequested []bool
	streamAuthIDs           []string
	streamCreditsRequested  []bool
}

type antigravityAuthIDSelector struct {
	authID string
}

func (s *antigravityAuthIDSelector) Pick(
	_ context.Context,
	_, _ string,
	_ cliproxyexecutor.Options,
	auths []*Auth,
) (*Auth, error) {
	for _, auth := range auths {
		if auth != nil && auth.ID == s.authID {
			return auth, nil
		}
	}
	return nil, fmt.Errorf("auth %q not found", s.authID)
}

func (e *antigravityCreditsFallbackExecutor) Identifier() string { return "antigravity" }

func (e *antigravityCreditsFallbackExecutor) Execute(
	ctx context.Context,
	auth *Auth,
	_ cliproxyexecutor.Request,
	_ cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	creditsRequested := AntigravityCreditsRequested(ctx)
	authID := ""
	if auth != nil {
		authID = auth.ID
	}
	e.executeAuthIDs = append(e.executeAuthIDs, authID)
	e.executeCreditsRequested = append(e.executeCreditsRequested, creditsRequested)
	if !creditsRequested {
		return cliproxyexecutor.Response{}, &Error{
			HTTPStatus: http.StatusTooManyRequests, Message: "quota exhausted",
		}
	}
	return cliproxyexecutor.Response{Payload: []byte("credits fallback")}, nil
}

func (e *antigravityCreditsFallbackExecutor) ExecuteStream(
	ctx context.Context,
	auth *Auth,
	req cliproxyexecutor.Request,
	_ cliproxyexecutor.Options,
) (*cliproxyexecutor.StreamResult, error) {
	creditsRequested := AntigravityCreditsRequested(ctx)
	authID := ""
	if auth != nil {
		authID = auth.ID
	}
	e.streamAuthIDs = append(e.streamAuthIDs, authID)
	e.streamCreditsRequested = append(e.streamCreditsRequested, creditsRequested)
	ch := make(chan cliproxyexecutor.StreamChunk, 1)
	if !creditsRequested {
		ch <- cliproxyexecutor.StreamChunk{
			Err: &Error{
				HTTPStatus: http.StatusTooManyRequests, Message: "quota exhausted",
			},
		}
		close(ch)
		return &cliproxyexecutor.StreamResult{Headers: http.Header{"X-Initial": {req.Model}}, Chunks: ch}, nil
	}
	ch <- cliproxyexecutor.StreamChunk{Payload: []byte("credits fallback")}
	close(ch)
	return &cliproxyexecutor.StreamResult{Headers: http.Header{"X-Credits": {req.Model}}, Chunks: ch}, nil
}

func (e *antigravityCreditsFallbackExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *antigravityCreditsFallbackExecutor) CountTokens(
	context.Context,
	*Auth,
	cliproxyexecutor.Request,
	cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, &Error{
		HTTPStatus: http.StatusNotImplemented, Message: "CountTokens not implemented",
	}
}

func (e *antigravityCreditsFallbackExecutor) HttpRequest(context.Context, *Auth, *http.Request) (
	*http.Response,
	error,
) {
	return nil, &Error{HTTPStatus: http.StatusNotImplemented, Message: "HttpRequest not implemented"}
}

func TestManagerExecuteStream_AntigravityCreditsFallbackAfterBootstrap429(t *testing.T) {
	const model = "claude-opus-4-6-thinking"
	executor := &antigravityCreditsFallbackExecutor{}
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(
		&internalconfig.Config{
			QuotaExceeded: internalconfig.QuotaExceeded{AntigravityCredits: true},
		},
	)
	manager.RegisterExecutor(executor)
	registry.GetGlobalRegistry().RegisterClient("ag-credits", "antigravity", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient("ag-credits") })
	if _, errRegister := manager.Register(
		context.Background(), &Auth{ID: "ag-credits", Provider: "antigravity"},
	); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	streamResult, errExecute := manager.ExecuteStream(
		context.Background(), []string{"antigravity"}, cliproxyexecutor.Request{Model: model},
		cliproxyexecutor.Options{},
	)
	if errExecute != nil {
		t.Fatalf("execute stream: %v", errExecute)
	}

	var payload []byte
	for chunk := range streamResult.Chunks {
		if chunk.Err != nil {
			t.Fatalf("unexpected stream error: %v", chunk.Err)
		}
		payload = append(payload, chunk.Payload...)
	}
	if string(payload) != "credits fallback" {
		t.Fatalf("payload = %q, want %q", string(payload), "credits fallback")
	}
	if got := streamResult.Headers.Get("X-Credits"); got != model {
		t.Fatalf("X-Credits header = %q, want routed model", got)
	}
	if len(executor.streamCreditsRequested) != 2 {
		t.Fatalf("stream calls = %d, want 2", len(executor.streamCreditsRequested))
	}
	if executor.streamCreditsRequested[0] || !executor.streamCreditsRequested[1] {
		t.Fatalf("credits flags = %v, want [false true]", executor.streamCreditsRequested)
	}
}

func TestManagerExecute_AntigravityCreditsFallbackHonorsDynamicAuthPin(t *testing.T) {
	const model = "claude-sonnet-4-6-thinking"
	const ownerAuthID = "ag-owner"
	executor := &antigravityCreditsFallbackExecutor{}
	manager := NewManager(nil, &antigravityAuthIDSelector{authID: ownerAuthID}, nil)
	manager.SetConfig(
		&internalconfig.Config{
			QuotaExceeded: internalconfig.QuotaExceeded{AntigravityCredits: true},
		},
	)
	manager.RegisterExecutor(executor)

	for _, authID := range []string{ownerAuthID, "ag-other"} {
		registry.GetGlobalRegistry().RegisterClient(
			authID, "antigravity", []*registry.ModelInfo{{ID: model}},
		)
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(authID) })
		if _, errRegister := manager.Register(
			context.Background(), &Auth{ID: authID, Provider: "antigravity", Status: StatusActive},
		); errRegister != nil {
			t.Fatalf("register auth %s: %v", authID, errRegister)
		}
	}

	resp, errExecute := manager.Execute(
		context.Background(), []string{"antigravity"}, cliproxyexecutor.Request{Model: model},
		cliproxyexecutor.Options{
			Metadata: map[string]any{cliproxyexecutor.PinSelectedAuthMetadataKey: true},
		},
	)
	if errExecute != nil {
		t.Fatalf("execute: %v", errExecute)
	}
	if string(resp.Payload) != "credits fallback" {
		t.Fatalf("payload = %q, want credits fallback", resp.Payload)
	}
	if got := executor.executeAuthIDs; len(got) != 2 || got[0] != ownerAuthID || got[1] != ownerAuthID {
		t.Fatalf("execute auth IDs = %v, want [%s %s]", got, ownerAuthID, ownerAuthID)
	}
}

func TestManagerExecuteStream_AntigravityCreditsFallbackHonorsDynamicAuthPin(t *testing.T) {
	const model = "claude-sonnet-4-6-thinking"
	const ownerAuthID = "ag-owner"
	executor := &antigravityCreditsFallbackExecutor{}
	manager := NewManager(nil, &antigravityAuthIDSelector{authID: ownerAuthID}, nil)
	manager.SetConfig(
		&internalconfig.Config{
			QuotaExceeded: internalconfig.QuotaExceeded{AntigravityCredits: true},
		},
	)
	manager.RegisterExecutor(executor)

	for _, authID := range []string{ownerAuthID, "ag-other"} {
		registry.GetGlobalRegistry().RegisterClient(
			authID, "antigravity", []*registry.ModelInfo{{ID: model}},
		)
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(authID) })
		if _, errRegister := manager.Register(
			context.Background(), &Auth{ID: authID, Provider: "antigravity", Status: StatusActive},
		); errRegister != nil {
			t.Fatalf("register auth %s: %v", authID, errRegister)
		}
	}

	metadata := map[string]any{cliproxyexecutor.PinSelectedAuthMetadataKey: true}
	streamResult, errExecute := manager.ExecuteStream(
		context.Background(), []string{"antigravity"}, cliproxyexecutor.Request{Model: model},
		cliproxyexecutor.Options{Metadata: metadata},
	)
	if errExecute != nil {
		t.Fatalf("execute stream: %v", errExecute)
	}

	var payload []byte
	for chunk := range streamResult.Chunks {
		if chunk.Err != nil {
			t.Fatalf("unexpected stream error: %v", chunk.Err)
		}
		payload = append(payload, chunk.Payload...)
	}
	if string(payload) != "credits fallback" {
		t.Fatalf("payload = %q, want credits fallback", payload)
	}
	if got := executor.streamAuthIDs; len(got) != 2 || got[0] != ownerAuthID || got[1] != ownerAuthID {
		t.Fatalf("stream auth IDs = %v, want [%s %s]", got, ownerAuthID, ownerAuthID)
	}
}

func TestStatusCodeFromError_UnwrapsStreamBootstrap429(t *testing.T) {
	bootstrapErr := newStreamBootstrapError(
		&Error{HTTPStatus: http.StatusTooManyRequests, Message: "quota exhausted"}, nil,
	)
	wrappedErr := fmt.Errorf("conductor stream failed: %w", bootstrapErr)

	if status := statusCodeFromError(wrappedErr); status != http.StatusTooManyRequests {
		t.Fatalf("statusCodeFromError() = %d, want %d", status, http.StatusTooManyRequests)
	}
}

func TestIsAuthBlockedForModel_ClaudeWithCreditsStillBlockedDuringCooldown(t *testing.T) {
	auth := &Auth{
		ID:       "ag-1",
		Provider: "antigravity",
		ModelStates: map[string]*ModelState{
			"claude-sonnet-4-6": {
				Unavailable:    true,
				NextRetryAfter: time.Now().Add(10 * time.Minute),
				Quota: QuotaState{
					Exceeded:      true,
					NextRecoverAt: time.Now().Add(10 * time.Minute),
				},
			},
		},
	}

	SetAntigravityCreditsHint(
		auth.ID, AntigravityCreditsHint{
			Known:     true,
			Available: true,
			UpdatedAt: time.Now(),
		},
	)

	blocked, reason, _ := isAuthBlockedForModel(auth, "claude-sonnet-4-6", time.Now())
	if !blocked || reason != blockReasonCooldown {
		t.Fatalf(
			"expected auth to be blocked during cooldown even with credits, got blocked=%v reason=%v", blocked, reason,
		)
	}
}

func TestIsAuthBlockedForModel_KeepsGeminiBlockedWithoutCreditsBypass(t *testing.T) {
	auth := &Auth{
		ID:       "ag-2",
		Provider: "antigravity",
		ModelStates: map[string]*ModelState{
			"gemini-3-flash": {
				Unavailable:    true,
				NextRetryAfter: time.Now().Add(10 * time.Minute),
				Quota: QuotaState{
					Exceeded:      true,
					NextRecoverAt: time.Now().Add(10 * time.Minute),
				},
			},
		},
	}

	SetAntigravityCreditsHint(
		auth.ID, AntigravityCreditsHint{
			Known:     true,
			Available: true,
			UpdatedAt: time.Now(),
		},
	)

	blocked, reason, _ := isAuthBlockedForModel(auth, "gemini-3-flash", time.Now())
	if !blocked || reason != blockReasonCooldown {
		t.Fatalf("expected gemini auth to remain blocked, got blocked=%v reason=%v", blocked, reason)
	}
}
