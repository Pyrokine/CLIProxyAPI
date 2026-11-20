package executor

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executionregistry"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
)

func TestAntigravityHomeUnauthorizedKeepsReadFailureEvidence(t *testing.T) {
	for _, entry := range []struct {
		name, model, path string
	}{
		{"generate", "gemini-2.5-flash", antigravityGeneratePath},
		{"claude aggregate", "gemini-3-pro-preview", antigravityStreamPath},
		{"stream", "gemini-2.5-flash", antigravityStreamPath},
		{"count", "gemini-2.5-flash", antigravityCountTokensPath},
	} {
		for _, bodyText := range []string{"", `{"error":{"message":"synthetic unauthorized"}}`, `{"error":`} {
			for _, readFailure := range []bool{false, true} {
				if bodyText == `{"error":` && !readFailure {
					continue
				}
				name := entry.name + "/" + bodyText
				if readFailure {
					name += "/read failure"
				}
				t.Run(
					name, func(t *testing.T) {
						ctx, records := xaiOutcomeContext(t)
						ctx = usage.WithRequestedModelAlias(ctx, "synthetic-client-model")
						cfg := &config.Config{Home: config.HomeConfig{Enabled: true}}
						credential := antigravityCountTokensAuth(
							"https://antigravity-401.synthetic.invalid",
							t.Name(),
						)
						credential.Index = "synthetic-home-index"
						credential.Status = coreauth.StatusActive
						payload, err := json.Marshal(
							map[string]any{
								"auth_index": credential.Index, "model": entry.model, "auth": credential,
							},
						)
						if err != nil {
							t.Fatal(err)
						}
						hook := &primaryErrorResults{}
						manager := coreauth.NewManager(nil, nil, hook)
						manager.SetConfig(cfg)
						manager.SetRetryConfig(2, 0, 2)
						manager.RegisterExecutor(NewAntigravityExecutor(cfg))
						manager.PublishHomeDispatch(
							&homeOpenAICompatDispatcher{payload: payload}, executionregistry.New(), 1,
						)
						t.Cleanup(
							func() {
								if err := manager.StopDeferredAuthRecreates(context.Background()); err != nil {
									t.Error(err)
								}
							},
						)
						calls := 0
						readCause := errors.New("synthetic unauthorized response read failure")
						transport := &auditClaudeContractTransport{
							respond: func(request *http.Request) (*http.Response, error) {
								calls++
								if request.URL.Host != "antigravity-401.synthetic.invalid" ||
									request.URL.Path != entry.path ||
									request.Header.Get("Authorization") != "Bearer test-token" {
									t.Fatalf("unexpected attempt endpoint or token: %s", request.URL)
								}
								var body io.Reader = strings.NewReader(bodyText)
								if readFailure {
									body = io.MultiReader(body, iotest.ErrReader(readCause))
								}
								return &http.Response{
									StatusCode: http.StatusUnauthorized,
									Header:     http.Header{"Content-Type": {"application/json"}},
									Body:       io.NopCloser(body), Request: request,
								}, nil
							},
						}
						ctx = util.WithRoundTripper(ctx, transport)
						if newAntigravityHTTPClient(ctx, cfg, credential, 0).Transport != transport {
							t.Fatal("Antigravity unauthorized transport is not memory-isolated")
						}
						req := core.Request{
							Model: entry.model,
							Payload: []byte(
								`{"request":{"contents":[{"role":"user","parts":[{"text":"synthetic"}]}]}}`,
							),
						}
						opts := core.Options{
							SourceFormat: translator.FormatAntigravity, ResponseFormat: translator.FormatAntigravity,
							Metadata: map[string]any{core.RequestedModelMetadataKey: "synthetic-client-model"},
						}
						switch entry.name {
						case "count":
							_, err = manager.ExecuteCount(ctx, []string{"antigravity"}, req, opts)
						case "stream":
							_, err = manager.ExecuteStream(ctx, []string{"antigravity"}, req, opts)
						default:
							_, err = manager.Execute(ctx, []string{"antigravity"}, req, opts)
						}
						if testExecutorStatusCode(err) != http.StatusUnauthorized || err.Error() != bodyText ||
							calls != 1 {
							t.Errorf(
								"status=%d error=%v expected body=%q calls=%d",
								testExecutorStatusCode(err),
								err,
								bodyText,
								calls,
							)
						}
						if readFailure && !errors.Is(err, readCause) {
							t.Errorf("read failure cause was lost: %v", err)
						}
						if len(hook.results) != 1 || hook.results[0].Error == nil ||
							hook.results[0].Error.HTTPStatus != http.StatusUnauthorized ||
							hook.results[0].Disposition.DispatchPhase != core.DispatchPhaseResponseStarted {
							t.Errorf("Home attempt result=%+v", hook.results)
						}
						captured := records()
						if len(captured) != 1 {
							t.Fatalf("failed attempt published %d records", len(captured))
						}
						got := captured[0]
						if !got.Failed || got.Fail.StatusCode != http.StatusUnauthorized ||
							got.Fail.Body != bodyText ||
							got.AuthIndex !=
								credential.Index ||
							got.AccessTokenSHA256 !=
								coreauth.AccessTokenSHA256(credential) ||
							got.Model !=
								entry.model ||
							got.Alias !=
								"synthetic-client-model" ||
							got.Detail.TotalTokens !=
								0 ||
							(entry.name == "count") == usage.GenerateEnabled(got.Generate) {
							t.Fatalf(
								"unauthorized attempt lost its status, body or token ownership: status="+
									"%d body=%q index=%q model=%q alias=%q tokens=%d generate=%t",
								got.Fail.StatusCode, got.Fail.Body, got.AuthIndex, got.Model, got.Alias,
								got.Detail.TotalTokens, usage.GenerateEnabled(got.Generate),
							)
						}
					},
				)
			}
		}
	}
}
