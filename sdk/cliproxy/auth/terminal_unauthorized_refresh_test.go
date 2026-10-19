package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type terminalUnauthorizedRefreshExecutor struct {
	schedulerTestExecutor
	err     error
	calls   int
	started chan struct{}
	proceed chan struct{}
}

func (*terminalUnauthorizedRefreshExecutor) Identifier() string { return "terminal-unauthorized-test" }

func (e *terminalUnauthorizedRefreshExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	e.calls++
	if e.started != nil {
		close(e.started)
		<-e.proceed
	}
	if e.err != nil {
		return nil, e.err
	}
	updated := auth.Clone()
	updated.Metadata["access_token"] = "new-access"
	return updated, nil
}

func TestForceRefreshAuthAcceptsNilContext(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	//lint:ignore SA1012 验证手动刷新入口接受 nil context 的既有合同
	if _, err := manager.ForceRefreshAuth(nil, "missing"); err == nil {
		t.Fatal("unknown credential refresh returned success")
	}
}

func TestRejectedAccessAndInvalidGrantRemainBlockedUntilTokenChanges(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	executor := &terminalUnauthorizedRefreshExecutor{
		err: &Error{HTTPStatus: http.StatusBadRequest, Message: "invalid_grant"},
	}
	manager.RegisterExecutor(executor)
	registered, err := manager.Register(context.Background(), &Auth{
		ID: "rejected-token", Provider: executor.Identifier(), Status: StatusActive,
		Metadata: map[string]any{
			"access_token": "old-access", "refresh_token": "invalid-refresh",
			"expired": time.Now().Add(time.Hour).Format(time.RFC3339),
		},
		ModelStates: map[string]*ModelState{"grok-4": {Status: StatusActive}},
	})
	if err != nil {
		t.Fatalf("Register() = %v", err)
	}
	rejected := &Error{HTTPStatus: http.StatusUnauthorized, Message: "access token rejected"}
	if refreshed, ok := manager.tryRefreshAfterUnauthorized(context.Background(), registered, executor, rejected, false); ok || refreshed != registered {
		t.Fatalf("unexpected retry after invalid_grant: refreshed=%#v retried=%t", refreshed, ok)
	}
	current, _ := manager.GetByID(registered.ID)
	if !current.Unavailable || current.Status != StatusError || current.NextRefreshAfter != (time.Time{}) ||
		!current.NextRetryAfter.IsZero() || !hasUnauthorizedAuthFailure(current) {
		t.Fatalf("rejected credential must remain terminal: %#v", current)
	}
	if blocked, _, _ := isAuthBlockedForModel(current, "grok-4", time.Now()); !blocked {
		t.Fatal("terminal credential with available model was selectable")
	}
	if projection := clientModelProjectionForAuth(current, "grok-4", time.Now()); !projection.Suspended {
		t.Fatal("terminal credential remained visible in model registry")
	}
	if _, err = manager.refreshAuthForRequest(context.Background(), registered.ID, "old-access"); err == nil || executor.calls != 1 {
		t.Fatalf("automatic refresh replayed invalid token: calls=%d error=%v", executor.calls, err)
	}
	if manager.markRefreshPending(registered.ID, registered.Generation, time.Now()) != nil {
		t.Fatal("scheduler queued rejected refresh token")
	}
	manager.MarkResult(context.Background(), Result{
		AuthID: registered.ID, AuthGeneration: registered.Generation,
		Provider: executor.Identifier(), Model: "grok-4", Success: true,
	})
	current, _ = manager.GetByID(registered.ID)
	if !hasTerminalUnauthorizedAuthFailure(current) {
		t.Fatalf("older success revived rejected credential: %#v", current)
	}
	if clearAuthStateOnSuccess(current, time.Now()) || clearCooldownStateForAuth(current, time.Now()) ||
		!hasTerminalUnauthorizedAuthFailure(current) {
		t.Fatal("result or cooldown clear revived terminal credential")
	}
	incoming := current.Clone()
	incoming.NextRefreshAfter = time.Now().Add(5 * time.Minute)
	incoming.Disabled = true
	incoming.Status = StatusDisabled
	if _, err = manager.Update(context.Background(), incoming); err != nil {
		t.Fatalf("Update(disable) = %v", err)
	}
	incoming, _ = manager.GetByID(registered.ID)
	incoming.Disabled = false
	incoming.Status = StatusActive
	if _, err = manager.Update(context.Background(), incoming); err != nil {
		t.Fatalf("Update(enable) = %v", err)
	}
	current, _ = manager.GetByID(registered.ID)
	if !hasTerminalUnauthorizedAuthFailure(current) || current.Disabled || !current.NextRefreshAfter.IsZero() {
		t.Fatalf("unchanged credential revived by persistent update: %#v", current)
	}
	manager.mu.Lock()
	restored := manager.restoreCooldownRecordLocked(CooldownStateRecord{
		AuthID: current.ID, NextRetryAfter: time.Now().Add(time.Minute),
	}, time.Now())
	manager.mu.Unlock()
	if restored {
		t.Fatal("saved cooldown overwrote terminal credential")
	}

	executor.err = errors.New("temporary network error")
	if _, err = manager.ForceRefreshAuth(context.Background(), registered.ID); err == nil {
		t.Fatal("manual force refresh should report failure")
	}
	current, _ = manager.GetByID(registered.ID)
	if !hasUnauthorizedAuthFailure(current) || !current.Unavailable {
		t.Fatalf("failed force refresh revived terminal credential: %#v", current)
	}
	executor.err = nil
	if _, err = manager.ForceRefreshAuth(context.Background(), registered.ID); err != nil {
		t.Fatalf("ForceRefreshAuth() = %v", err)
	}
	current, _ = manager.GetByID(registered.ID)
	if hasUnauthorizedAuthFailure(current) || current.Unavailable || current.Metadata["access_token"] != "new-access" {
		t.Fatalf("new tokens failed to clear terminal state: %#v", current)
	}
	if projection := clientModelProjectionForAuth(current, "grok-4", time.Now()); projection.Suspended {
		t.Fatalf("new token did not restore model registry: %#v", projection)
	}
}

