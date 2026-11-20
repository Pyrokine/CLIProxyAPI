package auth

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type auditLegacyVersionExecutor struct {
	id string

	mu        sync.Mutex
	onExecute func(auth *Auth)
	onStream  func(auth *Auth)
}

func (e *auditLegacyVersionExecutor) Identifier() string { return e.id }

func (e *auditLegacyVersionExecutor) Execute(
	_ context.Context,
	auth *Auth,
	_ cliproxyexecutor.Request,
	_ cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	e.mu.Lock()
	onExec := e.onExecute
	e.mu.Unlock()
	if onExec != nil {
		onExec(auth)
	}
	token := authAccessToken(auth)
	if token == "" {
		token = auth.Attributes[AttributeAPIKey]
	}
	if token == "old-access-token" || token == "old-api-key" {
		return cliproxyexecutor.Response{}, &Error{
			HTTPStatus: http.StatusTooManyRequests,
			Message:    "rate limited on old credential",
		}
	}
	if token == "invalid-old-token" {
		return cliproxyexecutor.Response{}, &Error{
			HTTPStatus: http.StatusUnauthorized,
			Message:    "unauthorized on old token",
		}
	}
	return cliproxyexecutor.Response{Payload: []byte(auth.ID + ":" + token)}, nil
}

func (e *auditLegacyVersionExecutor) ExecuteStream(
	_ context.Context,
	auth *Auth,
	_ cliproxyexecutor.Request,
	_ cliproxyexecutor.Options,
) (*cliproxyexecutor.StreamResult, error) {
	e.mu.Lock()
	onStream := e.onStream
	e.mu.Unlock()
	if onStream != nil {
		onStream(auth)
	}
	token := authAccessToken(auth)
	if token == "" {
		token = auth.Attributes[AttributeAPIKey]
	}
	chunks := make(chan cliproxyexecutor.StreamChunk, 2)
	chunks <- cliproxyexecutor.StreamChunk{Payload: []byte("chunk-1")}
	if token == "old-access-token" {
		chunks <- cliproxyexecutor.StreamChunk{
			Err: &Error{
				HTTPStatus: http.StatusTooManyRequests,
				Message:    "rate limited on old token",
			},
		}
	} else {
		chunks <- cliproxyexecutor.StreamChunk{Payload: []byte("chunk-2")}
	}
	close(chunks)
	return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
}

func (e *auditLegacyVersionExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *auditLegacyVersionExecutor) CountTokens(
	_ context.Context,
	_ *Auth,
	_ cliproxyexecutor.Request,
	_ cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *auditLegacyVersionExecutor) HttpRequest(_ context.Context, _ *Auth, _ *http.Request) (
	*http.Response,
	error,
) {
	return nil, nil
}

type auditLegacyVersionStore struct {
	mu      sync.Mutex
	records map[string]*Auth
}

func (s *auditLegacyVersionStore) Delete(context.Context, string) error { return nil }
func (s *auditLegacyVersionStore) Save(_ context.Context, auth *Auth) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.records == nil {
		s.records = make(map[string]*Auth)
	}
	s.records[auth.ID] = auth.Clone()
	return auth.ID + ".json", nil
}
func (s *auditLegacyVersionStore) List(context.Context) ([]*Auth, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	items := make([]*Auth, 0, len(s.records))
	for _, auth := range s.records {
		items = append(items, auth.Clone())
	}
	return items, nil
}

