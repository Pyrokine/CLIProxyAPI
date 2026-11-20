package auth

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type issue6415RefreshExecutor struct {
	schedulerTestExecutor
	mu              sync.Mutex
	calls           []string
	refreshErr      error
	refreshToken    string
	onRefresh       func()
	refreshCalls    int
	invalidTokens   map[string]struct{}
	streamBootstrap bool
}

func (*issue6415RefreshExecutor) Identifier() string { return "codex" }

func (e *issue6415RefreshExecutor) Execute(
	_ context.Context,
	auth *Auth,
	_ executor.Request,
	_ executor.Options,
) (executor.Response, error) {
	token := authAccessToken(auth)
	e.mu.Lock()
	e.calls = append(e.calls, auth.ID)
	_, invalid := e.invalidTokens[token]
	e.mu.Unlock()
	if token == "stale-access-token" || invalid {
		return executor.Response{}, &Error{
			HTTPStatus: http.StatusUnauthorized,
			Message:    "Your authentication token has been invalidated. Please try signing in again.",
		}
	}
	return executor.Response{Payload: []byte(auth.ID + ":" + token)}, nil
}

// noinspection GoDfaErrorMayBeNotNil
func (e *issue6415RefreshExecutor) ExecuteStream(
	ctx context.Context,
	auth *Auth,
	request executor.Request,
	options executor.Options,
) (*executor.StreamResult, error) {
	response, err := e.Execute(ctx, auth, request, options)
	if err != nil && !e.streamBootstrap {
		return nil, err
	}
	chunks := make(chan executor.StreamChunk, 1)
	chunks <- executor.StreamChunk{Payload: response.Payload, Err: err}
	close(chunks)
	return &executor.StreamResult{Headers: http.Header{"X-Auth": {auth.ID}}, Chunks: chunks}, nil
}

func (e *issue6415RefreshExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	e.mu.Lock()
	e.refreshCalls++
	callback, failure, token := e.onRefresh, e.refreshErr, e.refreshToken
	e.mu.Unlock()
	if callback != nil {
		callback()
	}
	if failure != nil {
		return nil, failure
	}
	updated := auth.Clone()
	updated.Metadata["access_token"] = token
	return updated, nil
}

func (e *issue6415RefreshExecutor) ExecuteCalls() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.calls...)
}

