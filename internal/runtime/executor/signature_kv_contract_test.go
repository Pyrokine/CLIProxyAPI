package executor

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/home"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor/helps"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
)

type signatureKVFailure struct {
	home.KVClient
	err   error
	reads int
}

func (*signatureKVFailure) Enabled() bool     { return true }
func (*signatureKVFailure) HeartbeatOK() bool { return true }
func (c *signatureKVFailure) KVGet(context.Context, string) ([]byte, bool, error) {
	c.reads++
	return nil, false, c.err
}

func (c *signatureKVFailure) RPopAuth(context.Context, string, string, http.Header, int) ([]byte, error) {
	return nil, c.err
}
func (c *signatureKVFailure) GetRefreshAuth(context.Context, string, string) ([]byte, error) {
	return nil, c.err
}

func TestSignatureHomePreparationFailurePreservesManagerCredentialState(t *testing.T) {
	const model = "gemini-3-pro-preview"
	payload := []byte((`{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"secret-` +
		`value"},{"type":"text","text":"previous answer"}]},{"role":"user","content":"next"}]}`))
	for _, entry := range []string{"execute", "count", "stream"} {
		t.Run(
			entry, func(t *testing.T) {
				sentinel := errors.New("ERR home kv unavailable")
				client := &signatureKVFailure{err: sentinel}
				manager := cliproxyauth.NewManager(nil, nil, nil)
				manager.SetHomeClientProvider(func() cliproxyauth.HomeClient { return client })
				manager.RegisterExecutor(NewGeminiExecutor(&config.Config{}))
				credential, err := manager.Register(
					cliproxyauth.WithSkipPersist(t.Context()), &cliproxyauth.Auth{
						ID: t.Name(), Provider: "gemini",
						Attributes: map[string]string{"auth_kind": "api_key", "api_key": "synthetic"},
					},
				)
				if err != nil {
					t.Fatal(err)
				}
				registry.GetGlobalRegistry().RegisterClient(credential.ID, "gemini", []*registry.ModelInfo{{ID: model}})
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(credential.ID) })
				requests := 0
				ctx := util.WithRoundTripper(
					t.Context(), &auditClaudeContractTransport{
						respond: func(*http.Request) (*http.Response, error) {
							requests++
							return nil, errors.New("unexpected provider request")
						},
					},
				)
				request := core.Request{Model: model, Payload: payload}
				options := core.Options{SourceFormat: translator.FormatClaude}
				switch entry {
				case "execute":
					_, err = manager.Execute(ctx, []string{"gemini"}, request, options)
				case "count":
					_, err = manager.ExecuteCount(ctx, []string{"gemini"}, request, options)
				case "stream":
					_, err = manager.ExecuteStream(ctx, []string{"gemini"}, request, options)
				}
				if !errors.Is(err, sentinel) || requests != 0 || client.reads != 1 {
					t.Fatalf("error=%v provider requests=%d Home reads=%d", err, requests, client.reads)
				}
				current, ok := manager.GetByID(credential.ID)
				if !ok || current.Disabled || current.Unavailable || !current.NextRetryAfter.IsZero() ||
					current.Generation != credential.Generation || len(current.ModelStates) != 0 {
					t.Fatalf("Home outage changed credential state: %+v", current)
				}
			},
		)
	}
}

func TestRequestSignatureCacheUsesHomeErrorAtCheckedEntrypoints(t *testing.T) {
	payload := []byte((`{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"secret-` +
		`value"},{"type":"text","text":"previous answer"}]},{"role":"user","content":"next"}]}`))
	for _, sample := range []struct {
		name, model string
		format      translator.Format
		compat      bool
	}{
		{"antigravity", "claude-sonnet-4-5-thinking", translator.FormatAntigravity, false},
		{"gemini", "gemini-3-pro-preview", translator.FormatGemini, false},
		{"gemini-compat", "gemini-3-pro-preview", translator.FormatGemini, true},
	} {
		t.Run(
			sample.name, func(t *testing.T) {
				sentinel := errors.New("ERR home kv unavailable")
				client := &signatureKVFailure{err: sentinel}
				ctx := home.WithClientScope(
					t.Context(), home.ClientScope{Client: client, ID: "signature-owner", Enabled: true},
				)
				var err error
				if sample.compat {
					_, err = helps.TranslateRequestWithAPIKeyModelCompatibilityChecked(
						ctx, nil, nil, translator.FormatClaude, sample.format, sample.model, payload, false, true,
					)
				} else {
					_, err = translator.TranslateRequestChecked(
						ctx, translator.FormatClaude, sample.format, sample.model, payload, false,
					)
				}
				if !errors.Is(err, sentinel) || client.reads != 1 {
					t.Fatalf(
						"request preparation error=%v Home reads=%d, want original error and one read", err,
						client.reads,
					)
				}
				var requestError interface {
					StatusCode() int
					IsRequestScoped() bool
				}
				if !errors.As(
					err, &requestError,
				) || requestError.StatusCode() != 503 || !requestError.IsRequestScoped() {
					t.Fatalf("Home preparation error lost HTTP 503 or request ownership: %v", err)
				}
			},
		)
	}
}
