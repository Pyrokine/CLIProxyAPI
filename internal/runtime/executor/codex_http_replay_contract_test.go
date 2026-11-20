package executor

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
)

type codexReplayTransportProvider struct{ transport http.RoundTripper }

func (p codexReplayTransportProvider) RoundTripperFor(*coreauth.Auth) http.RoundTripper {
	return p.transport
}

func TestCodexHTTPExecutionDoesNotReplayUnknownResults(t *testing.T) {
	for _, entry := range []string{"http", "auto", "manager"} {
		for _, upstream := range []struct {
			name       string
			status     int
			body       string
			retryAfter string
			wantError  bool
		}{
			{
				"HTTP 408", http.StatusRequestTimeout,
				`{"error":{"message":"stream error: stream disconnected before ` +
					`completion: stream closed before ` +
					`response.completed"}}`,
				"", true,
			},
			{
				"HTTP 408 Retry-After", http.StatusRequestTimeout,
				`{"error":{"message":"synthetic unknown processing outcome"}}`, "1", true,
			},
			{"HTTP 200 without terminal", http.StatusOK, "", "", true},
			{
				"completed response", http.StatusOK,
				"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"synthetic-complete\",\"model\"" +
					":\"gpt-5.5\",\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1," +
					"\"total_tokens\":2},\"output\":[]}}\n\n",
				"", false,
			},
		} {
			t.Run(
				entry+"/"+upstream.name, func(t *testing.T) {
					cfg := &config.Config{}
					auth := &coreauth.Auth{
						ID: "synthetic-replay-" + t.Name(), Provider: "codex", Status: coreauth.StatusActive,
						Attributes: map[string]string{
							"api_key": "synthetic-replay-key", "base_url": "https://codex-replay.invalid/v1",
						},
					}
					var sent [][]byte
					transport := &auditClaudeContractTransport{
						respond: func(request *http.Request) (*http.Response, error) {
							if request.Method != http.MethodPost ||
								request.URL.String() != "https://codex-replay.invalid/v1/responses" {
								t.Errorf(
									"unexpected isolated generation route %s %s",
									request.Method,
									request.URL,
								)
							}
							body, err := io.ReadAll(request.Body)
							if err != nil {
								return nil, err
							}
							sent = append(sent, body)
							headers := http.Header{"Content-Type": {"text/event-stream"}}
							if upstream.retryAfter != "" {
								headers.Set("Retry-After", upstream.retryAfter)
							}
							return &http.Response{
								StatusCode: upstream.status, Header: headers,
								Body: io.NopCloser(strings.NewReader(upstream.body)), Request: request,
							}, nil
						},
					}
					ctx := util.WithRoundTripper(translator.WithPluginHooks(t.Context(), nil), transport)
					client := newProtectedUtlsHTTPClient(ctx, cfg, auth, 0)
					branches, ok := client.Transport.(*claudeFallbackRoundTripper)
					if !ok || branches.anthropic != transport || branches.chrome != transport ||
						branches.fallback != transport ||
						auth.AuthKind() != coreauth.AuthKindAPIKey {
						t.Fatal(
							"actual Codex client branches are not isolated to the memory transport " +
								"and synthetic API " +
								"key",
						)
					}
					invoke := newCodexExecutor(cfg).Execute
					if entry == "auto" {
						invoke = NewCodexAutoExecutor(cfg).Execute
					} else if entry == "manager" {
						manager := coreauth.NewManager(nil, nil, nil)
						manager.SetHomeClientProvider(func() coreauth.HomeClient { return nil })
						manager.SetConfig(cfg)
						manager.SetRetryConfig(3, 0, 3)
						manager.SetRoundTripperProvider(codexReplayTransportProvider{transport: transport})
						manager.RegisterExecutor(NewCodexAutoExecutor(cfg))
						var err error
						auth, err = manager.Register(coreauth.WithSkipPersist(ctx), auth)
						if err != nil {
							t.Fatal(err)
						}
						registry.GetGlobalRegistry().RegisterClient(
							auth.ID, "codex", []*registry.ModelInfo{{ID: "gpt-5.5"}},
						)
						manager.RefreshSchedulerEntry(auth.ID)
						t.Cleanup(
							func() {
								registry.GetGlobalRegistry().UnregisterClient(auth.ID)
								_ = manager.StopDeferredAuthRecreates(context.Background())
							},
						)
						invoke = func(
							ctx context.Context,
							_ *coreauth.Auth,
							request coreexecutor.Request,
							options coreexecutor.Options,
						) (coreexecutor.Response, error) {
							return manager.Execute(ctx, []string{"codex"}, request, options)
						}
					}
					payload := []byte(
						`{"model":"gpt-5.5","input":[{"role":"user","content":[{"type":` +
							`"input_text","text":` +
							`"synthetic"}]}]}`,
					)
					original := bytes.Clone(payload)
					_, err := invoke(
						ctx, auth, coreexecutor.Request{Model: "gpt-5.5", Payload: payload},
						coreexecutor.Options{SourceFormat: translator.FormatOpenAIResponse},
					)
					if len(sent) != 1 || !bytes.Equal(payload, original) {
						t.Errorf(
							"generation POST count=%d, want one; caller payload unchanged=%t", len(sent),
							bytes.Equal(payload, original),
						)
					}
					if !upstream.wantError {
						if err != nil {
							t.Fatalf("completed response error=%v", err)
						}
						return
					}
					var classified coreexecutor.ExecutionDispositionError
					if err == nil || !errors.As(err, &classified) {
						t.Fatalf("unknown processing result lost its execution disposition: %v", err)
					}
					if disposition := classified.ExecutionDisposition(); disposition.DispatchPhase !=
						coreexecutor.DispatchPhaseResponseStarted ||
						disposition.Outcome != coreexecutor.ExecutionOutcomeUnknown ||
						disposition.RetryDirective != coreexecutor.RetryDirectiveNoRetry {
						t.Errorf("unknown processing result disposition=%+v", disposition)
					}
				},
			)
		}
	}
}
