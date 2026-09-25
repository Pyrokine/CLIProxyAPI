package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	internalconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestC08HomeDispatchForceMappingPropagation(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	manager.RegisterExecutor(&replaceAwareExecutor{id: "codex"})
	payload, err := json.Marshal(
		homeAuthDispatchResponse{
			Model:         "gpt-upstream",
			ForceMapping:  true,
			OriginalAlias: "gpt-client",
			Auth:          Auth{ID: "home-auth", Provider: "codex"},
		},
	)
	if err != nil {
		t.Fatalf("marshal Home response: %v", err)
	}
	dispatcher := &testHomeDispatcher{heartbeat: true, payload: payload}
	previous := currentHomeDispatcher
	currentHomeDispatcher = func() homeAuthDispatcher { return dispatcher }
	t.Cleanup(func() { currentHomeDispatcher = previous })

	auth, _, _, err := manager.pickNextViaHome(context.Background(), "gpt-client", cliproxyexecutor.Options{}, nil)
	if err != nil {
		t.Fatalf("pickNextViaHome() error = %v", err)
	}
	result := homeForceMappingAliasResult(auth, "ignored")
	if result.UpstreamModel != "gpt-upstream" || !result.ForceMapping || result.OriginalAlias != "gpt-client" {
		t.Fatalf("force mapping result = %#v", result)
	}
}

type c08CountTokensExecutor struct {
	err error
}

func (*c08CountTokensExecutor) Identifier() string { return "claude" }
func (*c08CountTokensExecutor) Execute(
	context.Context,
	*Auth,
	cliproxyexecutor.Request,
	cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}
func (*c08CountTokensExecutor) ExecuteStream(
	context.Context,
	*Auth,
	cliproxyexecutor.Request,
	cliproxyexecutor.Options,
) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}
func (*c08CountTokensExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}
func (e *c08CountTokensExecutor) CountTokens(
	context.Context,
	*Auth,
	cliproxyexecutor.Request,
	cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, e.err
}
func (*c08CountTokensExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func TestC08CountTokens404AvailabilityClassification(t *testing.T) {
	tests := []struct {
		name        string
		err         error
		unavailable bool
	}{
		{
			name: "generic route 404 is neutral",
			err:  &Error{HTTPStatus: http.StatusNotFound, Message: "404 page not found"},
		},
		{
			name: "explicit model 404 updates availability",
			err: &Error{
				Code:       "model_not_found",
				HTTPStatus: http.StatusNotFound,
				Message:    `{"error":{"type":"not_found_error","message":"model claude-missing was not found"}}`,
			},
			unavailable: true,
		},
	}
	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				if gotNeutral := isCountTokensEndpointNotFoundError(
					tt.err, "claude-missing",
				); gotNeutral == tt.unavailable {
					t.Fatalf("neutral classification = %v for unavailable=%v", gotNeutral, tt.unavailable)
				}
				manager := NewManager(nil, nil, nil)
				manager.RegisterExecutor(&c08CountTokensExecutor{err: tt.err})
				manager.SetRetryConfig(0, 0, 1)
				auth := &Auth{ID: "count-auth", Provider: "claude"}
				registry.GetGlobalRegistry().RegisterClient(
					auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "claude-missing"}},
				)
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
				if _, err := manager.Register(t.Context(), auth); err != nil {
					t.Fatalf("register auth: %v", err)
				}
				_, errCount := manager.ExecuteCount(
					t.Context(), []string{"claude"}, cliproxyexecutor.Request{Model: "claude-missing"},
					cliproxyexecutor.Options{},
				)
				if errCount == nil {
					t.Fatal("expected count_tokens error")
				}
				if gotNeutral := isCountTokensEndpointNotFoundError(
					errCount, "claude-missing",
				); gotNeutral == tt.unavailable {
					t.Fatalf(
						"returned error neutral classification = %v for unavailable=%v; err=%#v", gotNeutral,
						tt.unavailable, errCount,
					)
				}
				updated, ok := manager.GetByID(auth.ID)
				if !ok || updated == nil {
					t.Fatal("auth disappeared")
				}
				state := updated.ModelStates["claude-missing"]
				if got := state != nil && state.Unavailable; got != tt.unavailable {
					t.Fatalf(
						"model unavailable = %v, want %v; state=%#v failed=%d generation=%d",
						got, tt.unavailable, state, updated.Failed, updated.Generation,
					)
				}
			},
		)
	}
}

