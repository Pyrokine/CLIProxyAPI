package executor

import (
	"context"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	internalconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/constant"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	internalusage "github.com/Pyrokine/CLIProxyAPI/v6/internal/usage"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	sdkconfig "github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	"github.com/gin-gonic/gin"
)

type apiKeyReservationIntegrationMode uint8

const (
	apiKeyReservationSuccessZero apiKeyReservationIntegrationMode = iota
	apiKeyReservationFailedWithEvidence
	apiKeyReservationRejectedWithoutEvidence
	apiKeyReservationStreamDeliveryFailure
)

type apiKeyReservationIntegrationError struct {
	disposition coreexecutor.ExecutionDisposition
}

func (apiKeyReservationIntegrationError) Error() string {
	return "API key reservation integration failure"
}

func (e apiKeyReservationIntegrationError) ExecutionDisposition() coreexecutor.ExecutionDisposition {
	return e.disposition
}

type apiKeyReservationUsageExecutor struct {
	mode apiKeyReservationIntegrationMode
}

func (*apiKeyReservationUsageExecutor) Identifier() string {
	return "openai"
}

func (e *apiKeyReservationUsageExecutor) Execute(
	ctx context.Context,
	auth *coreauth.Auth,
	req coreexecutor.Request,
	_ coreexecutor.Options,
) (coreexecutor.Response, error) {
	reporter := newUsageReporter(ctx, e, req.Model, auth)
	switch e.mode {
	case apiKeyReservationSuccessZero:
		reporter.ensurePublished(ctx)
		return coreexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
	case apiKeyReservationFailedWithEvidence:
		err := apiKeyReservationIntegrationError{
			disposition: coreexecutor.ExecutionDisposition{
				DispatchPhase: coreexecutor.DispatchPhaseResponseStarted,
				Outcome:       coreexecutor.ExecutionOutcomeFailed,
			},
		}
		reporter.publishFailureWithDetail(
			ctx, coreusage.Detail{
				InputTokens: 150,
				TotalTokens: 150,
			}, err,
		)
		return coreexecutor.Response{}, err
	case apiKeyReservationRejectedWithoutEvidence:
		err := apiKeyReservationIntegrationError{
			disposition: coreexecutor.ExecutionDisposition{
				DispatchPhase: coreexecutor.DispatchPhaseResponseStarted,
				Outcome:       coreexecutor.ExecutionOutcomeRejectedBeforeProcessing,
			},
		}
		reporter.publishFailure(ctx, err)
		return coreexecutor.Response{}, err
	default:
		return coreexecutor.Response{}, errors.New("unsupported non-stream integration mode")
	}
}

func (e *apiKeyReservationUsageExecutor) ExecuteStream(
	ctx context.Context,
	auth *coreauth.Auth,
	req coreexecutor.Request,
	_ coreexecutor.Options,
) (*coreexecutor.StreamResult, error) {
	if e.mode != apiKeyReservationStreamDeliveryFailure {
		return nil, errors.New("unsupported stream integration mode")
	}
	reporter := newUsageReporter(ctx, e, req.Model, auth)
	reporter.publish(ctx, coreusage.Detail{InputTokens: 20, TotalTokens: 20})
	chunks := make(chan coreexecutor.StreamChunk, 1)
	chunks <- coreexecutor.StreamChunk{Terminal: true}
	close(chunks)
	return &coreexecutor.StreamResult{Chunks: chunks}, nil
}

func (e *apiKeyReservationUsageExecutor) CountTokens(
	ctx context.Context,
	auth *coreauth.Auth,
	req coreexecutor.Request,
	opts coreexecutor.Options,
) (coreexecutor.Response, error) {
	return e.Execute(ctx, auth, req, opts)
}

func (*apiKeyReservationUsageExecutor) Refresh(context.Context, *coreauth.Auth) (*coreauth.Auth, error) {
	return nil, nil
}

func (*apiKeyReservationUsageExecutor) HttpRequest(
	context.Context,
	*coreauth.Auth,
	*http.Request,
) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

type recordingAPIKeyReservationStore struct {
	delegate coreusage.APIKeyReservationStore

	mu          sync.Mutex
	request     coreusage.APIKeyReservationRequest
	reservation coreusage.APIKeyReservation
}

func (s *recordingAPIKeyReservationStore) ReserveAPIKey(
	ctx context.Context,
	request coreusage.APIKeyReservationRequest,
) (coreusage.APIKeyReservation, error) {
	reservation, err := s.delegate.ReserveAPIKey(ctx, request)
	if err != nil {
		return coreusage.APIKeyReservation{}, err
	}
	s.mu.Lock()
	s.request = request
	s.reservation = reservation
	s.mu.Unlock()
	return reservation, nil
}

