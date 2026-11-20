package auth_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/watcher/synthesizer"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
)

type auditExecutorKeyParser struct {
	logical string
	key     string
	id      string
}

func (p auditExecutorKeyParser) ParseAuth(
	_ context.Context,
	_ pluginapi.AuthParseRequest,
) (*auth.Auth, bool, error) {
	return &auth.Auth{
		ID: p.id, Provider: p.logical, Attributes: map[string]string{
			"compat_name":  "Audit-Pool",
			"provider_key": p.key,
		},
	}, true, nil
}

type auditExecutorKeySelector struct {
	auth.FillFirstSelector
	id string
}

func (s *auditExecutorKeySelector) LookupAffinity(
	_, _, _ string,
	filters ...func(string) bool,
) (string, string) {
	for _, filter := range filters {
		if !filter(s.id) {
			return "", "unbound"
		}
	}
	return s.id, "bound"
}

type auditExecutorKeyExecutor struct {
	auth.ProviderExecutor
	calls   int
	logical string
	model   string
}

func (*auditExecutorKeyExecutor) Identifier() string { return "audit-pool" }
func (e *auditExecutorKeyExecutor) Execute(
	_ context.Context,
	a *auth.Auth,
	req core.Request,
	_ core.Options,
) (core.Response, error) {
	e.calls++
	e.logical = a.Provider
	e.model = req.Model
	return core.Response{Payload: []byte(`{"model":"audit-key-model"}`)}, nil
}
func (e *auditExecutorKeyExecutor) CountTokens(
	ctx context.Context,
	a *auth.Auth,
	req core.Request,
	opts core.Options,
) (core.Response, error) {
	return e.Execute(ctx, a, req, opts)
}

// noinspection GoDfaErrorMayBeNotNil
func (e *auditExecutorKeyExecutor) ExecuteStream(
	ctx context.Context,
	a *auth.Auth,
	req core.Request,
	opts core.Options,
) (*core.StreamResult, error) {
	r, err := e.Execute(ctx, a, req, opts)
	chunks := make(chan core.StreamChunk, 1)
	chunks <- core.StreamChunk{Payload: r.Payload}
	close(chunks)
	return &core.StreamResult{Chunks: chunks}, err
}

func TestAuditFileCompatExecutorKeyReachesPublicManager(t *testing.T) {
	for _, logical := range []string{"openai-compatibility", "plugin-logical", "audit-pool"} {
		for _, key := range []string{"audit-pool", ""} {
			for _, custom := range []bool{false, true} {
				for _, operation := range []string{"pick", "execute", "count", "stream"} {
					t.Run(
						fmt.Sprintf(
							"%s/key=%s/custom=%t/%s",
							logical,
							key,
							custom,
							operation,
						),
						func(t *testing.T) {
							dir := t.TempDir()
							if err := os.WriteFile(
								filepath.Join(dir, "synthetic.json"), []byte(`{"type":"synthetic-plugin"}`), 0600,
							); err != nil {
								t.Fatal(err)
							}
							id := "audit-key-" + t.Name()
							cfg := &config.Config{}
							credentials, err := synthesizer.NewFileSynthesizer().Synthesize(
								&synthesizer.SynthesisContext{
									Config: cfg, AuthDir: dir, Now: time.Now(),
									PluginAuthParser: auditExecutorKeyParser{
										logical: logical,
										key:     key,
										id:      id,
									},
								},
							)
							if err != nil || len(credentials) != 1 || credentials[0].Provider != logical {
								t.Fatalf("synthesized=%v error=%v", credentials, err)
							}
							var selector auth.Selector = &auth.RoundRobinSelector{}
							if custom {
								selector = &auditExecutorKeySelector{id: id}
							}
							manager := auth.NewManager(nil, selector, nil)
							manager.SetHomeClientProvider(func() auth.HomeClient { return nil })
							manager.SetConfig(cfg)
							execute := &auditExecutorKeyExecutor{}
							manager.RegisterExecutor(execute)
							credential, err := manager.Register(t.Context(), credentials[0])
							if err != nil || credential.Provider != logical {
								t.Fatalf("registered=%v error=%v", credential, err)
							}
							const model = "audit-key-model"
							reg := registry.GetGlobalRegistry()
							reg.RegisterClient(id, "audit-pool", []*registry.ModelInfo{{ID: model}})
							t.Cleanup(func() { reg.UnregisterClient(id) })
							manager.RefreshSchedulerEntry(id)
							providers := reg.GetModelProviders(model)
							if len(providers) != 1 || providers[0] != "audit-pool" {
								t.Fatalf("published providers=%v", providers)
							}
							request := core.Request{Model: model}
							switch operation {
							case "pick":
								var chosen *auth.Auth
								var selected auth.ProviderExecutor
								chosen, selected, err = manager.PickNext(
									t.Context(), providers[0], model, core.Options{}, nil,
								)
								if err == nil &&
									(chosen.ID != id || chosen.Provider != logical || selected != execute) {
									t.Fatalf("chosen=%v executor=%T", chosen, selected)
								}
							case "execute":
								_, err = manager.Execute(t.Context(), providers, request, core.Options{})
							case "count":
								_, err = manager.ExecuteCount(t.Context(), providers, request, core.Options{})
							case "stream":
								var result *core.StreamResult
								result, err = manager.ExecuteStream(
									t.Context(),
									providers,
									request,
									core.Options{},
								)
								if err == nil {
									var output strings.Builder
									for chunk := range result.Chunks {
										output.Write(chunk.Payload)
										if chunk.Err != nil {
											err = chunk.Err
										}
									}
									if output.String() != `{"model":"audit-key-model"}` {
										t.Fatalf("stream=%s", output.String())
									}
								}
							}
							if err != nil {
								t.Fatal(err)
							}
							if operation != "pick" &&
								(execute.calls != 1 || execute.logical != logical || execute.model != model) {
								t.Fatalf(
									"calls=%d logical=%s model=%s",
									execute.calls,
									execute.logical,
									execute.model,
								)
							}
							if custom {
								chosen, status := manager.LookupSessionAffinity(
									"audit-pool", model, "synthetic-session",
								)
								if chosen == nil || chosen.ID != id || chosen.Provider != logical ||
									status != "bound" {
									t.Fatalf("affinity=%v status=%s", chosen, status)
								}
							}
							if _, _, err := manager.PickNext(
								t.Context(), "audit-pool", model, core.Options{}, map[string]struct{}{id: {}},
							); err == nil {
								t.Fatal("tried credential remained selectable")
							}
						},
					)
				}
			}
		}
	}
}
