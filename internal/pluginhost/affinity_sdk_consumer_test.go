package pluginhost

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginabi"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
)

func TestHostAffinityLookupConsumesPublishedSelectorAndManager(t *testing.T) {
	selector := coreauth.NewSessionAffinitySelectorWithConfig(coreauth.SessionAffinityConfig{TTL: time.Hour})
	defer selector.Stop()
	manager := coreauth.NewManager(nil, selector, nil)
	manager.SetHomeClientProvider(func() coreauth.HomeClient { return nil })
	var credentials []*coreauth.Auth
	for _, id := range []string{"first-private-credential", "second-private-credential"} {
		credential, err := manager.Register(
			coreauth.WithSkipPersist(t.Context()), &coreauth.Auth{
				ID: id, Provider: "openai", Metadata: map[string]any{"marker": "synthetic-private-marker"},
			},
		)
		if err != nil {
			t.Fatal(err)
		}
		credentials = append(credentials, credential)
	}
	//goland:noinspection SpellCheckingInspection
	const model, sessionID = "team::ModelA", "sdk-affinity-session"
	opts := coreexecutor.Options{
		Headers:  http.Header{"X-Session-ID": {sessionID}},
		Metadata: map[string]any{coreexecutor.SessionAffinityModelMetadataKey: model},
	}
	host := New()
	host.SetAffinityManager(manager)
	for _, credential := range credentials {
		selector.Remember("openai", opts, credential)
		if id, state := selector.LookupAffinity("openai", model, sessionID); state != "bound" || id != credential.ID {
			t.Fatalf("published selector observation=%q/%q, want %q/bound", id, state, credential.ID)
		}
		if found, state := manager.LookupSessionAffinity(
			"openai", model, sessionID,
		); state != "bound" || found == nil || found.ID != credential.ID {
			t.Fatalf("Manager observation=%+v/%q", found, state)
		}
		request, err := json.Marshal(
			pluginapi.HostAffinityLookupRequest{
				Provider: "openai", Model: model, SessionID: sessionID,
			},
		)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := host.callFromPlugin(t.Context(), pluginabi.MethodHostAffinityLookup, request)
		if err != nil {
			t.Fatal(err)
		}
		response, err := decodeRPCEnvelope[pluginapi.HostAffinityLookupResponse](raw)
		if err != nil || response.Status != pluginapi.HostAffinityStatusBound ||
			response.AuthIndex != credential.Index ||
			response.ObservedAt.IsZero() {
			t.Fatalf("plugin consumer response=%+v error=%v", response, err)
		}
		for _, privateValue := range []string{credentials[0].ID, credentials[1].ID, "synthetic-private-marker"} {
			if bytes.Contains(raw, []byte(privateValue)) {
				t.Fatalf("private credential field reached plugin response: %s", raw)
			}
		}
	}
	if id, state := selector.LookupAffinity(
		"openai", model, sessionID, func(id string) bool { return id == credentials[0].ID },
	); state != "unbound" || id != "" {
		t.Fatalf("old credential was rebound during filtered observation: %q/%q", id, state)
	}
	//goland:noinspection SpellCheckingInspection
	const differentlyCasedModel = "team::modela"
	for _, query := range []struct{ provider, model string }{
		{provider: "claude", model: model},
		{provider: "openai", model: differentlyCasedModel},
		{provider: "openai", model: "team"},
	} {
		if found, state := manager.LookupSessionAffinity(
			query.provider, query.model, sessionID,
		); state != "unbound" || found != nil {
			t.Fatalf("observation crossed provider/model namespace: %+v -> %+v/%q", query, found, state)
		}
	}
	if id, state := selector.LookupAffinity("openai", model, sessionID); state != "bound" || id != credentials[1].ID {
		t.Fatalf("missed observations changed current binding: %q/%q", id, state)
	}
}
