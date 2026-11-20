package auth

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type auditAuthorityRefreshExecutor struct {
	schedulerTestExecutor
	rotate    bool
	calls     []string
	refreshes int
}

func (e *auditAuthorityRefreshExecutor) Refresh(_ context.Context, a *Auth) (*Auth, error) {
	e.refreshes++
	updated := a.Clone()
	updated.Metadata["expired"] = time.Now().Add(3 * time.Hour).Format(time.RFC3339)
	updated.Metadata["refresh_token"] = "synthetic-replacement-refresh"
	if e.rotate {
		updated.Metadata["access_token"] = "synthetic-rotated-access"
	}
	return updated, nil
}
func (e *auditAuthorityRefreshExecutor) Execute(
	_ context.Context,
	a *Auth,
	_ core.Request,
	_ core.Options,
) (core.Response, error) {
	e.calls = append(e.calls, a.ID+":"+authAccessToken(a))
	if a.ID == "authority-a" && authAccessToken(a) == "synthetic-rejected-access" {
		return core.Response{}, errorWithState(
			&Error{
				Code: "unauthorized", HTTPStatus: 401, Message: "synthetic token rejection",
			}, errorState{
				DispatchPhase:  core.DispatchPhaseResponseStarted,
				Outcome:        core.ExecutionOutcomeRejectedBeforeProcessing,
				RetryDirective: core.RetryDirectiveSameAuthAfterRefresh, EffectScope: core.StateEffectScopeCredential,
			},
		)
	}
	return core.Response{Payload: []byte(`{"synthetic":"success"}`)}, nil
}

func TestAuditRefreshAuthorizationRequiresAccessTokenRotation(t *testing.T) {
	for _, rotate := range []bool{false, true} {
		name := "same-access"
		if rotate {
			name = "rotated-access"
		}
		t.Run(
			name, func(t *testing.T) {
				manager := NewManager(nil, &FillFirstSelector{}, nil)
				manager.SetHomeClientProvider(func() HomeClient { return nil })
				manager.SetRetryConfig(0, 0, 2)
				execute := &auditAuthorityRefreshExecutor{rotate: rotate}
				manager.RegisterExecutor(execute)
				for _, id := range []string{"authority-a", "authority-b"} {
					access := "synthetic-rejected-access"
					if id == "authority-b" {
						access = "synthetic-healthy-access"
					}
					_, err := manager.Register(
						t.Context(), &Auth{
							ID: id, Provider: "test", Status: StatusActive, Metadata: map[string]any{
								"access_token": access, "refresh_token": "synthetic-refresh",
								"expired": time.Now().Add(time.Hour).Format(time.RFC3339),
							},
						},
					)
					if err != nil {
						t.Fatal(err)
					}
					registry.GetGlobalRegistry().RegisterClient(
						id, "test", []*registry.ModelInfo{{ID: "authority-model"}},
					)
					t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
				}
				_, err := manager.Execute(
					t.Context(), []string{"test"}, core.Request{Model: "authority-model"}, core.Options{},
				)
				if err != nil || execute.refreshes != 1 || len(execute.calls) != 2 {
					t.Fatalf("error=%v refreshes=%d calls=%v", err, execute.refreshes, execute.calls)
				}
				want := "authority-b:synthetic-healthy-access"
				if rotate {
					want = "authority-a:synthetic-rotated-access"
				}
				if execute.calls[1] != want {
					t.Fatalf("replayed token instead of authorized transition: calls=%v", execute.calls)
				}
				current, _ := manager.GetByID("authority-a")
				if current.hasRejectedAccessToken() == rotate ||
					current.Metadata["refresh_token"] != "synthetic-replacement-refresh" {
					t.Fatalf("rejection=%t metadata=%v", current.hasRejectedAccessToken(), current.Metadata)
				}
			},
		)
	}
}

func TestAuditRotatedTokenClearsUnauthorizedDeadlineWithoutQuota(t *testing.T) {
	for _, modelState := range []bool{false, true} {
		name := "credential-deadline"
		if modelState {
			name = "model-deadline"
		}
		t.Run(
			name, func(t *testing.T) {
				manager := NewManager(nil, nil, nil)
				manager.SetHomeClientProvider(func() HomeClient { return nil })
				execute := &auditAuthorityRefreshExecutor{rotate: true}
				manager.RegisterExecutor(execute)
				deadline := time.Now().Add(time.Hour)
				credential := &Auth{
					ID: "authority-a", Provider: "test", Status: StatusError, Unavailable: true,
					NextRetryAfter: deadline, LastError: &Error{Code: "unauthorized", HTTPStatus: 401},
					Metadata: map[string]any{
						"access_token": "synthetic-rejected-access", "refresh_token": "synthetic-refresh",
						"expired": deadline.Format(time.RFC3339),
					},
				}
				if modelState {
					credential.ModelStates = map[string]*ModelState{
						"authority-model": {
							Status: StatusError, Unavailable: true, NextRetryAfter: deadline,
							LastError: &Error{
								Code: "unauthorized", HTTPStatus: 401,
							},
						},
					}
				}
				registered, err := manager.Register(t.Context(), credential)
				if err != nil {
					t.Fatal(err)
				}
				manager.markRejectedAccessToken(registered, execute)
				if _, err := manager.ForceRefreshAuth(t.Context(), registered.ID); err != nil {
					t.Fatal(err)
				}
				current, _ := manager.GetByID(registered.ID)
				if current.Quota.Exceeded || current.hasRejectedAccessToken() || current.LastError != nil ||
					current.Unavailable ||
					!current.NextRetryAfter.IsZero() {
					t.Fatalf("rotated credential retained an unauthorized deadline: %#v", current)
				}
				if blocked, _, _ := isAuthBlockedForModel(current, "authority-model", time.Now()); blocked {
					t.Fatal("non-quota unauthorized deadline still blocks the rotated token")
				}
			},
		)
	}
}

