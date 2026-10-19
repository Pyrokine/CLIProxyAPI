package pluginhost

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestHostRoutingResetCooldownDoesNotPersistOrEnableCredentials(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "active", true: "disabled"}[disabled], func(t *testing.T) {
			store := &commandLineAuthStore{}
			manager := coreauth.NewManager(store, nil, nil)
			auth := &coreauth.Auth{ID: "synthetic-routing", Provider: "codex", Disabled: disabled,
				Status: coreauth.StatusError, ModelStates: map[string]*coreauth.ModelState{
					"limited": {Status: coreauth.StatusError, Unavailable: true,
						NextRetryAfter: time.Now().Add(time.Hour), Quota: coreauth.QuotaState{Exceeded: true}},
					"transient": {Status: coreauth.StatusError, Unavailable: true,
						NextRetryAfter: time.Now().Add(time.Hour), LastError: &coreauth.Error{HTTPStatus: http.StatusServiceUnavailable}},
					"permanent": {Status: coreauth.StatusError, Unavailable: true,
						LastError: &coreauth.Error{HTTPStatus: http.StatusUnauthorized}},
				}}
			if disabled {
				auth.Status = coreauth.StatusDisabled
				for _, state := range auth.ModelStates {
					state.Status = coreauth.StatusDisabled
				}
			}
			registered, err := manager.Register(coreauth.WithSkipPersist(context.Background()), auth)
			if err != nil {
				t.Fatal(err)
			}
			host := New()
			host.SetAffinityManager(manager)
			request, err := json.Marshal(map[string]string{"auth_index": " " + registered.Index + " "})
			if err != nil {
				t.Fatal(err)
			}
			ctx := withHostCallbackPluginID(context.Background(), "fixture-plugin")
			raw, err := host.callFromPlugin(ctx, "host.routing.reset_cooldown", request)
			if err != nil {
				t.Fatal(err)
			}
			response, err := decodeRPCEnvelope[struct {
				AuthIndex string   `json:"auth_index"`
				Models    []string `json:"models"`
			}](raw)
			if err != nil || response.AuthIndex != registered.Index {
				t.Fatalf("callback response = %s, error = %v", raw, err)
			}
			updated, ok := manager.GetByID(registered.ID)
			if !ok || updated.Disabled != disabled || len(store.saved) != 0 {
				t.Fatalf("callback changed credential enablement or persisted a token: ok=%t, saves=%d", ok, len(store.saved))
			}
			if disabled {
				if updated.Status != coreauth.StatusDisabled || len(response.Models) != 0 {
					t.Fatal("callback enabled disabled routing state")
				}
			} else {
				if updated.ModelStates["limited"].Unavailable || updated.ModelStates["transient"].Unavailable || len(response.Models) != 2 {
					t.Fatal("callback did not clear quota and temporary routing cooldowns")
				}
				if !updated.ModelStates["permanent"].Unavailable || updated.ModelStates["permanent"].LastError == nil {
					t.Fatal("callback cleared a permanent credential error")
				}
			}
		})
	}
}

func TestHostRoutingResetCooldownRejectsInvalidCallersAndTargets(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	for _, scenario := range []struct {
		name    string
		request string
		manager bool
		closed  bool
		want    string
	}{
		{name: "invalid JSON", request: `{`, want: "decode"},
		{name: "invalid index type", request: `{"auth_index":1}`, want: "decode"},
		{name: "empty index", request: `{"auth_index":" "}`, want: "auth_index is required"},
		{name: "manager unavailable", request: `{"auth_index":"missing"}`, want: "manager unavailable"},
		{name: "unknown index", request: `{"auth_index":"missing"}`, manager: true, want: "auth not found"},
		{name: "closed instance", request: `{"auth_index":"missing"}`, manager: true, closed: true, want: "closed"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			host := New()
			if scenario.manager {
				host.SetAffinityManager(manager)
			}
			instance := &hostCallbackInstance{id: 101}
			instance.closed.Store(scenario.closed)
			ctx := withHostCallbackInstance(withHostCallbackPluginID(context.Background(), "fixture-plugin"), instance)
			if raw, err := host.callFromPlugin(ctx, "host.routing.reset_cooldown", []byte(scenario.request)); err == nil || !strings.Contains(err.Error(), scenario.want) {
				t.Fatalf("callback response = %s, error = %v; want %s", raw, err, scenario.want)
			}
		})
	}
}
