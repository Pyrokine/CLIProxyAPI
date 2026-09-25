package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type routingSelectionCause struct {
	status      int
	body        []byte
	retryAfter  time.Duration
	disposition cliproxyexecutor.ExecutionDisposition
}

func (e *routingSelectionCause) Error() string { return string(e.body) }

func (e *routingSelectionCause) StatusCode() int { return e.status }

func (e *routingSelectionCause) ResponseBody() []byte { return append([]byte(nil), e.body...) }

func (*routingSelectionCause) DirectResponse() bool { return true }

func (e *routingSelectionCause) RetryAfter() *time.Duration { return &e.retryAfter }

func (e *routingSelectionCause) ExecutionDisposition() cliproxyexecutor.ExecutionDisposition {
	return e.disposition
}

func requireSelectionCause(t *testing.T, err error, want *routingSelectionCause) {
	t.Helper()
	if err == nil {
		t.Fatal("selection error = nil")
	}
	var got *routingSelectionCause
	if !errors.As(err, &got) || got != want {
		t.Fatalf("errors.As(selection error) = %#v, want original cause %#v; err=%T %v", got, want, err, err)
	}
	if got.StatusCode() != want.status || !reflect.DeepEqual(got.ResponseBody(), want.body) ||
		got.RetryAfter() == nil || *got.RetryAfter() != want.retryAfter ||
		got.ExecutionDisposition() != want.disposition {
		t.Fatalf("preserved cause metadata = status:%d body:%q retry:%v disposition:%+v", got.StatusCode(), got.ResponseBody(), got.RetryAfter(), got.ExecutionDisposition())
	}
}

func TestStructuredModelNotFoundUsesExactModelDisposition(t *testing.T) {
	const model = "routing-model-not-found"
	for _, test := range []struct {
		name  string
		cause error
	}{
		{
			name: "structured body",
			cause: &routingSelectionCause{
				status: http.StatusBadRequest,
				body: []byte(
					`{"error":{"type":"invalid_request_error","code":"model_not_found","message":"unknown model"}}`,
				),
			},
		},
		{
			name: "auth error code",
			cause: &Error{
				Code:       "model_not_found",
				HTTPStatus: http.StatusBadRequest,
				Message:    "unknown model",
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			disposition := executionDispositionForAttempt(
				test.cause,
				cliproxyexecutor.DispatchPhaseMaybeSent,
				&Auth{ID: "model-not-found-auth", Provider: "codex"},
				"codex",
				model,
			)
			if disposition.Outcome != cliproxyexecutor.ExecutionOutcomeRejectedBeforeProcessing ||
				disposition.RetryDirective != cliproxyexecutor.RetryDirectiveNextAuth ||
				disposition.EffectScope != cliproxyexecutor.StateEffectScopeExactModel ||
				disposition.EffectKey != model {
				t.Fatalf("model_not_found disposition = %+v", disposition)
			}
			if isRequestInvalidError(test.cause) {
				t.Fatal("model_not_found was classified as a caller request fault")
			}
		})
	}
}

func TestAuthAndQuotaStatusesPrecedeModelNotFoundBody(t *testing.T) {
	const model = "routing-status-first-model"
	for _, test := range []struct {
		status    int
		wantRetry cliproxyexecutor.RetryDirective
		wantScope cliproxyexecutor.StateEffectScope
	}{
		{http.StatusUnauthorized, cliproxyexecutor.RetryDirectiveSameAuthAfterRefresh, cliproxyexecutor.StateEffectScopeCredential},
		{http.StatusPaymentRequired, cliproxyexecutor.RetryDirectiveNextAuth, cliproxyexecutor.StateEffectScopeCredential},
		{http.StatusTooManyRequests, cliproxyexecutor.RetryDirectiveRetryAfter, cliproxyexecutor.StateEffectScopeQuotaFamily},
	} {
		t.Run(strconv.Itoa(test.status), func(t *testing.T) {
			disposition := executionDispositionForAttempt(
				&Error{
					HTTPStatus: test.status,
					Message:    `{"error":{"type":"authentication_error","code":"model_not_found"}}`,
				},
				cliproxyexecutor.DispatchPhaseMaybeSent,
				&Auth{ID: "status-first-auth", Provider: "codex"},
				"codex",
				model,
			)
			if disposition.Outcome != cliproxyexecutor.ExecutionOutcomeRejectedBeforeProcessing ||
				disposition.RetryDirective != test.wantRetry || disposition.EffectScope != test.wantScope {
				t.Fatalf("HTTP %d disposition = %+v", test.status, disposition)
			}
		})
	}
}

