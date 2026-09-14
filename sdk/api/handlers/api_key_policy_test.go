package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	internalconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/constant"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	sdkconfig "github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

type apiKeyBootstrapRequestError struct{}

func (apiKeyBootstrapRequestError) Error() string {
	return "bootstrap failed"
}

func (apiKeyBootstrapRequestError) StatusCode() int {
	return http.StatusInternalServerError
}

func (apiKeyBootstrapRequestError) ExecutionDisposition() coreexecutor.ExecutionDisposition {
	return coreexecutor.ExecutionDisposition{
		DispatchPhase:  coreexecutor.DispatchPhaseNotSent,
		Outcome:        coreexecutor.ExecutionOutcomeRejectedBeforeProcessing,
		RetryDirective: coreexecutor.RetryDirectiveNextAuth,
		EffectScope:    coreexecutor.StateEffectScopeRequest,
	}
}

type apiKeyPolicyResultHook struct {
	coreauth.NoopHook
	results chan coreauth.Result
}

func (h *apiKeyPolicyResultHook) OnResult(_ context.Context, result coreauth.Result) {
	h.results <- result
}

type apiKeyPolicyStoreStub struct {
	mu sync.Mutex

	reserveRequest coreusage.APIKeyReservationRequest
	reserveResult  coreusage.APIKeyReservation
	reserveErr     error
	reserveCalls   int
	transitions    []string
}

func (s *apiKeyPolicyStoreStub) ReserveAPIKey(
	_ context.Context,
	request coreusage.APIKeyReservationRequest,
) (coreusage.APIKeyReservation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reserveCalls++
	s.reserveRequest = request
	if s.reserveErr != nil {
		return coreusage.APIKeyReservation{}, s.reserveErr
	}
	if s.reserveResult.ID == "" {
		s.reserveResult = coreusage.APIKeyReservation{
			ID:        "rsv_" + strings.Repeat("a", 64),
			ExpiresAt: time.Now().Add(time.Hour),
		}
	}
	return s.reserveResult, nil
}

func (s *apiKeyPolicyStoreStub) ReleaseAPIKeyReservation(
	_ context.Context,
	_ string,
	reason string,
) error {
	s.recordTransition("release:" + reason)
	return nil
}

func (s *apiKeyPolicyStoreStub) MarkAPIKeyReservationUnknown(
	_ context.Context,
	_ string,
	reason string,
) error {
	s.recordTransition("unknown:" + reason)
	return nil
}

func (s *apiKeyPolicyStoreStub) CommitAPIKeyReservation(
	_ context.Context,
	_ string,
	reason string,
) error {
	s.recordTransition("commit:" + reason)
	return nil
}

func (s *apiKeyPolicyStoreStub) recordTransition(transition string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.transitions = append(s.transitions, transition)
}

func (s *apiKeyPolicyStoreStub) snapshot() (
	coreusage.APIKeyReservationRequest,
	int,
	[]string,
) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reserveRequest, s.reserveCalls, append([]string(nil), s.transitions...)
}

func apiKeyPolicyContext(apiKey, requestID string) context.Context {
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	request = request.WithContext(logging.WithRequestID(request.Context(), requestID))
	ginCtx.Request = request
	ginCtx.Set("apiKey", apiKey)
	return util.WithGinContext(request.Context(), ginCtx)
}

func registerAPIKeyPolicyModel(t *testing.T, clientID, provider, model string) {
	t.Helper()
	registry.GetGlobalRegistry().RegisterClient(
		clientID,
		provider,
		[]*registry.ModelInfo{{ID: model}},
	)
	t.Cleanup(
		func() {
			registry.GetGlobalRegistry().UnregisterClient(clientID)
		},
	)
}

