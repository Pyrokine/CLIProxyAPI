package auth

import (
	"context"
	"sync/atomic"
	"testing"

	internalconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
)

func TestOriginalForceRefreshAuthInputs(t *testing.T) {
	for _, sample := range []struct {
		name, id string
		missing  bool
	}{
		{name: "clears authentication failure", id: "ag-err"},
		{name: "missing executor preserves failure", id: "ag-fail", missing: true},
	} {
		t.Run(
			sample.name, func(t *testing.T) {
				manager := NewManager(nil, &RoundRobinSelector{}, nil)
				manager.SetHomeClientProvider(func() HomeClient { return nil })
				var calls atomic.Int32
				if !sample.missing {
					manager.RegisterExecutor(
						&originalLifecycleExecutor{
							id: "antigravity",
							mutate: func(_ context.Context, auth *Auth) (*Auth, error) {
								calls.Add(1)
								auth.Metadata["access_token"] = "rotated-access-" + auth.ID
								auth.Metadata["refresh_token"] = "rotated-refresh-" + auth.ID
								return auth, nil
							},
						},
					)
				}
				credential := &Auth{
					ID: sample.id, Provider: "antigravity", Status: StatusError, Unavailable: true,
					LastError: &Error{Code: "unauthorized", Message: "token revoked"},
					Metadata:  map[string]any{"access_token": "old-tok", "refresh_token": "ref-tok"},
				}
				if _, err := manager.Register(t.Context(), credential); err != nil {
					t.Fatal(err)
				}
				if !hasUnauthorizedAuthFailure(credential) {
					t.Fatal("original revoked-token input lost its classification")
				}
				result, err := manager.ForceRefreshAuth(t.Context(), credential.ID)
				if (err != nil) != sample.missing {
					t.Fatalf("refresh result=%+v error=%v", result, err)
				}
				current, exists := manager.GetByID(credential.ID)
				if !exists || current == nil || result.FileName != credential.ID ||
					result.Provider != "antigravity" ||
					result.AuthIndex == "" {
					t.Fatalf("credential or safe refresh identity missing: %+v", result)
				}
				if sample.missing {
					if calls.Load() !=
						0 ||
						current.Status !=
							StatusError ||
						!current.Unavailable ||
						current.LastError ==
							nil ||
						current.LastError.Code != "unauthorized" || current.LastError.Message != "token revoked" ||
						current.Metadata["access_token"] != "old-tok" ||
						current.Metadata["refresh_token"] != "ref-tok" {
						t.Fatal("missing executor changed the original failure or credential material")
					}
					return
				}
				if calls.Load() !=
					1 ||
					current.Status !=
						StatusActive ||
					current.Unavailable ||
					current.LastError !=
						nil ||
					result.Status != StatusActive ||
					result.RefreshedAt.IsZero() ||
					current.Metadata["access_token"] != "rotated-access-"+credential.ID ||
					current.Metadata["refresh_token"] != "rotated-refresh-"+credential.ID {
					t.Fatal("successful rotation retained the authentication failure or lost new material")
				}
			},
		)
	}
}

func TestOriginalRefreshWorkerResolutionInputs(t *testing.T) {
	for _, sample := range []struct {
		name string
		cfg  *internalconfig.Config
		want int
	}{
		{name: "empty config", want: 16},
		{name: "zero", cfg: &internalconfig.Config{AuthAutoRefreshWorkers: 0}, want: 16},
		{name: "negative", cfg: &internalconfig.Config{AuthAutoRefreshWorkers: -5}, want: 16},
		{name: "positive", cfg: &internalconfig.Config{AuthAutoRefreshWorkers: 4}, want: 4},
	} {
		t.Run(
			sample.name, func(t *testing.T) {
				manager := NewManager(nil, &RoundRobinSelector{}, nil)
				manager.SetConfig(sample.cfg)
				if got := manager.autoRefreshWorkers(nil); got != sample.want {
					t.Fatalf("refresh workers=%d want=%d", got, sample.want)
				}
				if got := cap(manager.refreshSemaphore); got != sample.want {
					t.Fatalf("active refresh semaphore=%d want=%d", got, sample.want)
				}
			},
		)
	}
}