func TestSelectionExhaustionPreservesCredentialModelAndQuotaCauses(t *testing.T) {
	now := time.Now()
	for _, test := range []struct {
		name  string
		cause *routingSelectionCause
		auth  *Auth
	}{
		{
			name: "credential",
			cause: &routingSelectionCause{
				status:     http.StatusUnauthorized,
				body:       []byte(`{"error":{"type":"authentication_error"}}`),
				retryAfter: 7 * time.Second,
				disposition: cliproxyexecutor.ExecutionDisposition{
					DispatchPhase:  cliproxyexecutor.DispatchPhaseResponseStarted,
					Outcome:        cliproxyexecutor.ExecutionOutcomeRejectedBeforeProcessing,
					RetryDirective: cliproxyexecutor.RetryDirectiveSameAuthAfterRefresh,
					EffectScope:    cliproxyexecutor.StateEffectScopeCredential,
					EffectKey:      "credential-auth",
				},
			},
			auth: &Auth{ID: "credential-auth", Provider: "codex", Unavailable: true},
		},
		{
			name: "exact model",
			cause: &routingSelectionCause{
				status:     http.StatusNotFound,
				body:       []byte(`{"error":{"code":"model_not_found"}}`),
				retryAfter: 11 * time.Second,
				disposition: cliproxyexecutor.ExecutionDisposition{
					DispatchPhase:  cliproxyexecutor.DispatchPhaseResponseStarted,
					Outcome:        cliproxyexecutor.ExecutionOutcomeRejectedBeforeProcessing,
					RetryDirective: cliproxyexecutor.RetryDirectiveNextAuth,
					EffectScope:    cliproxyexecutor.StateEffectScopeExactModel,
					EffectKey:      "routing-selection-model",
				},
			},
			auth: &Auth{ID: "model-auth", Provider: "codex", ModelStates: map[string]*ModelState{
				"routing-selection-model": {Unavailable: true, UpdatedAt: now},
			}},
		},
		{
			name: "quota family",
			cause: &routingSelectionCause{
				status:     http.StatusTooManyRequests,
				body:       []byte(`{"error":{"type":"rate_limit_error"}}`),
				retryAfter: 13 * time.Second,
				disposition: cliproxyexecutor.ExecutionDisposition{
					DispatchPhase:  cliproxyexecutor.DispatchPhaseResponseStarted,
					Outcome:        cliproxyexecutor.ExecutionOutcomeRejectedBeforeProcessing,
					RetryDirective: cliproxyexecutor.RetryDirectiveRetryAfter,
					EffectScope:    cliproxyexecutor.StateEffectScopeQuotaFamily,
					EffectKey:      "routing-selection-family",
				},
			},
			auth: &Auth{ID: "quota-auth", Provider: "codex", ModelStates: map[string]*ModelState{
				"routing-selection-model": {
					Unavailable:    true,
					NextRetryAfter: now.Add(time.Minute),
					UpdatedAt:      now,
					Quota: QuotaState{
						Exceeded:      true,
						Family:        "routing-selection-family",
						NextRecoverAt: now.Add(time.Minute),
					},
				},
			}},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			resultErr := errorForResult(test.cause)
			switch test.name {
			case "credential":
				test.auth.LastError = resultErr
				test.auth.UpdatedAt = now
			case "exact model", "quota family":
				test.auth.ModelStates["routing-selection-model"].LastError = resultErr
			}
			_, errSelection := getAvailableAuths(
				[]*Auth{test.auth}, "codex", "routing-selection-model", cliproxyexecutor.Options{}, now,
			)
			requireSelectionCause(t, errSelection, test.cause)
		})
	}
}

