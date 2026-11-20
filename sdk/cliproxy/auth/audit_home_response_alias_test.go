package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/home"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executionregistry"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
)

type auditHomeAliasDispatcher struct {
	roundsHomeDispatcher
	force  bool
	prefix string
	alias  string
	calls  int
}

func (d *auditHomeAliasDispatcher) RPopAuthWithConstraints(
	_ context.Context,
	_, _ string,
	_ http.Header,
	_ int,
	_ home.AuthDispatchConstraints,
) ([]byte, error) {
	d.calls++
	return json.Marshal(
		homeAuthDispatchResponse{
			Model: "upstream-model", Provider: "test", ForceMapping: d.force, OriginalAlias: d.alias,
			Auth: Auth{ID: "audit-home-alias", Provider: "test", Prefix: d.prefix},
		},
	)
}

type auditHomeAliasExecutor struct {
	schedulerTestExecutor
	models []string
	retain bool
}

func (e *auditHomeAliasExecutor) Execute(
	_ context.Context,
	_ *Auth,
	req core.Request,
	opts core.Options,
) (core.Response, error) {
	e.models = append(e.models, req.Model)
	if e.retain {
		opts.ExecutionLifecycle.(*HomeDispatchSelection).Retain()
	}
	return core.Response{Payload: []byte(`{"model":"served-model","id":"synthetic-response"}`)}, nil
}

func (e *auditHomeAliasExecutor) CountTokens(
	ctx context.Context,
	a *Auth,
	req core.Request,
	opts core.Options,
) (core.Response, error) {
	return e.Execute(ctx, a, req, opts)
}

// noinspection GoDfaErrorMayBeNotNil
func (e *auditHomeAliasExecutor) ExecuteStream(
	ctx context.Context,
	a *Auth,
	req core.Request,
	opts core.Options,
) (*core.StreamResult, error) {
	r, err := e.Execute(ctx, a, req, opts)
	chunks := make(chan core.StreamChunk, 1)
	chunks <- core.StreamChunk{Payload: append(append([]byte("data: "), r.Payload...), '\n', '\n')}
	close(chunks)
	return &core.StreamResult{Chunks: chunks}, err
}

// noinspection GoDfaErrorMayBeNotNil
func auditHomeAliasCall(
	ctx context.Context,
	manager *Manager,
	operation, model string,
	metadata map[string]any,
) (string, error) {
	req := core.Request{Model: model, Payload: []byte(`{"model":"` + model + `"}`)}
	opts := core.Options{
		SourceFormat: translator.FormatOpenAI, ResponseFormat: translator.FormatOpenAI, Metadata: metadata,
	}
	if operation == "stream" {
		result, err := manager.ExecuteStream(ctx, []string{"test"}, req, opts)
		if err != nil {
			return "", err
		}
		var output strings.Builder
		for chunk := range result.Chunks {
			output.Write(chunk.Payload)
			if chunk.Err != nil {
				return output.String(), chunk.Err
			}
		}
		return output.String(), nil
	}
	var result core.Response
	var err error
	if operation == "count" {
		result, err = manager.ExecuteCount(ctx, []string{"test"}, req, opts)
	} else {
		result, err = manager.Execute(ctx, []string{"test"}, req, opts)
	}
	return string(result.Payload), err
}

func TestAuditHomeForceMappingKeepsClientVisibleAlias(t *testing.T) {
	for _, operation := range []string{"execute", "count", "stream"} {
		for _, force := range []bool{false, true} {
			for _, tc := range []struct {
				name      string
				request   string
				prefix    string
				homeAlias string
				requested any
				visible   string
			}{
				{"metadata", "route-model", "", "route-model", "client-alias", "client-alias"},
				{"metadata-bytes", "route-model", "", "route-model", []byte(" client-alias "), "client-alias"},
				{"prefix", "team/alias-a", "team", "alias-a", nil, "team/alias-a"},
				{"prefix-suffix", "team/alias-a(high)", "team", "alias-a", nil, "team/alias-a(high)"},
			} {
				t.Run(
					fmt.Sprintf("%s/force=%t/%s", operation, force, tc.name), func(t *testing.T) {
						dispatcher := &auditHomeAliasDispatcher{force: force, prefix: tc.prefix, alias: tc.homeAlias}
						manager := NewManager(nil, nil, nil)
						manager.SetConfig(&config.Config{Home: config.HomeConfig{Enabled: true}})
						manager.PublishHomeDispatch(dispatcher, executionregistry.New(), 1)
						execute := &auditHomeAliasExecutor{}
						manager.RegisterExecutor(execute)
						metadata := map[string]any{}
						if tc.requested != nil {
							metadata[core.RequestedModelMetadataKey] = tc.requested
						}
						output, err := auditHomeAliasCall(t.Context(), manager, operation, tc.request, metadata)
						if err != nil || len(execute.models) != 1 || execute.models[0] != "upstream-model" ||
							dispatcher.calls != 1 {
							t.Fatalf("error=%v models=%v dispatches=%d", err, execute.models, dispatcher.calls)
						}
						want := "served-model"
						if force {
							want = tc.visible
						}
						if !strings.Contains(output, `"model":"`+want+`"`) {
							t.Fatalf("response=%s want model=%s", output, want)
						}
					},
				)
			}
		}
	}
}

func TestAuditHomeRetainedAliasTracksEachClientTurn(t *testing.T) {
	for _, operation := range []string{"execute", "stream"} {
		t.Run(
			operation, func(t *testing.T) {
				dispatcher := &auditHomeAliasDispatcher{force: true, prefix: "team", alias: "alias-a"}
				manager := NewManager(nil, nil, nil)
				manager.SetConfig(&config.Config{Home: config.HomeConfig{Enabled: true}})
				manager.PublishHomeDispatch(dispatcher, executionregistry.New(), 1)
				execute := &auditHomeAliasExecutor{retain: true}
				manager.RegisterExecutor(execute)
				const sessionID = "audit-retained-alias-session"
				t.Cleanup(func() { manager.CloseExecutionSession(sessionID) })
				ctx := core.WithDownstreamWebsocket(t.Context())
				for turn, model := range []string{"team/alias-a", "team/alias-a(high)"} {
					output, err := auditHomeAliasCall(
						ctx, manager, operation, model, map[string]any{core.ExecutionSessionMetadataKey: sessionID},
					)
					if err != nil || !strings.Contains(output, `"model":"`+model+`"`) {
						t.Fatalf("turn=%d response=%s error=%v", turn, output, err)
					}
				}
				if dispatcher.calls != 1 || len(execute.models) != 2 || execute.models[0] != "upstream-model" ||
					execute.models[1] != "upstream-model(high)" {
					t.Fatalf("dispatches=%d models=%v", dispatcher.calls, execute.models)
				}
			},
		)
	}
}