func TestPrepareAPIKeyExecutionMapsAndReservesCanonicalRequest(t *testing.T) {
	apiKey := t.Name()
	const (
		requestedModel = "policy-alias"
		canonicalModel = "policy-canonical"
	)
	registerAPIKeyPolicyModel(t, "api-key-policy-map", "openai", canonicalModel)
	store := &apiKeyPolicyStoreStub{}
	handler := NewBaseAPIHandlers(
		&sdkconfig.SDKConfig{
			APIKeyRules: map[string]internalconfig.APIKeyRule{
				apiKey: {
					Providers:       []string{"openai"},
					CanonicalModels: []string{canonicalModel},
					ModelMappings:   map[string]string{requestedModel: canonicalModel},
					PolicyProfile:   "paid",
					ReservationTTL:  "2h",
					Limits: []internalconfig.APIKeyPolicyLimit{
						{
							Window: "1h", Requests: 10, Tokens: 1_000, CostMicro: 50_000,
						},
					},
				},
			},
		}, nil,
	)
	handler.SetAPIKeyPolicyStore(store)

	ctx, providers, model, lifecycle, errMsg := handler.prepareAPIKeyExecution(
		apiKeyPolicyContext(apiKey, "request-1"),
		constant.OpenaiResponse,
		requestedModel,
		false,
		"execution-1",
	)
	if errMsg != nil {
		t.Fatalf("prepareAPIKeyExecution() error = %#v", errMsg)
	}
	if len(providers) != 1 || providers[0] != "openai" || model != canonicalModel {
		t.Fatalf("resolved request = providers %v model %q", providers, model)
	}
	if lifecycle == nil || lifecycle.id == "" {
		t.Fatal("missing API key reservation lifecycle")
	}
	request, calls, _ := store.snapshot()
	if calls != 1 || request.LogicalRequestID != "execution-1" ||
		request.RequestedModel != requestedModel || request.CanonicalModel != canonicalModel ||
		request.PolicyProfile != "paid" || request.TTL != 2*time.Hour {
		t.Fatalf("reservation request = %+v, calls = %d", request, calls)
	}
	if len(request.Limits) != 1 || request.Limits[0].Window != time.Hour ||
		request.Limits[0].Requests != 10 || request.Limits[0].Tokens != 1_000 ||
		request.Limits[0].CostMicro != 50_000 {
		t.Fatalf("reservation limits = %+v", request.Limits)
	}
	wantIdentity := coreusage.HashAPIKeyIdentity(apiKey)
	if request.KeyIdentity != wantIdentity || coreusage.APIKeyIdentityFromContext(ctx) != wantIdentity {
		t.Fatalf(
			"API key identity = request %q context %q", request.KeyIdentity, coreusage.APIKeyIdentityFromContext(ctx),
		)
	}
	if got := coreusage.APIKeyReservationFromContext(ctx); got != lifecycle.id {
		t.Fatalf("reservation context = %q, want %q", got, lifecycle.id)
	}
}

func TestPrepareAPIKeyExecutionFailsClosed(t *testing.T) {
	apiKey := t.Name()
	const model = "policy-fail-closed-model"
	registerAPIKeyPolicyModel(t, "api-key-policy-fail-closed", "openai", model)
	tests := []struct {
		name       string
		rule       internalconfig.APIKeyRule
		store      coreusage.APIKeyReservationStore
		wantStatus int
	}{
		{
			name:       "provider denied",
			rule:       internalconfig.APIKeyRule{Providers: []string{"claude"}},
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "canonical model denied",
			rule:       internalconfig.APIKeyRule{CanonicalModels: []string{"another-model"}},
			wantStatus: http.StatusForbidden,
		},
		{
			name: "invalid window",
			rule: internalconfig.APIKeyRule{
				Limits: []internalconfig.APIKeyPolicyLimit{
					{
						Window: "invalid", Requests: 1,
					},
				},
			},
			store:      &apiKeyPolicyStoreStub{},
			wantStatus: http.StatusServiceUnavailable,
		},
		{
			name: "reservation store unavailable",
			rule: internalconfig.APIKeyRule{
				Limits: []internalconfig.APIKeyPolicyLimit{
					{
						Window: "1h", Requests: 1,
					},
				},
			},
			wantStatus: http.StatusServiceUnavailable,
		},
		{
			name: "limit exceeded",
			rule: internalconfig.APIKeyRule{
				Limits: []internalconfig.APIKeyPolicyLimit{
					{
						Window: "1h", Requests: 1,
					},
				},
			},
			store: &apiKeyPolicyStoreStub{
				reserveErr: &coreusage.APIKeyLimitExceededError{
					RetryAt: time.Now().Add(time.Minute),
				},
			},
			wantStatus: http.StatusTooManyRequests,
		},
	}
	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				handler := NewBaseAPIHandlers(
					&sdkconfig.SDKConfig{
						APIKeyRules: map[string]internalconfig.APIKeyRule{apiKey: test.rule},
					}, nil,
				)
				handler.SetAPIKeyPolicyStore(test.store)
				_, _, _, _, errMsg := handler.prepareAPIKeyExecution(
					apiKeyPolicyContext(apiKey, "request-fail-closed"),
					constant.OpenAI,
					model,
					false,
					"execution-fail-closed",
				)
				if errMsg == nil || errMsg.StatusCode != test.wantStatus || !errMsg.DirectResponse {
					t.Fatalf("policy error = %#v, want direct status %d", errMsg, test.wantStatus)
				}
				if test.wantStatus == http.StatusTooManyRequests && errMsg.Headers.Get("Retry-After") == "" {
					t.Fatalf("429 headers = %#v", errMsg.Headers)
				}
			},
		)
	}
}