func TestSelectionCausePriorityAndStableTieBreak(t *testing.T) {
	now := time.Now()
	credentialCause := &routingSelectionCause{status: 401, body: []byte("credential")}
	olderModelCause := &routingSelectionCause{status: 404, body: []byte("older-model")}
	newerModelCause := &routingSelectionCause{status: 429, body: []byte("newer-model")}
	makeAuth := func(id string, updatedAt time.Time, modelCause *routingSelectionCause) *Auth {
		return &Auth{
			ID:          id,
			Provider:    "codex",
			Unavailable: true,
			UpdatedAt:   now,
			LastError:   errorForResult(credentialCause),
			ModelStates: map[string]*ModelState{
				"routing-selection-model": {
					Unavailable: true,
					UpdatedAt:   updatedAt,
					LastError:   errorForResult(modelCause),
				},
			},
		}
	}
	_, errSelection := getAvailableAuths(
		[]*Auth{
			makeAuth("auth-a", now.Add(-time.Minute), olderModelCause),
			makeAuth("auth-b", now, newerModelCause),
		},
		"codex",
		"routing-selection-model",
		cliproxyexecutor.Options{},
		now,
	)
	requireSelectionCause(t, errSelection, newerModelCause)

	tieACause := &routingSelectionCause{status: 429, body: []byte("tie-a")}
	tieBCause := &routingSelectionCause{status: 429, body: []byte("tie-b")}
	_, tieErr := getAvailableAuths(
		[]*Auth{
			makeAuth("auth-b", now, tieBCause),
			makeAuth("auth-a", now, tieACause),
		},
		"codex",
		"routing-selection-model",
		cliproxyexecutor.Options{},
		now,
	)
	requireSelectionCause(t, tieErr, tieBCause)
}

func TestSchedulerAndContext1MExhaustionPreserveCause(t *testing.T) {
	const (
		provider = "codex"
		model    = "routing-selection-scheduler"
		authID   = "routing-selection-scheduler-auth"
	)
	now := time.Now()
	cause := &routingSelectionCause{
		status:     http.StatusTooManyRequests,
		body:       []byte(`{"error":{"type":"rate_limit_error","message":"safe"}}`),
		retryAfter: 17 * time.Second,
		disposition: cliproxyexecutor.ExecutionDisposition{
			DispatchPhase:  cliproxyexecutor.DispatchPhaseResponseStarted,
			Outcome:        cliproxyexecutor.ExecutionOutcomeRejectedBeforeProcessing,
			RetryDirective: cliproxyexecutor.RetryDirectiveRetryAfter,
			EffectScope:    cliproxyexecutor.StateEffectScopeQuotaFamily,
			EffectKey:      model,
		},
	}
	auth := &Auth{ID: authID, Provider: provider, ModelStates: map[string]*ModelState{
		model: {
			Unavailable:    true,
			NextRetryAfter: now.Add(time.Minute),
			UpdatedAt:      now,
			LastError:      errorForResult(cause),
			Quota: QuotaState{
				Exceeded:      true,
				Family:        model,
				NextRecoverAt: now.Add(time.Minute),
			},
		},
	}}
	registerSchedulerModels(t, provider, model, authID)
	scheduler := newSchedulerForTest(&RoundRobinSelector{}, auth)
	_, errScheduler := scheduler.pickSingle(context.Background(), provider, model, cliproxyexecutor.Options{}, nil)
	requireSelectionCause(t, errScheduler, cause)

	registerContext1MTestAuthForModel(t, nil, authID, provider, model, context1MMinimumTokens)
	manager := NewManager(nil, nil, nil)
	registered, errRegister := manager.Register(context.Background(), auth)
	if errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	errContext := manager.context1MUnavailableSelectionError(
		[]*Auth{registered}, provider, model+"[1m]",
	)
	requireSelectionCause(t, errContext, cause)
}

