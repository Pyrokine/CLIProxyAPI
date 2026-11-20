package management

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/quota"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestCodexQuotaRefreshCredentialTimeBarrier(t *testing.T) {
	startedAt := time.Now().UTC()
	for _, test := range []struct {
		name      string
		updatedAt time.Time
		data      string
		wantClear bool
	}{
		{"older quota", startedAt.Add(-time.Second), `{"rate_limit":{"allowed":true}}`, true},
		{"same instant quota", startedAt, `{"rate_limit":{"allowed":true}}`, true},
		{"newer quota", startedAt.Add(time.Second), `{"rate_limit":{"allowed":true}}`, false},
		{"unknown availability", startedAt.Add(-time.Second), `{"rate_limit":{}}`, false},
		{"still exhausted", startedAt.Add(-time.Second), `{"rate_limit":{"allowed":false}}`, false},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				manager := coreauth.NewManager(nil, nil, nil)
				registered, err := manager.Register(
					context.Background(), &coreauth.Auth{
						ID:               "quota-time-barrier-" + test.name,
						Provider:         "codex",
						Status:           coreauth.StatusError,
						StatusMessage:    "credential quota exhausted",
						Unavailable:      true,
						NextRetryAfter:   startedAt.Add(time.Hour),
						UpdatedAt:        test.updatedAt,
						LastError:        &coreauth.Error{HTTPStatus: http.StatusTooManyRequests},
						Quota:            coreauth.QuotaState{Exceeded: true, NextRecoverAt: startedAt.Add(time.Hour)},
						StateEffectScope: coreexecutor.StateEffectScopeCredential,
					},
				)
				if err != nil {
					t.Fatal(err)
				}
				h := &Handler{authManager: manager}
				if err := h.finalizeQuotaRefresh(
					quota.RefreshAttempt{
						Type: quota.TypeCodex, AuthIndex: registered.Index,
						StartedAt: startedAt, Data: []byte(test.data),
					},
				); err != nil {
					t.Fatal(err)
				}
				current, ok := manager.GetByID(registered.ID)
				if !ok {
					t.Fatal("credential disappeared")
				}
				cleared := !current.Quota.Exceeded && !current.Unavailable && current.LastError == nil
				if cleared != test.wantClear {
					t.Fatalf("credential quota cleared=%t, want %t; state=%+v", cleared, test.wantClear, current)
				}
				if !test.wantClear && current.Generation != registered.Generation {
					t.Fatalf("retained quota generation=%d, want %d", current.Generation, registered.Generation)
				}
			},
		)
	}
}
