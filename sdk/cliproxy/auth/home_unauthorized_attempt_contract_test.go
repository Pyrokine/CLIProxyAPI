package auth

import (
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	internalconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executionregistry"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestHomeUnauthorizedOriginalAttemptsReportAndReleaseOnce(t *testing.T) {
	for _, mode := range []string{"execute", "count", "synchronous", "bootstrap", "started", "started acknowledged"} {
		t.Run(
			mode, func(t *testing.T) {
				dispatcher := &homeUnauthorizedRefreshDispatcher{}
				streamMode := mode
				if mode == "started acknowledged" {
					streamMode = "started"
				}
				provider := &homeUnauthorizedRefreshExecutor{
					streamMode: streamMode, keepStale: true, retainSelection: true,
				}
				hook := &resultCaptureHook{}
				manager := NewManager(nil, nil, hook)
				manager.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}})
				manager.SetRetryConfig(3, time.Second, 2)
				registry := executionregistry.New()
				manager.PublishHomeDispatch(dispatcher, registry, 1)
				manager.RegisterExecutor(provider)
				t.Cleanup(func() { manager.CloseExecutionSession("refresh-session") })
				var selection *HomeDispatchSelection
				var closes atomic.Int32
				provider.observe = func(opts cliproxyexecutor.Options) {
					selection, _ = opts.ExecutionLifecycle.(*HomeDispatchSelection)
					if selection == nil {
						t.Error("upstream attempt has no Home selection")
						return
					}
					selection.Retain()
					if err := selection.Bind(func() error { closes.Add(1); return nil }); err != nil {
						t.Errorf("bind attempt resource: %v", err)
					}
				}
				opts := cliproxyexecutor.Options{
					Metadata: map[string]any{
						cliproxyexecutor.ExecutionSessionMetadataKey: "refresh-session",
						cliproxyexecutor.PinnedAuthMetadataKey:       "home-refresh-auth",
					},
				}
				ctx := cliproxyexecutor.WithDownstreamWebsocket(t.Context())
				if mode == "started acknowledged" {
					ctx = cliproxyexecutor.WithStreamDeliveryAcknowledgement(ctx)
				}
				req := cliproxyexecutor.Request{Model: "model-a"}
				var errRun error
				switch mode {
				case "execute":
					_, errRun = manager.Execute(ctx, []string{homeUnauthorizedRefreshProvider}, req, opts)
				case "count":
					_, errRun = manager.ExecuteCount(ctx, []string{homeUnauthorizedRefreshProvider}, req, opts)
				default:
					opts.Stream = true
					var result *cliproxyexecutor.StreamResult
					result, errRun = manager.ExecuteStream(
						ctx,
						[]string{homeUnauthorizedRefreshProvider},
						req,
						opts,
					)
					if result != nil {
						for chunk := range result.Chunks {
							if chunk.Err != nil {
								errRun = chunk.Err
							}
							if chunk.Acknowledge != nil {
								chunk.Acknowledge(nil)
							}
						}
					}
				}
				if errRun == nil || statusCodeFromError(errRun) != http.StatusUnauthorized ||
					errRun.Error() != "access token expired" {
					t.Fatalf("upstream result=%v, want original 401 and body", errRun)
				}
				if got := provider.executeCalls.Load() +
					provider.countCalls.Load() +
					provider.streamCalls.Load(); got !=
					1 ||
					dispatcher.calls.Load() != 1 ||
					provider.refreshCalls.Load() != 0 {
					t.Fatalf(
						"attempts=%d dispatches=%d refreshes=%d", got, dispatcher.calls.Load(),
						provider.refreshCalls.Load(),
					)
				}
				results := hook.Results()
				if len(results) != 1 || results[0].AuthID != "home-refresh-auth" ||
					results[0].Provider != homeUnauthorizedRefreshProvider ||
					results[0].Model != "model-a" ||
					results[0].Success {
					t.Fatalf("real attempt results=%+v, want one original failure", results)
				}
				if results[0].Error == nil || results[0].Error.StatusCode() != http.StatusUnauthorized ||
					results[0].Error.Message != "access token expired" {
					t.Fatalf("reported error=%+v, want original upstream error", results[0].Error)
				}
				if selection == nil || selection.Active() || closes.Load() != 1 {
					t.Fatalf(
						"selection=%p resource closes=%d, want ended selection and one close",
						selection,
						closes.Load(),
					)
				}
				selection.End("duplicate_end")
				manager.CloseExecutionSession("refresh-session")
				if closes.Load() != 1 || len(registry.FreezeInFlight(time.Now()).Executions) != 0 {
					t.Fatal("repeated completion retained an execution or released a resource twice")
				}
				if _, found := manager.GetExecutionSessionAuthByID("refresh-session", "home-refresh-auth"); found {
					t.Fatal("failed retained selection left a readable session credential")
				}
			},
		)
	}
}