func TestCloudflareChallengeMarkersAndOriginErrors(t *testing.T) {
	origin := `<html><body><h1>Web server is returning an unknown error</h1><p>There is an unknown connection issue between Cloudflare and the origin web server.</p></body></html>`
	for _, message := range []string{
		origin,
		"generic Cloudflare edge failure",
		"Just a moment while another proxy responds",
	} {
		if isCloudflareChallengeErrorMessage(message) {
			t.Fatalf("origin/generic message classified as challenge: %q", message)
		}
	}
	for _, message := range []string{
		"cf-mitigated: challenge",
		`/cdn-cgi/challenge-platform/h/b/orchestrate/chl_page/v1`,
		"cloudflare challenge required",
		"<title>Just-a-moment</title> Cloudflare",
		"<title>Just a moment...</title> checking your browser through cloudflare",
	} {
		if !isCloudflareChallengeErrorMessage(message) {
			t.Fatalf("specific challenge marker was not recognized: %q", message)
		}
	}
	for _, status := range []int{500, 502, 503, 504, 520, 521, 522, 523, 524, 525, 526} {
		if isCloudflareChallengeResultError(&Error{HTTPStatus: status, Message: "cf-mitigated: challenge"}) {
			t.Fatalf("HTTP %d was classified as Cloudflare challenge", status)
		}
	}
	if !isCloudflareChallengeResultError(&Error{HTTPStatus: http.StatusForbidden, Message: "cf-mitigated: challenge"}) {
		t.Fatal("HTTP 403 challenge marker was not recognized")
	}
}

