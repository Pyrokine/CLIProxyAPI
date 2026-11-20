package auth

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestOriginalOverrideCountEndpoint404Inputs(t *testing.T) {
	for _, test := range []struct {
		name, model string
		err         error
		want        bool
	}{
		{"empty router 404", "", &Error{HTTPStatus: 404}, true},
		{"plain router 404", "", &Error{HTTPStatus: 404, Message: "404 page not found"}, true},
		{
			"wrapped router 404", "", &Error{HTTPStatus: 404, Message: "upstream request failed: 404 page not found"},
			true,
		},
		{"fastapi route 404", "", &Error{HTTPStatus: 404, Message: `{"detail":"Not Found"}`}, true},
		{"problem details route 404", "", &Error{HTTPStatus: 404, Message: `{"title":"Not Found","status":404}`}, true},
		{
			"nested generic route 404", "",
			&Error{HTTPStatus: 404, Message: `{"error":{"type":"not_found_error","message":"Not Found"}}`}, true,
		},
		{
			"generic model api route 404", "",
			&Error{HTTPStatus: 404, Message: `{"type":"not_found_error","title":"Model API","detail":"Not Found"}`},
			true,
		},
		{
			"generic model metadata route 404", "", &Error{
				HTTPStatus: 404, Message: `{"error":{"type":"not_found_error","message":"model metadata ` +
					`route not ` +
					`found"}}`,
			}, true,
		},
		{
			"generic model provider 404", "", &Error{
				HTTPStatus: 404, Message: `{"error":{"type":"not_found_error","message":"model provider was ` +
					`not found"}}`,
			}, true,
		},
		{
			"generic route with misleading metadata", "",
			&Error{HTTPStatus: 404, Message: `{"message":"Not Found","request_id":"model_not_found"}`}, true,
		},
		{
			"express count route 404", "", &Error{HTTPStatus: 404, Message: "Cannot POST /v1/messages/count_tokens"},
			true,
		},
		{"html route 404", "", &Error{HTTPStatus: 404, Message: "<html><title>404 Not Found</title></html>"}, true},
		{
			"structured model 404", "", &Error{
				HTTPStatus: 404,
				Message:    `{"error":{"type":"not_found_error","message":"model claude-missing was not found"}}`,
			}, false,
		},
		{
			"anthropic exact model reference", "",
			&Error{HTTPStatus: 404, Message: `{"error":{"type":"not_found_error","message":"model: claude-missing"}}`},
			false,
		},
		{
			"anthropic model reference with thinking suffix", "claude-missing(high)",
			&Error{HTTPStatus: 404, Message: `{"error":{"type":"not_found_error","message":"model: claude-missing"}}`},
			false,
		},
		{
			"requested model does not exist", "", &Error{
				HTTPStatus: 404,
				Message:    `{"error":{"type":"not_found_error","message":"The requested model does not exist"}}`,
			}, false,
		},
		{
			"requested quoted model could not be found", "foo", &Error{
				HTTPStatus: 404,
				Message: `{"error":{"type":"not_found_error","message":"The requested model ` +
					`'foo' could not be ` +
					`found"}}`,
			}, false,
		},
		{
			"problem details model type uri", "", &Error{
				HTTPStatus: 404,
				Message:    `{"type":"https://example.com/problems/model-not-found","title":"Not Found","status":404}`,
			}, false,
		},
		{
			"structured model error string", "",
			&Error{HTTPStatus: 404, Message: `{"error":"model claude-missing does not exist"}`}, false,
		},
		{
			"model code with generic message", "", &Error{
				HTTPStatus: 404, Message: `{"message":"Not Found","code":"model_not_found","model":"claude-missing"}`,
			}, false,
		},
		{
			"typed model not found code", "", &Error{Code: "model_not_found", HTTPStatus: 404, Message: "Not Found"},
			false,
		},
		{
			"typed wrapper with structured model code", "", &Error{
				Code: "not_found", HTTPStatus: 404, Message: `{"error":{"code":"model_not_found","message":` +
					`"Not Found"}}`,
			}, false,
		},
		{
			"wrapped structured model code", "", fmt.Errorf(
				"upstream failed: %w",
				NewRequestScopedError(`{"error":{"code":"model_not_found","message":"Not Found"}}`, 404),
			), false,
		},
		{
			"joined structured model code", "", errors.Join(
				errors.New("upstream failed"),
				NewRequestScopedError(`{"error":{"code":"model_not_found","message":"Not Found"}}`, 404),
			), false,
		},
		{
			"outer generic inner model 404", "", &Error{
				HTTPStatus: 404,
				Message: `{"message":"Not Found","error":{"type":"not_found_error","message":` +
					`"model claude-missing does not ` +
					`exist"}}`,
			}, false,
		},
		{"unstructured model text", "", &Error{HTTPStatus: 404, Message: "model claude-missing was not found"}, true},
		{"non 404", "", &Error{HTTPStatus: 500, Message: "404 page not found"}, false},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				model := test.model
				if model == "" {
					model = "claude-missing"
				}
				if got := isCountTokensEndpointNotFoundError(test.err, model); got != test.want {
					t.Fatalf("count endpoint classification=%t want=%t error=%v", got, test.want, test.err)
				}
			},
		)
	}
}