func (s *recordingAPIKeyReservationStore) ReleaseAPIKeyReservation(
	ctx context.Context,
	reservationID,
	reason string,
) error {
	return s.delegate.ReleaseAPIKeyReservation(ctx, reservationID, reason)
}

func (s *recordingAPIKeyReservationStore) MarkAPIKeyReservationUnknown(
	ctx context.Context,
	reservationID,
	reason string,
) error {
	return s.delegate.MarkAPIKeyReservationUnknown(ctx, reservationID, reason)
}

func (s *recordingAPIKeyReservationStore) CommitAPIKeyReservation(
	ctx context.Context,
	reservationID,
	reason string,
) error {
	return s.delegate.CommitAPIKeyReservation(ctx, reservationID, reason)
}

func (s *recordingAPIKeyReservationStore) snapshot() (
	coreusage.APIKeyReservationRequest,
	coreusage.APIKeyReservation,
) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.request, s.reservation
}

type blockingUsagePlugin struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newBlockingUsagePlugin() *blockingUsagePlugin {
	return &blockingUsagePlugin{entered: make(chan struct{}), release: make(chan struct{})}
}

func (p *blockingUsagePlugin) HandleUsage(context.Context, coreusage.Record) {
	p.once.Do(func() { close(p.entered) })
	<-p.release
}

func (p *blockingUsagePlugin) unblock() {
	select {
	case <-p.release:
	default:
		close(p.release)
	}
}

type apiKeyReservationIntegrationHarness struct {
	handler      *handlers.BaseAPIHandler
	usageManager *coreusage.Manager
	persister    *internalusage.Persister
	store        *recordingAPIKeyReservationStore
	gate         *blockingUsagePlugin
	apiKey       string
	model        string
}

func newAPIKeyReservationIntegrationHarness(
	t *testing.T,
	mode apiKeyReservationIntegrationMode,
	withGate bool,
) *apiKeyReservationIntegrationHarness {
	t.Helper()
	apiKey := "sk-api-key-reservation-integration"
	model := "api-key-reservation-integration-model"
	persister := internalusage.NewPersister(t.TempDir(), internalconfig.UsageRetention{Days: -1})
	persister.SetPriceFunc(
		func(candidate string) (float64, float64, float64, bool) {
			return 10, 0, 0, candidate == model
		},
	)
	persister.Start(context.Background())
	if !persister.IsReady() {
		t.Fatal("usage persister did not start")
	}

	usageManager := coreusage.NewManager(8)
	var gate *blockingUsagePlugin
	if withGate {
		gate = newBlockingUsagePlugin()
		usageManager.Register(gate)
	}
	usageManager.Register(internalusage.NewLoggerPlugin(persister, nil, func() bool { return true }))
	usageManager.Start(context.Background())

	executor := &apiKeyReservationUsageExecutor{mode: mode}
	authManager := coreauth.NewManager(nil, nil, nil)
	authManager.RegisterExecutor(executor)
	auth := &coreauth.Auth{
		ID:       "api-key-reservation-integration-auth",
		Provider: executor.Identifier(),
		Status:   coreauth.StatusActive,
	}
	if _, err := authManager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(
		auth.ID,
		auth.Provider,
		[]*registry.ModelInfo{{ID: model}},
	)

	store := &recordingAPIKeyReservationStore{delegate: persister}
	handler := handlers.NewBaseAPIHandlers(
		&sdkconfig.SDKConfig{
			APIKeyRules: map[string]internalconfig.APIKeyRule{
				apiKey: {
					ReservationTTL: "1h",
					Limits: []internalconfig.APIKeyPolicyLimit{
						{
							Window: "1h", Requests: 10, Tokens: 100, CostMicro: 1_000,
						},
					},
				},
			},
		}, authManager,
	)
	handler.SetAPIKeyPolicyStore(store)

	t.Cleanup(
		func() {
			if gate != nil {
				gate.unblock()
			}
			usageManager.Stop()
			persister.Stop()
			registry.GetGlobalRegistry().UnregisterClient(auth.ID)
		},
	)
	return &apiKeyReservationIntegrationHarness{
		handler:      handler,
		usageManager: usageManager,
		persister:    persister,
		store:        store,
		gate:         gate,
		apiKey:       apiKey,
		model:        model,
	}
}

func (h *apiKeyReservationIntegrationHarness) requestContext() context.Context {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	ginCtx.Set("apiKey", h.apiKey)
	ctx := util.WithGinContext(ginCtx.Request.Context(), ginCtx)
	return coreusage.WithManager(ctx, h.usageManager)
}