func TestCloudflare520Through526AreTransientAndHonorCooldownPolicy(t *testing.T) {
	previous := transientErrorCooldownSeconds.Load()
	SetTransientErrorCooldownSeconds(5)
	t.Cleanup(func() { transientErrorCooldownSeconds.Store(previous) })

	for status := 520; status <= 526; status++ {
		manager := NewManager(nil, nil, nil)
		authID := "cloudflare-origin-" + strconv.Itoa(status)
		if _, errRegister := manager.Register(context.Background(), &Auth{
			ID: authID, Provider: "codex", Status: StatusActive,
		}); errRegister != nil {
			t.Fatalf("Register(%d) error = %v", status, errRegister)
		}
		manager.MarkResult(context.Background(), Result{
			AuthID: authID, Provider: "codex", Model: "gpt-5.6-sol",
			Error: &Error{HTTPStatus: status, Message: "Cloudflare origin failure"},
		})
		updated, _ := manager.GetByID(authID)
		state := updated.ModelStates["gpt-5.6-sol"]
		if state == nil || !state.Unavailable || state.Quota.Exceeded ||
			state.LastError == nil || state.LastError.HTTPStatus != status {
			t.Fatalf("HTTP %d transient state = %#v", status, state)
		}
		if state.LastError.Outcome != cliproxyexecutor.ExecutionOutcomeFailed ||
			state.LastError.RetryDirective != cliproxyexecutor.RetryDirectiveNoRetry ||
			state.LastError.EffectScope != cliproxyexecutor.StateEffectScopeExactModel ||
			state.LastError.EffectKey != "gpt-5.6-sol" {
			t.Fatalf("HTTP %d disposition = %+v", status, state.LastError.ExecutionDisposition())
		}
		remaining := time.Until(state.NextRetryAfter)
		if remaining < 3*time.Second || remaining > 7*time.Second {
			t.Fatalf("HTTP %d cooldown = %v, want about 5s", status, remaining)
		}

		credentialManager := NewManager(nil, nil, nil)
		credentialID := "cloudflare-origin-credential-" + strconv.Itoa(status)
		if _, errRegister := credentialManager.Register(context.Background(), &Auth{
			ID: credentialID, Provider: "codex", Status: StatusActive,
		}); errRegister != nil {
			t.Fatalf("Register credential scope (%d) error = %v", status, errRegister)
		}
		credentialManager.MarkResult(context.Background(), Result{
			AuthID: credentialID, Provider: "codex",
			Error: &Error{HTTPStatus: status, Message: "Cloudflare origin failure"},
		})
		credential, _ := credentialManager.GetByID(credentialID)
		if credential == nil || !credential.Unavailable || credential.Quota.Exceeded ||
			credential.LastError == nil || credential.LastError.HTTPStatus != status ||
			credential.LastError.Outcome != cliproxyexecutor.ExecutionOutcomeFailed ||
			credential.LastError.RetryDirective != cliproxyexecutor.RetryDirectiveNoRetry ||
			credential.LastError.EffectScope != cliproxyexecutor.StateEffectScopeCredential {
			t.Fatalf("HTTP %d credential transient state = %#v", status, credential)
		}
		credentialRemaining := time.Until(credential.NextRetryAfter)
		if credentialRemaining < 3*time.Second || credentialRemaining > 7*time.Second {
			t.Fatalf("HTTP %d credential cooldown = %v, want about 5s", status, credentialRemaining)
		}
	}

	manager := NewManager(nil, nil, nil)
	if _, errRegister := manager.Register(context.Background(), &Auth{
		ID: "cloudflare-hint", Provider: "codex", Status: StatusActive,
	}); errRegister != nil {
		t.Fatal(errRegister)
	}
	hint := 15 * time.Second
	manager.MarkResult(context.Background(), Result{
		AuthID: "cloudflare-hint", Provider: "codex", Model: "gpt-5.6-sol", RetryAfter: &hint,
		Error: &Error{HTTPStatus: 520, Message: "Cloudflare origin failure"},
	})
	withHint, _ := manager.GetByID("cloudflare-hint")
	remaining := time.Until(withHint.ModelStates["gpt-5.6-sol"].NextRetryAfter)
	if remaining < 12*time.Second || remaining > 18*time.Second {
		t.Fatalf("Retry-After hint cooldown = %v, want about 15s", remaining)
	}

	SetTransientErrorCooldownSeconds(-1)
	disabled := NewManager(nil, nil, nil)
	if _, errRegister := disabled.Register(context.Background(), &Auth{
		ID: "cloudflare-disabled", Provider: "codex", Status: StatusActive,
	}); errRegister != nil {
		t.Fatal(errRegister)
	}
	disabled.MarkResult(context.Background(), Result{
		AuthID: "cloudflare-disabled", Provider: "codex", Model: "gpt-5.6-sol", RetryAfter: &hint,
		Error: &Error{HTTPStatus: 520, Message: "Cloudflare origin failure"},
	})
	withoutCooldown, _ := disabled.GetByID("cloudflare-disabled")
	state := withoutCooldown.ModelStates["gpt-5.6-sol"]
	if state == nil || state.Unavailable || !state.NextRetryAfter.IsZero() {
		t.Fatalf("disabled transient cooldown used hint: %#v", state)
	}

	disabledCredential := NewManager(nil, nil, nil)
	if _, errRegister := disabledCredential.Register(context.Background(), &Auth{
		ID: "cloudflare-disabled-credential", Provider: "codex", Status: StatusActive,
	}); errRegister != nil {
		t.Fatal(errRegister)
	}
	disabledCredential.MarkResult(context.Background(), Result{
		AuthID: "cloudflare-disabled-credential", Provider: "codex", RetryAfter: &hint,
		Error: &Error{HTTPStatus: 520, Message: "Cloudflare origin failure"},
	})
	credentialWithoutCooldown, _ := disabledCredential.GetByID("cloudflare-disabled-credential")
	if credentialWithoutCooldown == nil || credentialWithoutCooldown.Unavailable ||
		!credentialWithoutCooldown.NextRetryAfter.IsZero() {
		t.Fatalf("disabled credential transient cooldown used hint: %#v", credentialWithoutCooldown)
	}
}

