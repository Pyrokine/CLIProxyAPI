package handlers

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	internalconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/constant"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	sdkconfig "github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
)

type uniqueAPIKeyPolicyStore struct {
	mu       sync.Mutex
	requests []coreusage.APIKeyReservationRequest
}

func (s *uniqueAPIKeyPolicyStore) ReserveAPIKey(
	_ context.Context,
	request coreusage.APIKeyReservationRequest,
) (coreusage.APIKeyReservation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, request)
	return coreusage.APIKeyReservation{
		ID:        fmt.Sprintf("reservation-%d", len(s.requests)),
		ExpiresAt: time.Now().Add(time.Hour),
	}, nil
}

func (*uniqueAPIKeyPolicyStore) ReleaseAPIKeyReservation(context.Context, string, string) error {
	return nil
}

func (*uniqueAPIKeyPolicyStore) MarkAPIKeyReservationUnknown(context.Context, string, string) error {
	return nil
}

func (*uniqueAPIKeyPolicyStore) CommitAPIKeyReservation(context.Context, string, string) error {
	return nil
}

func (s *uniqueAPIKeyPolicyStore) snapshot() []coreusage.APIKeyReservationRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]coreusage.APIKeyReservationRequest(nil), s.requests...)
}

func TestExecuteWithAuthManagerRejectsTerminalReservationReplayBeforeDispatch(t *testing.T) {
	apiKey := t.Name()
	const model = "api-key-terminal-replay-model"
	registerAPIKeyPolicyModel(t, "api-key-terminal-replay", "openai", model)
	executeCalls := 0
	executor := &lifecycleCaptureExecutor{
		provider: "openai",
		execute: func(
			context.Context,
			*coreauth.Auth,
			coreexecutor.Request,
			coreexecutor.Options,
		) (coreexecutor.Response, error) {
			executeCalls++
			return coreexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
		},
	}
	handler := newLifecycleHandler(t, model, executor)
	handler.UpdateClients(
		&sdkconfig.SDKConfig{
			APIKeyRules: map[string]internalconfig.APIKeyRule{
				apiKey: {
					Limits: []internalconfig.APIKeyPolicyLimit{{Window: "1h", Requests: 10}},
				},
			},
		},
	)
	handler.SetAPIKeyPolicyStore(
		&apiKeyPolicyStoreStub{
			reserveErr: &coreusage.APIKeyReservationReplayError{
				ReservationID: "reservation-terminal",
				State:         "committed",
			},
		},
	)

	_, _, errMsg := handler.ExecuteWithAuthManager(
		apiKeyPolicyContext(apiKey, "terminal-replay-trace"),
		constant.OpenAI,
		model,
		[]byte(`{"model":"`+model+`"}`),
		"",
	)
	if errMsg == nil || errMsg.StatusCode != http.StatusServiceUnavailable || !errMsg.DirectResponse {
		t.Fatalf("terminal replay error = %#v", errMsg)
	}
	if executeCalls != 0 {
		t.Fatalf("executor calls = %d, want 0", executeCalls)
	}
}

func TestExecuteWithAuthManagerSeparatesExecutionsUnderOneTrace(t *testing.T) {
	apiKey := t.Name()
	const (
		model   = "api-key-shared-trace-model"
		traceID = "shared-http-trace"
	)
	registerAPIKeyPolicyModel(t, "api-key-shared-trace", "openai", model)
	var (
		mu             sync.Mutex
		reservationIDs []string
	)
	executor := &lifecycleCaptureExecutor{
		provider: "openai",
		execute: func(
			ctx context.Context,
			_ *coreauth.Auth,
			_ coreexecutor.Request,
			_ coreexecutor.Options,
		) (coreexecutor.Response, error) {
			mu.Lock()
			reservationIDs = append(reservationIDs, coreusage.APIKeyReservationFromContext(ctx))
			mu.Unlock()
			return coreexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
		},
	}
	handler := newLifecycleHandler(t, model, executor)
	handler.UpdateClients(
		&sdkconfig.SDKConfig{
			APIKeyRules: map[string]internalconfig.APIKeyRule{
				apiKey: {
					Limits: []internalconfig.APIKeyPolicyLimit{{Window: "1h", Requests: 10}},
				},
			},
		},
	)
	store := &uniqueAPIKeyPolicyStore{}
	handler.SetAPIKeyPolicyStore(store)
	ctx := apiKeyPolicyContext(apiKey, traceID)

	for range 2 {
		if _, _, errMsg := handler.ExecuteWithAuthManager(
			ctx,
			constant.OpenAI,
			model,
			[]byte(`{"model":"`+model+`"}`),
			"",
		); errMsg != nil {
			t.Fatalf("ExecuteWithAuthManager() error = %#v", errMsg)
		}
	}

	requests := store.snapshot()
	mu.Lock()
	gotReservationIDs := append([]string(nil), reservationIDs...)
	mu.Unlock()
	if len(requests) != 2 || len(gotReservationIDs) != 2 {
		t.Fatalf("requests/reservations = %d/%v", len(requests), gotReservationIDs)
	}
	if requests[0].LogicalRequestID == "" || requests[1].LogicalRequestID == "" ||
		requests[0].LogicalRequestID == requests[1].LogicalRequestID ||
		requests[0].LogicalRequestID == traceID || requests[1].LogicalRequestID == traceID {
		t.Fatalf(
			"logical request IDs = %q/%q under trace %q", requests[0].LogicalRequestID, requests[1].LogicalRequestID,
			traceID,
		)
	}
	if gotReservationIDs[0] != "reservation-1" || gotReservationIDs[1] != "reservation-2" {
		t.Fatalf("execution reservation IDs = %v", gotReservationIDs)
	}
}
