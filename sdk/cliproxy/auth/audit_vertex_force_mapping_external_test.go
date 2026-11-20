package auth_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/watcher/synthesizer"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
)

type auditVertexAliasExecutor struct {
	auth.ProviderExecutor
	model string
	calls int
}

func (*auditVertexAliasExecutor) Identifier() string { return "vertex" }
func (e *auditVertexAliasExecutor) Execute(
	_ context.Context,
	_ *auth.Auth,
	req core.Request,
	_ core.Options,
) (core.Response, error) {
	e.model = req.Model
	e.calls++
	return core.Response{Payload: []byte(`{"model":"served-vertex-model"}`)}, nil
}
func (e *auditVertexAliasExecutor) CountTokens(
	ctx context.Context,
	a *auth.Auth,
	req core.Request,
	opts core.Options,
) (core.Response, error) {
	return e.Execute(ctx, a, req, opts)
}

// noinspection GoDfaErrorMayBeNotNil
func (e *auditVertexAliasExecutor) ExecuteStream(
	ctx context.Context,
	a *auth.Auth,
	req core.Request,
	opts core.Options,
) (*core.StreamResult, error) {
	r, err := e.Execute(ctx, a, req, opts)
	ch := make(chan core.StreamChunk, 1)
	ch <- core.StreamChunk{Payload: append(append([]byte("data: "), r.Payload...), '\n', '\n')}
	close(ch)
	return &core.StreamResult{Chunks: ch}, err
}

func TestAuditVertexForceMappingReachesPublicManager(t *testing.T) {
	for _, force := range []bool{false, true} {
		for _, requestSuffix := range []string{"", "(high)"} {
			for _, configuredSuffix := range []string{"", "(low)"} {
				for _, operation := range []string{"execute", "count", "stream"} {
					t.Run(
						fmt.Sprintf(
							"force=%t/request=%s/config=%s/%s", force, requestSuffix, configuredSuffix, operation,
						), func(t *testing.T) {
							const alias = "vertex-public"
							const upstream = "gemini-2.5-pro"
							cfg := &config.Config{
								VertexCompatAPIKey: []config.VertexCompatKey{
									{
										APIKey: "synthetic-key", BaseURL: "http://127.0.0.1:1",
										Models: []config.VertexCompatModel{
											{
												Name: upstream + configuredSuffix, Alias: alias, ForceMapping: force,
											},
										},
									},
								},
							}
							credentials, err := synthesizer.NewConfigSynthesizer().Synthesize(
								&synthesizer.SynthesisContext{
									Config: cfg, Now: time.Now(), IDGenerator: synthesizer.NewStableIDGenerator(),
								},
							)
							if err != nil {
								t.Fatal(err)
							}
							var credential *auth.Auth
							for _, candidate := range credentials {
								if candidate.Provider == "vertex" {
									credential = candidate
								}
							}
							if credential == nil {
								t.Fatal("no synthesized Vertex credential")
							}
							manager := auth.NewManager(nil, nil, nil)
							manager.SetHomeClientProvider(func() auth.HomeClient { return nil })
							manager.SetConfig(cfg)
							execute := &auditVertexAliasExecutor{}
							manager.RegisterExecutor(execute)
							credential, err = manager.Register(t.Context(), credential)
							if err != nil {
								t.Fatal(err)
							}
							reg := registry.GetGlobalRegistry()
							reg.RegisterClient(
								credential.ID, "vertex", []*registry.ModelInfo{
									{
										ID: alias, CatalogName: upstream,
										Thinking: &registry.ThinkingSupport{Levels: []string{"low", "high"}},
									},
								},
							)
							t.Cleanup(func() { reg.UnregisterClient(credential.ID) })
							manager.RefreshSchedulerEntry(credential.ID)
							request := core.Request{
								Model:   alias + requestSuffix,
								Payload: []byte(`{"model":"` + alias + requestSuffix + `"}`),
							}
							opts := core.Options{
								SourceFormat: translator.FormatOpenAI, ResponseFormat: translator.FormatOpenAI,
							}
							var output strings.Builder
							switch operation {
							case "execute":
								var r core.Response
								r, err = manager.Execute(t.Context(), []string{"vertex"}, request, opts)
								output.Write(r.Payload)
							case "count":
								var r core.Response
								r, err = manager.ExecuteCount(t.Context(), []string{"vertex"}, request, opts)
								output.Write(r.Payload)
							case "stream":
								var r *core.StreamResult
								r, err = manager.ExecuteStream(t.Context(), []string{"vertex"}, request, opts)
								if err == nil {
									for chunk := range r.Chunks {
										output.Write(chunk.Payload)
										if chunk.Err != nil {
											err = chunk.Err
										}
									}
								}
							}
							want := upstream + requestSuffix
							if configuredSuffix != "" {
								want = upstream + configuredSuffix
							}
							if err != nil || execute.calls != 1 || execute.model != want {
								t.Fatalf("error=%v calls=%d model=%s want=%s", err, execute.calls, execute.model, want)
							}
							if operation != "count" {
								wantResponse := "served-vertex-model"
								if force {
									wantResponse = alias
								}
								if !strings.Contains(output.String(), `"model":"`+wantResponse+`"`) {
									t.Fatalf("forced=%t response=%s", force, output.String())
								}
							}
						},
					)
				}
			}
		}
	}
}
