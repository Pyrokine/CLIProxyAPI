package executor

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	homekv "github.com/Pyrokine/CLIProxyAPI/v6/internal/home"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executionregistry"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
)

type dispatchedPrimaryErrorHome struct {
	*primaryErrorHome
	credential *coreauth.Auth
	dispatches int
}

func (h *dispatchedPrimaryErrorHome) RPopAuth(_ context.Context, model, _ string, _ http.Header, _ int) (
	[]byte,
	error,
) {
	h.dispatches++
	return json.Marshal(
		map[string]any{
			"auth": h.credential, "auth_index": h.credential.ID, "model": model,
			"concurrency": map[string]any{"accounted": true, "credential_id": h.credential.ID, "model": model},
		},
	)
}

func TestAntigravityHomePrimaryErrorReleasesManagerSelection(t *testing.T) {
	for _, entry := range []struct {
		name, model string
		stream      bool
	}{
		{"generate", "gemini-2.5-flash", false},
		{"claude nonstream route", "gemini-3-pro-preview", false},
		{"stream", "gemini-2.5-flash", true},
	} {
		t.Run(
			entry.name, func(t *testing.T) {
				const body = `{"error":{"code":400,"message":"invalid thoughtSignature: synthetic-home-primary"}}`
				value, err := json.Marshal(
					[][]byte{
						[]byte(
							(`{"type":"thought_signature","thoughtSignature":` +
								`"synthetic-signature-123456789","contentIndex":0,"partIndex":` +
								`0}`),
						),
					},
				)
				if err != nil {
					t.Fatal(err)
				}
				credential := antigravityCountTokensAuth("https://synthetic.invalid", "home-primary-credential")
				credential.Status = coreauth.StatusActive
				home := &dispatchedPrimaryErrorHome{
					primaryErrorHome: &primaryErrorHome{t: t, value: value, casErr: homekv.ErrNotConnected},
					credential:       credential,
				}
				cfg := &config.Config{Home: config.HomeConfig{Enabled: true}}
				hook := &primaryErrorResults{}
				manager := coreauth.NewManager(nil, nil, hook)
				manager.SetConfig(cfg)
				manager.SetHomeClientProvider(func() coreauth.HomeClient { return home })
				manager.RegisterExecutor(NewAntigravityExecutor(cfg))
				registry := executionregistry.New()
				releases := make(chan executionregistry.ReleaseGroup, 2)
				registry.SetReleaseSink(
					func(group executionregistry.ReleaseGroup, _ int64) { releases <- group },
				)
				manager.PublishHomeDispatch(home, registry, 1)
				requests := 0
				ctx := util.WithRoundTripper(
					t.Context(), antigravityCountTokensRoundTripFunc(
						func(request *http.Request) (*http.Response, error) {
							requests++
							if request.Body != nil {
								_, _ = io.Copy(io.Discard, request.Body)
								_ = request.Body.Close()
							}
							return &http.Response{
								StatusCode: http.StatusBadRequest,
								Header:     http.Header{"Content-Type": {"application/json"}},
								Body:       io.NopCloser(strings.NewReader(body)), Request: request,
							}, nil
						},
					),
				)
				request := core.Request{
					Model: entry.model,
					Payload: []byte(
						`{"request":{"contents":[{"role":"user","parts":[{"text":"synthetic input"}]}]}}`,
					),
				}
				opts := core.Options{
					Stream: entry.stream, SourceFormat: translator.FormatAntigravity,
					Headers: http.Header{"Session-Id": {"home-primary-session"}},
				}
				if entry.stream {
					_, err = manager.ExecuteStream(ctx, []string{"antigravity"}, request, opts)
				} else {
					_, err = manager.Execute(ctx, []string{"antigravity"}, request, opts)
				}
				var status interface{ StatusCode() int }
				if !errors.As(err, &status) || status.StatusCode() != http.StatusBadRequest ||
					!strings.Contains(err.Error(), body) || errors.Is(err, homekv.ErrNotConnected) {
					t.Fatalf("Home Manager replaced the primary status or body: %v", err)
				}
				if requests != 1 || home.dispatches != 1 || home.casCalls != 1 {
					t.Fatalf(
						"requests=%d dispatches=%d cleanup attempts=%d",
						requests,
						home.dispatches,
						home.casCalls,
					)
				}
				if len(hook.results) != 1 || hook.results[0].Success || hook.results[0].Error == nil ||
					hook.results[0].Error.HTTPStatus != http.StatusBadRequest ||
					hook.results[0].Disposition.EffectScope != core.StateEffectScopeRequest ||
					hook.results[0].Disposition.RetryDirective != core.RetryDirectiveNoRetry {
					t.Fatalf("Home Manager primary result=%+v", hook.results)
				}
				if len(registry.FreezeInFlight(time.Now()).Executions) != 0 {
					t.Fatal("primary error retained the Home execution")
				}
				select {
				case group := <-releases:
					if group != (executionregistry.ReleaseGroup{CredentialID: credential.ID, Model: entry.model}) {
						t.Fatalf("released group=%+v", group)
					}
				default:
					t.Fatal("primary error did not release the Home admission")
				}
				select {
				case group := <-releases:
					t.Fatalf("duplicate Home release=%+v", group)
				default:
				}
				if _, exists := manager.GetByID(credential.ID); exists {
					t.Fatal("Home primary error published a local credential")
				}
			},
		)
	}
}
