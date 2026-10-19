package api

import (
	"fmt"
	"testing"
	"time"

	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestServerTransientCooldownInitializationAndHotReloadReachAuthState(t *testing.T) {
	coreauth.SetTransientErrorCooldownSeconds(9)
	t.Cleanup(func() { coreauth.SetTransientErrorCooldownSeconds(0) })
	server := newTestServer(t)
	manager := server.handlers.AuthManager
	for index, seconds := range []int{0, 7, -1, 0} {
		if index != 0 {
			cfg := *server.configSnapshot()
			cfg.TransientErrorCooldownSeconds = seconds
			server.UpdateClients(&cfg)
		}
		id := fmt.Sprintf("synthetic-transient-cooldown-%d", index)
		registered, err := manager.Register(t.Context(), &coreauth.Auth{
			ID: id, Provider: "codex", Status: coreauth.StatusActive,
		})
		if err != nil {
			t.Fatal(err)
		}
		manager.MarkResult(t.Context(), coreauth.Result{
			AuthID: id, AuthGeneration: registered.Generation, Provider: "codex", Model: "synthetic-model",
			Error: &coreauth.Error{HTTPStatus: 520, Message: "synthetic upstream failure"},
		})
		updated, ok := manager.GetByID(id)
		if !ok || updated.ModelStates["synthetic-model"] == nil {
			t.Fatal("transient failure did not publish model state")
		}
		state := updated.ModelStates["synthetic-model"]
		if seconds < 0 {
			if state.Unavailable || !state.NextRetryAfter.IsZero() {
				t.Fatalf("disabled cooldown retained a retry deadline: %+v", state)
			}
			continue
		}
		want := time.Duration(seconds) * time.Second
		if seconds == 0 {
			want = time.Minute
		}
		remaining := time.Until(state.NextRetryAfter)
		if !state.Unavailable || remaining < want-time.Second || remaining > want+time.Second {
			t.Fatalf("configured cooldown %d produced %v, want about %v", seconds, remaining, want)
		}
	}
}
