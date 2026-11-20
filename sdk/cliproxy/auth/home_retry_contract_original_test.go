package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	internalconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/home"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executionregistry"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type retryContractHomeDispatcher struct {
	mu               sync.Mutex
	authIDs          []string
	excluded         [][]string
	counts           []int
	metadata         map[string]any
	requestRetry     *int
	websocket        bool
	exhaustedPayload []byte
	exhaustedErr     error
}

type legacyRepeatedStreamDispatcher struct {
	calls atomic.Int32
}

type originalHomeIgnoringConstraintsDispatcher struct {
	*legacyRepeatedStreamDispatcher
}

func (d *originalHomeIgnoringConstraintsDispatcher) RPopAuthWithConstraints(
	ctx context.Context,
	model, sessionID string,
	headers http.Header,
	count int,
	_ home.AuthDispatchConstraints,
) ([]byte, error) {
	return d.RPopAuth(ctx, model, sessionID, headers, count)
}

type retryRoundStartCooldownDispatcher struct {
	calls atomic.Int32
}

type retryRoundRepeatedCooldownDispatcher struct {
	calls atomic.Int32
}

type retryRoundLimitDownshiftDispatcher struct {
	calls atomic.Int32
}

type retryRoundSelectionFailureDispatcher struct {
	selectionPayload []byte
	selectionErr     error
}

type aggregateRetryHomeDispatcher struct {
	calls atomic.Int32
}

func (*legacyRepeatedStreamDispatcher) HeartbeatOK() bool { return true }

func (d *legacyRepeatedStreamDispatcher) RPopAuth(
	context.Context,
	string,
	string,
	http.Header,
	int,
) ([]byte, error) {
	d.calls.Add(1)
	return json.Marshal(
		homeAuthDispatchResponse{
			Auth: Auth{
				ID:       "home-retry-a",
				Provider: "home-retry-contract",
				Status:   StatusActive,
			},
		},
	)
}

func (*legacyRepeatedStreamDispatcher) AbortAmbiguousDispatch() {}

func (*retryRoundStartCooldownDispatcher) HeartbeatOK() bool { return true }

func (d *retryRoundStartCooldownDispatcher) RPopAuth(
	ctx context.Context,
	model string,
	sessionID string,
	headers http.Header,
	count int,
) ([]byte, error) {
	return d.RPopAuthWithConstraints(ctx, model, sessionID, headers, count, home.AuthDispatchConstraints{})
}

func (d *retryRoundStartCooldownDispatcher) RPopAuthWithConstraints(
	_ context.Context,
	_ string,
	_ string,
	_ http.Header,
	_ int,
	_ home.AuthDispatchConstraints,
) ([]byte, error) {
	if d.calls.Add(1) == 2 {
		return []byte((`{"error":{"type":"model_cooldown","message":"credential is cooling dow` +
			`n","retryable":true,"retry_after_ms":1,"request_retry":1}}`)), nil
	}
	return json.Marshal(
		homeAuthDispatchResponse{
			Auth: Auth{
				ID:       "home-retry-a",
				Provider: "home-retry-contract",
				Status:   StatusActive,
			},
		},
	)
}

func (*retryRoundStartCooldownDispatcher) AbortAmbiguousDispatch() {}

func (*retryRoundRepeatedCooldownDispatcher) HeartbeatOK() bool { return true }

func (d *retryRoundRepeatedCooldownDispatcher) RPopAuth(
	ctx context.Context,
	model string,
	sessionID string,
	headers http.Header,
	count int,
) ([]byte, error) {
	return d.RPopAuthWithConstraints(ctx, model, sessionID, headers, count, home.AuthDispatchConstraints{})
}

func (d *retryRoundRepeatedCooldownDispatcher) RPopAuthWithConstraints(
	_ context.Context,
	_ string,
	_ string,
	_ http.Header,
	_ int,
	_ home.AuthDispatchConstraints,
) ([]byte, error) {
	if d.calls.Add(1) > 1 {
		return []byte((`{"error":{"type":"model_cooldown","message":"credential is cooling dow` +
			`n","retryable":true,"retry_after_ms":1,"request_retry":1}}`)), nil
	}
	return json.Marshal(
		homeAuthDispatchResponse{
			Auth: Auth{
				ID:       "home-retry-a",
				Provider: "home-retry-contract",
				Status:   StatusActive,
			},
		},
	)
}

func (*retryRoundRepeatedCooldownDispatcher) AbortAmbiguousDispatch() {}

func (*retryRoundLimitDownshiftDispatcher) HeartbeatOK() bool { return true }

func (d *retryRoundLimitDownshiftDispatcher) RPopAuth(
	ctx context.Context,
	model string,
	sessionID string,
	headers http.Header,
	count int,
) ([]byte, error) {
	return d.RPopAuthWithConstraints(ctx, model, sessionID, headers, count, home.AuthDispatchConstraints{})
}

func (d *retryRoundLimitDownshiftDispatcher) RPopAuthWithConstraints(
	_ context.Context,
	_ string,
	_ string,
	_ http.Header,
	_ int,
	_ home.AuthDispatchConstraints,
) ([]byte, error) {
	switch d.calls.Add(1) {
	case 1:
		retryLimit := 1
		return json.Marshal(
			homeAuthDispatchResponse{
				RequestRetry: &retryLimit, Auth: Auth{
					ID:       "home-retry-a",
					Provider: "home-retry-contract",
					Status:   StatusActive,
				},
			},
		)
	case 2:
		return []byte((`{"error":{"type":"model_cooldown","message":"remaining credentials are` +
			` cooling down","retryable":true,"retry_after_ms":1,"request_retry":0}}`)), nil
	default:
		return json.Marshal(
			homeAuthDispatchResponse{
				Auth: Auth{
					ID:       "home-retry-b",
					Provider: "home-retry-contract",
					Status:   StatusActive,
				},
			},
		)
	}
}

func (*retryRoundLimitDownshiftDispatcher) AbortAmbiguousDispatch() {}

func (*retryRoundSelectionFailureDispatcher) HeartbeatOK() bool { return true }

func (d *retryRoundSelectionFailureDispatcher) RPopAuth(
	ctx context.Context,
	model string,
	sessionID string,
	headers http.Header,
	count int,
) ([]byte, error) {
	return d.RPopAuthWithConstraints(ctx, model, sessionID, headers, count, home.AuthDispatchConstraints{})
}

func (d *retryRoundSelectionFailureDispatcher) RPopAuthWithConstraints(
	_ context.Context,
	_ string,
	_ string,
	_ http.Header,
	_ int,
	constraints home.AuthDispatchConstraints,
) ([]byte, error) {
	retryRound, excludedAuthIDs := constraints.RetryRound, constraints.ExcludedAuthIDs
	if retryRound == 0 {
		if len(excludedAuthIDs) > 0 {
			return nil, home.ErrAuthNotFound
		}
		return json.Marshal(
			homeAuthDispatchResponse{
				Auth: Auth{
					ID:       "home-retry-a",
					Provider: "home-retry-contract",
					Status:   StatusActive,
				},
			},
		)
	}
	if len(d.selectionPayload) > 0 {
		return append([]byte(nil), d.selectionPayload...), nil
	}
	return nil, d.selectionErr
}

func (*retryRoundSelectionFailureDispatcher) AbortAmbiguousDispatch() {}

func (*aggregateRetryHomeDispatcher) HeartbeatOK() bool { return true }

func (d *aggregateRetryHomeDispatcher) RPopAuth(
	ctx context.Context,
	model string,
	sessionID string,
	headers http.Header,
	count int,
) ([]byte, error) {
	return d.RPopAuthWithConstraints(ctx, model, sessionID, headers, count, home.AuthDispatchConstraints{})
}

func (d *aggregateRetryHomeDispatcher) RPopAuthWithConstraints(
	_ context.Context,
	_ string,
	_ string,
	_ http.Header,
	_ int,
	_ home.AuthDispatchConstraints,
) ([]byte, error) {
	authID := "home-retry-a"
	override := 0
	if d.calls.Add(1) > 1 {
		authID = "home-retry-b"
		override = 2
	}
	requestRetry := 2
	return json.Marshal(
		homeAuthDispatchResponse{
			RequestRetry: &requestRetry,
			Auth: Auth{
				ID:       authID,
				Provider: "home-retry-contract",
				Status:   StatusActive,
				Metadata: map[string]any{"request_retry": override},
			},
		},
	)
}

func (*aggregateRetryHomeDispatcher) AbortAmbiguousDispatch() {}

func (*retryContractHomeDispatcher) HeartbeatOK() bool { return true }

func (d *retryContractHomeDispatcher) RPopAuth(
	ctx context.Context,
	model string,
	sessionID string,
	headers http.Header,
	count int,
) ([]byte, error) {
	return d.RPopAuthWithConstraints(ctx, model, sessionID, headers, count, home.AuthDispatchConstraints{})
}

func (d *retryContractHomeDispatcher) RPopAuthWithConstraints(
	_ context.Context,
	_ string,
	_ string,
	_ http.Header,
	count int,
	constraints home.AuthDispatchConstraints,
) ([]byte, error) {
	excludedAuthIDs, pinnedAuthID := constraints.ExcludedAuthIDs, constraints.PinnedAuthID
	d.mu.Lock()
	defer d.mu.Unlock()
	d.counts = append(d.counts, count)
	d.excluded = append(d.excluded, append([]string(nil), excludedAuthIDs...))
	excluded := make(map[string]struct{}, len(excludedAuthIDs))
	for _, authID := range excludedAuthIDs {
		excluded[authID] = struct{}{}
	}
	for _, authID := range d.authIDs {
		if pinnedAuthID != "" && authID != pinnedAuthID {
			continue
		}
		if _, okExcluded := excluded[authID]; okExcluded {
			continue
		}
		attributes := map[string]string{}
		if d.websocket {
			attributes["websockets"] = "true"
		}
		return json.Marshal(
			homeAuthDispatchResponse{
				RequestRetry: d.requestRetry,
				Auth: Auth{
					ID:         authID,
					Provider:   "home-retry-contract",
					Status:     StatusActive,
					Metadata:   d.metadata,
					Attributes: attributes,
				},
			},
		)
	}
	if len(d.exhaustedPayload) > 0 {
		return append([]byte(nil), d.exhaustedPayload...), nil
	}
	if d.exhaustedErr != nil {
		return nil, d.exhaustedErr
	}
	return nil, home.ErrAuthNotFound
}

