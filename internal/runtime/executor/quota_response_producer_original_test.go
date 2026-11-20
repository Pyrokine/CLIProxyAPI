package executor

import (
	"context"
	"net/http"
	"reflect"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type quotaResponseProducerExecutor struct {
	*codexWebsocketsExecutor
	record func(context.Context)
}

func (e *quotaResponseProducerExecutor) Execute(
	ctx context.Context,
	_ *coreauth.Auth,
	_ core.Request,
	_ core.Options,
) (core.Response, error) {
	e.record(ctx)
	return core.Response{}, &coreauth.Error{
		HTTPStatus: http.StatusInternalServerError, Message: "first upstream attempt failed",
	}
}

func TestOriginalQuotaResponseProducersObserveWithRequestLoggingDisabled(t *testing.T) {
	for _, producer := range []struct {
		name   string
		record func(context.Context, *config.Config, int, http.Header, ...*http.Response)
	}{
		{"http", recordAPIResponseMetadata},
		{"websocket", recordAPIWebsocketHandshake},
	} {
		t.Run(
			producer.name, func(t *testing.T) {
				cfg := &config.Config{}
				if requestLogCaptureEnabled(cfg) {
					t.Fatal("fixture unexpectedly enabled request logging")
				}
				headers := http.Header{
					"X-Codex-Plan-Type": {"pro"}, "X-Codex-Primary-Used-Percent": {"91"},
					"X-Codex-Primary-Window-Minutes": {"10080"}, "X-Codex-Primary-Reset-After-Seconds": {"3600"},
				}
				manager := coreauth.NewManager(nil, nil, nil)
				manager.SetHomeClientProvider(func() coreauth.HomeClient { return nil })
				manager.RegisterExecutor(
					&quotaResponseProducerExecutor{
						codexWebsocketsExecutor: newCodexWebsocketsExecutor(cfg),
						record: func(ctx context.Context) {
							producer.record(
								ctx, cfg, http.StatusInternalServerError, headers,
							)
						},
					},
				)
				auth, err := manager.Register(
					coreauth.WithSkipPersist(context.Background()), &coreauth.Auth{
						ID: t.Name(), Provider: "codex", Status: coreauth.StatusActive,
					},
				)
				if err != nil {
					t.Fatal(err)
				}
				const model = "quota-model"
				reg := registry.GetGlobalRegistry()
				reg.RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: model}})
				manager.RefreshSchedulerEntry(auth.ID)
				t.Cleanup(func() { reg.UnregisterClient(auth.ID) })
				ctx := logging.WithResponseHeadersHolder(context.Background())
				_, err = manager.Execute(ctx, []string{"codex"}, core.Request{Model: model}, core.Options{})
				if err == nil {
					t.Fatal("expected the original upstream failure")
				}
				current, ok := manager.GetByID(auth.ID)
				want := map[string]string{
					"X-Codex-Plan-Type": "pro", "X-Codex-Primary-Used-Percent": "91",
					"X-Codex-Primary-Window-Minutes": "10080", "X-Codex-Primary-Reset-After-Seconds": "3600",
				}
				if !ok || current == nil || !reflect.DeepEqual(
					current.Quota.Signals, want,
				) || current.Quota.ObservedAt.IsZero() {
					t.Fatalf("actual response producer lost quota observation: %+v", current)
				}
				if len(logging.GetResponseHeaders(ctx)) != 0 {
					t.Fatal("attempt response headers reached the request holder")
				}
			},
		)
	}
}
