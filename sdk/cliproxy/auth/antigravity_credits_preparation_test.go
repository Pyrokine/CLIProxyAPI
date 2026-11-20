package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type originalCreditsPreparationExecutor struct {
	forceMappingCreditsFallbackExecutor
	prepareErr   error
	failedAuthID string
	prepareCalls int
	executeCalls int
}

func (*originalCreditsPreparationExecutor) ShouldPrepareRequestAuth(auth *Auth) bool {
	return auth != nil && auth.Metadata["project_id"] == nil
}

func (e *originalCreditsPreparationExecutor) PrepareRequestAuth(
	ctx context.Context,
	auth *Auth,
) (*Auth, error) {
	e.prepareCalls++
	if !AntigravityCreditsRequested(ctx) {
		return nil, errors.New("credits preparation lost its request context")
	}
	if e.prepareErr != nil && (e.failedAuthID == "" || e.failedAuthID == auth.ID) {
		return auth, e.prepareErr
	}
	prepared := auth.Clone()
	prepared.Metadata["project_id"] = "prepared-project"
	return prepared, nil
}

func (e *originalCreditsPreparationExecutor) Execute(
	ctx context.Context,
	auth *Auth,
	req executor.Request,
	opts executor.Options,
) (executor.Response, error) {
	e.executeCalls++
	if auth.Metadata["project_id"] != "prepared-project" {
		return executor.Response{}, errors.New("credits execution received an unprepared credential")
	}
	if req.Metadata["original-credits"] != "kept" ||
		string(req.Payload) != `{"input":"original credits payload"}` {
		return executor.Response{}, errors.New("credits execution lost the original request")
	}
	return e.forceMappingCreditsFallbackExecutor.Execute(ctx, auth, req, opts)
}

func (e *originalCreditsPreparationExecutor) ExecuteStream(
	ctx context.Context,
	auth *Auth,
	req executor.Request,
	opts executor.Options,
) (*executor.StreamResult, error) {
	e.executeCalls++
	if auth.Metadata["project_id"] != "prepared-project" {
		return nil, errors.New("credits stream received an unprepared credential")
	}
	if req.Metadata["original-credits"] != "kept" ||
		string(req.Payload) != `{"input":"original credits payload"}` {
		return nil, errors.New("credits stream lost the original request")
	}
	return e.forceMappingCreditsFallbackExecutor.ExecuteStream(ctx, auth, req, opts)
}