func TestOriginalIssue6416LateExecutionResultKeepsReplacement(t *testing.T) {
	previous := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(previous) })
	for _, test := range []struct {
		name, id, oldToken, newToken string
		stream                       bool
		status                       int
	}{
		{"execute", "auth-issue-6416-exec", "old-access-token", "new-access-token", false, http.StatusTooManyRequests},
		{"stream", "auth-issue-6416-stream", "old-access-token", "new-access-token", true, http.StatusTooManyRequests},
		{"unauthorized", "auth-issue-6416-401", "invalid-old-token", "valid-new-token", false, http.StatusUnauthorized},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				const model = "gpt-5"
				manager := NewManager(nil, nil, nil)
				manager.SetRetryConfig(0, 0, 0)
				executor := &auditLegacyVersionExecutor{id: "codex"}
				manager.RegisterExecutor(executor)
				reg := registry.GetGlobalRegistry()
				reg.RegisterClient(
					test.id,
					"codex",
					[]*registry.ModelInfo{{ID: model, Created: time.Now().Unix()}},
				)
				t.Cleanup(func() { reg.UnregisterClient(test.id) })
				registered, errRegister := manager.Register(
					t.Context(), &Auth{
						ID: test.id, Provider: "codex", Status: StatusActive,
						Metadata: map[string]any{"access_token": test.oldToken},
					},
				)
				if errRegister != nil {
					t.Fatal(errRegister)
				}
				started, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
				var releaseOnce sync.Once
				allowResult := func() { releaseOnce.Do(func() { close(release) }) }
				block := func(*Auth) { close(started); <-release }
				executor.onExecute, executor.onStream = block, block
				done := make(chan error, 1)
				go func() {
					defer close(finished)
					if !test.stream {
						_, errExecute := manager.Execute(
							t.Context(), []string{"codex"},
							cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{},
						)
						done <- errExecute
						return
					}
					stream, errStream := manager.ExecuteStream(
						t.Context(), []string{"codex"},
						cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{Stream: true},
					)
					if errStream != nil {
						done <- errStream
						return
					}
					var streamError error
					for chunk := range stream.Chunks {
						if chunk.Err != nil {
							streamError = chunk.Err
						}
					}
					done <- streamError
				}()
				t.Cleanup(func() { allowResult(); <-finished })
				<-started
				updated := registered.Clone()
				updated.Metadata["access_token"] = test.newToken
				published, errUpdate := manager.Update(t.Context(), updated)
				if errUpdate != nil || published == nil || published.Generation <= registered.Generation {
					t.Fatalf("replacement publication = %+v, %v", published, errUpdate)
				}
				allowResult()
				if errResult := <-done; statusCodeFromError(errResult) != test.status {
					t.Fatalf("old execution error = %v, want HTTP %d", errResult, test.status)
				}
				current, found := manager.GetByID(test.id)
				if !found ||
					current.Metadata["access_token"] !=
						test.newToken ||
					current.Generation !=
						published.Generation ||
					current.Unavailable || current.Status == StatusError ||
					current.Failed != 0 {
					t.Fatalf("late result changed replacement credential: %+v", current)
				}
				if state := current.ModelStates[model]; state != nil &&
					(state.Unavailable || state.Quota.Exceeded || !state.NextRetryAfter.IsZero()) {
					t.Fatalf("late result changed replacement model state: %+v", state)
				}
			},
		)
	}
}

func TestAuditLegacyVersion_LoadInitializesCredentialVersion(t *testing.T) {
	previous := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(previous) })

	model := "gpt-5"
	executor := &auditLegacyVersionExecutor{id: "codex"}
	authID := "auth-issue-6416-load"

	store := &auditLegacyVersionStore{
		records: map[string]*Auth{
			authID: {
				ID:         authID,
				Provider:   "codex",
				Generation: 0, // unpersisted on disk
				Metadata: map[string]any{
					"access_token": "old-access-token",
				},
			},
		},
	}

	m := NewManager(store, nil, nil)
	m.RegisterExecutor(executor)

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(authID, "codex", []*registry.ModelInfo{{ID: model, Created: time.Now().Unix()}})
	t.Cleanup(func() { reg.UnregisterClient(authID) })

	if errLoad := m.Load(context.Background()); errLoad != nil {
		t.Fatalf("Load error = %v", errLoad)
	}

	loadedAuth, ok := m.GetByID(authID)
	if !ok || loadedAuth == nil {
		t.Fatal("auth not found after Load")
	}
	if loadedAuth.Generation == 0 {
		t.Fatal("loaded credential has no manager generation")
	}

	execStarted := make(chan struct{})
	allowExecToFail := make(chan struct{})

	executor.mu.Lock()
	executor.onExecute = func(auth *Auth) {
		select {
		case <-execStarted:
		default:
			close(execStarted)
		}
		<-allowExecToFail
	}
	executor.mu.Unlock()

	reqDone := make(chan struct{})
	go func() {
		defer close(reqDone)
		_, _ = m.Execute(
			context.Background(),
			[]string{"codex"},
			cliproxyexecutor.Request{Model: model},
			cliproxyexecutor.Options{},
		)
	}()

	<-execStarted

	// Update with new token
	newAuth := loadedAuth.Clone()
	newAuth.Metadata = map[string]any{"access_token": "new-access-token"}
	updated, errUpdate := m.Update(context.Background(), newAuth)
	if errUpdate != nil {
		t.Fatalf("update: %v", errUpdate)
	}
	if updated.Generation <= loadedAuth.Generation {
		t.Fatalf("updated generation = %d, want newer than %d", updated.Generation, loadedAuth.Generation)
	}

	close(allowExecToFail)
	<-reqDone

	freshAuth, _ := m.GetByID(authID)
	if freshAuth.Unavailable {
		t.Fatalf("fresh credential must not be unavailable after late 429 from old token")
	}
	if ms := freshAuth.ModelStates[model]; ms != nil && ms.Quota.Exceeded {
		t.Fatalf("fresh credential model must not be in cooldown after late 429 from old token")
	}
}