func TestAPIKeyPolicyErrorProtocolMatrix(t *testing.T) {
	protocols := []struct {
		name        string
		handlerType string
		path        string
	}{
		{name: "OpenAI Chat", handlerType: constant.OpenAI, path: "error.type"},
		{name: "OpenAI Responses", handlerType: constant.OpenaiResponse, path: "error.type"},
		{name: "Anthropic Messages", handlerType: constant.Claude, path: "error.type"},
	}
	statuses := []struct {
		status        int
		openAIType    string
		anthropicType string
	}{
		{status: http.StatusForbidden, openAIType: "invalid_request_error", anthropicType: "permission_error"},
		{status: http.StatusTooManyRequests, openAIType: "rate_limit_error", anthropicType: "rate_limit_error"},
		{status: http.StatusServiceUnavailable, openAIType: "server_error", anthropicType: "api_error"},
	}
	for _, protocol := range protocols {
		for _, status := range statuses {
			t.Run(
				protocol.name+"/"+http.StatusText(status.status), func(t *testing.T) {
					msg := apiKeyPolicyError(protocol.handlerType, status.status, time.Now().Add(time.Minute))
					if msg == nil || !msg.DirectResponse || msg.StatusCode != status.status ||
						msg.Headers.Get("Content-Type") != "application/json" {
						t.Fatalf("policy error = %#v", msg)
					}
					wantType := status.openAIType
					if protocol.handlerType == constant.Claude {
						wantType = status.anthropicType
						if got := gjson.GetBytes(msg.Body, "type").String(); got != "error" {
							t.Fatalf("Anthropic envelope type = %q", got)
						}
					}
					if got := gjson.GetBytes(msg.Body, protocol.path).String(); got != wantType {
						t.Fatalf("error type = %q, want %q; body=%s", got, wantType, msg.Body)
					}
					if status.status == http.StatusTooManyRequests && msg.Headers.Get("Retry-After") == "" {
						t.Fatalf("429 headers = %#v", msg.Headers)
					}
				},
			)
		}
	}
}

type apiKeyPolicyDispositionError struct {
	disposition coreexecutor.ExecutionDisposition
}

func (e apiKeyPolicyDispositionError) Error() string {
	return "policy disposition test error"
}

func (e apiKeyPolicyDispositionError) ExecutionDisposition() coreexecutor.ExecutionDisposition {
	return e.disposition
}

func TestAPIKeyReservationLifecycleUsesExecutionDisposition(t *testing.T) {
	tests := []struct {
		name           string
		err            error
		wantTransition string
	}{
		{
			name: "not sent releases",
			err: apiKeyPolicyDispositionError{
				disposition: coreexecutor.ExecutionDisposition{
					DispatchPhase: coreexecutor.DispatchPhaseNotSent,
					Outcome:       coreexecutor.ExecutionOutcomeUnknown,
				},
			},
			wantTransition: "release:",
		},
		{
			name: "rejected releases",
			err: apiKeyPolicyDispositionError{
				disposition: coreexecutor.ExecutionDisposition{
					DispatchPhase: coreexecutor.DispatchPhaseResponseStarted,
					Outcome:       coreexecutor.ExecutionOutcomeRejectedBeforeProcessing,
				},
			},
			wantTransition: "release:",
		},
		{
			name: "processed failure commits",
			err: apiKeyPolicyDispositionError{
				disposition: coreexecutor.ExecutionDisposition{
					DispatchPhase: coreexecutor.DispatchPhaseResponseStarted,
					Outcome:       coreexecutor.ExecutionOutcomeFailed,
				},
			},
			wantTransition: "commit:",
		},
		{
			name:           "unclassified error remains unknown",
			err:            errors.New("connection lost"),
			wantTransition: "unknown:",
		},
		{
			name: "success waits for usage settlement",
			err: apiKeyPolicyDispositionError{
				disposition: coreexecutor.ExecutionDisposition{
					DispatchPhase: coreexecutor.DispatchPhaseResponseStarted,
					Outcome:       coreexecutor.ExecutionOutcomeSucceeded,
				},
			},
		},
	}
	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				store := &apiKeyPolicyStoreStub{}
				lifecycle := &apiKeyReservationLifecycle{store: store, id: "rsv_" + strings.Repeat("b", 64)}
				lifecycle.settleError(context.Background(), test.err)
				_, _, transitions := store.snapshot()
				if test.wantTransition == "" {
					if len(transitions) != 0 {
						t.Fatalf("transitions = %v, want none", transitions)
					}
					return
				}
				if len(transitions) != 1 || !strings.HasPrefix(transitions[0], test.wantTransition) {
					t.Fatalf("transitions = %v, want prefix %q", transitions, test.wantTransition)
				}
			},
		)
	}
}