func (*retryContractHomeDispatcher) AbortAmbiguousDispatch() {}

func (d *retryContractHomeDispatcher) Excluded() [][]string {
	d.mu.Lock()
	defer d.mu.Unlock()
	result := make([][]string, len(d.excluded))
	for index := range d.excluded {
		result[index] = append([]string(nil), d.excluded[index]...)
	}
	return result
}

type retryContractHomeExecutor struct {
	mu               sync.Mutex
	calls            []string
	failAll          bool
	failure          error
	failures         map[string]error
	internalFailures map[string]bool
	streamBootstrap  bool
	streamHeaders    http.Header
	refreshCalls     atomic.Int32
}

func (*retryContractHomeExecutor) Identifier() string { return "home-retry-contract" }

func (e *retryContractHomeExecutor) Execute(
	_ context.Context,
	auth *Auth,
	_ cliproxyexecutor.Request,
	_ cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	e.mu.Lock()
	e.calls = append(e.calls, auth.ID)
	e.mu.Unlock()
	if auth.ID == "home-retry-a" || e.failAll {
		return cliproxyexecutor.Response{}, e.failureError(auth.ID)
	}
	return cliproxyexecutor.Response{Payload: []byte(auth.ID)}, nil
}

func (e *retryContractHomeExecutor) ExecuteStream(
	_ context.Context,
	auth *Auth,
	_ cliproxyexecutor.Request,
	_ cliproxyexecutor.Options,
) (*cliproxyexecutor.StreamResult, error) {
	e.mu.Lock()
	e.calls = append(e.calls, auth.ID)
	e.mu.Unlock()
	if auth.ID == "home-retry-a" || e.failAll {
		errFailure := e.failureError(auth.ID)
		if e.streamBootstrap {
			chunks := make(chan cliproxyexecutor.StreamChunk, 1)
			chunks <- cliproxyexecutor.StreamChunk{Err: errFailure}
			close(chunks)
			return &cliproxyexecutor.StreamResult{Headers: e.streamHeaders.Clone(), Chunks: chunks}, nil
		}
		return nil, errFailure
	}
	chunks := make(chan cliproxyexecutor.StreamChunk, 1)
	chunks <- cliproxyexecutor.StreamChunk{Payload: []byte(auth.ID)}
	close(chunks)
	return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
}

func (e *retryContractHomeExecutor) failureError(authID string) error {
	if failure := e.failures[authID]; failure != nil {
		return failure
	}
	if e.failure != nil {
		return e.failure
	}
	return retryContractRateLimitError{}
}

func (e *retryContractHomeExecutor) Refresh(context.Context, *Auth) (*Auth, error) {
	e.refreshCalls.Add(1)
	return nil, nil
}

func (e *retryContractHomeExecutor) CountTokens(
	_ context.Context,
	auth *Auth,
	_ cliproxyexecutor.Request,
	_ cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	e.mu.Lock()
	e.calls = append(e.calls, auth.ID)
	e.mu.Unlock()
	if auth.ID == "home-retry-a" || e.failAll {
		return cliproxyexecutor.Response{}, e.failureError(auth.ID)
	}
	return cliproxyexecutor.Response{Payload: []byte(auth.ID)}, nil
}

func (*retryContractHomeExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func (e *retryContractHomeExecutor) Calls() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.calls...)
}

func (e *retryContractHomeExecutor) ShouldPrepareRequestAuth(auth *Auth) bool {
	return e.internalFailures[auth.ID]
}

func (e *retryContractHomeExecutor) PrepareRequestAuth(_ context.Context, auth *Auth) (*Auth, error) {
	return nil, e.failureError(auth.ID)
}

type retryContractRateLimitError struct {
	retryAfter time.Duration
}

func (retryContractRateLimitError) Error() string { return "credential rate limited" }

func (retryContractRateLimitError) StatusCode() int { return http.StatusTooManyRequests }

func (e retryContractRateLimitError) RetryAfter() *time.Duration {
	value := e.retryAfter
	if value == 0 {
		value = time.Millisecond
	}
	return &value
}

type retainingRetryContractHomeExecutor struct {
	*retryContractHomeExecutor
}

func (e *retainingRetryContractHomeExecutor) Execute(
	_ context.Context,
	auth *Auth,
	_ cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	if lifecycle, ok := opts.ExecutionLifecycle.(interface{ Retain() }); ok {
		lifecycle.Retain()
	}
	return cliproxyexecutor.Response{Payload: []byte(auth.ID)}, nil
}

func originalHomeRetryState(manager *Manager, cap int) (context.Context, *retryRoundState) {
	fallback, _, _ := manager.retrySettings()
	state := &retryRoundState{
		defaultRetry: fallback, maxCredentials: cap, attempted: make(map[string]struct{}), homeLimit: -1,
	}
	return context.WithValue(context.Background(), retryRoundContextKey{}, state), state
}

func originalHomeRetryAttempt(manager *Manager, stream bool, cap int) (*retryRoundState, error) {
	ctx, state := originalHomeRetryState(manager, cap)
	request := cliproxyexecutor.Request{Model: "gpt"}
	var err error
	if stream {
		_, err = manager.executeHomeStream(
			ctx, []string{"home-retry-contract"}, request, cliproxyexecutor.Options{Stream: true},
		)
	} else {
		_, err = manager.executeHome(ctx, []string{"home-retry-contract"}, request, cliproxyexecutor.Options{}, false)
	}
	return state, err
}

func originalHomeRetryRejection(err *Error) *Error {
	return err.WithExecutionDisposition(
		cliproxyexecutor.ExecutionDisposition{
			DispatchPhase:  cliproxyexecutor.DispatchPhaseResponseStarted,
			Outcome:        cliproxyexecutor.ExecutionOutcomeRejectedBeforeProcessing,
			RetryDirective: cliproxyexecutor.RetryDirectiveNextAuth,
			EffectScope:    cliproxyexecutor.StateEffectScopeRequest,
		}, 0,
	)
}

func TestHomePinnedAuthRejectsMismatchedDispatch(t *testing.T) {
	dispatcher := &retryContractHomeDispatcher{authIDs: []string{"home-retry-b"}}
	executor := &retryContractHomeExecutor{}
	executionRegistry := executionregistry.New()
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}})
	manager.PublishHomeDispatch(dispatcher, executionRegistry, 1)
	manager.RegisterExecutor(executor)

	_, errExecute := manager.Execute(
		context.Background(), []string{"home-retry-contract"}, cliproxyexecutor.Request{Model: "gpt"},
		cliproxyexecutor.Options{
			Metadata: map[string]any{
				cliproxyexecutor.PinnedAuthMetadataKey: "home-retry-a",
			},
		},
	)
	var authErr *Error
	if !errors.As(errExecute, &authErr) || authErr == nil || authErr.Code != "auth_not_found" {
		t.Fatalf("Execute() error = %T %v, want pinned auth_not_found", errExecute, errExecute)
	}
	if got := executor.Calls(); len(got) != 0 {
		t.Fatalf("executor calls = %v, want no mismatched credential execution", got)
	}
	if errDrain := executionRegistry.Drain(context.Background()); errDrain != nil {
		t.Fatalf("Drain() error = %v", errDrain)
	}
}

func TestOriginalHomePinnedAuthRequiresRetryAuthority(t *testing.T) {
	for _, kind := range []string{"execute", "count", "stream"} {
		for _, sample := range []struct {
			name               string
			remote, credential int
		}{
			{name: "original", remote: 3, credential: 1},
			{name: "remote zero", remote: 0, credential: 3},
			{name: "remote lower", remote: 1, credential: 3},
		} {
			for _, safe := range []bool{false, true} {
				t.Run(
					kind+"/"+sample.name+fmt.Sprintf("/safe=%t", safe), func(t *testing.T) {
						dispatcher := &retryContractHomeDispatcher{
							authIDs: []string{"home-retry-b", "home-retry-a"},
							metadata: map[string]any{
								"request_retry": sample.credential,
							}, requestRetry: &sample.remote,
						}
						failure := &Error{HTTPStatus: http.StatusBadGateway, Message: "upstream unavailable"}
						if safe {
							failure = originalHomeRetryRejection(failure)
						}
						executor := &retryContractHomeExecutor{failAll: true, failure: failure}
						manager := NewManager(nil, nil, nil)
						manager.SetConfig(
							&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}},
						)
						manager.SetRetryConfig(0, time.Second, 0)
						executionRegistry := executionregistry.New()
						manager.PublishHomeDispatch(dispatcher, executionRegistry, 1)
						manager.RegisterExecutor(executor)
						opts := cliproxyexecutor.Options{
							Metadata: map[string]any{cliproxyexecutor.PinnedAuthMetadataKey: "home-retry-a"},
						}
						req := cliproxyexecutor.Request{Model: "gpt"}
						var err error
						switch kind {
						case "execute":
							_, err = manager.Execute(
								context.Background(),
								[]string{"home-retry-contract"},
								req,
								opts,
							)
						case "count":
							_, err = manager.ExecuteCount(
								context.Background(), []string{"home-retry-contract"}, req, opts,
							)
						case "stream":
							_, err = manager.ExecuteStream(
								context.Background(), []string{"home-retry-contract"}, req, opts,
							)
						}
						if !errors.Is(
							err, failure,
						) ||
							statusCodeFromError(err) !=
								http.StatusBadGateway ||
							err.Error() !=
								("upstream "+
									"unavailable") {
							t.Fatalf("error=%v, want original 502", err)
						}
						want := 1
						if safe {
							want += min(sample.remote, sample.credential)
						}
						if got := executor.Calls(); len(got) != want {
							t.Fatalf("calls=%v, want %d pinned attempts", got, want)
						}
						for _, id := range executor.Calls() {
							if id != "home-retry-a" {
								t.Fatalf("unpinned credential executed: %s", id)
							}
						}
						if excluded := dispatcher.Excluded(); len(excluded) != want {
							t.Fatalf("Home selections=%v, want %d", excluded, want)
						}
						for _, ids := range dispatcher.Excluded() {
							if len(ids) != 0 {
								t.Fatalf("new pinned round retained exclusions: %v", ids)
							}
						}
						if freeze := executionRegistry.FreezeInFlight(time.Now()); len(freeze.Executions) != 0 {
							t.Fatalf("pinned selection remains active: %+v", freeze)
						}
					},
				)
			}
		}
	}
}