func TestAuditLegacyVersion_APIKeyRotation_LateResultDoesNotCooldownNewAPIKey(t *testing.T) {
	previous := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(previous) })

	model := "gpt-5"
	executor := &auditLegacyVersionExecutor{id: "codex"}
	m := NewManager(nil, nil, nil)
	m.RegisterExecutor(executor)

	reg := registry.GetGlobalRegistry()
	authID := "auth-issue-6416-apikey"
	reg.RegisterClient(authID, "codex", []*registry.ModelInfo{{ID: model, Created: time.Now().Unix()}})
	t.Cleanup(func() { reg.UnregisterClient(authID) })

	initialAuth := &Auth{
		ID:       authID,
		Provider: "codex",
		Attributes: map[string]string{
			AttributeAPIKey: "old-api-key",
		},
	}
	registered, errRegister := m.Register(context.Background(), initialAuth)
	if errRegister != nil {
		t.Fatalf("register: %v", errRegister)
	}
	if registered.Generation == 0 {
		t.Fatal("registered credential has no manager generation")
	}

	execStarted := make(chan struct{})
	allowExecToFail := make(chan struct{})

	executor.mu.Lock()
	executor.onExecute = func(auth *Auth) {
		select {
		case <-execStarted:
		default:
			close(execStarted)
		}
		<-allowExecToFail
	}
	executor.mu.Unlock()

	reqDone := make(chan struct{})
	go func() {
		defer close(reqDone)
		_, _ = m.Execute(
			context.Background(),
			[]string{"codex"},
			cliproxyexecutor.Request{Model: model},
			cliproxyexecutor.Options{},
		)
	}()

	<-execStarted

	currentAuth, _ := m.GetByID(authID)
	newAuth := currentAuth.Clone()
	newAuth.Attributes = map[string]string{
		AttributeAPIKey: "new-api-key",
	}
	updated, errUpdate := m.Update(context.Background(), newAuth)
	if errUpdate != nil {
		t.Fatalf("update: %v", errUpdate)
	}
	if updated.Generation <= registered.Generation {
		t.Fatalf("updated generation = %d, want newer than %d", updated.Generation, registered.Generation)
	}

	close(allowExecToFail)
	<-reqDone

	freshAuth, _ := m.GetByID(authID)
	if freshAuth.Unavailable {
		t.Fatalf("fresh API key must not be unavailable after late 429 from old key")
	}
	if ms := freshAuth.ModelStates[model]; ms != nil && ms.Quota.Exceeded {
		t.Fatalf("fresh API key model must not be in cooldown after late 429 from old key")
	}
}