func TestOriginalCreditsPreparationRespectsRetryAuthority(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		retry bool
	}{
		{name: "not sent", err: errors.New("original preparation failure"), retry: true},
		{name: "canceled", err: context.Canceled},
		{
			name: "unknown", err: errorWithState(
				&Error{HTTPStatus: http.StatusBadGateway, Message: "unknown preparation outcome"},
				errorState{Outcome: executor.ExecutionOutcomeUnknown, RetryDirective: executor.RetryDirectiveNextAuth},
			),
		},
		{
			name: "no retry", err: errorWithState(
				&Error{Message: "preparation stops this request"}, errorState{
					Outcome:        executor.ExecutionOutcomeRejectedBeforeProcessing,
					RetryDirective: executor.RetryDirectiveNoRetry,
				},
			),
		},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(
				fmt.Sprintf("%s/stream=%t", tc.name, stream), func(t *testing.T) {
					const model = "claude-opus-4-6-thinking"
					manager := NewManager(nil, nil, nil)
					manager.SetHomeClientProvider(func() HomeClient { return nil })
					manager.SetConfig(
						&config.Config{QuotaExceeded: config.QuotaExceeded{AntigravityCredits: true}},
					)
					capture := &originalCreditsPreparationExecutor{
						id:         "antigravity",
						prepareErr: tc.err, failedAuthID: "a-credits-prepare",
					}
					manager.RegisterExecutor(capture)
					models := registry.GetGlobalRegistry()
					for _, id := range []string{"a-credits-prepare", "b-credits-ready"} {
						auth, err := manager.Register(
							t.Context(), &Auth{
								ID: id, Provider: "antigravity",
								Metadata: map[string]any{"access_token": "synthetic"},
							},
						)
						if err != nil {
							t.Fatal(err)
						}
						models.RegisterClient(id, "antigravity", []*registry.ModelInfo{{ID: model}})
						t.Cleanup(
							func() { models.UnregisterClient(id); antigravityCreditsHintByAuth.Delete(id) },
						)
						manager.MarkResult(
							t.Context(), Result{
								AuthID: id, AuthGeneration: auth.Generation,
								Provider: "antigravity", Model: model, RetryAfter: new(time.Hour),
								Error: &Error{
									HTTPStatus: http.StatusTooManyRequests,
									Message:    "quota exhausted",
								},
							},
						)
						SetAntigravityCreditsHint(
							id, AntigravityCreditsHint{Known: true, Available: true, UpdatedAt: time.Now()},
						)
					}
					req := executor.Request{
						Model: model, Payload: []byte(`{"input":"original credits payload"}`),
						Metadata: map[string]any{"original-credits": "kept"},
					}
					var err error
					if stream {
						var result *executor.StreamResult
						result, err = manager.ExecuteStream(
							t.Context(), []string{"antigravity"}, req, executor.Options{},
						)
						if err == nil {
							for chunk := range result.Chunks {
								err = errors.Join(err, chunk.Err)
							}
						}
					} else {
						_, err = manager.Execute(t.Context(), []string{"antigravity"}, req, executor.Options{})
					}
					if tc.retry {
						if err != nil || capture.prepareCalls != 2 || capture.executeCalls != 1 {
							t.Fatalf(
								"unsent failure: error=%v prepares=%d executions=%d", err, capture.prepareCalls,
								capture.executeCalls,
							)
						}
					} else if !errors.Is(err, tc.err) || capture.prepareCalls != 1 || capture.executeCalls != 0 {
						t.Fatalf(
							"terminal preparation: error=%v prepares=%d executions=%d", err, capture.prepareCalls,
							capture.executeCalls,
						)
					}
				},
			)
		}
	}
}

func TestCreditsFallbackWaitsForUnavailableFreeCandidates(t *testing.T) {
	for _, mode := range []string{"execute", "stream"} {
		for _, scenario := range []string{"all-free-exhausted", "credential-budget", "pinned-free-exhausted"} {
			t.Run(
				mode+"/"+scenario, func(t *testing.T) {
					const model = "claude-credits-free-candidates"
					manager := NewManager(nil, &RoundRobinSelector{}, nil)
					manager.SetHomeClientProvider(func() HomeClient { return nil })
					manager.SetConfig(
						&config.Config{QuotaExceeded: config.QuotaExceeded{AntigravityCredits: true}},
					)
					limit := 0
					if scenario != "all-free-exhausted" {
						limit = 1
					}
					manager.SetRetryConfig(0, 0, limit)
					capture := &forceMappingCreditsFallbackExecutor{id: "antigravity"}
					manager.RegisterExecutor(capture)
					for _, id := range []string{"a-free-credits-" + t.Name(), "b-free-credits-" + t.Name()} {
						if _, err := manager.Register(
							t.Context(), &Auth{ID: id, Provider: "antigravity", Status: StatusActive},
						); err != nil {
							t.Fatal(err)
						}
						registry.GetGlobalRegistry().RegisterClient(
							id, "antigravity", []*registry.ModelInfo{{ID: model}},
						)
						SetAntigravityCreditsHint(
							id, AntigravityCreditsHint{Known: true, Available: true, UpdatedAt: time.Now()},
						)
						t.Cleanup(
							func() {
								registry.GetGlobalRegistry().UnregisterClient(id)
								antigravityCreditsHintByAuth.Delete(id)
							},
						)
					}
					opts := executor.Options{}
					if scenario == "pinned-free-exhausted" {
						opts.Metadata = map[string]any{
							executor.PinnedAuthMetadataKey: "a-free-credits-" + t.Name(),
						}
					}
					var err error
					var flags []bool
					if mode == "stream" {
						var result *executor.StreamResult
						result, err = manager.ExecuteStream(
							t.Context(), []string{"antigravity"}, executor.Request{Model: model}, opts,
						)
						if err == nil {
							for chunk := range result.Chunks {
								err = errors.Join(err, chunk.Err)
							}
						}
						flags = capture.StreamCreditsRequested()
					} else {
						_, err = manager.Execute(
							t.Context(), []string{"antigravity"}, executor.Request{Model: model}, opts,
						)
						flags = capture.ExecuteCreditsRequested()
					}
					wantFree, wantPaid := 2, scenario != "credential-budget"
					if scenario != "all-free-exhausted" {
						wantFree = 1
					}
					wantCalls := wantFree
					if wantPaid {
						wantCalls++
					}
					if len(flags) != wantCalls || (wantPaid && (err != nil || !flags[len(flags)-1])) ||
						(!wantPaid && (err == nil || statusCodeFromError(err) != http.StatusServiceUnavailable)) {
						t.Fatalf(
							"free candidates: credits=%v error=%v, want free=%d paid=%t",
							flags,
							err,
							wantFree,
							wantPaid,
						)
					}
					for _, flag := range flags[:wantFree] {
						if flag {
							t.Fatal("paid attempt preceded free candidate exhaustion")
						}
					}
				},
			)
		}
	}
}