func TestHomeExcludedCredentialEndsRetainedWebsocketSelection(t *testing.T) {
	dispatcher := &retryContractHomeDispatcher{
		authIDs:   []string{"home-retry-a", "home-retry-b"},
		websocket: true,
	}
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}})
	manager.SetRetryConfig(0, time.Second, 2)
	manager.PublishHomeDispatch(dispatcher, executionregistry.New(), 1)
	manager.RegisterExecutor(
		&retainingRetryContractHomeExecutor{retryContractHomeExecutor: &retryContractHomeExecutor{}},
	)

	ctx := cliproxyexecutor.WithDownstreamWebsocket(context.Background())
	opts := cliproxyexecutor.Options{
		Metadata: map[string]any{
			cliproxyexecutor.ExecutionSessionMetadataKey: "home-retry-session",
		},
	}
	if _, errExecute := manager.Execute(
		ctx, []string{"home-retry-contract"}, cliproxyexecutor.Request{Model: "gpt"}, opts,
	); errExecute != nil {
		t.Fatalf("first Execute() error = %v", errExecute)
	}

	pickOpts := cloneExecutorOptions(opts)
	pickOpts.Metadata[ExcludedAuthIDsMetadataKey] = []string{"home-retry-a"}
	selection, errPick := manager.pickHomeDispatchSelection(ctx, "gpt", pickOpts)
	if errPick != nil {
		t.Fatalf("pickHomeDispatchSelection() error = %v", errPick)
	}
	defer selection.End("test_complete")
	if auth := selection.CloneAuth(); auth == nil || auth.ID != "home-retry-b" {
		t.Fatalf("selected auth = %#v, want home-retry-b", auth)
	}

	excluded := dispatcher.Excluded()
	if len(excluded) != 2 || len(excluded[0]) != 0 || len(excluded[1]) != 1 ||
		excluded[1][0] != "home-retry-a" {
		t.Fatalf("Home excluded auth IDs = %v, want [[], [home-retry-a]]", excluded)
	}
}

func TestHomeRetryRoundTriesFreshCredentialWhenRequestRetryIsZero(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(
			map[bool]string{false: "nonstream", true: "stream"}[stream], func(t *testing.T) {
				dispatcher := &retryContractHomeDispatcher{authIDs: []string{"home-retry-a", "home-retry-b"}}
				executor := &retryContractHomeExecutor{}
				manager := NewManager(nil, nil, nil)
				manager.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}})
				manager.SetRetryConfig(0, time.Second, 2)
				manager.PublishHomeDispatch(dispatcher, executionregistry.New(), 1)
				manager.RegisterExecutor(executor)

				if stream {
					result, errExecute := manager.ExecuteStream(
						context.Background(),
						[]string{"home-retry-contract"},
						cliproxyexecutor.Request{Model: "gpt"},
						cliproxyexecutor.Options{Stream: true},
					)
					if errExecute != nil {
						t.Fatalf("ExecuteStream() error = %v", errExecute)
					}
					for range result.Chunks {
					}
				} else {
					response, errExecute := manager.Execute(
						context.Background(), []string{"home-retry-contract"}, cliproxyexecutor.Request{Model: "gpt"},
						cliproxyexecutor.Options{},
					)
					if errExecute != nil {
						t.Fatalf("Execute() error = %v", errExecute)
					}
					if string(response.Payload) != "home-retry-b" {
						t.Fatalf("response payload = %q, want home-retry-b", string(response.Payload))
					}
				}

				if got := executor.Calls(); len(got) != 2 || got[0] != "home-retry-a" ||
					got[1] != "home-retry-b" {
					t.Fatalf("executor calls = %v, want [home-retry-a home-retry-b]", got)
				}
				excluded := dispatcher.Excluded()
				if len(excluded) != 2 || len(excluded[0]) != 0 || len(excluded[1]) != 1 ||
					excluded[1][0] != "home-retry-a" {
					t.Fatalf("Home excluded auth IDs = %v, want [[], [home-retry-a]]", excluded)
				}
			},
		)
	}
}

func TestHomeCountTokensTriesFreshCredentialWhenRequestRetryIsZero(t *testing.T) {
	dispatcher := &retryContractHomeDispatcher{authIDs: []string{"home-retry-a", "home-retry-b"}}
	executor := &retryContractHomeExecutor{}
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}})
	manager.SetRetryConfig(0, time.Second, 2)
	manager.PublishHomeDispatch(dispatcher, executionregistry.New(), 1)
	manager.RegisterExecutor(executor)

	response, errExecute := manager.ExecuteCount(
		context.Background(), []string{"home-retry-contract"}, cliproxyexecutor.Request{Model: "gpt"},
		cliproxyexecutor.Options{},
	)
	if errExecute != nil {
		t.Fatalf("ExecuteCount() error = %v", errExecute)
	}
	if string(response.Payload) != "home-retry-b" {
		t.Fatalf("response payload = %q, want home-retry-b", string(response.Payload))
	}
	if got := executor.Calls(); len(got) != 2 || got[0] != "home-retry-a" || got[1] != "home-retry-b" {
		t.Fatalf("executor calls = %v, want [home-retry-a home-retry-b]", got)
	}
	excluded := dispatcher.Excluded()
	if len(excluded) != 2 || len(excluded[0]) != 0 || len(excluded[1]) != 1 ||
		excluded[1][0] != "home-retry-a" {
		t.Fatalf("Home excluded auth IDs = %v, want [[], [home-retry-a]]", excluded)
	}
}

func TestHomeRetryPolicyAllowsRemoteCooldownWithoutLocalCredentials(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}})
	manager.SetRetryConfig(1, time.Second, 0)
	ctx, state := originalHomeRetryState(manager, 0)
	remote := &homeDispatchRetryAfterError{
		cause: &Error{
			Code: "model_cooldown", HTTPStatus: http.StatusTooManyRequests,
			Message: "all Home credentials are cooling down",
		}, retryAfter: 10 * time.Millisecond,
	}
	state.observeHomeError(remote)
	if wait, ok := manager.shouldRetryAfterError(
		ctx, cliproxyexecutor.Options{}, remote, 0, []string{"home-retry-contract"}, "gpt", time.Second,
	); !ok || wait != 10*time.Millisecond {
		t.Fatalf("remote wait=%v allowed=%t", wait, ok)
	}
	if wait, ok := manager.shouldRetryAfterError(
		ctx, cliproxyexecutor.Options{}, remote, 0, []string{"home-retry-contract"}, "gpt", 0,
	); ok || wait != 0 {
		t.Fatalf("zero budget wait=%v allowed=%t", wait, ok)
	}
	state.attempted["home-retry-a"] = struct{}{}
	if _, ok := manager.shouldRetryAfterError(
		ctx, cliproxyexecutor.Options{}, remote, 1, []string{"home-retry-contract"}, "gpt", time.Second,
	); ok {
		t.Fatal("exhausted round retried")
	}
	state.homeWait = 0
	for _, safe := range []bool{false, true} {
		failure := &Error{HTTPStatus: http.StatusBadGateway, Message: "upstream unavailable"}
		if safe {
			failure = originalHomeRetryRejection(failure)
		}
		if wait, ok := manager.shouldRetryAfterError(
			ctx, cliproxyexecutor.Options{}, failure, 0, []string{"home-retry-contract"}, "gpt", 0,
		); ok != safe || wait != 0 {
			t.Fatalf("safe=%t immediate wait=%v allowed=%t", safe, wait, ok)
		}
	}
	state.observeHomeAttempt(retryContractRateLimitError{retryAfter: -time.Millisecond})
	if wait, ok := manager.shouldRetryAfterError(
		ctx, cliproxyexecutor.Options{}, retryContractRateLimitError{retryAfter: -time.Millisecond}, 0,
		[]string{"home-retry-contract"}, "gpt", 0,
	); ok || wait != 0 {
		t.Fatalf("negative wait=%v allowed=%t", wait, ok)
	}
}

func TestRetryIntervalFiltersCooldownCredentials(t *testing.T) {
	tests := []struct {
		name        string
		cooldowns   []time.Duration
		wantRetry   bool
		maxWantWait time.Duration
	}{
		{
			name: "short and long cooldowns", cooldowns: []time.Duration{10 * time.Second, time.Minute},
			wantRetry: true, maxWantWait: 10 * time.Second,
		},
		{name: "only long cooldown", cooldowns: []time.Duration{time.Minute}},
	}
	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				const (
					provider = "retry-interval-contract"
					model    = "gpt"
				)
				manager := NewManager(nil, nil, nil)
				manager.SetRetryConfig(1, 30*time.Second, 0)
				manager.RegisterExecutor(&replaceAwareExecutor{id: provider})
				now := time.Now()
				for index, cooldown := range test.cooldowns {
					authID := fmt.Sprintf(
						"retry-interval-%s-%d",
						strings.ReplaceAll(test.name, " ", "-"),
						index,
					)
					deadline := now.Add(cooldown)
					auth := &Auth{
						ID:       authID,
						Provider: provider,
						Status:   StatusActive,
						ModelStates: map[string]*ModelState{
							model: {
								Status:         StatusError,
								Unavailable:    true,
								NextRetryAfter: deadline,
								LastError:      &Error{HTTPStatus: http.StatusTooManyRequests},
								Quota:          QuotaState{Exceeded: true, NextRecoverAt: deadline},
							},
						},
					}
					registry.GetGlobalRegistry().RegisterClient(
						auth.ID, auth.Provider, []*registry.ModelInfo{{ID: model}},
					)
					t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(authID) })
					if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
						t.Fatalf("Register() error = %v", errRegister)
					}
				}

				wait, shouldRetry := manager.shouldRetryAfterError(
					context.Background(),
					cliproxyexecutor.Options{},
					&Error{HTTPStatus: http.StatusTooManyRequests},
					0,
					[]string{provider},
					model,
					30*time.Second,
				)
				if shouldRetry != test.wantRetry {
					t.Fatalf(
						"shouldRetryAfterError() = (%v, %t), want retry %t",
						wait,
						shouldRetry,
						test.wantRetry,
					)
				}
				if test.wantRetry && (wait <= 0 || wait > test.maxWantWait) {
					t.Fatalf(
						"shouldRetryAfterError() wait = %v, want the earliest cooldown within %v", wait,
						test.maxWantWait,
					)
				}
			},
		)
	}
}