func TestAuditLegacyVersion_DirectMarkResult_StaleCredentialVersionIgnored(t *testing.T) {
	previous := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(previous) })

	model := "gpt-5"
	m := NewManager(nil, nil, nil)
	authID := "auth-direct-stale-version"

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(authID, "codex", []*registry.ModelInfo{{ID: model, Created: time.Now().Unix()}})
	t.Cleanup(func() { reg.UnregisterClient(authID) })

	initialAuth := &Auth{
		ID:       authID,
		Provider: "codex",
		Metadata: map[string]any{"access_token": "token-v1"},
	}
	registered, errRegister := m.Register(context.Background(), initialAuth)
	if errRegister != nil {
		t.Fatalf("register: %v", errRegister)
	}
	if registered.Generation == 0 {
		t.Fatal("registered credential has no manager generation")
	}

	// Update credentials to version 2.
	updatedAuth := registered.Clone()
	updatedAuth.Metadata = map[string]any{"access_token": "token-v2"}
	updated, errUpdate := m.Update(context.Background(), updatedAuth)
	if errUpdate != nil {
		t.Fatalf("update: %v", errUpdate)
	}
	if updated.Generation <= registered.Generation {
		t.Fatalf("updated generation = %d, want newer than %d", updated.Generation, registered.Generation)
	}

	disposition := cliproxyexecutor.ExecutionDisposition{
		DispatchPhase:  cliproxyexecutor.DispatchPhaseResponseStarted,
		Outcome:        cliproxyexecutor.ExecutionOutcomeRejectedBeforeProcessing,
		RetryDirective: cliproxyexecutor.RetryDirectiveRetryAfter,
		EffectScope:    cliproxyexecutor.StateEffectScopeQuotaFamily,
		EffectKey:      model,
	}
	// 迟到结果使用开始执行时的公开代际
	m.MarkResult(
		context.Background(), Result{
			AuthID:         authID,
			Provider:       "codex",
			Model:          model,
			Success:        false,
			AuthGeneration: registered.Generation,
			Error:          &Error{HTTPStatus: http.StatusTooManyRequests, Message: "stale 429"},
			Disposition:    disposition,
		},
	)

	afterStale, _ := m.GetByID(authID)
	if afterStale.Unavailable {
		t.Fatalf("afterStale must not be unavailable after stale MarkResult (v1)")
	}
	if ms := afterStale.ModelStates[model]; ms != nil && ms.Quota.Exceeded {
		t.Fatalf("afterStale model state must not be in quota cooldown after stale MarkResult (v1)")
	}

	// 缺少代际的结果同样保持与替换凭据隔离
	m.MarkResult(
		context.Background(), Result{
			AuthID:         authID,
			Provider:       "codex",
			Model:          model,
			Success:        false,
			AuthGeneration: 0,
			Error:          &Error{HTTPStatus: http.StatusTooManyRequests, Message: "stale 429 v0"},
			Disposition:    disposition,
		},
	)

	afterStaleZero, _ := m.GetByID(authID)
	if afterStaleZero.Unavailable {
		t.Fatalf("afterStaleZero must not be unavailable after stale MarkResult (v0)")
	}
	if ms := afterStaleZero.ModelStates[model]; ms != nil && ms.Quota.Exceeded {
		t.Fatalf("afterStaleZero model state must not be in quota cooldown after stale MarkResult (v0)")
	}

	// 当前发布快照的结果按明确配额范围生效
	m.MarkResult(
		context.Background(), Result{
			AuthID:         authID,
			Provider:       "codex",
			Model:          model,
			Success:        false,
			AuthGeneration: updated.Generation,
			Error:          &Error{HTTPStatus: http.StatusTooManyRequests, Message: "current 429"},
			Disposition:    disposition,
		},
	)

	afterCurrent, _ := m.GetByID(authID)
	ms := afterCurrent.ModelStates[model]
	if ms == nil || !ms.Quota.Exceeded {
		t.Fatalf("afterCurrent model state must be in quota cooldown after current MarkResult, got %+v", ms)
	}
}