func TestAuditInFlightSuccessCannotReleaseRejectedToken(t *testing.T) {
	manager := NewManager(nil, &FillFirstSelector{}, nil)
	manager.SetHomeClientProvider(func() HomeClient { return nil })
	execute := &auditAuthorityRefreshExecutor{}
	manager.RegisterExecutor(execute)
	registerSchedulerModels(t, "test", "authority-model", "authority-a")
	registered, err := manager.Register(
		t.Context(), &Auth{
			ID: "authority-a", Provider: "test", Status: StatusActive,
			Metadata: map[string]any{"access_token": "synthetic-rejected-access"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	selected, err := manager.scheduler.pickSingle(t.Context(), "test", "authority-model", core.Options{}, nil)
	if err != nil || selected == nil {
		t.Fatalf("selection before rejection: selected=%t error=%v", selected != nil, err)
	}
	inFlight := executionResult(
		selected, selected.Provider, "authority-model", nil, core.DispatchPhaseDownstreamCommitted,
	)
	manager.markRejectedAccessToken(registered, execute)
	manager.MarkResult(t.Context(), inFlight)
	current, _ := manager.GetByID(registered.ID)
	if current.Generation != registered.Generation || !current.hasRejectedAccessToken() {
		t.Fatalf(
			"rejection authority changed after in-flight success: generation=%d original=%d rejected=%t",
			current.Generation, registered.Generation, current.hasRejectedAccessToken(),
		)
	}
	if picked, errPick := manager.scheduler.pickSingle(
		t.Context(), "test", "authority-model", core.Options{}, nil,
	); picked != nil || errPick == nil {
		t.Fatalf("rejected token selected after old success: selected=%t error=%v", picked != nil, errPick)
	}
}

func TestAuditForceRefreshPreservesIndependentQuotaAuthority(t *testing.T) {
	for _, rotate := range []bool{false, true} {
		for _, deadline := range []bool{false, true} {
			name := "same"
			if rotate {
				name = "rotate"
			}
			if deadline {
				name += "/deadline"
			} else {
				name += "/operator-reset"
			}
			t.Run(
				name, func(t *testing.T) {
					manager := NewManager(nil, nil, nil)
					manager.SetHomeClientProvider(func() HomeClient { return nil })
					execute := &auditAuthorityRefreshExecutor{rotate: rotate}
					manager.RegisterExecutor(execute)
					resetAt := time.Time{}
					if deadline {
						resetAt = time.Now().Add(time.Hour)
					}
					credential := &Auth{
						ID: "authority-a", FileName: "synthetic.json", Provider: "test", Status: StatusError,
						Unavailable: true, NextRetryAfter: resetAt, StateEffectScope: core.StateEffectScopeCredential,
						StateEffectKey: "authority-a", StateGeneration: 7, ResetAuthority: "operator",
						Quota: QuotaState{
							Exceeded: true, Reason: "independent quota", Family: "independent", NextRecoverAt: resetAt,
							Generation: 7,
						}, LastError: &Error{Code: "unauthorized", HTTPStatus: 401}, Metadata: map[string]any{
							"access_token": "synthetic-rejected-access", "refresh_token": "synthetic-refresh",
							"expired": time.Now().Add(time.Hour).Format(time.RFC3339),
						},
					}
					registered, err := manager.Register(t.Context(), credential)
					if err != nil {
						t.Fatal(err)
					}
					manager.markRejectedAccessToken(registered, execute)
					var refresh = manager.ForceRefreshAuth
					result, err := refresh(t.Context(), registered.ID)
					if (err == nil) != rotate {
						t.Fatalf("rotate=%t result=%+v error=%v", rotate, result, err)
					}
					current, _ := manager.GetByID(registered.ID)
					if !current.Quota.Exceeded || current.Quota.Family != "independent" ||
						!current.Quota.NextRecoverAt.Equal(resetAt) ||
						current.ResetAuthority != "operator" ||
						current.Quota.Generation != 7 {
						t.Fatalf("quota authority changed: quota=%+v reset=%s", current.Quota, current.ResetAuthority)
					}
					if blocked, _, _ := isAuthBlockedForModel(current, "arbitrary-model", time.Now()); !blocked {
						t.Fatal("independent quota stopped blocking after refresh")
					}
					if current.Metadata["refresh_token"] != "synthetic-replacement-refresh" ||
						current.LastRefreshedAt.IsZero() {
						t.Fatal("valid exchange material was not committed")
					}
					encoded, errJSON := json.Marshal(result)
					if errJSON != nil || strings.Contains(
						string(encoded), "synthetic-rejected-access",
					) || strings.Contains(
						string(encoded), "synthetic-replacement-refresh",
					) || strings.Contains(string(encoded), "access_token") || strings.Contains(
						string(encoded), "refresh_token",
					) {
						t.Fatalf("unsafe refresh result=%s error=%v", encoded, errJSON)
					}
					var summary map[string]any
					if err := json.Unmarshal(encoded, &summary); err != nil || len(summary) != 5 {
						t.Fatalf("published summary fields=%v error=%v", summary, err)
					}
					//lint:ignore SA1012 验证 nil context 使用独立的 5 s 停止期限
					if err := manager.StopAutoRefresh(nil); err != nil {
						t.Fatal(err)
					}
					if err := manager.StopAutoRefresh(t.Context()); err != nil {
						t.Fatal(err)
					}
				},
			)
		}
	}
}