func TestOriginalOverrideCountAndOrdinary404State(t *testing.T) {
	for _, mode := range []string{"count route", "count model", "ordinary route"} {
		t.Run(
			mode, func(t *testing.T) {
				id, model := "count-route-not-found-auth", "count-route-not-found-model"
				failure := &Error{HTTPStatus: http.StatusNotFound, Message: "404 page not found"}
				if mode == "count model" {
					id, model = "count-model-not-found-auth", "count-explicitly-missing-model"
					failure = &Error{
						Code: "model_not_found", HTTPStatus: 404,
						Message: `{"type":"error","error":{"type":"not_found_error","message":"model ` +
							`count-explicitly-missing-model was not ` +
							`found"}}`,
					}
				}
				if mode == "ordinary route" {
					id, model = "messages-route-not-found-auth", "messages-route-not-found-model"
				}
				hook := &resultCaptureHook{}
				manager := NewManager(nil, &FillFirstSelector{}, hook)
				manager.SetHomeClientProvider(func() HomeClient { return nil })
				executor := &requestScopedTestExecutor{provider: "claude"}
				executor.count = func(*Auth) (core.Response, error) { return core.Response{}, failure }
				executor.execute = func(auth *Auth) (core.Response, error) {
					if mode == "ordinary route" {
						return core.Response{}, failure
					}
					return core.Response{Payload: []byte(auth.ID)}, nil
				}
				manager.RegisterExecutor(executor)
				if _, err := manager.Register(
					WithSkipPersist(t.Context()), &Auth{ID: id, Provider: "claude"},
				); err != nil {
					t.Fatal(err)
				}
				reg := registerProjectionTestClient(t, id, "claude", &registry.ModelInfo{ID: model})
				manager.RefreshSchedulerEntry(id)
				var err error
				if mode == "ordinary route" {
					_, err = manager.Execute(
						t.Context(), []string{"claude"}, core.Request{Model: model}, core.Options{},
					)
				} else {
					_, err = manager.ExecuteCount(
						t.Context(), []string{"claude"}, core.Request{Model: model}, core.Options{},
					)
				}
				if statusCodeFromError(err) != 404 {
					t.Fatalf("execution error=%v", err)
				}
				current, _ := manager.GetByID(id)
				observed := hook.Results()
				if current.Failed != 1 || len(observed) != 1 || observed[0].Success ||
					observed[0].Error == nil ||
					observed[0].Error.HTTPStatus != 404 {
					t.Fatalf("failure count=%d results=%+v", current.Failed, observed)
				}
				listed := slices.ContainsFunc(
					reg.GetAvailableModels(
						"openai",
					),
					func(info map[string]any) bool { return info["id"] == model },
				)
				if mode == "count route" {
					if current.Unavailable || current.ModelStates[model] != nil || !listed {
						t.Fatalf("count route error changed availability: %+v", current)
					}
					response, err := manager.Execute(
						t.Context(), []string{"claude"}, core.Request{Model: model}, core.Options{},
					)
					if err != nil || string(response.Payload) != id {
						t.Fatalf("generation after count route 404=%s error=%v", response.Payload, err)
					}
				} else {
					state := current.ModelStates[model]
					if state == nil || !state.Unavailable || state.NextRetryAfter.IsZero() || listed {
						t.Fatalf("model restriction=%+v", state)
					}
					if mode == "count model" {
						if state.LastError ==
							nil ||
							state.LastError.Code !=
								"model_not_found" ||
							observed[0].Error.Code !=
								"model_not_found" {
							t.Fatalf("model-not-found code state=%+v result=%+v", state.LastError, observed[0])
						}
						if remaining := time.Until(state.NextRetryAfter); remaining <
							11*
								time.Hour ||
							remaining >
								12*
									time.Hour {
							t.Fatalf("model-not-found cooldown=%v", remaining)
						}
					}
				}
			},
		)
	}
}