func TestC08SessionSignalPriorityAndValidation(t *testing.T) {
	headers := http.Header{
		"X-Claude-Code-Session-Id": {"claude-native"},
		"Session-Id":               {"codex-native"},
		"X-Session-ID":             {"generic-native"},
	}
	body := []byte(`{"prompt_cache_key":"body-cache","conversation":{"id":"conversation-id"}}`)
	primary, fallback := extractSessionIDs(headers, body, nil)
	if primary != "claude:claude-native" || fallback != "" {
		t.Fatalf("native priority = %q, %q", primary, fallback)
	}
	primary, fallback = extractSessionIDs(nil, body, nil)
	if primary != "pck:body-cache" || fallback != "conv:conversation-id" {
		t.Fatalf("body priority = %q, %q", primary, fallback)
	}
	if got := normalizedSessionCandidate("bad\nvalue"); got != "" {
		t.Fatalf("control-bearing session ID accepted: %q", got)
	}
}

func TestC08HomeSessionAliasTTLRefreshPreservesLiveGroup(t *testing.T) {
	var cache homeSessionAliasCache
	now := time.Now()
	if got := cache.canonical("pck:cache", "conv:conversation", time.Minute, now); got != "pck:cache" {
		t.Fatalf("initial canonical = %q", got)
	}
	if got := cache.canonical("pck:cache", "", time.Minute, now.Add(40*time.Second)); got != "pck:cache" {
		t.Fatalf("primary refresh canonical = %q", got)
	}
	if got := cache.canonical("conv:conversation", "", time.Minute, now.Add(80*time.Second)); got != "pck:cache" {
		t.Fatalf("live alias canonical = %q", got)
	}
}

func TestHomeDispatchSessionIDsPreserveParentLineage(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, nil, nil)
	child, parent := manager.homeDispatchSessionIDs(cliproxyexecutor.Options{
		OriginalRequest: []byte(`{"session_id":"child","parent_session_id":"parent"}`),
	})
	if child != "session:child" || parent != "session:parent" {
		t.Fatalf("home session hierarchy = (%q, %q), want (session:child, session:parent)", child, parent)
	}

	alias, aliasParent := manager.homeDispatchSessionIDs(cliproxyexecutor.Options{
		OriginalRequest: []byte(`{"prompt_cache_key":"cache","conversation":{"id":"conversation"}}`),
	})
	if alias != "pck:cache" || aliasParent != "" {
		t.Fatalf("home aliases = (%q, %q), want (pck:cache, empty parent)", alias, aliasParent)
	}
}

func TestC08HomeSessionAliasCacheClearsOnTTLChange(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	firstConfig := &internalconfig.Config{}
	firstConfig.Routing.SessionAffinityTTL = "1h"
	manager.SetConfig(firstConfig)
	combined := cliproxyexecutor.Options{OriginalRequest: []byte(`{"prompt_cache_key":"cache","conversation":{"id":"conversation"}}`)}
	conversation := cliproxyexecutor.Options{OriginalRequest: []byte(`{"conversation":{"id":"conversation"}}`)}
	if got := manager.homeDispatchSessionID(combined); got != "pck:cache" {
		t.Fatalf("combined canonical = %q", got)
	}
	if got := manager.homeDispatchSessionID(conversation); got != "pck:cache" {
		t.Fatalf("conversation canonical before TTL change = %q", got)
	}
	secondConfig := &internalconfig.Config{}
	secondConfig.Routing.SessionAffinityTTL = "1m"
	manager.SetConfig(secondConfig)
	if got := manager.homeDispatchSessionID(conversation); got != "conv:conversation" {
		t.Fatalf("conversation canonical after TTL change = %q", got)
	}
}