func TestExecuteWithAuthManagerPropagatesReservationContext(t *testing.T) {
	apiKey := t.Name()
	const model = "api-key-policy-execute-model"
	var executionIdentity string
	var executionReservation string
	executor := &lifecycleCaptureExecutor{
		provider: "openai",
		execute: func(
			ctx context.Context,
			_ *coreauth.Auth,
			_ coreexecutor.Request,
			_ coreexecutor.Options,
		) (coreexecutor.Response, error) {
			executionIdentity = coreusage.APIKeyIdentityFromContext(ctx)
			executionReservation = coreusage.APIKeyReservationFromContext(ctx)
			return coreexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
		},
	}
	handler := newLifecycleHandler(t, model, executor)
	handler.UpdateClients(
		&sdkconfig.SDKConfig{
			APIKeyRules: map[string]internalconfig.APIKeyRule{
				apiKey: {
					Providers: []string{"openai"},
					Limits: []internalconfig.APIKeyPolicyLimit{
						{
							Window: "1h", Requests: 10,
						},
					},
				},
			},
		},
	)
	store := &apiKeyPolicyStoreStub{}
	handler.SetAPIKeyPolicyStore(store)

	_, _, errMsg := handler.ExecuteWithAuthManager(
		apiKeyPolicyContext(apiKey, "request-execute"),
		constant.OpenAI,
		model,
		[]byte(`{"model":"`+model+`"}`),
		"",
	)
	if errMsg != nil {
		t.Fatalf("ExecuteWithAuthManager() error = %#v", errMsg)
	}
	request, calls, transitions := store.snapshot()
	if calls != 1 || len(transitions) != 0 {
		t.Fatalf("reservation calls = %d transitions = %v", calls, transitions)
	}
	if executionIdentity != request.KeyIdentity ||
		executionReservation != "rsv_"+strings.Repeat("a", 64) {
		t.Fatalf(
			"execution identity/reservation = %q/%q, request identity = %q", executionIdentity, executionReservation,
			request.KeyIdentity,
		)
	}
}