func TestHomeRetryPolicyUsesRemoteCredentialOverrideBeforeSelection(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}})
	manager.SetRetryConfig(0, time.Second, 0)
	ctx, state := originalHomeRetryState(manager, 0)
	limit := 1
	remote := &homeDispatchRetryAfterError{
		cause: &Error{
			Code: "model_cooldown", HTTPStatus: http.StatusTooManyRequests,
			Message: "all Home credentials are cooling down",
		}, retryAfter: 10 * time.Millisecond, requestRetry: &limit,
	}
	state.observeHomeError(remote)
	if wait, ok := manager.shouldRetryAfterError(
		ctx, cliproxyexecutor.Options{}, remote, 0, []string{"home-retry-contract"}, "gpt", time.Second,
	); !ok || wait != 10*time.Millisecond {
		t.Fatalf("remote override wait=%v allowed=%t", wait, ok)
	}
	state.attempted["home-retry-a"] = struct{}{}
	if _, ok := manager.shouldRetryAfterError(
		ctx, cliproxyexecutor.Options{}, remote, 1, []string{"home-retry-contract"}, "gpt", time.Second,
	); ok {
		t.Fatal("remote override exceeded one additional round")
	}
	pinnedCtx, _ := originalHomeRetryState(manager, 0)
	pinnedOpts := cliproxyexecutor.Options{
		Metadata: map[string]any{cliproxyexecutor.PinnedAuthMetadataKey: "home-retry-a"},
	}
	if _, ok := manager.shouldRetryAfterError(
		pinnedCtx, pinnedOpts, remote, 0, []string{"home-retry-contract"}, "gpt", time.Second,
	); ok {
		t.Fatal("unobserved aggregate authorized pinned retry")
	}
	limit = 0
	manager.SetRetryConfig(3, time.Second, 0)
	state.observeHomeError(remote)
	if _, ok := manager.shouldRetryAfterError(
		ctx, cliproxyexecutor.Options{}, remote, 0, []string{"home-retry-contract"}, "gpt", time.Second,
	); ok {
		t.Fatal("authoritative zero did not stop global retry")
	}
	if state.homeLimit != 0 || !state.homeAuthority {
		t.Fatalf("remote limit=%d authoritative=%t", state.homeLimit, state.homeAuthority)
	}
}

func TestHomeRetryRoundCredentialLimitStartsNextRoundImmediately(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}})
	manager.SetRetryConfig(1, time.Second, 1)
	ctx, state := originalHomeRetryState(manager, 1)
	state.attempted["home-retry-a"] = struct{}{}
	failure := retryContractRateLimitError{retryAfter: 5 * time.Second}
	state.observeHomeAttempt(failure)
	if wait, ok := manager.shouldRetryAfterError(
		ctx, cliproxyexecutor.Options{}, failure, 0, []string{"home-retry-contract"}, "gpt", time.Second,
	); !ok || wait != 0 {
		t.Fatalf("credential cap did not start the next round: wait=%v allowed=%t", wait, ok)
	}
	if got := SafeResponseHeaders(
		&homeDispatchRetryAfterError{
			cause: failure, retryAfter: state.homeWait,
		},
	).Get("Retry-After"); got != "5" {
		t.Fatalf("Retry-After=%q, want 5", got)
	}
	state.observeHomeError(&Error{Code: "auth_unavailable"})
	if wait, ok := manager.shouldRetryAfterError(
		ctx, cliproxyexecutor.Options{}, failure, 0, []string{"home-retry-contract"}, "gpt", time.Second,
	); !ok || wait != 0 {
		t.Fatalf("available round wait=%v allowed=%t", wait, ok)
	}
}

func TestHomeCredentialLimitWaitsBeforeConsumingAdditionalRound(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(
			map[bool]string{false: "nonstream", true: "stream"}[stream], func(t *testing.T) {
				dispatcher := &retryRoundStartCooldownDispatcher{}
				executor := &retryContractHomeExecutor{failAll: true}
				manager := NewManager(nil, nil, nil)
				manager.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}})
				manager.SetRetryConfig(1, time.Second, 1)
				manager.PublishHomeDispatch(dispatcher, executionregistry.New(), 1)
				manager.RegisterExecutor(executor)

				if stream {
					result, errExecute := manager.ExecuteStream(
						context.Background(),
						[]string{"home-retry-contract"},
						cliproxyexecutor.Request{Model: "gpt"},
						cliproxyexecutor.Options{Stream: true},
					)
					if errExecute == nil || result != nil {
						t.Fatalf(
							"ExecuteStream() = result %#v, error %v; want terminal retry error",
							result,
							errExecute,
						)
					}
				} else {
					if _, errExecute := manager.Execute(
						context.Background(), []string{"home-retry-contract"}, cliproxyexecutor.Request{Model: "gpt"},
						cliproxyexecutor.Options{},
					); errExecute == nil {
						t.Fatal("Execute() error = nil, want terminal retry error")
					}
				}
				if got := executor.Calls(); len(got) != 2 {
					t.Fatalf("executor calls = %v, want one execution in each of two rounds", got)
				}
				if got := dispatcher.calls.Load(); got != 3 {
					t.Fatalf("Home dispatch calls = %d, want selection, cooldown wait, selection", got)
				}
			},
		)
	}
}

func TestHomePendingRetryRoundStopsWhenRemoteLimitDrops(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(
			map[bool]string{false: "nonstream", true: "stream"}[stream], func(t *testing.T) {
				dispatcher := &retryRoundLimitDownshiftDispatcher{}
				executor := &retryContractHomeExecutor{}
				manager := NewManager(nil, nil, nil)
				manager.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}})
				manager.SetRetryConfig(1, time.Second, 1)
				manager.PublishHomeDispatch(dispatcher, executionregistry.New(), 1)
				manager.RegisterExecutor(executor)

				if stream {
					result, errExecute := manager.ExecuteStream(
						context.Background(),
						[]string{"home-retry-contract"},
						cliproxyexecutor.Request{Model: "gpt"},
						cliproxyexecutor.Options{Stream: true},
					)
					if errExecute == nil || result != nil {
						t.Fatalf(
							"ExecuteStream() = result %#v, error %v; want terminal cooldown error",
							result,
							errExecute,
						)
					}
				} else {
					if _, errExecute := manager.Execute(
						context.Background(), []string{"home-retry-contract"}, cliproxyexecutor.Request{Model: "gpt"},
						cliproxyexecutor.Options{},
					); errExecute == nil {
						t.Fatal("Execute() error = nil, want terminal cooldown error")
					}
				}
				if got := executor.Calls(); len(got) != 1 || got[0] != "home-retry-a" {
					t.Fatalf("executor calls = %v, want only the initial credential", got)
				}
				if got := dispatcher.calls.Load(); got != 2 {
					t.Fatalf("Home dispatch calls = %d, want initial selection and one cooldown response", got)
				}
			},
		)
	}
}

func TestHomePendingRetryRoundStopsAfterRepeatedCooldown(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(
			map[bool]string{false: "nonstream", true: "stream"}[stream], func(t *testing.T) {
				dispatcher := &retryRoundRepeatedCooldownDispatcher{}
				executor := &retryContractHomeExecutor{failAll: true}
				manager := NewManager(nil, nil, nil)
				manager.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}})
				manager.SetRetryConfig(1, time.Second, 1)
				manager.PublishHomeDispatch(dispatcher, executionregistry.New(), 1)
				manager.RegisterExecutor(executor)

				var errExecute error
				if stream {
					var result *cliproxyexecutor.StreamResult
					result, errExecute = manager.ExecuteStream(
						context.Background(),
						[]string{"home-retry-contract"},
						cliproxyexecutor.Request{Model: "gpt"},
						cliproxyexecutor.Options{Stream: true},
					)
					if errExecute == nil || result != nil {
						t.Fatalf(
							"ExecuteStream() = result %#v, error %v; want terminal cooldown error",
							result,
							errExecute,
						)
					}
				} else {
					_, errExecute = manager.Execute(
						context.Background(), []string{"home-retry-contract"}, cliproxyexecutor.Request{Model: "gpt"},
						cliproxyexecutor.Options{},
					)
					if errExecute == nil {
						t.Fatal("Execute() error = nil, want terminal cooldown error")
					}
				}
				var cooldown *homeDispatchRetryAfterError
				var cause *Error
				if !errors.As(errExecute, &cooldown) || !errors.As(
					cooldown.cause, &cause,
				) ||
					cause.HTTPStatus !=
						http.StatusTooManyRequests ||
					cause.Message !=
						("credential rate "+
							"limited") ||
					cooldown.retryAfter !=
						time.Millisecond {
					t.Fatalf(
						"terminal error = %#v, want original upstream 429 with current Home wait",
						errExecute,
					)
				}
				if got := executor.Calls(); len(got) != 1 {
					t.Fatalf("executor calls = %v, want only the initial round execution", got)
				}
				if got := dispatcher.calls.Load(); got < 3 {
					t.Fatalf(
						"Home dispatch calls = %d, want bounded pending selection with at least two cooldown responses",
						got,
					)
				}
			},
		)
	}
}

func TestHomeRetryRoundUsesEarliestCredentialRetryAfter(t *testing.T) {
	for _, kind := range []string{"execute", "count", "stream"} {
		t.Run(
			kind, func(t *testing.T) {
				dispatcher := &retryContractHomeDispatcher{authIDs: []string{"home-retry-a", "home-retry-b"}}
				executor := &retryContractHomeExecutor{
					failAll: true, failures: map[string]error{
						"home-retry-a": retryContractRateLimitError{retryAfter: 5 * time.Millisecond},
						"home-retry-b": retryContractRateLimitError{retryAfter: 50 * time.Millisecond},
					},
				}
				manager := NewManager(nil, nil, nil)
				manager.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}})
				manager.SetRetryConfig(1, time.Second, 2)
				manager.PublishHomeDispatch(dispatcher, executionregistry.New(), 1)
				manager.RegisterExecutor(executor)
				ctx, state := originalHomeRetryState(manager, 2)
				req := cliproxyexecutor.Request{Model: "gpt"}
				var err error
				if kind == "stream" {
					_, err = manager.executeHomeStream(
						ctx, []string{"home-retry-contract"}, req, cliproxyexecutor.Options{Stream: true},
					)
				} else {
					_, err = manager.executeHome(
						ctx, []string{"home-retry-contract"}, req, cliproxyexecutor.Options{}, kind == "count",
					)
				}
				retryAfter := retryAfterFromError(err)
				if err == nil || len(state.attempted) != 2 || !state.hasHomeWait ||
					state.homeWait != 5*time.Millisecond ||
					retryAfter == nil ||
					*retryAfter != 5*time.Millisecond {
					t.Fatalf(
						"error=%v state=%+v retry-after=%v, want two attempts and earliest 5 ms",
						err,
						state,
						retryAfter,
					)
				}
				if statusCodeFromError(err) != http.StatusTooManyRequests ||
					SafeResponseHeaders(err).Get("Retry-After") != "1" ||
					!retryDirectiveAllowsRedispatch(
						executionDispositionForError(
							err, cliproxyexecutor.DispatchPhaseMaybeSent,
						),
					) {
					t.Fatalf("aggregation lost HTTP status, safe timing or retry authority: %v", err)
				}
			},
		)
	}
}