func TestAuditLegacyVersion_DirectMarkResult_StaleRegistrationEpochIgnored(t *testing.T) {
	previous := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(previous) })

	model := "gpt-5"
	m := NewManager(nil, nil, nil)
	authID := "auth-direct-stale-epoch"

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(authID, "codex", []*registry.ModelInfo{{ID: model, Created: time.Now().Unix()}})
	t.Cleanup(func() { reg.UnregisterClient(authID) })

	initialAuth := &Auth{
		ID:       authID,
		Provider: "codex",
		Metadata: map[string]any{"access_token": "token-v1"},
	}
	registered, errRegister := m.Register(context.Background(), initialAuth)
	if errRegister != nil {
		t.Fatalf("register: %v", errRegister)
	}
	epoch1 := registered.Generation

	// Remove and re-register to advance RegistrationEpoch.
	if err := m.Remove(context.Background(), authID); err != nil {
		t.Fatalf("remove original registration: %v", err)
	}
	reRegistered, errReReg := m.Register(WithAllowAuthRecreate(context.Background()), initialAuth.Clone())
	if errReReg != nil {
		t.Fatalf("re-register: %v", errReReg)
	}
	epoch2 := reRegistered.Generation
	if epoch2 <= epoch1 {
		t.Fatalf("expected epoch2 (%d) > epoch1 (%d)", epoch2, epoch1)
	}

	// Stale MarkResult with epoch1 and 429 error must be ignored.
	m.MarkResult(
		context.Background(), Result{
			AuthID:         authID,
			Provider:       "codex",
			Model:          model,
			Success:        false,
			AuthGeneration: registered.Generation,
			Error:          &Error{HTTPStatus: http.StatusTooManyRequests, Message: "stale epoch 429"},
		},
	)

	afterStale, _ := m.GetByID(authID)
	if afterStale.Unavailable {
		t.Fatalf("afterStale must not be unavailable after stale epoch MarkResult")
	}
	if ms := afterStale.ModelStates[model]; ms != nil && ms.Quota.Exceeded {
		t.Fatalf("afterStale model state must not be in quota cooldown after stale epoch MarkResult")
	}
}

func TestAuditLegacyVersion_LateResultBeforeCredentialUpdate_PreservesSessionAffinity(t *testing.T) {
	previous := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(previous) })

	model := "gpt-5"
	fallback := &RoundRobinSelector{}
	selector := NewSessionAffinitySelector(fallback)
	m := NewManager(nil, selector, nil)
	//lint:ignore SA1012 验证 nil context 使用独立的 5 s 停止期限
	t.Cleanup(func() { _ = m.StopAutoRefresh(nil) })

	authID := "auth-session-affinity"
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(authID, "codex", []*registry.ModelInfo{{ID: model, Created: time.Now().Unix()}})
	t.Cleanup(func() { reg.UnregisterClient(authID) })

	initialAuth := &Auth{
		ID:       authID,
		Provider: "codex",
		Status:   StatusActive,
		Metadata: map[string]any{"access_token": "token-v1"},
	}
	registered, errRegister := m.Register(context.Background(), initialAuth)
	if errRegister != nil {
		t.Fatalf("register: %v", errRegister)
	}

	sessionID := "test-session-123"
	opts := cliproxyexecutor.Options{
		Headers: http.Header{
			"X-Session-Id": []string{sessionID},
		},
	}

	// First execution succeeds and establishes affinity binding.
	m.MarkResult(
		context.Background(), Result{
			AuthID:         authID,
			Provider:       "codex",
			Model:          model,
			Success:        true,
			AuthGeneration: registered.Generation,
			Options:        opts,
		},
	)

	// Pick with session ID must select authID.
	picked, errPick := selector.Pick(context.Background(), "codex", model, opts, []*Auth{registered})
	if errPick != nil || picked == nil || picked.ID != authID {
		t.Fatalf("expected selector to pick %s via affinity, got %v (err=%v)", authID, picked, errPick)
	}

	// Update credentials to version 2.
	updatedAuth := registered.Clone()
	updatedAuth.Metadata = map[string]any{"access_token": "token-v2"}
	updated, errUpdate := m.Update(context.Background(), updatedAuth)
	if errUpdate != nil {
		t.Fatalf("update: %v", errUpdate)
	}

	// 新材料的请求建立自己的会话绑定
	pickedCurrent, errPickCurrent := selector.Pick(context.Background(), "codex", model, opts, []*Auth{updated})
	if errPickCurrent != nil || pickedCurrent == nil || pickedCurrent.ID != authID {
		t.Fatalf("updated credential selection = %v, %v", pickedCurrent, errPickCurrent)
	}
	if bound, state := selector.LookupAffinity("codex", model, sessionID); bound != authID || state != "bound" {
		t.Fatalf("current session binding = %q, %q", bound, state)
	}

	// 旧代际的迟到失败保留新材料建立的会话绑定
	m.MarkResult(
		context.Background(), Result{
			AuthID:         authID,
			Provider:       "codex",
			Model:          model,
			Success:        false,
			AuthGeneration: registered.Generation,
			Error:          &Error{HTTPStatus: http.StatusTooManyRequests, Message: "stale 429"},
			Options:        opts,
		},
	)

	if bound, state := selector.LookupAffinity("codex", model, sessionID); bound != authID || state != "bound" {
		t.Fatalf("late result changed current session binding = %q, %q", bound, state)
	}
	// Session affinity must still be preserved for the new credential version.
	pickedAfter, errPickAfter := selector.Pick(context.Background(), "codex", model, opts, []*Auth{updated})
	if errPickAfter != nil || pickedAfter == nil || pickedAfter.ID != authID {
		t.Fatalf(
			"expected session affinity to remain preserved after stale MarkResult, got %v (err=%v)",
			pickedAfter,
			errPickAfter,
		)
	}
}

