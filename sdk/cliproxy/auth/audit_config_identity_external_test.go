package auth_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/watcher/synthesizer"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type auditConfigIdentityExecutor struct {
	auth.ProviderExecutor
	provider    string
	seen        core.Request
	seenSession string
	calls       int
}

func (e *auditConfigIdentityExecutor) Identifier() string { return e.provider }
func (e *auditConfigIdentityExecutor) Execute(
	ctx context.Context,
	_ *auth.Auth,
	req core.Request,
	_ core.Options,
) (core.Response, error) {
	e.seen = req
	e.seenSession = util.SessionIDFromContext(ctx)
	e.calls++
	return core.Response{Payload: []byte(`{}`)}, nil
}
func (e *auditConfigIdentityExecutor) CountTokens(
	ctx context.Context,
	credential *auth.Auth,
	req core.Request,
	opts core.Options,
) (core.Response, error) {
	return e.Execute(ctx, credential, req, opts)
}
func (e *auditConfigIdentityExecutor) ExecuteStream(
	ctx context.Context,
	credential *auth.Auth,
	req core.Request,
	opts core.Options,
) (*core.StreamResult, error) {
	_, err := e.Execute(ctx, credential, req, opts)
	ch := make(chan core.StreamChunk, 1)
	ch <- core.StreamChunk{Payload: []byte(`{}`)}
	close(ch)
	return &core.StreamResult{Chunks: ch}, err
}

func TestAuditSynthesizedConfigIdentityReachesEveryManagerEntry(t *testing.T) {
	for _, provider := range []string{"claude", "synthetic-pool"} {
		providerKey := provider
		if provider != "claude" {
			providerKey = "openai-compatible-" + provider
		}
		for _, keyless := range []bool{false, true} {
			if provider == "claude" && keyless {
				continue
			}
			for index := range 2 {
				for _, mode := range []string{"execute", "count", "stream"} {
					t.Run(
						fmt.Sprintf(
							"%s/keyless=%t/index=%d/%s",
							provider,
							keyless,
							index,
							mode,
						),
						func(t *testing.T) {
							cfg := &config.Config{}
							for i, upstream := range []string{"first-upstream", "second-upstream"} {
								levels := []string{"low"}
								if i == 1 {
									levels = []string{"high"}
								}
								thinking := &registry.ThinkingSupport{Levels: levels}
								if provider == "claude" {
									cfg.ClaudeKey = append(
										cfg.ClaudeKey, config.ClaudeKey{
											APIKey: "synthetic-key", BaseURL: "http://127.0.0.1:1",
											Models: []config.ClaudeModel{
												{
													Name: upstream, Alias: "public-model", IsCompat: true,
													Thinking: thinking,
												},
											},
										},
									)
								} else {
									entry := config.OpenAICompatibility{
										Name: provider, BaseURL: "http://127.0.0.1:1",
										Models: []config.OpenAICompatibilityModel{
											{
												Name: upstream, Alias: "public-model", IsCompat: true,
												Thinking: thinking,
											},
										},
									}
									if !keyless {
										entry.APIKeyEntries =
											[]config.OpenAICompatibilityAPIKey{{APIKey: "synthetic-key"}}
									}
									cfg.OpenAICompatibility = append(cfg.OpenAICompatibility, entry)
								}
							}
							credentials, err := synthesizer.NewConfigSynthesizer().Synthesize(
								&synthesizer.SynthesisContext{
									Config: cfg, Now: time.Now(), IDGenerator: synthesizer.NewStableIDGenerator(),
								},
							)
							if err != nil {
								t.Fatal(err)
							}
							var selected *auth.Auth
							for _, candidate := range credentials {
								if candidate.Provider == providerKey &&
									candidate.Attributes[auth.AttributeConfigIndex] == fmt.Sprint(index) {
									selected = candidate
								}
							}
							if selected == nil || selected.AuthKind() != auth.AuthKindAPIKey {
								t.Fatalf("synthesized credential has no API-key identity: %+v", selected)
							}
							if kind, _ := selected.AccountInfo(); kind != "api_key" {
								t.Fatalf("account kind=%s", kind)
							}
							manager := auth.NewManager(nil, nil, nil)
							manager.SetHomeClientProvider(func() auth.HomeClient { return nil })
							manager.SetConfig(cfg)
							capture := &auditConfigIdentityExecutor{provider: providerKey}
							manager.RegisterExecutor(capture)
							selected, err = manager.Register(t.Context(), selected)
							if err != nil {
								t.Fatal(err)
							}
							upstream, level := "first-upstream", "low"
							if index == 1 {
								upstream, level = "second-upstream", "high"
							}
							reg := registry.GetGlobalRegistry()
							reg.RegisterClient(
								selected.ID, providerKey, []*registry.ModelInfo{
									{
										ID: "public-model", CatalogName: upstream, IsCompat: true,
										Thinking: &registry.ThinkingSupport{Levels: []string{level}},
									},
								},
							)
							t.Cleanup(func() { reg.UnregisterClient(selected.ID) })
							manager.RefreshSchedulerEntry(selected.ID)
							request := core.Request{Model: "public-model"}
							switch mode {
							case "execute":
								_, err = manager.Execute(
									t.Context(),
									[]string{providerKey},
									request,
									core.Options{},
								)
							case "count":
								_, err = manager.ExecuteCount(
									t.Context(), []string{providerKey}, request, core.Options{},
								)
							case "stream":
								var result *core.StreamResult
								result, err = manager.ExecuteStream(
									t.Context(), []string{providerKey}, request, core.Options{},
								)
								if err == nil {
									for range result.Chunks {
									}
								}
							}
							info, bound := auth.ResolvedAPIKeyModelInfo(capture.seen)
							if err != nil || capture.calls != 1 || capture.seen.Model != upstream || !bound ||
								info == nil ||
								!info.IsCompat ||
								info.Thinking == nil ||
								len(info.Thinking.Levels) != 1 ||
								info.Thinking.Levels[0] != level {
								t.Fatalf(
									"calls=%d model=%s bound=%t info=%+v error=%v",
									capture.calls,
									capture.seen.Model,
									bound,
									info,
									err,
								)
							}
						},
					)
				}
			}
		}
	}
}
