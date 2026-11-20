package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	internalconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	homekv "github.com/Pyrokine/CLIProxyAPI/v6/internal/home"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
)

type originalAntigravityCreditsFallbackExecutor struct {
	streamCreditsRequested []bool
}

func (e *originalAntigravityCreditsFallbackExecutor) Identifier() string { return "antigravity" }

func (e *originalAntigravityCreditsFallbackExecutor) Execute(
	context.Context,
	*Auth,
	cliproxyexecutor.Request,
	cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, &Error{
		HTTPStatus: http.StatusNotImplemented, Message: "Execute not implemented",
	}
}

func (e *originalAntigravityCreditsFallbackExecutor) ExecuteStream(
	ctx context.Context,
	_ *Auth,
	req cliproxyexecutor.Request,
	_ cliproxyexecutor.Options,
) (*cliproxyexecutor.StreamResult, error) {
	creditsRequested := AntigravityCreditsRequested(ctx)
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

func (e *originalAntigravityCreditsFallbackExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *originalAntigravityCreditsFallbackExecutor) CountTokens(
	context.Context,
	*Auth,
	cliproxyexecutor.Request,
	cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, &Error{
		HTTPStatus: http.StatusNotImplemented, Message: "CountTokens not implemented",
	}
}

func (e *originalAntigravityCreditsFallbackExecutor) HttpRequest(context.Context, *Auth, *http.Request) (
	*http.Response,
	error,
) {
	return nil, &Error{HTTPStatus: http.StatusNotImplemented, Message: "HttpRequest not implemented"}
}

type originalCodexOnlyFailureExecutor struct{}

func (originalCodexOnlyFailureExecutor) Identifier() string { return "codex" }

func (originalCodexOnlyFailureExecutor) Execute(
	context.Context,
	*Auth,
	cliproxyexecutor.Request,
	cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, &Error{HTTPStatus: http.StatusTooManyRequests, Message: "codex quota exhausted"}
}

func (originalCodexOnlyFailureExecutor) ExecuteStream(
	context.Context,
	*Auth,
	cliproxyexecutor.Request,
	cliproxyexecutor.Options,
) (*cliproxyexecutor.StreamResult, error) {
	return nil, &Error{HTTPStatus: http.StatusTooManyRequests, Message: "codex quota exhausted"}
}

func (originalCodexOnlyFailureExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (originalCodexOnlyFailureExecutor) CountTokens(
	context.Context,
	*Auth,
	cliproxyexecutor.Request,
	cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, &Error{HTTPStatus: http.StatusTooManyRequests, Message: "codex quota exhausted"}
}

func (originalCodexOnlyFailureExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, &Error{HTTPStatus: http.StatusTooManyRequests, Message: "codex quota exhausted"}
}

type originalCaptureLogHook struct {
	messages []string
}

func (h *originalCaptureLogHook) Levels() []log.Level {
	return log.AllLevels
}

func (h *originalCaptureLogHook) Fire(entry *log.Entry) error {
	h.messages = append(h.messages, entry.Message)
	return nil
}

func TestOriginalManagerExecuteStream_AntigravityCreditsFallbackAfterBootstrap429(t *testing.T) {
	const model = "claude-opus-4-6-thinking"
	executor := &originalAntigravityCreditsFallbackExecutor{}
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

func TestCreditsHomeUnavailableKeepsOriginalRejection(t *testing.T) {
	const model = "claude-opus-4-6-thinking"
	executor := &originalAntigravityCreditsFallbackExecutor{}
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(
		&internalconfig.Config{
			Home:          internalconfig.HomeConfig{Enabled: true},
			QuotaExceeded: internalconfig.QuotaExceeded{AntigravityCredits: true},
		},
	)
	manager.RegisterExecutor(executor)
	registry.GetGlobalRegistry().RegisterClient("ag-credits-home-kv", "antigravity", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient("ag-credits-home-kv") })
	previousHome := homekv.Current()
	homekv.SetCurrent(homekv.New(internalconfig.HomeConfig{Enabled: false}))
	t.Cleanup(func() { homekv.SetCurrent(previousHome) })
	if _, errRegister := manager.Register(
		context.Background(), &Auth{ID: "ag-credits-home-kv", Provider: "antigravity"},
	); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	_, errExecute := manager.ExecuteStream(
		context.Background(), []string{"antigravity"}, cliproxyexecutor.Request{Model: model},
		cliproxyexecutor.Options{},
	)
	if errExecute == nil {
		t.Fatal("ExecuteStream() error = nil, want home kv unavailable error")
	}
	if status := statusCodeFromError(errExecute); status != http.StatusServiceUnavailable {
		t.Fatalf("ExecuteStream() status = %d, want %d; err=%v", status, http.StatusServiceUnavailable, errExecute)
	}
	var unavailable *Error
	if !errors.As(
		errExecute, &unavailable,
	) || unavailable.Code != "home_unavailable" || unavailable.Message != "home control center unavailable" {
		t.Fatalf("ExecuteStream() error = %v, want unavailable Home control center", errExecute)
	}
	if len(executor.streamCreditsRequested) != 0 {
		t.Fatalf("Home rejection executed local credits fallback: %v", executor.streamCreditsRequested)
	}
}

func TestOriginalManagerExecuteStream_CodexOnlyDoesNotEnterAntigravityCreditsFallback(t *testing.T) {
	const model = "gpt-5.5"
	logger := log.StandardLogger()
	oldLevel := logger.GetLevel()
	oldHooks := logger.ReplaceHooks(make(log.LevelHooks))
	hook := &originalCaptureLogHook{}
	logger.SetLevel(log.DebugLevel)
	logger.AddHook(hook)
	t.Cleanup(
		func() {
			logger.SetLevel(oldLevel)
			logger.ReplaceHooks(oldHooks)
		},
	)

	manager := NewManager(nil, nil, nil)
	manager.SetConfig(
		&internalconfig.Config{
			QuotaExceeded: internalconfig.QuotaExceeded{AntigravityCredits: true},
		},
	)
	manager.RegisterExecutor(originalCodexOnlyFailureExecutor{})
	manager.RegisterExecutor(&originalAntigravityCreditsFallbackExecutor{})
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient("codex-only", "codex", []*registry.ModelInfo{{ID: model}})
	reg.RegisterClient("ag-unrelated", "antigravity", []*registry.ModelInfo{{ID: "gemini-3-flash"}})
	t.Cleanup(
		func() {
			reg.UnregisterClient("codex-only")
			reg.UnregisterClient("ag-unrelated")
		},
	)
	if _, errRegister := manager.Register(
		context.Background(), &Auth{ID: "codex-only", Provider: "codex"},
	); errRegister != nil {
		t.Fatalf("register codex auth: %v", errRegister)
	}
	if _, errRegister := manager.Register(
		context.Background(), &Auth{ID: "ag-unrelated", Provider: "antigravity"},
	); errRegister != nil {
		t.Fatalf("register antigravity auth: %v", errRegister)
	}

	_, errExecute := manager.ExecuteStream(
		context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{},
	)
	if errExecute == nil {
		t.Fatal("expected codex execution failure")
	}

	for _, message := range hook.messages {
		if strings.Contains(message, "shouldAttemptAntigravityCreditsFallback") {
			t.Fatalf("codex-only request entered antigravity credits fallback gate; messages=%v", hook.messages)
		}
	}
}

func TestOriginalStatusCodeFromError_UnwrapsStreamBootstrap429(t *testing.T) {
	bootstrapErr := newStreamBootstrapError(
		&Error{HTTPStatus: http.StatusTooManyRequests, Message: "quota exhausted"}, nil,
	)
	wrappedErr := fmt.Errorf("conductor stream failed: %w", bootstrapErr)

	if status := statusCodeFromError(wrappedErr); status != http.StatusTooManyRequests {
		t.Fatalf("statusCodeFromError() = %d, want %d", status, http.StatusTooManyRequests)
	}
}

func TestOriginalIsAuthBlockedForModel_ClaudeWithCreditsStillBlockedDuringCooldown(t *testing.T) {
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

	t.Cleanup(func() { antigravityCreditsHintByAuth.Delete(auth.ID) })
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

func TestOriginalIsAuthBlockedForModel_KeepsGeminiBlockedWithoutCreditsBypass(t *testing.T) {
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

	t.Cleanup(func() { antigravityCreditsHintByAuth.Delete(auth.ID) })
	SetAntigravityCreditsHint(
		auth.ID, AntigravityCreditsHint{
			Known:     true,
			Available: true,
			UpdatedAt: time.Now(),
		},
	)

	blocked, reason, _ := isAuthBlockedForModel(auth, "gemini-3-flash", time.Now())
	if !blocked || reason != blockReasonCooldown {
		t.Fatalf("expected gemini model to remain blocked, got blocked=%v reason=%v", blocked, reason)
	}
}