func TestStreamBootstrapRetryReusesAPIKeyReservation(t *testing.T) {
	apiKey := t.Name()
	const (
		model         = "api-key-policy-stream-retry-model"
		upstreamModel = "api-key-policy-stream-retry-upstream"
	)
	resultHook := &apiKeyPolicyResultHook{results: make(chan coreauth.Result, 2)}
	var mu sync.Mutex
	var reservationIDs []string
	calls := 0
	executor := &lifecycleCaptureExecutor{
		provider: "openai",
		stream: func(
			ctx context.Context,
			_ *coreauth.Auth,
			_ coreexecutor.Request,
			_ coreexecutor.Options,
		) (*coreexecutor.StreamResult, error) {
			mu.Lock()
			calls++
			call := calls
			reservationIDs = append(reservationIDs, coreusage.APIKeyReservationFromContext(ctx))
			mu.Unlock()
			if call == 1 {
				chunks := make(chan coreexecutor.StreamChunk, 1)
				chunks <- coreexecutor.StreamChunk{Err: apiKeyBootstrapRequestError{}}
				close(chunks)
				return &coreexecutor.StreamResult{Chunks: chunks}, nil
			}
			select {
			case result := <-resultHook.results:
				if result.Success || result.Error == nil ||
					result.Disposition.DispatchPhase != coreexecutor.DispatchPhaseNotSent ||
					result.Disposition.Outcome != coreexecutor.ExecutionOutcomeRejectedBeforeProcessing ||
					result.Disposition.RetryDirective != coreexecutor.RetryDirectiveNextAuth ||
					result.Disposition.EffectScope != coreexecutor.StateEffectScopeRequest {
					return nil, errors.New("first bootstrap failure lost its replay disposition")
				}
			default:
				return nil, errors.New("first bootstrap stream was not completed before retry")
			}
			chunks := make(chan coreexecutor.StreamChunk, 1)
			chunks <- coreexecutor.StreamChunk{Payload: []byte(`data: {"model":"` + upstreamModel + `"}` + "\n\n")}
			close(chunks)
			return &coreexecutor.StreamResult{Chunks: chunks}, nil
		},
	}
	handler := newLifecycleHandler(t, model, executor, resultHook)
	handler.UpdateClients(
		&sdkconfig.SDKConfig{
			Streaming: sdkconfig.StreamingConfig{BootstrapRetries: 1},
			APIKeyRules: map[string]internalconfig.APIKeyRule{
				apiKey: {Limits: []internalconfig.APIKeyPolicyLimit{{Window: "1h", Requests: 10}}},
			},
		},
	)
	handler.AuthManager.SetOAuthModelAlias(
		map[string][]internalconfig.OAuthModelAlias{
			"openai": {
				{
					Name: upstreamModel, Alias: model, Fork: true, ForceMapping: true,
				},
			},
		},
	)
	store := &apiKeyPolicyStoreStub{}
	handler.SetAPIKeyPolicyStore(store)

	chunks, _, errs := handler.ExecuteStreamChunksWithAuthManager(
		apiKeyPolicyContext(apiKey, "request-stream-retry"),
		constant.OpenAI,
		model,
		[]byte(`{"model":"`+model+`","stream":true}`),
		"",
	)
	payloads := 0
	terminals := 0
	for chunk := range chunks {
		if chunk.Acknowledge == nil {
			t.Fatalf("stream chunk has no delivery acknowledgement: %+v", chunk)
		}
		if chunk.Terminal {
			terminals++
			chunk.Acknowledge(nil)
			continue
		}
		if chunk.Err != nil || len(chunk.Payload) == 0 {
			t.Fatalf("unexpected stream chunk = %+v", chunk)
		}
		payloads++
		chunk.Acknowledge(nil)
	}
	if payloads != 1 || terminals != 1 {
		t.Fatalf("stream payload/terminal counts = %d/%d, want 1/1", payloads, terminals)
	}
	for errMsg := range errs {
		if errMsg != nil {
			t.Fatalf("stream error = %#v", errMsg)
		}
	}
	request, reserveCalls, transitions := store.snapshot()
	mu.Lock()
	gotCalls := calls
	gotReservationIDs := append([]string(nil), reservationIDs...)
	mu.Unlock()
	if gotCalls != 2 || reserveCalls != 1 || len(gotReservationIDs) != 2 ||
		gotReservationIDs[0] == "" || gotReservationIDs[0] != gotReservationIDs[1] {
		t.Fatalf(
			"stream calls/reservations = %d/%d IDs %v request %+v",
			gotCalls,
			reserveCalls,
			gotReservationIDs,
			request,
		)
	}
	if len(transitions) != 0 {
		t.Fatalf("bootstrap retry changed reservation state: %v", transitions)
	}
}

func TestStreamDeliveryFailureMarksAPIKeyReservationUnknown(t *testing.T) {
	store := &apiKeyPolicyStoreStub{}
	lifecycle := &apiKeyReservationLifecycle{store: store, id: "rsv_" + strings.Repeat("c", 64)}
	acknowledged := false
	ack := lifecycle.wrapAcknowledgement(
		context.Background(), func(error) {
			acknowledged = true
		},
	)
	ack(errors.New("downstream write failed"))
	_, _, transitions := store.snapshot()
	if !acknowledged || len(transitions) != 1 || !strings.HasPrefix(transitions[0], "unknown:") {
		t.Fatalf("acknowledged = %t transitions = %v", acknowledged, transitions)
	}
}