func TestAuditLegacyVersion_NonSecretUpdate_PreservesVersionAndAppliesCooldown(t *testing.T) {
	previous := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(previous) })

	model := "gpt-5"
	executor := &auditLegacyVersionExecutor{id: "codex"}
	m := NewManager(nil, nil, nil)
	m.RegisterExecutor(executor)

	reg := registry.GetGlobalRegistry()
	authID := "auth-nonsecret-update"
	reg.RegisterClient(authID, "codex", []*registry.ModelInfo{{ID: model, Created: time.Now().Unix()}})
	t.Cleanup(func() { reg.UnregisterClient(authID) })

	initialAuth := &Auth{
		ID:       authID,
		Provider: "codex",
		Metadata: map[string]any{
			"access_token": "old-access-token",
		},
		Label: "initial-label",
	}
	registered, errRegister := m.Register(context.Background(), initialAuth)
	if errRegister != nil {
		t.Fatalf("register: %v", errRegister)
	}
	if registered.Generation == 0 {
		t.Fatal("registered credential has no manager generation")
	}

	execStarted := make(chan struct{})
	allowExecToFail := make(chan struct{})

	executor.mu.Lock()
	executor.onExecute = func(auth *Auth) {
		select {
		case <-execStarted:
		default:
			close(execStarted)
		}
		<-allowExecToFail
	}
	executor.mu.Unlock()

	reqDone := make(chan struct{})
	go func() {
		defer close(reqDone)
		_, _ = m.Execute(
			context.Background(),
			[]string{"codex"},
			cliproxyexecutor.Request{Model: model},
			cliproxyexecutor.Options{},
		)
	}()

	<-execStarted

	// Update non-secret metadata (Label only, token unchanged).
	currentAuth, _ := m.GetByID(authID)
	nonSecretAuth := currentAuth.Clone()
	nonSecretAuth.Label = "updated-label"
	updated, errUpdate := m.Update(context.Background(), nonSecretAuth)
	if errUpdate != nil {
		t.Fatalf("update: %v", errUpdate)
	}
	if updated.Generation <= registered.Generation || updated.Label != "updated-label" {
		t.Fatalf("label update did not publish a new snapshot: %+v", updated)
	}

	close(allowExecToFail)
	<-reqDone

	// Because secret was not replaced, 429 should cool down the credential.
	finalAuth, _ := m.GetByID(authID)
	ms := finalAuth.ModelStates[model]
	if ms == nil || !ms.Quota.Exceeded {
		t.Fatalf("expected model to be in quota cooldown when secrets were unchanged, got %+v", ms)
	}
}
