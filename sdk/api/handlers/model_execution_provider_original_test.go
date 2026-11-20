package handlers

import (
	"context"
	"net/http"
	"slices"
	"testing"

	internalconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	runtimeexecutor "github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdkconfig "github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
)

func TestModelExecutionOriginalPrivateRoutingReplacements(t *testing.T) {
	for _, tt := range []struct {
		original  string
		entry     string
		providers []string
		forced    string
		status    int
	}{
		{
			"TestPreferExecutionProviderMovesPreferredFirst", "interactions",
			[]string{"gemini", "gemini-interactions", "claude"}, "gemini-interactions", http.StatusOK,
		},
		{
			"TestAdjustExecutionProvidersExcludesInteractionsProviderForUnsupportedEntry", "codex",
			[]string{"gemini-interactions", "codex"}, "gemini-interactions", http.StatusOK,
		},
		{
			"TestProvidersForExecutionForcedGeminiRejectsRouterProvider", "interactions",
			[]string{"claude"}, "gemini", http.StatusBadRequest,
		},
		{
			"TestProvidersForExecutionForcedGeminiUsesGeminiProvider", "interactions",
			[]string{"gemini"}, "gemini", http.StatusOK,
		},
	} {
		t.Run(
			tt.original, func(t *testing.T) {
				const model = "agents/test-agent"
				body := []byte(`{"agent":"agents/test-agent","input":"hi"}`)
				manager := coreauth.NewManager(nil, nil, nil)
				manager.SetRetryConfig(0, 0, 1)
				seen := make(chan string, len(tt.providers)+1)
				for _, provider := range tt.providers {
					executor := &modelExecutionCaptureExecutor{
						provider: provider,
						execute: func(
							_ context.Context,
							auth *coreauth.Auth,
							request coreexecutor.Request,
							opts coreexecutor.Options,
						) (coreexecutor.Response, error) {
							seen <- auth.Provider
							if request.Model != model || string(request.Payload) != string(body) ||
								opts.SourceFormat != sdktranslator.Format(tt.entry) {
								t.Errorf("original route request changed: request=%#v options=%#v", request, opts)
							}
							return coreexecutor.Response{Payload: []byte(`{"id":"interaction_1"}`)}, nil
						},
					}
					manager.RegisterExecutor(executor)
					auth := &coreauth.Auth{
						ID: "original-routing-" + provider, Provider: provider, Status: coreauth.StatusActive,
					}
					if _, err := manager.Register(context.Background(), auth); err != nil {
						t.Fatal(err)
					}
					registry.GetGlobalRegistry().RegisterClient(auth.ID, provider, []*registry.ModelInfo{{ID: model}})
					t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
				}
				handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
				response, err := handler.ExecuteProtocolWithAuthManager(
					t.Context(), ProtocolExecutionRequest{
						EntryProtocol: tt.entry, ExitProtocol: tt.entry, ForcedProvider: tt.forced, Model: model,
						Body: body,
					},
				)
				if tt.status != http.StatusOK {
					if err == nil || err.StatusCode != tt.status || len(seen) != 0 {
						t.Fatalf("unavailable forced provider: error=%v calls=%d", err, len(seen))
					}
					return
				}
				if err != nil || response.StatusCode != tt.status ||
					string(response.Body) != `{"id":"interaction_1"}` ||
					len(seen) != 1 {
					t.Fatalf("current forced route: response=%#v error=%v calls=%d", response, err, len(seen))
				}
				if selected := <-seen; selected != tt.forced {
					t.Fatalf("selected provider=%q, want %q", selected, tt.forced)
				}
				available, _, err := handler.getRequestDetailsWithOptions(model, false, "")
				if err != nil {
					t.Fatal(err)
				}
				for _, provider := range tt.providers {
					if !slices.Contains(available, provider) {
						t.Errorf("explicit provider choice removed registered candidate %q: %v", provider, available)
					}
				}
				if tt.entry == "codex" {
					executor := runtimeexecutor.NewGeminiInteractionsExecutor(&internalconfig.Config{})
					if format := executor.RequestToFormat(
						coreexecutor.Request{}, coreexecutor.Options{SourceFormat: sdktranslator.FormatCodex},
					); format != sdktranslator.FormatGemini {
						t.Fatalf("codex entered native Interactions: %q", format)
					}
				}
			},
		)
	}
}

func TestModelExecutionOriginalSupportedInteractionsEntries(t *testing.T) {
	executor := runtimeexecutor.NewGeminiInteractionsExecutor(&internalconfig.Config{})
	for _, format := range []sdktranslator.Format{
		sdktranslator.FormatOpenAI, sdktranslator.FormatOpenAIResponse, sdktranslator.FormatClaude,
		sdktranslator.FormatGemini,
	} {
		t.Run(
			format.String(), func(t *testing.T) {
				if got := executor.RequestToFormat(
					coreexecutor.Request{}, coreexecutor.Options{SourceFormat: format},
				); got != sdktranslator.FormatInteractions {
					t.Fatalf("original supported entry %q selected %q, want interactions", format, got)
				}
			},
		)
	}
}