// noinspection GoErrorStringFormat
func TestHomePreferredErrorIgnoresLaterInternalExecutorFailure(t *testing.T) {
	for _, safe := range []bool{false, true} {
		t.Run(
			fmt.Sprintf("safe=%t", safe), func(t *testing.T) {
				upstream := &Error{Message: "model upstream failed", HTTPStatus: http.StatusBadGateway}
				if safe {
					upstream = originalHomeRetryRejection(upstream)
				}
				internal := errors.New("Home KV lookup failed")
				dispatcher := &retryContractHomeDispatcher{authIDs: []string{"home-retry-a", "home-retry-b"}}
				executor := &retryContractHomeExecutor{
					failAll: true, failures: map[string]error{"home-retry-a": upstream, "home-retry-b": internal},
					internalFailures: map[string]bool{"home-retry-b": true},
				}
				manager := NewManager(nil, nil, nil)
				manager.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}})
				manager.SetRetryConfig(0, 0, 2)
				manager.PublishHomeDispatch(dispatcher, executionregistry.New(), 1)
				manager.RegisterExecutor(executor)
				_, err := manager.Execute(
					context.Background(),
					[]string{"home-retry-contract"},
					cliproxyexecutor.Request{Model: "gpt"},
					cliproxyexecutor.Options{},
				)
				if !errors.Is(err, upstream) || errors.Is(err, internal) {
					t.Fatalf("error=%v, want original upstream cause", err)
				}
				if calls := executor.Calls(); len(calls) != 1 || calls[0] != "home-retry-a" {
					t.Fatalf("executor calls=%v, prepared failure must stay unsent", calls)
				}
				want := 1
				if safe {
					want = 2
				}
				if len(dispatcher.Excluded()) != want {
					t.Fatalf("Home selections=%v, want %d", dispatcher.Excluded(), want)
				}
			},
		)
	}
}

// noinspection GoErrorStringFormat
func TestHomeSelectionFailureIsNotHiddenByEarlierUpstreamAttempt(t *testing.T) {
	tests := []struct {
		name             string
		exhaustedPayload []byte
		exhaustedErr     error
		wantCode         string
	}{
		{
			name:         "Home unavailable",
			exhaustedErr: errors.New("Home transport failed"),
			wantCode:     "home_unavailable",
		},
		{name: "invalid auth payload", exhaustedPayload: []byte(`{}`), wantCode: "invalid_auth"},
		{
			name: "malformed concurrency payload",
			exhaustedPayload: []byte((`{"concurrency":{"accounted":true,"credential_id":"home-retry-a","model"` +
				`:"gpt"},"error":"busy","auth":{"id":"home-retry-a","provider":"home-retry-contract"}}`)),
			wantCode: "invalid_home_concurrency",
		},
	}
	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				upstreamErr := originalHomeRetryRejection(
					&Error{
						Message: "model upstream failed", HTTPStatus: http.StatusBadGateway,
					},
				)
				dispatcher := &retryContractHomeDispatcher{
					authIDs:          []string{"home-retry-a"},
					exhaustedPayload: test.exhaustedPayload,
					exhaustedErr:     test.exhaustedErr,
				}
				executor := &retryContractHomeExecutor{failAll: true, failure: upstreamErr}
				manager := NewManager(nil, nil, nil)
				manager.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}})
				manager.PublishHomeDispatch(dispatcher, executionregistry.New(), 1)
				manager.RegisterExecutor(executor)

				_, errExecute := originalHomeRetryAttempt(manager, false, 2)
				var authErr *Error
				if !errors.As(errExecute, &authErr) || authErr == nil || authErr.Code != test.wantCode {
					t.Fatalf(
						"executeHomeOnce() error = %#v, want selection error %q",
						errExecute,
						test.wantCode,
					)
				}
				if errors.Is(errExecute, upstreamErr) {
					t.Fatalf(
						"executeHomeOnce() error = %v, earlier upstream error hid selection failure",
						errExecute,
					)
				}
			},
		)
	}
}

// noinspection GoErrorStringFormat
func TestOriginalHomeNextRoundSelectionErrorKeepsExecutionPhase(t *testing.T) {
	selectionFailures := []struct {
		name    string
		payload []byte
		err     error
		code    string
	}{
		{name: "Home unavailable", err: errors.New("Home transport failed"), code: "home_unavailable"},
		{name: "invalid auth payload", payload: []byte(`{}`), code: "invalid_auth"},
	}
	for _, selectionFailure := range selectionFailures {
		for _, phase := range []cliproxyexecutor.DispatchPhase{
			cliproxyexecutor.DispatchPhaseNotSent, cliproxyexecutor.DispatchPhaseResponseStarted,
			cliproxyexecutor.DispatchPhaseMaybeSent,
		} {
			for _, stream := range []bool{false, true} {
				name := selectionFailure.name + "/" + string(phase) + "/" + map[bool]string{
					false: "nonstream", true: "stream",
				}[stream]
				t.Run(
					name, func(t *testing.T) {
						upstreamErr := &Error{
							Message:    "model upstream failed",
							HTTPStatus: http.StatusBadGateway,
						}
						if phase != cliproxyexecutor.DispatchPhaseMaybeSent {
							upstreamErr = originalHomeRetryRejection(upstreamErr)
							disposition := upstreamErr.ExplicitExecutionDisposition()
							disposition.DispatchPhase = phase
							upstreamErr = upstreamErr.WithExecutionDisposition(disposition, 0)
						}
						dispatcher := &retryRoundSelectionFailureDispatcher{
							selectionPayload: selectionFailure.payload, selectionErr: selectionFailure.err,
						}
						executor := &retryContractHomeExecutor{failAll: true, failure: upstreamErr}
						manager := NewManager(nil, nil, nil)
						manager.SetConfig(
							&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}},
						)
						manager.SetRetryConfig(1, time.Second, 1)
						manager.PublishHomeDispatch(dispatcher, executionregistry.New(), 1)
						manager.RegisterExecutor(executor)
						var errExecute error
						if stream {
							var result *cliproxyexecutor.StreamResult
							result, errExecute = manager.ExecuteStream(
								context.Background(), []string{"home-retry-contract"},
								cliproxyexecutor.Request{Model: "gpt"}, cliproxyexecutor.Options{Stream: true},
							)
							if result != nil {
								t.Fatalf("ExecuteStream() result = %#v, want nil", result)
							}
						} else {
							_, errExecute = manager.Execute(
								context.Background(), []string{"home-retry-contract"},
								cliproxyexecutor.Request{Model: "gpt"}, cliproxyexecutor.Options{},
							)
						}
						if phase == cliproxyexecutor.DispatchPhaseNotSent {
							var authErr *Error
							if !errors.As(
								errExecute, &authErr,
							) || authErr.Code != selectionFailure.code || errors.Is(errExecute, upstreamErr) {
								t.Fatalf("unsent failure hid selection error: %v", errExecute)
							}
						} else if !errors.Is(errExecute, upstreamErr) || errExecute.Error() != "model upstream failed" {
							t.Fatalf("upstream failure lost: %v", errExecute)
						}
						if len(executor.Calls()) != 1 {
							t.Fatalf("unexpected upstream replay: %v", executor.Calls())
						}
					},
				)
			}
		}
	}
}

func TestHomeStreamBootstrapErrorPreservesAggregatedRetryAfter(t *testing.T) {
	dispatcher := &retryContractHomeDispatcher{authIDs: []string{"home-retry-a", "home-retry-b"}}
	executor := &retryContractHomeExecutor{
		failAll:         true,
		streamBootstrap: true,
		streamHeaders:   http.Header{"Retry-After": {"30"}},
		failures: map[string]error{
			"home-retry-a": retryContractRateLimitError{retryAfter: 1500 * time.Millisecond},
			"home-retry-b": retryContractRateLimitError{retryAfter: 5 * time.Second},
		},
	}
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}})
	manager.SetRetryConfig(0, time.Second, 2)
	manager.PublishHomeDispatch(dispatcher, executionregistry.New(), 1)
	manager.RegisterExecutor(executor)

	result, errExecute := manager.ExecuteStream(
		context.Background(), []string{"home-retry-contract"}, cliproxyexecutor.Request{Model: "gpt"},
		cliproxyexecutor.Options{Stream: true},
	)
	if errExecute != nil {
		t.Fatalf("ExecuteStream() error = %v", errExecute)
	}
	if result == nil {
		t.Fatal("ExecuteStream() result = nil")
	}
	chunk, ok := <-result.Chunks
	if !ok || chunk.Err == nil {
		t.Fatalf("stream bootstrap chunk = %#v, %t; want terminal error", chunk, ok)
	}
	if executionDispositionForError(
		chunk.Err, cliproxyexecutor.DispatchPhaseMaybeSent,
	).Outcome == cliproxyexecutor.ExecutionOutcomeUnknown {
		t.Fatalf("stream bootstrap error = %v, want exhausted retry round", chunk.Err)
	}
	if got := SafeResponseHeaders(chunk.Err).Get("Retry-After"); got != "2" ||
		result.Headers.Get("Retry-After") != "2" {
		t.Fatalf(
			"safe Retry-After = %q, response headers = %v, want aggregated delay rounded to 2 seconds", got,
			result.Headers,
		)
	}
}