func TestOriginalCreditsFallbackPreparesSuspendedCredential(t *testing.T) {
	for _, mode := range []string{"execute", "stream"} {
		for _, prepareErr := range []error{nil, errors.New("original credits preparation failed"), context.Canceled} {
			t.Run(
				fmt.Sprintf("%s/prepare=%v", mode, prepareErr), func(t *testing.T) {
					const model = "claude-opus-4-6-thinking"
					manager := NewManager(nil, nil, nil)
					manager.SetHomeClientProvider(func() HomeClient { return nil })
					manager.SetConfig(
						&config.Config{QuotaExceeded: config.QuotaExceeded{AntigravityCredits: true}},
					)
					capture := &originalCreditsPreparationExecutor{
						id:         "antigravity",
						prepareErr: prepareErr,
					}
					manager.RegisterExecutor(capture)
					auth, err := manager.Register(
						t.Context(), &Auth{
							ID: t.Name(), Provider: "antigravity",
							Metadata: map[string]any{"access_token": "synthetic"},
						},
					)
					if err != nil {
						t.Fatal(err)
					}
					models := registry.GetGlobalRegistry()
					models.RegisterClient(auth.ID, "antigravity", []*registry.ModelInfo{{ID: model}})
					t.Cleanup(
						func() {
							models.UnregisterClient(auth.ID)
							antigravityCreditsHintByAuth.Delete(auth.ID)
						},
					)
					manager.MarkResult(
						t.Context(), Result{
							AuthID: auth.ID, AuthGeneration: auth.Generation,
							Provider: "antigravity", Model: model, RetryAfter: new(time.Hour),
							Error: &Error{HTTPStatus: http.StatusTooManyRequests, Message: "quota exhausted"},
						},
					)
					SetAntigravityCreditsHint(
						auth.ID, AntigravityCreditsHint{Known: true, Available: true, UpdatedAt: time.Now()},
					)
					req := executor.Request{
						Model: model, Payload: []byte(`{"input":"original credits payload"}`),
						Metadata: map[string]any{"original-credits": "kept"},
					}
					if mode == "stream" {
						var result *executor.StreamResult
						result, err = manager.ExecuteStream(
							t.Context(), []string{"antigravity"}, req, executor.Options{},
						)
						if err == nil {
							for chunk := range result.Chunks {
								err = errors.Join(err, chunk.Err)
							}
						}
					} else {
						_, err = manager.Execute(t.Context(), []string{"antigravity"}, req, executor.Options{})
					}
					if capture.prepareCalls != 1 {
						t.Fatalf(
							"credits preparation calls=%d, executions=%d, error=%v", capture.prepareCalls,
							capture.executeCalls, err,
						)
					}
					if prepareErr != nil {
						if !errors.Is(err, prepareErr) || capture.executeCalls != 0 {
							t.Fatalf(
								"preparation error=%v, executions=%d, want %v without execution", err,
								capture.executeCalls, prepareErr,
							)
						}
						return
					}
					current, found := manager.GetByID(auth.ID)
					if err != nil || capture.executeCalls != 1 || !found ||
						current.Metadata["project_id"] != "prepared-project" {
						t.Fatalf(
							"prepared credits result: error=%v executions=%d credential=%+v",
							err,
							capture.executeCalls,
							current,
						)
					}
				},
			)
		}
	}
}
