package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	runtimeexecutor "github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
)

func TestServerUpdateClientsInvalidatesChangedAntigravityPool(t *testing.T) {
	runtimeexecutor.ResetAntigravityTransports()
	t.Cleanup(runtimeexecutor.ResetAntigravityTransports)

	server := newTestServer(t)
	auth := &coreauth.Auth{
		ID:       "server-antigravity-pool",
		Provider: "antigravity",
		Metadata: map[string]any{
			"access_token": "test-token",
			"project_id":   "test-project",
			"expired":      time.Now().Add(time.Hour).Format(time.RFC3339),
		},
	}
	primeServerAntigravityTransport(t, server.configSnapshot(), auth)
	if got := runtimeexecutor.AntigravityTransportsLen(); got != 1 {
		t.Fatalf("transport cache length = %d, want 1", got)
	}

	enabled := true
	changed, errClone := config.Clone(server.configSnapshot())
	if errClone != nil {
		t.Fatalf("clone changed config: %v", errClone)
	}
	changed.Antigravity.ConnectionPool.Enabled = &enabled
	server.UpdateClients(changed)
	if got := runtimeexecutor.AntigravityTransportsLen(); got != 0 {
		t.Fatalf("transport cache length = %d, want 0 after pool change", got)
	}

	primeServerAntigravityTransport(t, changed, auth)
	unchanged, errClone := config.Clone(changed)
	if errClone != nil {
		t.Fatalf("clone unchanged config: %v", errClone)
	}
	server.UpdateClients(unchanged)
	if got := runtimeexecutor.AntigravityTransportsLen(); got != 1 {
		t.Fatalf("transport cache length = %d, want unchanged pool to remain", got)
	}
}

func primeServerAntigravityTransport(t *testing.T, cfg *config.Config, auth *coreauth.Auth) {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"totalTokens":1}`)
	}))
	defer upstream.Close()

	selected := auth.Clone()
	selected.Attributes = map[string]string{"base_url": upstream.URL}
	_, errCount := runtimeexecutor.NewAntigravityExecutor(cfg).CountTokens(
		context.Background(),
		selected,
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