func TestHomeRetryRoundUsesAuthoritativeRemoteCooldown(t *testing.T) {
	tests := []struct {
		name    string
		execute func(*Manager) (*retryRoundState, error)
	}{
		{
			name: "nonstream",
			execute: func(manager *Manager) (*retryRoundState, error) {
				return originalHomeRetryAttempt(manager, false, 2)
			},
		},
		{
			name: "stream",
			execute: func(manager *Manager) (*retryRoundState, error) {
				return originalHomeRetryAttempt(manager, true, 2)
			},
		},
	}
	for _, tc := range tests {
		t.Run(
			tc.name, func(t *testing.T) {
				dispatcher := &retryContractHomeDispatcher{
					authIDs: []string{"home-retry-a"},
					exhaustedPayload: []byte(
						(`{"error":{"type":"model_cooldown","message":"remaining Home credential` +
							`s are cooling down","retryable":true,"retry_after_ms":5000}}`),
					),
				}
				executor := &retryContractHomeExecutor{
					failAll: true,
					failure: retryContractRateLimitError{retryAfter: 1500 * time.Millisecond},
				}
				manager := NewManager(nil, nil, nil)
				manager.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}})
				manager.SetRetryConfig(1, time.Second, 2)
				manager.PublishHomeDispatch(dispatcher, executionregistry.New(), 1)
				manager.RegisterExecutor(executor)

				state, errExecute := tc.execute(manager)
				if errExecute == nil {
					t.Fatalf("execution error = %v, want exhausted retry round", errExecute)
				}
				retryAfter := retryAfterFromError(errExecute)
				if !state.hasHomeWait || retryAfter == nil || *retryAfter != 5*time.Second {
					t.Fatalf("retry after = %v, want Home next-round cooldown 5s", retryAfter)
				}
				if got := SafeResponseHeaders(errExecute).Get("Retry-After"); got != "5" {
					t.Fatalf("safe Retry-After header = %q, want Home next-round delay 5 seconds", got)
				}
			},
		)
	}
}

func TestOriginalHomeUnauthorizedStopsBeforeRemoteCooldown(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(
			fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
				dispatcher := &retryContractHomeDispatcher{
					authIDs: []string{"home-retry-a"},
					exhaustedPayload: []byte(
						(`{"error":{"type":"model_cooldown","message":"another credential is coo` +
							`ling down","retryable":true,"retry_after_ms":5,"request_retry":2}}`),
					),
				}
				failure := &Error{HTTPStatus: http.StatusUnauthorized, Message: "invalid credential"}
				executor := &retryContractHomeExecutor{failAll: true, failure: failure}
				manager := NewManager(nil, nil, nil)
				manager.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}})
				manager.SetRetryConfig(3, time.Second, 2)
				manager.PublishHomeDispatch(dispatcher, executionregistry.New(), 1)
				manager.RegisterExecutor(executor)
				state, err := originalHomeRetryAttempt(manager, stream, 2)
				if !errors.Is(
					err, failure,
				) ||
					statusCodeFromError(err) !=
						http.StatusUnauthorized ||
					len(dispatcher.Excluded()) !=
						1 ||
					len(executor.Calls()) !=
						1 {
					t.Fatalf("error=%v selections=%v calls=%v", err, dispatcher.Excluded(), executor.Calls())
				}
				ctx := context.WithValue(context.Background(), retryRoundContextKey{}, state)
				if wait, ok := manager.shouldRetryAfterError(
					ctx, cliproxyexecutor.Options{}, err, 0, []string{"home-retry-contract"}, "gpt", time.Second,
				); ok || wait != 0 {
					t.Fatalf("401 wait=%v allowed=%t", wait, ok)
				}
			},
		)
	}
}

func TestHomeRetryRoundStartsImmediatelyWhenHomeReportsAvailableNextRound(t *testing.T) {
	tests := []struct {
		name    string
		execute func(*Manager) (*retryRoundState, error)
	}{
		{
			name: "nonstream",
			execute: func(manager *Manager) (*retryRoundState, error) {
				return originalHomeRetryAttempt(manager, false, 0)
			},
		},
		{
			name: "stream",
			execute: func(manager *Manager) (*retryRoundState, error) {
				return originalHomeRetryAttempt(manager, true, 0)
			},
		},
	}
	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				dispatcher := &retryContractHomeDispatcher{
					authIDs: []string{"home-retry-a", "home-retry-b"},
					exhaustedPayload: []byte(
						(`{"error":{"type":"auth_unavailable","message":"a credential is ` +
							`immediately available next ` +
							`round"}}`),
					),
				}
				executor := &retryContractHomeExecutor{
					failAll: true,
					failures: map[string]error{
						"home-retry-a": retryContractRateLimitError{retryAfter: 5 * time.Second},
						"home-retry-b": originalHomeRetryRejection(
							&Error{
								HTTPStatus: http.StatusBadGateway, Message: "upstream unavailable",
							},
						),
					},
				}
				manager := NewManager(nil, nil, nil)
				manager.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}})
				manager.SetRetryConfig(1, 10*time.Second, 0)
				manager.PublishHomeDispatch(dispatcher, executionregistry.New(), 1)
				manager.RegisterExecutor(executor)

				state, errExecute := test.execute(manager)
				if errExecute == nil {
					t.Fatalf("execution error = %v, want exhausted retry round", errExecute)
				}
				wait, shouldRetry := manager.shouldRetryAfterError(
					context.WithValue(
						context.Background(), retryRoundContextKey{}, state,
					), cliproxyexecutor.Options{}, errExecute, 0, []string{"home-retry-contract"}, "gpt", 10*
						time.Second,
				)
				if !shouldRetry || wait != 0 {
					t.Fatalf("next-round retry = (%v, %t), want immediate", wait, shouldRetry)
				}
			},
		)
	}
}

func TestHomeRetryRoundUsesRemoteCooldownWhenAttemptedErrorHasNoTiming(t *testing.T) {
	tests := []struct {
		name    string
		execute func(*Manager) (*retryRoundState, error)
	}{
		{
			name: "nonstream",
			execute: func(manager *Manager) (*retryRoundState, error) {
				return originalHomeRetryAttempt(manager, false, 2)
			},
		},
		{
			name: "stream",
			execute: func(manager *Manager) (*retryRoundState, error) {
				return originalHomeRetryAttempt(manager, true, 2)
			},
		},
	}
	for _, tc := range tests {
		t.Run(
			tc.name, func(t *testing.T) {
				dispatcher := &retryContractHomeDispatcher{
					authIDs: []string{"home-retry-a"},
					exhaustedPayload: []byte(
						(`{"error":{"type":"model_cooldown","message":"remaining Home credential` +
							`s are cooling down","retryable":true,"retry_after_ms":1500}}`),
					),
				}
				executor := &retryContractHomeExecutor{
					failAll: true,
					failure: originalHomeRetryRejection(
						&Error{
							HTTPStatus: http.StatusBadGateway, Message: "upstream unavailable",
						},
					),
				}
				manager := NewManager(nil, nil, nil)
				manager.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}})
				manager.SetRetryConfig(1, 2*time.Second, 2)
				manager.PublishHomeDispatch(dispatcher, executionregistry.New(), 1)
				manager.RegisterExecutor(executor)

				state, errExecute := tc.execute(manager)
				if errExecute == nil {
					t.Fatalf("execution error = %v, want exhausted retry round", errExecute)
				}
				retryAfter := retryAfterFromError(errExecute)
				if !state.hasHomeWait || retryAfter == nil || *retryAfter != 1500*time.Millisecond {
					t.Fatalf("retry after = %v, want remote cooldown delay 1500ms", retryAfter)
				}
				if got := SafeResponseHeaders(errExecute).Get("Retry-After"); got != "2" {
					t.Fatalf("safe Retry-After header = %q, want 2", got)
				}
			},
		)
	}
}

func TestOriginalHomeStreamOAuthUnauthorizedStops(t *testing.T) {
	testOriginalHomeStreamTerminalFailure(
		t,
		"oauth",
		&Error{HTTPStatus: http.StatusUnauthorized, Message: "expired"},
	)
}

func TestOriginalHomeStreamUnknownRecoveryFailureStops(t *testing.T) {
	testOriginalHomeStreamTerminalFailure(t, "", errors.New("unexpected EOF"))
}

func TestRetryRoundAvailabilityRejectsStaleQuotaForNonRetryableStatus(t *testing.T) {
	now := time.Now()
	for _, test := range []struct {
		name      string
		lastError *Error
		want      bool
	}{
		{name: "implicit quota", want: true},
		{name: "rate limit", lastError: &Error{HTTPStatus: http.StatusTooManyRequests}, want: true},
		{name: "payment required", lastError: &Error{HTTPStatus: http.StatusPaymentRequired}, want: false},
		{name: "not found", lastError: &Error{HTTPStatus: http.StatusNotFound}, want: false},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				nextRetry := now.Add(time.Minute)
				auth := &Auth{
					ID:       "retry-round-stale-quota",
					Provider: "codex",
					Status:   StatusActive,
					ModelStates: map[string]*ModelState{
						"gpt": {
							Status:         StatusError,
							Unavailable:    true,
							NextRetryAfter: nextRetry,
							LastError:      test.lastError,
							Quota:          QuotaState{Exceeded: true, NextRecoverAt: nextRetry},
						},
					},
				}
				next, got := retryRoundStateEligible(auth, "gpt", now)
				if got != test.want {
					t.Fatalf("retryRoundStateEligible() eligible = %t, want %t", got, test.want)
				}
				if got && !next.Equal(nextRetry) {
					t.Fatalf("retryRoundStateEligible() next = %v, want %v", next, nextRetry)
				}
			},
		)
	}
}

func TestOriginalHomeStreamAPIKeyUnauthorizedStops(t *testing.T) {
	testOriginalHomeStreamTerminalFailure(
		t, "apikey", &Error{HTTPStatus: http.StatusUnauthorized, Message: "invalid api key"},
	)
}