func TestCooldownSnapshotIsReadOnlyStableAndSafe(t *testing.T) {
	now := time.Date(2026, 7, 17, 10, 0, 0, 500_000_000, time.FixedZone("offset", 3600))
	auth := &Auth{
		Metadata: map[string]any{"access_token": "secret-token"},
		Quota: QuotaState{
			Exceeded:      true,
			Reason:        "credential_quota",
			NextRecoverAt: now.Add(20 * time.Second),
			BackoffLevel:  9,
		},
		ModelStates: map[string]*ModelState{
			" model-a(high) ": {
				Unavailable:    true,
				NextRetryAfter: now.Add(time.Minute),
				LastError:      &Error{HTTPStatus: 520, Message: "secret upstream body"},
			},
			"model-a": {
				Unavailable:    true,
				NextRetryAfter: now.Add(32 * time.Second),
				Quota: QuotaState{
					Exceeded:      true,
					Reason:        "quota",
					NextRecoverAt: now.Add(32 * time.Second),
					BackoffLevel:  2,
				},
				LastError: &Error{HTTPStatus: 429, Message: "secret quota body"},
			},
			"model-b": {
				Unavailable:    true,
				NextRetryAfter: now.Add(1500 * time.Millisecond),
				StatusMessage:  "secret message",
			},
			"nil": nil,
		},
	}
	before := auth.Clone()
	views := CooldownSnapshotForAuth(auth, now)
	if len(views) != 3 {
		t.Fatalf("cooldown views = %+v, want credential plus two models", views)
	}
	if views[0].Scope != "credential" || views[0].Reason != "credential_quota" ||
		views[0].RemainingSeconds != 20 || views[0].BackoffLevel != nil {
		t.Fatalf("credential cooldown view = %+v", views[0])
	}
	if views[1].Scope != "model" || views[1].ModelKey != "model-a" ||
		views[1].Reason != "transient_error" || views[1].HTTPStatus != 520 ||
		views[1].RemainingSeconds != 60 || views[1].BackoffLevel != nil {
		t.Fatalf("deduplicated model-a view = %+v", views[1])
	}
	if views[2].ModelKey != "model-b" || views[2].Reason != "unknown" ||
		views[2].RemainingSeconds != 2 {
		t.Fatalf("model-b view = %+v", views[2])
	}
	for _, view := range views {
		if view.RetryAt.Location() != time.UTC {
			t.Fatalf("RetryAt is not UTC: %v", view.RetryAt)
		}
		encoded, errMarshal := json.Marshal(view)
		if errMarshal != nil {
			t.Fatal(errMarshal)
		}
		if strings.Contains(string(encoded), "secret") {
			t.Fatalf("cooldown view leaked raw state: %s", encoded)
		}
	}
	for iteration := 0; iteration < 20; iteration++ {
		if again := CooldownSnapshotForAuth(auth, now); !reflect.DeepEqual(again, views) {
			t.Fatalf("cooldown snapshot changed at iteration %d: %+v", iteration, again)
		}
	}
	if !reflect.DeepEqual(auth, before) {
		t.Fatalf("cooldown snapshot mutated auth: before=%#v after=%#v", before, auth)
	}
}

func TestCooldownSnapshotEqualDeadlinePrefersQuota(t *testing.T) {
	now := time.Now().UTC()
	auth := &Auth{ModelStates: map[string]*ModelState{
		"model-a(high)": {
			Unavailable:    true,
			NextRetryAfter: now.Add(time.Minute),
			LastError:      &Error{HTTPStatus: http.StatusServiceUnavailable},
		},
		"model-a(low)": {
			Unavailable:    true,
			NextRetryAfter: now.Add(time.Minute),
			LastError:      &Error{HTTPStatus: http.StatusTooManyRequests},
			Quota: QuotaState{
				Exceeded:      true,
				Reason:        "quota",
				NextRecoverAt: now.Add(time.Minute),
			},
		},
	}}
	for iteration := 0; iteration < 20; iteration++ {
		views := CooldownSnapshotForAuth(auth, now)
		if len(views) != 1 || views[0].ModelKey != "model-a" ||
			views[0].Reason != "quota" || views[0].HTTPStatus != http.StatusTooManyRequests {
			t.Fatalf("quota tie result = %+v", views)
		}
	}
}
