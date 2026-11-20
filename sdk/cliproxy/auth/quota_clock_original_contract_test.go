package auth

import (
	"net/http"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestOriginalQuotaClockDeadlines(t *testing.T) {
	for _, test := range []struct {
		id                   string
		credential, explicit bool
	}{
		{"quota-clock-model-explicit", false, true},
		{"quota-clock-model-fallback", false, false},
		{"quota-clock-auth-explicit", true, true},
		{"quota-clock-auth-fallback", true, false},
	} {
		t.Run(
			test.id, func(t *testing.T) {
				manager := NewManager(nil, nil, nil)
				auth, err := manager.Register(
					WithSkipPersist(t.Context()), &Auth{
						ID: test.id, Provider: "codex", Metadata: map[string]any{"disable_cooling": false},
					},
				)
				if err != nil {
					t.Fatal(err)
				}
				model := "gpt-5"
				if test.credential && !test.explicit {
					model = ""
				}
				result := Result{
					AuthID: auth.ID, Provider: auth.Provider, Model: model,
					Error: &Error{HTTPStatus: http.StatusTooManyRequests, Message: "quota exceeded"},
				}
				if test.credential {
					result.Disposition = originalCredentialQuotaDisposition(auth.ID)
				}
				if test.explicit {
					result.RetryAfter = new(time.Hour)
				}
				markOriginalCooldownResult(t, manager, result)
				current, _ := manager.GetByID(auth.ID)
				retry, recoverAt := current.NextRetryAfter, current.Quota.NextRecoverAt
				if !test.credential {
					state := current.ModelStates[model]
					if state == nil {
						t.Fatal("missing model quota state")
					}
					retry, recoverAt = state.NextRetryAfter, state.Quota.NextRecoverAt
				}
				if retry.IsZero() || recoverAt.IsZero() || retry != retry.Round(0) ||
					recoverAt != recoverAt.Round(0) ||
					!retry.Equal(recoverAt) {
					t.Fatalf("quota wall-clock retry=%v recovery=%v", retry, recoverAt)
				}
			},
		)
	}
}

func TestOriginalQuotaSupportedCountPreservesObservation(t *testing.T) {
	for _, status := range []int{0, http.StatusNotFound, http.StatusTooManyRequests} {
		name := "success"
		if status != 0 {
			name = http.StatusText(status)
		}
		t.Run(
			name, func(t *testing.T) {
				const id, model = "count-observation-auth", "count-observation-model"
				manager := NewManager(nil, &FillFirstSelector{}, nil)
				manager.SetHomeClientProvider(func() HomeClient { return nil })
				executor := &quotaCoreTestExecutor{
					provider: "codex", countHeaders: http.Header{"X-Codex-Primary-Used-Percent": {"99"}},
				}
				if status != 0 {
					executor.countErr = &Error{HTTPStatus: status, Message: "404 page not found"}
					if status == http.StatusTooManyRequests {
						executor.countErr = &Error{HTTPStatus: status, Message: "quota"}
					}
				}
				manager.RegisterExecutor(executor)
				original := time.Unix(10, 0)
				auth, err := manager.Register(
					WithSkipPersist(t.Context()), &Auth{
						ID: id, Provider: "codex", Status: StatusError, StatusMessage: "existing failure",
						LastError: &Error{
							HTTPStatus: http.StatusServiceUnavailable,
							Message:    "existing failure",
						},
						Quota: QuotaState{
							ObservedAt: original, Signals: map[string]string{"X-Codex-Primary-Used-Percent": "40"},
						},
					},
				)
				if err != nil {
					t.Fatal(err)
				}
				registerProjectionTestClient(t, id, auth.Provider, &registry.ModelInfo{ID: model})
				manager.RefreshSchedulerEntry(id)
				_, err = manager.ExecuteCount(
					t.Context(), []string{auth.Provider}, core.Request{Model: model}, core.Options{},
				)
				if statusCodeFromError(err) != status {
					t.Fatalf("count result=%v want status=%d", err, status)
				}
				current, _ := manager.GetByID(id)
				if current.Quota.Signals["X-Codex-Primary-Used-Percent"] != "40" ||
					!current.Quota.ObservedAt.Equal(original) {
					t.Fatalf("read-only count advanced supported-provider quota: %+v", current.Quota)
				}
				if status != http.StatusTooManyRequests &&
					(current.Status !=
						StatusError ||
						current.StatusMessage !=
							("existing "+
								"failure") ||
						current.LastError ==
							nil) {
					t.Fatalf("read-only count recovered availability: %+v", current)
				}
			},
		)
	}
}