func TestHomeModelCooldownErrorPreservesRetryContract(t *testing.T) {
	errDecoded := decodeHomeDispatchError(
		[]byte((`{"error":{"type":"model_cooldown","message":"all credentials are cooling down","retryable":` +
			`true,"retry_after_ms":1500,"request_retry":2}}`)),
	)
	var retryErr *homeDispatchRetryAfterError
	if !errors.As(errDecoded, &retryErr) || retryErr == nil {
		t.Fatalf("decodeHomeDispatchError() = %#v, want retry-after error", errDecoded)
	}
	if statusCodeFromError(retryErr) != http.StatusTooManyRequests || retryErr.RetryAfter() == nil ||
		*retryErr.RetryAfter() != 1500*time.Millisecond {
		t.Fatalf(
			"decoded Home cooldown = status %d retry-after %v, want 429/1500ms", statusCodeFromError(retryErr),
			retryErr.RetryAfter(),
		)
	}
	if retryErr.requestRetry == nil || *retryErr.requestRetry != 2 {
		t.Fatalf("decoded Home request retry limit = %v, want 2", retryErr.requestRetry)
	}
	var cause *Error
	if !errors.As(errDecoded, &cause) || cause == nil || cause.Code != "model_cooldown" || !cause.Retryable {
		t.Fatalf("decoded Home cooldown cause = %#v, want retryable model_cooldown", cause)
	}
	if got := SafeResponseHeaders(errDecoded).Get("Retry-After"); got != "2" {
		t.Fatalf("safe Retry-After header = %q, want 2", got)
	}
}

func TestHomeRequestRetryCountsAdditionalCredentialRounds(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(
			map[bool]string{false: "nonstream", true: "stream"}[stream], func(t *testing.T) {
				dispatcher := &retryContractHomeDispatcher{authIDs: []string{"home-retry-a", "home-retry-b"}}
				executor := &retryContractHomeExecutor{failAll: true}
				manager := NewManager(nil, nil, nil)
				manager.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}})
				manager.SetRetryConfig(1, time.Second, 2)
				manager.PublishHomeDispatch(dispatcher, executionregistry.New(), 1)
				manager.RegisterExecutor(executor)

				if stream {
					result, errExecute := manager.ExecuteStream(
						context.Background(),
						[]string{"home-retry-contract"},
						cliproxyexecutor.Request{Model: "gpt"},
						cliproxyexecutor.Options{Stream: true},
					)
					if errExecute == nil || result != nil {
						t.Fatalf(
							"ExecuteStream() = result %#v, error %v; want terminal rate-limit error",
							result,
							errExecute,
						)
					}
				} else {
					_, errExecute := manager.Execute(
						context.Background(), []string{"home-retry-contract"}, cliproxyexecutor.Request{Model: "gpt"},
						cliproxyexecutor.Options{},
					)
					if errExecute == nil {
						t.Fatal("Execute() error = nil, want rate-limit error")
					}
				}
				if got := executor.Calls(); len(got) != 4 {
					t.Fatalf("executor calls = %v, want four calls across two rounds", got)
				}
				excluded := dispatcher.Excluded()
				if len(excluded) != 4 || len(excluded[0]) != 0 || len(excluded[1]) != 1 ||
					excluded[1][0] != "home-retry-a" ||
					len(excluded[2]) != 0 ||
					len(excluded[3]) != 1 ||
					excluded[3][0] != "home-retry-a" {
					t.Fatalf(
						"Home excluded auth IDs = %v, want [[], [home-retry-a], [], [home-retry-a]]",
						excluded,
					)
				}
			},
		)
	}
}

func TestHomeRequestRetryRoundDoesNotRequireRetryAfter(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(
			map[bool]string{false: "nonstream", true: "stream"}[stream], func(t *testing.T) {
				dispatcher := &retryContractHomeDispatcher{authIDs: []string{"home-retry-a", "home-retry-b"}}
				executor := &retryContractHomeExecutor{
					failAll: true,
					failure: &Error{HTTPStatus: http.StatusBadGateway, Message: "upstream unavailable"},
				}
				manager := NewManager(nil, nil, nil)
				manager.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}})
				manager.SetRetryConfig(1, time.Second, 2)
				manager.PublishHomeDispatch(dispatcher, executionregistry.New(), 1)
				manager.RegisterExecutor(executor)

				if stream {
					result, errExecute := manager.ExecuteStream(
						context.Background(),
						[]string{"home-retry-contract"},
						cliproxyexecutor.Request{Model: "gpt"},
						cliproxyexecutor.Options{Stream: true},
					)
					if errExecute == nil || result != nil {
						t.Fatalf(
							"ExecuteStream() = result %#v, error %v; want terminal upstream error",
							result,
							errExecute,
						)
					}
				} else {
					_, errExecute := manager.Execute(
						context.Background(), []string{"home-retry-contract"}, cliproxyexecutor.Request{Model: "gpt"},
						cliproxyexecutor.Options{},
					)
					if errExecute == nil {
						t.Fatal("Execute() error = nil, want upstream error")
					}
				}
				if got := executor.Calls(); len(got) != 1 {
					t.Fatalf("executor calls = %v, want one attempt for an unknown result", got)
				}
			},
		)
	}
}

func TestHomeStreamLegacyDispatcherDoesNotSpinOnIgnoredExclusions(t *testing.T) {
	testOriginalHomeLegacyRetry(t, true)
}

func TestHomeNonStreamLegacyDispatcherCompletesAdditionalRetryRound(t *testing.T) {
	testOriginalHomeLegacyRetry(t, false)
}

func testOriginalHomeLegacyRetry(t *testing.T, stream bool) {
	t.Helper()
	for _, constrained := range []bool{false, true} {
		for _, safe := range []bool{false, true} {
			t.Run(
				fmt.Sprintf("constrained=%t/safe=%t", constrained, safe), func(t *testing.T) {
					dispatcher := &legacyRepeatedStreamDispatcher{}
					var client homeAuthDispatcher = dispatcher
					if constrained {
						client = &originalHomeIgnoringConstraintsDispatcher{dispatcher}
					}
					failure := &Error{HTTPStatus: http.StatusBadGateway, Message: "upstream unavailable"}
					if safe {
						failure = originalHomeRetryRejection(failure)
					}
					executor := &retryContractHomeExecutor{failAll: true, failure: failure}
					manager := NewManager(nil, nil, nil)
					manager.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}})
					limit := 0
					if stream {
						limit = 2
					}
					manager.SetRetryConfig(1, time.Second, limit)
					executionRegistry := executionregistry.New()
					manager.PublishHomeDispatch(client, executionRegistry, 1)
					manager.RegisterExecutor(executor)
					var err error
					if stream {
						result, executeErr := manager.ExecuteStream(
							context.Background(), []string{"home-retry-contract"},
							cliproxyexecutor.Request{Model: "gpt"}, cliproxyexecutor.Options{Stream: true},
						)
						if result != nil {
							t.Fatalf("unexpected stream result: %#v", result)
						}
						err = executeErr
					} else {
						_, err = manager.Execute(
							context.Background(), []string{"home-retry-contract"},
							cliproxyexecutor.Request{Model: "gpt"}, cliproxyexecutor.Options{},
						)
					}
					if !errors.Is(err, failure) {
						t.Fatalf("original upstream error lost: %v", err)
					}
					wantCalls, wantDispatches := 1, int32(1)
					if safe {
						wantDispatches = 2
						if !constrained {
							wantCalls, wantDispatches = 2, 4
						}
					}
					if got := len(executor.Calls()); got != wantCalls {
						t.Fatalf("executor calls=%d, want %d", got, wantCalls)
					}
					if got := dispatcher.calls.Load(); got != wantDispatches {
						t.Fatalf("Home dispatches=%d, want %d", got, wantDispatches)
					}
					if freeze := executionRegistry.FreezeInFlight(time.Now()); len(freeze.Executions) != 0 {
						t.Fatalf("selection remains active: %+v", freeze)
					}
				},
			)
		}
	}
}

func TestHomeLocalSelectionRejectionWaitsForReleaseAcknowledgement(t *testing.T) {
	tests := []struct {
		name                string
		dispatcher          homeAuthDispatcher
		executor            *retryContractHomeExecutor
		maxRetryCredentials int
		blockedGroup        executionregistry.ReleaseGroup
		blockedSequence     int64
		execute             func(*Manager, int, *int) error
	}{
		{
			name: "nonstream repeated auth",
			dispatcher: &accountedHomeExecutionDispatcher{
				auths: []Auth{
					{ID: "home-retry-a", Provider: "home-retry-contract", Status: StatusActive},
					{ID: "home-retry-a", Provider: "home-retry-contract", Status: StatusActive},
				},
			},
			executor: &retryContractHomeExecutor{
				failure: originalHomeRetryRejection(
					&Error{
						HTTPStatus: http.StatusBadGateway, Message: "upstream unavailable",
					},
				),
			},
			maxRetryCredentials: 0,
			blockedGroup:        executionregistry.ReleaseGroup{CredentialID: "home-retry-a", Model: "gpt"},
			blockedSequence:     2,
			execute: func(manager *Manager, maxRetryCredentials int, retryLimit *int) error {
				_, err := originalHomeRetryAttempt(manager, false, maxRetryCredentials)
				return err
			},
		},
		{
			name: "stream repeated excluded auth",
			dispatcher: &accountedHomeExecutionDispatcher{
				auths: []Auth{
					{ID: "home-retry-a", Provider: "home-retry-contract", Status: StatusActive},
					{ID: "home-retry-a", Provider: "home-retry-contract", Status: StatusActive},
				},
			},
			executor: &retryContractHomeExecutor{
				failure: originalHomeRetryRejection(
					&Error{
						HTTPStatus: http.StatusBadGateway, Message: "upstream unavailable",
					},
				),
			},
			maxRetryCredentials: 0,
			blockedGroup:        executionregistry.ReleaseGroup{CredentialID: "home-retry-a", Model: "gpt"},
			blockedSequence:     2,
			execute: func(manager *Manager, maxRetryCredentials int, retryLimit *int) error {
				_, err := originalHomeRetryAttempt(manager, true, maxRetryCredentials)
				return err
			},
		},
		{
			name: "stream max retry credentials",
			dispatcher: &accountedHomeExecutionDispatcher{
				auths: []Auth{
					{ID: "home-retry-a", Provider: "home-retry-contract", Status: StatusActive},
					{ID: "home-retry-b", Provider: "home-retry-contract", Status: StatusActive},
				},
			},
			executor:            &retryContractHomeExecutor{failure: errors.New("unexpected EOF")},
			maxRetryCredentials: 1,
			blockedGroup:        executionregistry.ReleaseGroup{CredentialID: "home-retry-b", Model: "gpt"},
			blockedSequence:     1,
			execute: func(manager *Manager, maxRetryCredentials int, retryLimit *int) error {
				_, err := originalHomeRetryAttempt(manager, true, maxRetryCredentials)
				return err
			},
		},
	}

	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				executionRegistry := executionregistry.New()
				acknowledged := make(chan struct{})
				close(acknowledged)
				unacknowledged := make(chan struct{})
				var blockedReleaseSeen atomic.Bool
				executionRegistry.SetReleaseSink(
					func(group executionregistry.ReleaseGroup, sequence int64) *executionregistry.ReleaseTicket {
						done := (<-chan struct{})(acknowledged)
						if group == test.blockedGroup && sequence == test.blockedSequence {
							blockedReleaseSeen.Store(true)
							done = unacknowledged
						}
						return executionregistry.NewReleaseTicket(group, sequence, done)
					},
				)

				manager := NewManager(nil, nil, nil)
				manager.SetConfig(
					&internalconfig.Config{
						Home: internalconfig.HomeConfig{Enabled: true},
						CredentialConcurrency: internalconfig.CredentialConcurrencyConfig{
							CPACancelBound: 10 * time.Millisecond,
						},
					},
				)
				manager.PublishHomeDispatch(test.dispatcher, executionRegistry, 1)
				manager.RegisterExecutor(test.executor)

				retryLimit := -1
				errExecute := test.execute(manager, test.maxRetryCredentials, &retryLimit)
				if test.name == "stream max retry credentials" {
					if errExecute == nil || errExecute.Error() != "unexpected EOF" ||
						len(test.executor.Calls()) != 1 ||
						blockedReleaseSeen.Load() {
						t.Fatalf(
							"unknown result=%v calls=%v blocked=%t", errExecute, test.executor.Calls(),
							blockedReleaseSeen.Load(),
						)
					}
					return
				}
				if !blockedReleaseSeen.Load() {
					t.Fatal("target release was not attempted")
				}
				var homeErr *Error
				if !errors.As(errExecute, &homeErr) || homeErr == nil || homeErr.Code != "home_unavailable" {
					t.Fatalf(
						"execution error = %T %v, want Home release acknowledgement timeout",
						errExecute,
						errExecute,
					)
				}
			},
		)
	}
}