func waitForAPIKeyReservationUsageEvent(
	t *testing.T,
	persister *internalusage.Persister,
	model string,
) internalusage.FlatDetail {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		events, total, err := persister.QueryEvents(
			time.Now().Add(-time.Hour),
			time.Now().Add(time.Hour),
			internalusage.EventFilters{Model: model},
			1,
			10,
			"timestamp",
			true,
		)
		if err != nil {
			t.Fatalf("query usage events: %v", err)
		}
		if total == 1 && len(events) == 1 {
			return events[0]
		}
		if time.Now().After(deadline) {
			t.Fatalf("usage event count = %d/%d before deadline", len(events), total)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func assertAPIKeyReservationReplayState(
	t *testing.T,
	store coreusage.APIKeyReservationStore,
	request coreusage.APIKeyReservationRequest,
	reservationID,
	wantState string,
) {
	t.Helper()
	_, err := store.ReserveAPIKey(context.Background(), request)
	var replayErr *coreusage.APIKeyReservationReplayError
	if !errors.As(err, &replayErr) {
		t.Fatalf("reservation replay error = %v", err)
	}
	if replayErr.ReservationID != reservationID || replayErr.State != wantState {
		t.Fatalf("reservation replay = %+v, want %s/%s", replayErr, reservationID, wantState)
	}
}

func TestAPIKeyReservationSettlesThroughHandlerUsagePublication(t *testing.T) {
	tests := []struct {
		name         string
		mode         apiKeyReservationIntegrationMode
		wantState    string
		wantFailed   bool
		wantTokens   int64
		wantCost     float64
		wantResponse bool
	}{
		{
			name:         "successful zero usage counts request",
			mode:         apiKeyReservationSuccessZero,
			wantState:    "settled",
			wantResponse: true,
		},
		{
			name:       "processed failure keeps usage evidence",
			mode:       apiKeyReservationFailedWithEvidence,
			wantState:  "committed",
			wantFailed: true,
			wantTokens: 150,
			wantCost:   0.0015,
		},
		{
			name:       "rejected failure without evidence stays released",
			mode:       apiKeyReservationRejectedWithoutEvidence,
			wantState:  "released",
			wantFailed: true,
		},
	}
	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				harness := newAPIKeyReservationIntegrationHarness(t, test.mode, false)
				body, _, errMsg := harness.handler.ExecuteWithAuthManager(
					harness.requestContext(),
					constant.OpenAI,
					harness.model,
					[]byte(`{"model":"`+harness.model+`"}`),
					"",
				)
				if test.wantResponse {
					if errMsg != nil || len(body) == 0 {
						t.Fatalf("handler result = %s/%#v", body, errMsg)
					}
				} else if errMsg == nil {
					t.Fatal("handler returned success for failing executor")
				}

				event := waitForAPIKeyReservationUsageEvent(t, harness.persister, harness.model)
				if event.APIKey != coreusage.HashAPIKeyIdentity(harness.apiKey) ||
					event.Failed != test.wantFailed || event.Tokens.TotalTokens != test.wantTokens ||
					math.Abs(event.Cost-test.wantCost) > 1e-12 {
					t.Fatalf("usage event = %+v", event)
				}
				request, reservation := harness.store.snapshot()
				if request.LogicalRequestID == "" || reservation.ID == "" {
					t.Fatalf("reservation snapshot = %+v/%+v", request, reservation)
				}
				assertAPIKeyReservationReplayState(
					t,
					harness.persister,
					request,
					reservation.ID,
					test.wantState,
				)
			},
		)
	}
}

func TestAPIKeyReservationUsageEvidenceSettlesAfterStreamDeliveryFailure(t *testing.T) {
	harness := newAPIKeyReservationIntegrationHarness(t, apiKeyReservationStreamDeliveryFailure, true)
	chunks, _, errs := harness.handler.ExecuteStreamChunksWithAuthManager(
		harness.requestContext(),
		constant.OpenAI,
		harness.model,
		[]byte(`{"model":"`+harness.model+`","stream":true}`),
		"",
	)
	for chunk := range chunks {
		if !chunk.Terminal || chunk.Acknowledge == nil {
			t.Fatalf("stream chunk = %+v", chunk)
		}
		select {
		case <-harness.gate.entered:
		case <-time.After(time.Second):
			t.Fatal("usage publication did not reach the blocking plugin")
		}
		chunk.Acknowledge(errors.New("downstream write failed"))
		harness.gate.unblock()
	}
	for errMsg := range errs {
		if errMsg != nil {
			t.Fatalf("stream error = %#v", errMsg)
		}
	}

	event := waitForAPIKeyReservationUsageEvent(t, harness.persister, harness.model)
	if event.Tokens.TotalTokens != 20 || math.Abs(event.Cost-0.0002) > 1e-12 {
		t.Fatalf("usage event = %+v", event)
	}
	request, reservation := harness.store.snapshot()
	assertAPIKeyReservationReplayState(
		t,
		harness.persister,
		request,
		reservation.ID,
		"settled",
	)
}