func TestRejectedAccessTokenNotSelectedDuringRefresh(t *testing.T) {
	for _, test := range []struct {
		name       string
		refreshErr error
	}{
		{name: "successful refresh"},
		{name: "failed refresh", refreshErr: errors.New("refresh service unavailable")},
	} {
		t.Run(test.name, func(t *testing.T) {
			const provider = "terminal-unauthorized-test"
			model := "rejected-access-" + test.name
			authID := "rejected-access-auth-" + test.name
			manager := NewManager(nil, &RoundRobinSelector{}, nil)
			executor := &terminalUnauthorizedRefreshExecutor{
				err: test.refreshErr, started: make(chan struct{}), proceed: make(chan struct{}),
			}
			manager.RegisterExecutor(executor)
			registerSchedulerModels(t, provider, model, authID)
			registered, err := manager.Register(context.Background(), &Auth{
				ID: authID, Provider: provider, Status: StatusActive,
				Metadata: map[string]any{
					"access_token": "old-access", "refresh_token": "refresh-token",
					"expired": time.Now().Add(time.Hour).Format(time.RFC3339),
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			pick := func() (*Auth, error) {
				return manager.scheduler.pickSingle(context.Background(), provider, model, cliproxyexecutor.Options{}, nil)
			}
			if selected, errPick := pick(); errPick != nil || selected == nil || selected.ID != authID {
				t.Fatalf("pick before 401: selected=%t error=%v", selected != nil, errPick)
			}
			if test.refreshErr == nil {
				updated := registered.Clone()
				updated.Label = "edited while request was in flight"
				if _, errUpdate := manager.Update(context.Background(), updated); errUpdate != nil {
					t.Fatal(errUpdate)
				}
			}

			result := make(chan bool, 1)
			go func() {
				_, retried := manager.tryRefreshAfterUnauthorized(
					context.Background(), registered, executor,
					&Error{HTTPStatus: http.StatusUnauthorized, Message: "access token rejected"}, false,
				)
				result <- retried
			}()
			released := false
			defer func() {
				if !released {
					close(executor.proceed)
					<-result
				}
			}()
			select {
			case <-executor.started:
			case <-time.After(5 * time.Second):
				t.Fatal("refresh did not start")
			}
			if selected, errPick := pick(); selected != nil || errPick == nil {
				t.Fatalf("rejected token selected during refresh: selected=%t error=%v", selected != nil, errPick)
			}
			close(executor.proceed)
			released = true
			retried := <-result
			if (test.refreshErr == nil) != retried {
				t.Fatalf("refresh retried = %t, error = %v", retried, test.refreshErr)
			}
			if test.refreshErr == nil {
				if selected, errPick := pick(); errPick != nil || selected == nil || authAccessToken(selected) != "new-access" {
					t.Fatalf("refreshed token unavailable: selected=%t error=%v", selected != nil, errPick)
				}
				return
			}
			if selected, errPick := pick(); selected != nil || errPick == nil {
				t.Fatalf("rejected token selected after refresh failure: selected=%t error=%v", selected != nil, errPick)
			}
			current, _ := manager.GetByID(authID)
			current.Label = "edited"
			if _, errUpdate := manager.Update(context.Background(), current); errUpdate != nil {
				t.Fatal(errUpdate)
			}
			if selected, errPick := pick(); selected != nil || errPick == nil {
				t.Fatalf("unchanged rejected token selected after metadata update: selected=%t error=%v", selected != nil, errPick)
			}
			current, _ = manager.GetByID(authID)
			current.Metadata["access_token"] = "replacement-access"
			if _, errUpdate := manager.Update(context.Background(), current); errUpdate != nil {
				t.Fatal(errUpdate)
			}
			if selected, errPick := pick(); errPick != nil || selected == nil || authAccessToken(selected) != "replacement-access" {
				t.Fatalf("replacement token unavailable: selected=%t error=%v", selected != nil, errPick)
			}
		})
	}
}

func TestSuccessfulRefreshDoesNotClearIndependentModelQuota(t *testing.T) {
	now := time.Now()
	quotaUntil := now.Add(time.Hour)
	auth := &Auth{ModelStates: map[string]*ModelState{
		"quota-model": {
			Status: StatusError, Unavailable: true, NextRetryAfter: quotaUntil,
			StatusMessage: "unauthorized", LastError: &Error{HTTPStatus: http.StatusUnauthorized},
			Quota: QuotaState{Exceeded: true, Family: "paid", NextRecoverAt: quotaUntil},
		},
		"auth-model": {
			Status: StatusError, Unavailable: true,
			LastError: &Error{HTTPStatus: http.StatusUnauthorized},
		},
	}}
	clearUnauthorizedModelStates(auth, now)
	quota := auth.ModelStates["quota-model"]
	if quota.LastError != nil || !quota.Quota.Exceeded || !quota.Unavailable || !quota.NextRetryAfter.Equal(quotaUntil) {
		t.Fatalf("refresh cleared independent quota restriction: %#v", quota)
	}
	cleared := auth.ModelStates["auth-model"]
	if cleared.Unavailable || cleared.Status != StatusActive || cleared.LastError != nil {
		t.Fatalf("refresh did not clear unauthorized model state: %#v", cleared)
	}
}

func TestTerminalUnauthorizedRefreshPersistsAcrossRestart(t *testing.T) {
	store := &recordingCooldownStateStore{}
	manager := NewManager(nil, nil, nil)
	manager.SetCooldownStateStore(store)
	executor := &terminalUnauthorizedRefreshExecutor{
		err: &Error{HTTPStatus: http.StatusBadRequest, Message: "invalid_grant"},
	}
	manager.RegisterExecutor(executor)
	original := &Auth{
		ID: "persisted-terminal", FileName: "persisted-terminal.json",
		Provider: executor.Identifier(), Status: StatusActive,
		Metadata: map[string]any{
			"access_token": "rejected", "refresh_token": "invalid", "disable_cooling": true,
			"expired": time.Now().Add(time.Hour).Format(time.RFC3339),
		},
	}
	registered, err := manager.Register(context.Background(), original)
	if err != nil {
		t.Fatalf("Register() = %v", err)
	}
	manager.tryRefreshAfterUnauthorized(context.Background(), registered, executor,
		&Error{HTTPStatus: http.StatusUnauthorized}, false)
	disabled, _ := manager.GetByID(original.ID)
	disabled.Disabled = true
	disabled.Status = StatusDisabled
	if _, err = manager.Update(context.Background(), disabled); err != nil {
		t.Fatalf("Update(disable) = %v", err)
	}
	manager.PersistCooldownStates(context.Background())
	store.mu.Lock()
	records := cloneCooldownStateRecords(store.records)
	store.mu.Unlock()
	if len(records) != 1 || records[0].NextRetryAfter != (time.Time{}) ||
		records[0].LastError == nil || !isInvalidGrantResultError(records[0].LastError) {
		t.Fatalf("terminal state not persisted: %#v", records)
	}

	restarted := NewManager(nil, nil, nil)
	restarted.RegisterExecutor(executor)
	restoredAuth := original.Clone()
	restoredAuth.Disabled = true
	restoredAuth.Status = StatusDisabled
	if _, err = restarted.Register(context.Background(), restoredAuth); err != nil {
		t.Fatalf("restarted Register() = %v", err)
	}
	restoredStore := &recordingCooldownStateStore{load: records}
	restarted.SetCooldownStateStore(restoredStore)
	if err = restarted.RestoreCooldownStates(context.Background()); err != nil {
		t.Fatalf("RestoreCooldownStates() = %v", err)
	}
	current, _ := restarted.GetByID(original.ID)
	if !hasTerminalUnauthorizedAuthFailure(current) || !current.Disabled {
		t.Fatalf("disabled credential lost its terminal state: %#v", current)
	}
	current.Disabled = false
	current.Status = StatusActive
	if _, err = restarted.Update(context.Background(), current); err != nil {
		t.Fatalf("Update(enable after restore) = %v", err)
	}
	current, _ = restarted.GetByID(original.ID)
	if !hasTerminalUnauthorizedAuthFailure(current) || current.Disabled {
		t.Fatalf("restored credential was selectable: %#v", current)
	}
	if restarted.shouldRefresh(current, time.Now()) || restarted.markRefreshPending(current.ID, current.Generation, time.Now()) != nil {
		t.Fatal("restored credential scheduled invalid refresh token")
	}
}

func TestInvalidGrantWithoutRejectedAccessKeepsUnexpiredCredentialActive(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	executor := &terminalUnauthorizedRefreshExecutor{
		err: &Error{HTTPStatus: http.StatusBadRequest, Message: "invalid_grant"},
	}
	manager.RegisterExecutor(executor)
	registered, err := manager.Register(context.Background(), &Auth{
		ID: "preflight-token", Provider: executor.Identifier(), Status: StatusActive,
		Metadata: map[string]any{
			"access_token": "old-access", "refresh_token": "invalid-refresh",
			"expired": time.Now().Add(time.Hour).Format(time.RFC3339),
		},
	})
	if err != nil {
		t.Fatalf("Register() = %v", err)
	}
	if _, err = manager.refreshAuthForRequest(context.Background(), registered.ID, "old-access"); err == nil {
		t.Fatal("expected preflight refresh error")
	}
	current, _ := manager.GetByID(registered.ID)
	if hasTerminalUnauthorizedAuthFailure(current) || current.Unavailable || current.Status != StatusActive {
		t.Fatalf("unrejected access token was disabled: %#v", current)
	}
}

func TestInvalidatedOAuthMessagesUseTerminalStatusClassification(t *testing.T) {
	for _, message := range []string{"invalid_grant", "Encountered invalidated oauth token: test-token", "ENCOUNTERED INVALIDATED OAUTH TOKEN: test-token"} {
		for _, status := range []int{0, http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests, http.StatusInternalServerError} {
			err := &Error{HTTPStatus: status, Message: message}
			want := status == 0 || status == http.StatusBadRequest || status == http.StatusUnauthorized
			if isInvalidGrantError(err) != want || isInvalidGrantResultError(err) != want {
				t.Errorf("status=%d message=%q terminal classification differs from %t", status, message, want)
			}
		}
	}
	if isInvalidGrantResultError(&Error{HTTPStatus: http.StatusUnauthorized, Message: "access token temporarily rejected"}) {
		t.Fatal("ordinary unauthorized response was classified as terminal invalidation")
	}
}

type sameAccessTokenRefreshExecutor struct {
	schedulerTestExecutor
	renew bool
	err   error
}

func (*sameAccessTokenRefreshExecutor) Identifier() string { return "same-access-token-refresh" }

func (e *sameAccessTokenRefreshExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	if e.err != nil {
		return nil, e.err
	}
	updated := auth.Clone()
	if e.renew {
		updated.Metadata["expired"] = time.Now().Add(2 * time.Hour).Format(time.RFC3339)
	}
	return updated, nil
}

func TestSuccessfulRefreshClearsRejectedUnchangedAccessToken(t *testing.T) {
	for _, quota := range []bool{false, true} {
		for _, test := range []struct {
			name  string
			renew bool
			err   error
		}{
			{name: "renewed", renew: true},
			{name: "unchanged"},
			{name: "failed", err: errors.New("synthetic renewal failure")},
		} {
			name := test.name
			if quota {
				name += " with independent quota"
			}
			t.Run(name, func(t *testing.T) {
				manager := NewManager(nil, nil, nil)
				executor := &sameAccessTokenRefreshExecutor{renew: test.renew, err: test.err}
				manager.RegisterExecutor(executor)
				auth := &Auth{
					ID: "synthetic-same-token-renewal", Provider: executor.Identifier(), Status: StatusActive,
					Metadata: map[string]any{
						"access_token": "synthetic-same-access", "refresh_token": "synthetic-same-refresh",
						"expired": time.Now().Add(time.Hour).Format(time.RFC3339),
					},
				}
				quotaDeadline := time.Now().Add(time.Hour)
				if quota {
					auth.ModelStates = map[string]*ModelState{
						"free-model": {Status: StatusActive},
						"quota-model": {
							Status: StatusError, Unavailable: true, NextRetryAfter: quotaDeadline,
							Quota:     QuotaState{Exceeded: true, Family: "independent", NextRecoverAt: quotaDeadline},
							LastError: &Error{HTTPStatus: http.StatusTooManyRequests, Message: "synthetic quota"},
						},
					}
				}
				registered, errRegister := manager.Register(context.Background(), auth)
				if errRegister != nil {
					t.Fatal(errRegister)
				}
				manager.markRejectedAccessToken(registered, executor)
				rejected, _ := manager.GetByID(registered.ID)
				if !rejected.hasRejectedAccessToken() {
					t.Fatal("rejected-token precondition missing")
				}
				_, errRefresh := manager.ForceRefreshAuth(context.Background(), registered.ID)
				current, _ := manager.GetByID(registered.ID)
				if !test.renew {
					if errRefresh == nil || !current.hasRejectedAccessToken() {
						t.Fatalf("unsuccessful renewal cleared rejection: error=%v rejected=%t", errRefresh, current.hasRejectedAccessToken())
					}
					return
				}
				if errRefresh != nil {
					t.Fatalf("ForceRefreshAuth: %v", errRefresh)
				}
				if current.hasRejectedAccessToken() || authAccessToken(current) != authAccessToken(registered) {
					t.Fatalf("renewed unchanged token stayed rejected: rejected=%t", current.hasRejectedAccessToken())
				}
				if blocked, _, _ := isAuthBlockedForModel(current, "free-model", time.Now()); blocked {
					t.Fatal("renewed credential remained blocked")
				}
				if quota {
					state := current.ModelStates["quota-model"]
					if state == nil || !state.Unavailable || !state.Quota.Exceeded || !state.Quota.NextRecoverAt.Equal(quotaDeadline) {
						t.Fatalf("renewal cleared independent quota: %+v", state)
					}
					if blocked, _, _ := isAuthBlockedForModel(current, "quota-model", time.Now()); !blocked {
						t.Fatal("independent model quota became selectable")
					}
				}
			})
		}
	}
}

type unauthorizedRedispatchExecutor struct {
	schedulerTestExecutor
	mode         string
	calls        []string
	refreshCalls int
}

func (e *unauthorizedRedispatchExecutor) Execute(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.calls = append(e.calls, auth.ID)
	if auth.ID == "bb-healthy" || e.mode == "recovered" && authAccessToken(auth) == "fresh-access" {
		return cliproxyexecutor.Response{Payload: []byte(auth.ID)}, nil
	}
	failure := &Error{HTTPStatus: http.StatusUnauthorized, Message: "synthetic credential rejection"}
	switch e.mode {
	case "no-retry":
		failure.DispatchPhase = cliproxyexecutor.DispatchPhaseResponseStarted
		failure.Outcome = cliproxyexecutor.ExecutionOutcomeRejectedBeforeProcessing
		failure.RetryDirective = cliproxyexecutor.RetryDirectiveNoRetry
		failure.EffectScope = cliproxyexecutor.StateEffectScopeCredential
	case "unknown":
		failure.DispatchPhase = cliproxyexecutor.DispatchPhaseMaybeSent
		failure.Outcome = cliproxyexecutor.ExecutionOutcomeUnknown
		failure.RetryDirective = cliproxyexecutor.RetryDirectiveSameAuthAfterRefresh
		failure.EffectScope = cliproxyexecutor.StateEffectScopeRequest
	case "committed":
		failure.DispatchPhase = cliproxyexecutor.DispatchPhaseDownstreamCommitted
		failure.Outcome = cliproxyexecutor.ExecutionOutcomeRejectedBeforeProcessing
		failure.RetryDirective = cliproxyexecutor.RetryDirectiveSameAuthAfterRefresh
		failure.EffectScope = cliproxyexecutor.StateEffectScopeRequest
	}
	return cliproxyexecutor.Response{}, failure
}

func (e *unauthorizedRedispatchExecutor) ExecuteStream(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	response, errExecute := e.Execute(ctx, auth, req, opts)
	if errExecute != nil {
		return nil, errExecute
	}
	chunks := make(chan cliproxyexecutor.StreamChunk, 1)
	chunks <- cliproxyexecutor.StreamChunk{Payload: response.Payload}
	close(chunks)
	return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
}

func (e *unauthorizedRedispatchExecutor) CountTokens(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return e.Execute(ctx, auth, req, opts)
}

func (e *unauthorizedRedispatchExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	e.refreshCalls++
	if e.mode == "failed" {
		return nil, errors.New("synthetic refresh service unavailable")
	}
	if e.mode == "invalid-grant" {
		return nil, errors.New(`{"error":"invalid_grant","error_description":"synthetic refresh credential rejected"}`)
	}
	updated := auth.Clone()
	updated.Metadata["access_token"] = "fresh-access"
	return updated, nil
}

func TestUnauthorizedRecoveryPreservesSafeRedispatch(t *testing.T) {
	for _, method := range []string{"execute", "stream", "count"} {
		for _, scenario := range []struct {
			mode         string
			refreshCalls int
			calls        string
			failed       bool
		}{
			{"no-refresh", 0, "aa-stale,bb-healthy", false},
			{"failed", 1, "aa-stale,bb-healthy", false},
			{"invalid-grant", 1, "aa-stale,bb-healthy", false},
			{"rejected", 1, "aa-stale,aa-stale,bb-healthy", false},
			{"recovered", 1, "aa-stale,aa-stale", false},
			{"no-retry", 0, "aa-stale", true},
			{"unknown", 0, "aa-stale", true},
			{"committed", 0, "aa-stale", true},
		} {
			t.Run(method+"/"+scenario.mode, func(t *testing.T) {
				manager := NewManager(nil, &FillFirstSelector{}, nil)
				executor := &unauthorizedRedispatchExecutor{mode: scenario.mode}
				manager.RegisterExecutor(executor)
				const model = "synthetic-unauthorized-redispatch"
				registerSchedulerModels(t, "test", model, "aa-stale", "bb-healthy")
				for _, id := range []string{"aa-stale", "bb-healthy"} {
					metadata := map[string]any{"access_token": "stale-access", "expired": time.Now().Add(time.Hour).Format(time.RFC3339)}
					if scenario.mode != "no-refresh" {
						metadata["refresh_token"] = "synthetic-refresh"
					}
					if _, errRegister := manager.Register(t.Context(), &Auth{ID: id, Provider: "test", Status: StatusActive, Metadata: metadata}); errRegister != nil {
						t.Fatal(errRegister)
					}
				}
				request := cliproxyexecutor.Request{Model: model}
				var failure error
				switch method {
				case "execute":
					_, failure = manager.Execute(t.Context(), []string{"test"}, request, cliproxyexecutor.Options{})
				case "count":
					_, failure = manager.ExecuteCount(t.Context(), []string{"test"}, request, cliproxyexecutor.Options{})
				case "stream":
					stream, errStream := manager.ExecuteStream(t.Context(), []string{"test"}, request, cliproxyexecutor.Options{})
					failure = errStream
					if errStream == nil {
						for chunk := range stream.Chunks {
							if chunk.Err != nil {
								failure = chunk.Err
							}
							if chunk.Acknowledge != nil {
								chunk.Acknowledge(chunk.Err)
							}
						}
					}
				}
				if (failure != nil) != scenario.failed {
					t.Errorf("execution error=%v, want failed=%v", failure, scenario.failed)
				}
				if executor.refreshCalls != scenario.refreshCalls {
					t.Errorf("refresh calls=%d, want %d", executor.refreshCalls, scenario.refreshCalls)
				}
				if calls := strings.Join(executor.calls, ","); calls != scenario.calls {
					t.Errorf("credential attempts=%s, want %s", calls, scenario.calls)
				}
			})
		}
	}
}