func TestHomeRetryRoundHonorsCredentialRequestRetryOverride(t *testing.T) {
	tests := []struct {
		name          string
		globalRetry   int
		override      int
		wantCallCount int
	}{
		{name: "override disables global rounds", globalRetry: 3, override: 0, wantCallCount: 2},
		{name: "override enables rounds over global", globalRetry: 0, override: 1, wantCallCount: 4},
	}
	for _, tc := range tests {
		t.Run(
			tc.name, func(t *testing.T) {
				dispatcher := &retryContractHomeDispatcher{
					authIDs:  []string{"home-retry-a", "home-retry-b"},
					metadata: map[string]any{"request_retry": tc.override},
				}
				executor := &retryContractHomeExecutor{failAll: true}
				manager := NewManager(nil, nil, nil)
				manager.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}})
				manager.SetRetryConfig(tc.globalRetry, time.Second, 2)
				manager.PublishHomeDispatch(dispatcher, executionregistry.New(), 1)
				manager.RegisterExecutor(executor)

				_, errExecute := manager.Execute(
					context.Background(),
					[]string{"home-retry-contract"},
					cliproxyexecutor.Request{Model: "gpt"},
					cliproxyexecutor.Options{},
				)
				if errExecute == nil {
					t.Fatal("Execute() error = nil, want terminal rate-limit error")
				}
				if got := len(executor.Calls()); got != tc.wantCallCount {
					t.Fatalf("executor call count = %d, want %d", got, tc.wantCallCount)
				}
			},
		)
	}
}

func TestHomeRetryRoundUsesSuccessfulDispatchAggregate(t *testing.T) {
	tests := []struct {
		name    string
		execute func(*Manager) error
	}{
		{
			name: "nonstream",
			execute: func(manager *Manager) error {
				_, errExecute := manager.Execute(
					context.Background(),
					[]string{"home-retry-contract"},
					cliproxyexecutor.Request{Model: "gpt"},
					cliproxyexecutor.Options{},
				)
				return errExecute
			},
		},
		{
			name: "count tokens",
			execute: func(manager *Manager) error {
				_, errExecute := manager.ExecuteCount(
					context.Background(),
					[]string{"home-retry-contract"},
					cliproxyexecutor.Request{Model: "gpt"},
					cliproxyexecutor.Options{},
				)
				return errExecute
			},
		},
		{
			name: "stream",
			execute: func(manager *Manager) error {
				result, errExecute := manager.ExecuteStream(
					context.Background(),
					[]string{"home-retry-contract"},
					cliproxyexecutor.Request{Model: "gpt"},
					cliproxyexecutor.Options{Stream: true},
				)
				if errExecute != nil {
					return errExecute
				}
				for range result.Chunks {
				}
				return nil
			},
		},
	}
	for _, test := range tests {
		for _, wait := range []time.Duration{time.Millisecond, 5 * time.Second} {
			t.Run(
				test.name+"/"+wait.String(), func(t *testing.T) {
					dispatcher := &aggregateRetryHomeDispatcher{}
					executor := &retryContractHomeExecutor{
						failure: retryContractRateLimitError{retryAfter: wait},
					}
					manager := NewManager(nil, nil, nil)
					manager.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}})
					manager.SetRetryConfig(0, time.Second, 1)
					manager.PublishHomeDispatch(dispatcher, executionregistry.New(), 1)
					manager.RegisterExecutor(executor)
					if errExecute := test.execute(manager); errExecute != nil {
						t.Fatalf("execution error = %v", errExecute)
					}
					if got := executor.Calls(); len(got) != 2 || got[0] != "home-retry-a" ||
						got[1] != "home-retry-b" {
						t.Fatalf("executor calls = %v, want [home-retry-a home-retry-b]", got)
					}
					if got := dispatcher.calls.Load(); got != 2 {
						t.Fatalf("Home dispatch calls = %d, want 2", got)
					}
				},
			)
		}
	}
}

func TestHomeRetryRoundUsesAuthoritativeZeroAggregate(t *testing.T) {
	tests := []struct {
		name    string
		execute func(*Manager) error
	}{
		{
			name: "nonstream",
			execute: func(manager *Manager) error {
				_, errExecute := manager.Execute(
					context.Background(),
					[]string{"home-retry-contract"},
					cliproxyexecutor.Request{Model: "gpt"},
					cliproxyexecutor.Options{},
				)
				return errExecute
			},
		},
		{
			name: "count tokens",
			execute: func(manager *Manager) error {
				_, errExecute := manager.ExecuteCount(
					context.Background(),
					[]string{"home-retry-contract"},
					cliproxyexecutor.Request{Model: "gpt"},
					cliproxyexecutor.Options{},
				)
				return errExecute
			},
		},
		{
			name: "stream",
			execute: func(manager *Manager) error {
				_, errExecute := manager.ExecuteStream(
					context.Background(),
					[]string{"home-retry-contract"},
					cliproxyexecutor.Request{Model: "gpt"},
					cliproxyexecutor.Options{Stream: true},
				)
				return errExecute
			},
		},
	}
	for _, test := range tests {
		for _, safe := range []bool{false, true} {
			t.Run(
				test.name+fmt.Sprintf("/safe=%t", safe), func(t *testing.T) {
					remoteRetry := 0
					dispatcher := &retryContractHomeDispatcher{
						authIDs: []string{
							"home-retry-a",
							"home-retry-b",
						}, metadata: map[string]any{"request_retry": 3},
						requestRetry: &remoteRetry,
					}
					failure := &Error{HTTPStatus: http.StatusBadGateway, Message: "upstream unavailable"}
					if safe {
						failure = originalHomeRetryRejection(failure)
					}
					executor := &retryContractHomeExecutor{failAll: true, failure: failure}
					manager := NewManager(nil, nil, nil)
					manager.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}})
					manager.SetRetryConfig(3, time.Second, 2)
					manager.PublishHomeDispatch(dispatcher, executionregistry.New(), 1)
					manager.RegisterExecutor(executor)
					if errExecute := test.execute(manager); !errors.Is(errExecute, failure) {
						t.Fatalf("original first-round failure lost: %v", errExecute)
					}
					want := 1
					if safe {
						want = 2
					}
					if got := executor.Calls(); len(got) != want {
						t.Fatalf("calls=%v, want %d first-round attempts", got, want)
					}
					if len(dispatcher.Excluded()) != want {
						t.Fatalf("zero aggregate allowed another round: %v", dispatcher.Excluded())
					}
				},
			)
		}
	}
}

func testOriginalHomeStreamTerminalFailure(t *testing.T, kind string, failure error) {
	t.Helper()
	dispatcher := &retryContractHomeDispatcher{
		authIDs: []string{"home-retry-a", "home-retry-b"}, metadata: map[string]any{"auth_kind": kind},
	}
	executor := &retryContractHomeExecutor{failures: map[string]error{"home-retry-a": failure}}
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}})
	manager.SetRetryConfig(0, time.Second, 2)
	executionRegistry := executionregistry.New()
	manager.PublishHomeDispatch(dispatcher, executionRegistry, 1)
	manager.RegisterExecutor(executor)
	result, err := manager.ExecuteStream(
		context.Background(), []string{"home-retry-contract"}, cliproxyexecutor.Request{Model: "gpt"},
		cliproxyexecutor.Options{Stream: true},
	)
	if err == nil && result != nil {
		for chunk := range result.Chunks {
			if chunk.Err != nil {
				err = chunk.Err
			}
		}
	}
	if !errors.Is(err, failure) {
		t.Fatalf("error=%v, want original %v", err, failure)
	}
	if calls := executor.Calls(); len(calls) != 1 || calls[0] != "home-retry-a" {
		t.Fatalf("terminal failure replayed: %v", calls)
	}
	if len(dispatcher.Excluded()) != 1 || executor.refreshCalls.Load() != 0 {
		t.Fatalf("selections=%v refreshes=%d", dispatcher.Excluded(), executor.refreshCalls.Load())
	}
	if freeze := executionRegistry.FreezeInFlight(time.Now()); len(freeze.Executions) != 0 {
		t.Fatalf("terminal selection remains active: %+v", freeze)
	}
}