func newIssue6415RefreshFixture(t *testing.T, states map[string]*ModelState) (
	*Manager,
	*issue6415RefreshExecutor,
	*Auth,
	*Auth,
	string,
) {
	t.Helper()
	const model = "gpt-5.5"
	capture := &issue6415RefreshExecutor{refreshToken: "fresh-access-token"}
	manager := NewManager(nil, nil, nil)
	manager.SetHomeClientProvider(func() HomeClient { return nil })
	manager.RegisterExecutor(capture)
	primary, err := manager.Register(
		t.Context(), &Auth{
			ID: "aa-primary", Provider: "codex", ModelStates: states,
			Metadata: map[string]any{"access_token": "stale-access-token", "refresh_token": "primary-refresh-token"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	backup, err := manager.Register(
		t.Context(), &Auth{
			ID: "bb-backup", Provider: "codex",
			Metadata: map[string]any{"access_token": "backup-access-token", "refresh_token": "backup-refresh-token"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	models := registry.GetGlobalRegistry()
	for _, id := range []string{primary.ID, backup.ID} {
		models.RegisterClient(id, "codex", []*registry.ModelInfo{{ID: model}})
		t.Cleanup(func() { models.UnregisterClient(id) })
	}
	return manager, capture, primary, backup, model
}

func TestManager_Execute_UnauthorizedRefreshesCurrentAuthBeforeFallback(t *testing.T) {
	manager, capture, primary, _, model := newIssue6415RefreshFixture(t, nil)
	response, err := manager.Execute(t.Context(), []string{"codex"}, executor.Request{Model: model}, executor.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if string(response.Payload) != primary.ID+":fresh-access-token" {
		t.Fatalf("refreshed primary response = %q, error = %v", response.Payload, err)
	}
	if calls := capture.ExecuteCalls(); capture.refreshCalls != 1 || len(calls) != 2 || calls[0] != primary.ID ||
		calls[1] != primary.ID {
		t.Fatalf("refreshes = %d, execution order = %v", capture.refreshCalls, calls)
	}
	current, ok := manager.GetByID(primary.ID)
	if !ok || authAccessToken(current) != "fresh-access-token" {
		t.Fatal("refreshed access token was not published")
	}
	if state := current.ModelStates[model]; state != nil && state.Unavailable {
		t.Fatal("successful refreshed request left the original model unavailable")
	}
}

func TestManager_ExecuteStream_UnauthorizedRefreshesCurrentAuthBeforeFallback(t *testing.T) {
	for _, bootstrap := range []bool{false, true} {
		t.Run(
			map[bool]string{false: "direct-error", true: "bootstrap-error"}[bootstrap], func(t *testing.T) {
				manager, capture, primary, _, model := newIssue6415RefreshFixture(t, nil)
				capture.streamBootstrap = bootstrap
				stream, err := manager.ExecuteStream(
					t.Context(), []string{"codex"}, executor.Request{Model: model}, executor.Options{},
				)
				if err != nil || stream == nil || stream.Chunks == nil {
					t.Fatalf("refreshed stream = %+v, error = %v", stream, err)
				}
				var payload []byte
				for chunk := range stream.Chunks {
					if chunk.Err != nil {
						t.Fatal(chunk.Err)
					}
					payload = append(payload, chunk.Payload...)
				}
				if string(payload) != primary.ID+":fresh-access-token" || stream.Headers.Get("X-Auth") != primary.ID {
					t.Fatalf("refreshed stream payload = %q, headers = %v", payload, stream.Headers)
				}
				if calls := capture.ExecuteCalls(); capture.refreshCalls != 1 || len(calls) != 2 ||
					calls[0] != primary.ID ||
					calls[1] != primary.ID {
					t.Fatalf("stream refreshes = %d, execution order = %v", capture.refreshCalls, calls)
				}
			},
		)
	}
}

func TestManager_Execute_UnauthorizedRefreshFailureFallsBackToNextAuth(t *testing.T) {
	manager, capture, primary, backup, model := newIssue6415RefreshFixture(t, nil)
	capture.refreshErr = &Error{HTTPStatus: http.StatusUnauthorized, Message: "refresh token invalid"}
	response, err := manager.Execute(t.Context(), []string{"codex"}, executor.Request{Model: model}, executor.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if string(response.Payload) != backup.ID+":backup-access-token" {
		t.Fatalf("backup response = %q, error = %v", response.Payload, err)
	}
	if calls := capture.ExecuteCalls(); capture.refreshCalls != 1 || len(calls) != 2 || calls[0] != primary.ID ||
		calls[1] != backup.ID {
		t.Fatalf("failed refreshes = %d, execution order = %v", capture.refreshCalls, calls)
	}
	current, ok := manager.GetByID(primary.ID)
	if !ok {
		t.Fatal("original primary disappeared after refresh failure")
	}
	if blocked, _, _ := isAuthBlockedForModel(
		current, model, time.Now(),
	); !blocked || !current.hasRejectedAccessToken() {
		t.Fatal("refresh failure made the original rejected token selectable")
	}
}

func TestManager_Execute_UnauthorizedWithoutRefreshTokenDoesNotCallRefresh(t *testing.T) {
	const model = "gpt-5.5"
	manager := NewManager(nil, nil, nil)
	manager.SetHomeClientProvider(func() HomeClient { return nil })
	capture := &issue6415RefreshExecutor{}
	manager.RegisterExecutor(capture)
	for _, credential := range []struct{ id, token string }{
		{"aa-primary-api-key", "stale-access-token"},
		{"bb-backup-api-key", "backup-access-token"},
	} {
		if _, err := manager.Register(
			t.Context(), &Auth{
				ID: credential.id, Provider: "codex",
				Metadata: map[string]any{"access_token": credential.token},
			},
		); err != nil {
			t.Fatal(err)
		}
		registry.GetGlobalRegistry().RegisterClient(credential.id, "codex", []*registry.ModelInfo{{ID: model}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(credential.id) })
	}
	response, err := manager.Execute(t.Context(), []string{"codex"}, executor.Request{Model: model}, executor.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if string(response.Payload) != "bb-backup-api-key:backup-access-token" {
		t.Fatalf("request without refresh credential = %q, error = %v", response.Payload, err)
	}
	calls := capture.ExecuteCalls()
	if capture.refreshCalls != 0 || len(calls) != 2 || calls[0] != "aa-primary-api-key" ||
		calls[1] != "bb-backup-api-key" {
		t.Fatalf("refreshes = %d, execution order = %v", capture.refreshCalls, calls)
	}
}

func TestManager_Execute_UnauthorizedRefreshThenRetryStillFailsFallsBackOnce(t *testing.T) {
	manager, capture, primary, backup, model := newIssue6415RefreshFixture(t, nil)
	capture.refreshToken = "still-invalid-token"
	capture.invalidTokens = map[string]struct{}{"still-invalid-token": {}}
	response, err := manager.Execute(t.Context(), []string{"codex"}, executor.Request{Model: model}, executor.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if string(response.Payload) != backup.ID+":backup-access-token" {
		t.Fatalf("request after rejected refreshed token = %q, error = %v", response.Payload, err)
	}
	if calls := capture.ExecuteCalls(); capture.refreshCalls != 1 || len(calls) != 3 || calls[0] != primary.ID ||
		calls[1] != primary.ID ||
		calls[2] != backup.ID {
		t.Fatalf("refreshes = %d, execution order = %v", capture.refreshCalls, calls)
	}
}

func TestOriginalIssue6415RefreshPendingDoesNotSelectRejectedCredential(t *testing.T) {
	manager, capture, primary, backup, model := newIssue6415RefreshFixture(t, nil)
	started, proceed, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(proceed) })
	capture.onRefresh = func() { close(started); <-proceed }
	var first executor.Response
	var firstErr error
	go func() {
		defer close(done)
		first, firstErr = manager.Execute(
			t.Context(), []string{"codex"}, executor.Request{Model: model}, executor.Options{},
		)
	}()
	t.Cleanup(func() { release(); <-done })
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("primary did not enter its pending refresh")
	}
	current, found := manager.GetByID(primary.ID)
	if blocked, _, _ := isAuthBlockedForModel(current, model, time.Now()); !found || !blocked {
		t.Fatal("rejected primary was selectable during refresh")
	}
	before := len(capture.ExecuteCalls())
	second, err := manager.Execute(t.Context(), []string{"codex"}, executor.Request{Model: model}, executor.Options{})
	if err != nil || string(second.Payload) != backup.ID+":backup-access-token" {
		t.Fatalf("concurrent request result=%q error=%v", second.Payload, err)
	}
	for _, id := range capture.ExecuteCalls()[before:] {
		if id == primary.ID {
			t.Fatal("concurrent request used the rejected primary")
		}
	}
	release()
	<-done
	if firstErr != nil || string(first.Payload) != primary.ID+":fresh-access-token" {
		t.Fatalf("first request result=%q error=%v", first.Payload, firstErr)
	}
}

func TestOriginalIssue6415NonTerminalRefreshFailureKeepsCredentialUnavailable(t *testing.T) {
	manager, capture, primary, _, model := newIssue6415RefreshFixture(t, nil)
	failure := errors.New("upstream 503 Service Unavailable")
	capture.refreshErr = failure
	manager.markRejectedAccessToken(primary, capture)
	if _, err := manager.refreshAuthForRequest(t.Context(), primary.ID, authAccessToken(primary)); !errors.Is(
		err, failure,
	) {
		t.Fatalf("refresh error=%v, want %v", err, failure)
	}
	current, found := manager.GetByID(primary.ID)
	if !found || !current.Unavailable || current.Status != StatusError || !current.hasRejectedAccessToken() {
		t.Fatalf("failed refresh credential=%+v", current)
	}
	if blocked, _, _ := isAuthBlockedForModel(current, model, time.Now()); !blocked {
		t.Fatal("refresh failure made the rejected token selectable")
	}
	for _, snapshot := range manager.List() {
		if snapshot.ID == primary.ID && (!snapshot.Unavailable || snapshot.Status != StatusError) {
			t.Fatal("List and GetByID disagree about the rejected credential")
		}
	}
}

func TestOriginalIssue6415RefreshPreservesThirtyMinuteQuota(t *testing.T) {
	quotaRecover := time.Now().Add(30 * time.Minute)
	manager, _, primary, _, model := newIssue6415RefreshFixture(
		t, map[string]*ModelState{
			"gpt-5.5": {
				Status: StatusError, StatusMessage: "unauthorized", Unavailable: true,
				NextRetryAfter: quotaRecover, EffectScope: executor.StateEffectScopeExactModel, EffectKey: "gpt-5.5",
				ResetAuthority: StateResetAuthoritySuccess,
				Quota: QuotaState{
					Exceeded: true, Reason: "quota", NextRecoverAt: quotaRecover, Generation: 1,
					ResetAuthority: StateResetAuthoritySuccess,
				},
				LastError: &Error{HTTPStatus: http.StatusUnauthorized, Message: "unauthorized"},
			},
		},
	)
	var refresh = manager.ForceRefreshAuth
	result, err := refresh(t.Context(), primary.ID)
	if err != nil || result.RefreshedAt.IsZero() {
		t.Fatalf("SDK refresh result=%+v error=%v", result, err)
	}
	current, found := manager.GetByID(primary.ID)
	state := current.ModelStates[model]
	if !found || state == nil || !state.Quota.Exceeded || !state.Quota.NextRecoverAt.Equal(quotaRecover) {
		t.Fatalf("refresh changed the original 30 minute quota: %+v", state)
	}
	current.Metadata["access_token"] = "caller-change"
	current.ModelStates[model].Quota.NextRecoverAt = time.Time{}
	current, _ = manager.GetByID(primary.ID)
	if authAccessToken(current) != "fresh-access-token" ||
		!current.ModelStates[model].Quota.NextRecoverAt.Equal(quotaRecover) {
		t.Fatal("GetByID snapshot shares mutable credential or model state")
	}
}

func TestOriginalIssue6415ExecuteRetryPreservesFortyFiveMinuteSiblingQuota(t *testing.T) {
	quotaRecover := time.Now().Add(45 * time.Minute)
	const sibling = "gpt-5-mini"
	manager, capture, primary, _, model := newIssue6415RefreshFixture(
		t, map[string]*ModelState{
			sibling: {
				Status: StatusError, StatusMessage: "quota_exceeded", Unavailable: true,
				NextRetryAfter: quotaRecover, EffectScope: executor.StateEffectScopeExactModel, EffectKey: sibling,
				ResetAuthority: StateResetAuthoritySuccess,
				Quota: QuotaState{
					Exceeded: true, Reason: "quota", NextRecoverAt: quotaRecover, Generation: 1,
					ResetAuthority: StateResetAuthoritySuccess,
				},
				LastError: &Error{HTTPStatus: http.StatusTooManyRequests, Message: "429 rate limited"},
			},
		},
	)
	registry.GetGlobalRegistry().RegisterClient(primary.ID, "codex", []*registry.ModelInfo{{ID: model}, {ID: sibling}})
	response, err := manager.Execute(t.Context(), []string{"codex"}, executor.Request{Model: model}, executor.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if string(response.Payload) != primary.ID+":fresh-access-token" || capture.refreshCalls != 1 {
		t.Fatalf("refreshed request result=%q refreshes=%d error=%v", response.Payload, capture.refreshCalls, err)
	}
	current, found := manager.GetByID(primary.ID)
	state := current.ModelStates[sibling]
	if !found || state == nil || !state.Quota.Exceeded || !state.Quota.NextRecoverAt.Equal(quotaRecover) {
		t.Fatalf("request changed the original 45 minute sibling quota: %+v", state)
	}
}

func TestOriginalIssue6415RejectionIsolatesEarlierSuccess(t *testing.T) {
	manager, capture, primary, backup, model := newIssue6415RefreshFixture(t, nil)
	result := executionResult(primary, "codex", model, nil, executor.DispatchPhaseDownstreamCommitted)
	manager.markRejectedAccessToken(primary, capture)
	current, _ := manager.GetByID(primary.ID)
	if current.Generation != primary.Generation || !current.UpdatedAt.After(primary.UpdatedAt) ||
		current.rejectedAccessTokenSHA256 != AccessTokenSHA256(primary) {
		t.Fatal("rejected token publication lost its state and credential identity")
	}
	manager.MarkResult(t.Context(), result)
	current, _ = manager.GetByID(primary.ID)
	if !current.hasRejectedAccessToken() {
		t.Fatal("earlier success released the rejected token")
	}
	response, err := manager.Execute(t.Context(), []string{"codex"}, executor.Request{Model: model}, executor.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if string(response.Payload) != backup.ID+":backup-access-token" {
		t.Fatalf("request after rejection result=%q error=%v", response.Payload, err)
	}
}

func TestOriginalIssue6415MetadataUpdateKeepsRejectedToken(t *testing.T) {
	manager, capture, primary, _, _ := newIssue6415RefreshFixture(t, nil)
	manager.markRejectedAccessToken(primary, capture)
	reload := primary.Clone()
	reload.rejectedAccessTokenSHA256 = ""
	reload.Metadata["note"] = "reloaded from file"
	updated, err := manager.Update(t.Context(), reload)
	current, found := manager.GetByID(primary.ID)
	if err != nil || updated == nil || !found || !updated.hasRejectedAccessToken() ||
		!current.hasRejectedAccessToken() ||
		current.Metadata["note"] != "reloaded from file" ||
		authAccessToken(current) != "stale-access-token" {
		t.Fatalf("metadata update result=%+v current=%+v error=%v", updated, current, err)
	}
	updated.Metadata["access_token"] = "fresh-access-token"
	restored, err := manager.Update(t.Context(), updated)
	if err != nil || restored == nil || restored.hasRejectedAccessToken() || restored.Unavailable ||
		restored.Status == StatusError {
		t.Fatalf("public snapshot retained stale rejection after token rotation: result=%+v error=%v", restored, err)
	}
}

// noinspection GoDfaErrorMayBeNotNil
func TestOriginalIssue6415SameTokenRefreshKeepsRejection(t *testing.T) {
	manager, capture, primary, _, model := newIssue6415RefreshFixture(t, nil)
	manager.markRejectedAccessToken(primary, capture)
	capture.refreshToken = authAccessToken(primary)
	refreshed, err := manager.ForceRefreshAuth(t.Context(), primary.ID)
	current, found := manager.GetByID(primary.ID)
	if err == nil || refreshed.Status != StatusError || !found || !current.hasRejectedAccessToken() {
		t.Fatalf("unchanged refresh result=%+v current=%+v error=%v", refreshed, current, err)
	}
	if blocked, _, _ := isAuthBlockedForModel(current, model, time.Now()); !blocked {
		t.Fatal("unchanged refresh made the rejected token selectable")
	}
}

func TestOriginalIssue6415CanceledRefreshKeepsRejectionWithoutProviderCooldown(t *testing.T) {
	manager, capture, primary, _, model := newIssue6415RefreshFixture(t, nil)
	manager.markRejectedAccessToken(primary, capture)
	capture.refreshErr = context.Canceled
	if _, err := manager.refreshAuthForRequest(t.Context(), primary.ID, authAccessToken(primary)); !errors.Is(
		err, context.Canceled,
	) {
		t.Fatalf("canceled refresh error=%v", err)
	}
	current, found := manager.GetByID(primary.ID)
	if !found || !current.NextRefreshAfter.IsZero() || !current.NextRetryAfter.IsZero() || current.LastError != nil ||
		!current.hasRejectedAccessToken() {
		t.Fatalf("cancellation published a cooldown or cleared rejection: %+v", current)
	}
	if blocked, _, _ := isAuthBlockedForModel(current, model, time.Now()); !blocked {
		t.Fatal("canceled refresh made the rejected token selectable")
	}
	capture.refreshErr = nil
	refreshed, err := manager.ForceRefreshAuth(t.Context(), primary.ID)
	current, found = manager.GetByID(primary.ID)
	if err != nil || refreshed.RefreshedAt.IsZero() || !found || current.hasRejectedAccessToken() ||
		authAccessToken(current) != "fresh-access-token" {
		t.Fatalf("later token rotation result=%+v current=%+v error=%v", refreshed, current, err)
	}
}
