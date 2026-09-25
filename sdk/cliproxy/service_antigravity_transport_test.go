package cliproxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	runtimeexecutor "github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
)

func TestServiceInvalidatesAntigravityTransportsOnCredentialLifecycle(t *testing.T) {
	previousFetch := fetchAntigravityModels
	fetchAntigravityModels = func(
		context.Context,
		*coreauth.Auth,
		*config.Config,
	) runtimeexecutor.ModelDiscoveryResult {
		return runtimeexecutor.ModelDiscoveryResult{Outcome: runtimeexecutor.ModelDiscoveryInconclusive}
	}
	t.Cleanup(func() { fetchAntigravityModels = previousFetch })

	for _, test := range []struct {
		name  string
		apply func(*Service, *coreauth.Auth) error
	}{
		{
			name: "credential rebind",
			apply: func(service *Service, auth *coreauth.Auth) error {
				rebound := auth.Clone()
				rebound.Metadata["access_token"] = "rebound-token"
				return service.applyCoreAuthAddOrUpdate(coreauth.WithSkipPersist(context.Background()), rebound)
			},
		},
		{
			name: "credential removal",
			apply: func(service *Service, auth *coreauth.Auth) error {
				service.applyCoreAuthRemoval(coreauth.WithSkipPersist(context.Background()), auth.ID)
				return nil
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			runtimeexecutor.ResetAntigravityTransports()
			t.Cleanup(runtimeexecutor.ResetAntigravityTransports)

			auth := &coreauth.Auth{
				ID:       "antigravity-transport-" + test.name,
				Provider: "antigravity",
				Status:   coreauth.StatusActive,
				Metadata: map[string]any{
					"access_token": "initial-token",
					"project_id":   "test-project",
					"expired":      time.Now().Add(time.Hour).Format(time.RFC3339),
				},
			}
			manager := coreauth.NewManager(nil, nil, nil)
			registered, errRegister := manager.Register(coreauth.WithSkipPersist(context.Background()), auth)
			if errRegister != nil {
				t.Fatalf("register auth: %v", errRegister)
			}
			service := &Service{cfg: &config.Config{}, coreManager: manager}
			t.Cleanup(func() {
				if err := service.stopModelRefreshWorkers(context.Background()); err != nil {
					t.Errorf("stopModelRefreshWorkers() error = %v", err)
				}
			})

			primeAntigravityTransport(t, registered)
			if got := runtimeexecutor.AntigravityTransportsLen(); got != 1 {
				t.Fatalf("transport cache length = %d, want 1", got)
			}
			if errApply := test.apply(service, registered); errApply != nil {
				t.Fatalf("apply credential lifecycle: %v", errApply)
			}
			if got := runtimeexecutor.AntigravityTransportsLen(); got != 0 {
				t.Fatalf("transport cache length = %d, want 0 after %s", got, test.name)
			}
		})
	}
}

func primeAntigravityTransport(t *testing.T, auth *coreauth.Auth) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"totalTokens":1}`)
	}))
	defer server.Close()

	auth.Attributes = map[string]string{"base_url": server.URL}
	_, errCount := runtimeexecutor.NewAntigravityExecutor(&config.Config{}).CountTokens(
		context.Background(),
		auth,
		coreexecutor.Request{
			Model:   "gemini-2.5-flash",
			Payload: []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}}`),
		},
		coreexecutor.Options{SourceFormat: sdktranslator.FormatAntigravity},
	)
	if errCount != nil {
		t.Fatalf("prime Antigravity transport: %v", errCount)
	}
}
