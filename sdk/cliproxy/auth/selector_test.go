package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestFillFirstSelectorPick_Deterministic(t *testing.T) {
	t.Parallel()

	selector := &FillFirstSelector{}
	auths := []*Auth{
		{ID: "b"},
		{ID: "a"},
		{ID: "c"},
	}

	got, err := selector.Pick(context.Background(), "gemini", "", cliproxyexecutor.Options{}, auths)
	if err != nil {
		t.Fatalf("Pick() error = %v", err)
	}
	if got == nil {
		t.Fatalf("Pick() auth = nil")
	}
	if got.ID != "a" {
		t.Fatalf("Pick() auth.ID = %q, want %q", got.ID, "a")
	}
}

func TestRoundRobinSelectorPick_CyclesDeterministic(t *testing.T) {
	t.Parallel()

	selector := &RoundRobinSelector{}
	auths := []*Auth{
		{ID: "b"},
		{ID: "a"},
		{ID: "c"},
	}

	want := []string{"a", "b", "c", "a", "b"}
	for i, id := range want {
		got, err := selector.Pick(context.Background(), "gemini", "", cliproxyexecutor.Options{}, auths)
		if err != nil {
			t.Fatalf("Pick() #%d error = %v", i, err)
		}
		if got == nil {
			t.Fatalf("Pick() #%d auth = nil", i)
		}
		if got.ID != id {
			t.Fatalf("Pick() #%d auth.ID = %q, want %q", i, got.ID, id)
		}
	}
}

func TestRoundRobinSelectorPick_PriorityBuckets(t *testing.T) {
	t.Parallel()

	selector := &RoundRobinSelector{}
	auths := []*Auth{
		{ID: "c", Attributes: map[string]string{"priority": "0"}},
		{ID: "a", Attributes: map[string]string{"priority": "10"}},
		{ID: "b", Attributes: map[string]string{"priority": "10"}},
	}

	want := []string{"a", "b", "a", "b"}
	for i, id := range want {
		got, err := selector.Pick(context.Background(), "mixed", "", cliproxyexecutor.Options{}, auths)
		if err != nil {
			t.Fatalf("Pick() #%d error = %v", i, err)
		}
		if got == nil {
			t.Fatalf("Pick() #%d auth = nil", i)
		}
		if got.ID != id {
			t.Fatalf("Pick() #%d auth.ID = %q, want %q", i, got.ID, id)
		}
		if got.ID == "c" {
			t.Fatalf("Pick() #%d unexpectedly selected lower priority auth", i)
		}
	}
}

func TestFillFirstSelectorPick_PriorityFallbackCooldown(t *testing.T) {
	t.Parallel()

	selector := &FillFirstSelector{}
	now := time.Now()
	model := "test-model"

	high := &Auth{
		ID:         "high",
		Attributes: map[string]string{"priority": "10"},
		ModelStates: map[string]*ModelState{
			model: {
				Status:         StatusActive,
				Unavailable:    true,
				NextRetryAfter: now.Add(30 * time.Minute),
				Quota: QuotaState{
					Exceeded: true,
				},
			},
		},
	}
	low := &Auth{ID: "low", Attributes: map[string]string{"priority": "0"}}

	got, err := selector.Pick(context.Background(), "mixed", model, cliproxyexecutor.Options{}, []*Auth{high, low})
	if err != nil {
		t.Fatalf("Pick() error = %v", err)
	}
	if got == nil {
		t.Fatalf("Pick() auth = nil")
	}
	if got.ID != "low" {
		t.Fatalf("Pick() auth.ID = %q, want %q", got.ID, "low")
	}
}

func TestRoundRobinSelectorPick_Concurrent(t *testing.T) {
	selector := &RoundRobinSelector{}
	auths := []*Auth{
		{ID: "b"},
		{ID: "a"},
		{ID: "c"},
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	errCh := make(chan error, 1)

	goroutines := 32
	iterations := 100
	for range goroutines {
		wg.Go(
			func() {
				<-start
				for range iterations {
					got, err := selector.Pick(context.Background(), "gemini", "", cliproxyexecutor.Options{}, auths)
					if err != nil {
						select {
						case errCh <- err:
						default:
						}
						return
					}
					if got == nil {
						select {
						case errCh <- errors.New("pick() returned nil auth"):
						default:
						}
						return
					}
					if got.ID == "" {
						select {
						case errCh <- errors.New("pick() returned auth with empty ID"):
						default:
						}
						return
					}
				}
			},
		)
	}

	close(start)
	wg.Wait()

	select {
	case err := <-errCh:
		t.Fatalf("concurrent Pick() error = %v", err)
	default:
	}
}

func TestSelectorPick_AllCooldownReturnsModelCooldownError(t *testing.T) {
	t.Parallel()

	model := "test-model"
	now := time.Now()
	next := now.Add(60 * time.Second)
	auths := []*Auth{
		{
			ID: "a",
			ModelStates: map[string]*ModelState{
				model: {
					Status:         StatusActive,
					Unavailable:    true,
					NextRetryAfter: next,
					Quota: QuotaState{
						Exceeded:      true,
						NextRecoverAt: next,
					},
				},
			},
		},
		{
			ID: "b",
			ModelStates: map[string]*ModelState{
				model: {
					Status:         StatusActive,
					Unavailable:    true,
					NextRetryAfter: next,
					Quota: QuotaState{
						Exceeded:      true,
						NextRecoverAt: next,
					},
				},
			},
		},
	}

	t.Run(
		"mixed provider redacts provider field", func(t *testing.T) {
			t.Parallel()

			selector := &FillFirstSelector{}
			_, err := selector.Pick(context.Background(), "mixed", model, cliproxyexecutor.Options{}, auths)
			if err == nil {
				t.Fatalf("Pick() error = nil")
			}

			var mce *modelCooldownError
			if !errors.As(err, &mce) {
				t.Fatalf("Pick() error = %T, want *modelCooldownError", err)
			}
			if mce.StatusCode() != http.StatusTooManyRequests {
				t.Fatalf("StatusCode() = %d, want %d", mce.StatusCode(), http.StatusTooManyRequests)
			}

			headers := mce.Headers()
			if got := headers.Get("Retry-After"); got == "" {
				t.Fatalf("Headers().Get(Retry-After) = empty")
			}

			var payload map[string]any
			if err := json.Unmarshal([]byte(mce.Error()), &payload); err != nil {
				t.Fatalf("json.Unmarshal(Error()) error = %v", err)
			}
			rawErr, ok := payload["error"].(map[string]any)
			if !ok {
				t.Fatalf("Error() payload missing error object: %v", payload)
			}
			if got, _ := rawErr["code"].(string); got != "model_cooldown" {
				t.Fatalf("Error().error.code = %q, want %q", got, "model_cooldown")
			}
			if _, ok := rawErr["provider"]; ok {
				t.Fatalf("Error().error.provider exists for mixed provider: %v", rawErr["provider"])
			}
		},
	)

	t.Run(
		"non-mixed provider includes provider field", func(t *testing.T) {
			t.Parallel()

			selector := &FillFirstSelector{}
			_, err := selector.Pick(context.Background(), "gemini", model, cliproxyexecutor.Options{}, auths)
			if err == nil {
				t.Fatalf("Pick() error = nil")
			}

			var mce *modelCooldownError
			if !errors.As(err, &mce) {
				t.Fatalf("Pick() error = %T, want *modelCooldownError", err)
			}

			var payload map[string]any
			if err := json.Unmarshal([]byte(mce.Error()), &payload); err != nil {
				t.Fatalf("json.Unmarshal(Error()) error = %v", err)
			}
			rawErr, ok := payload["error"].(map[string]any)
			if !ok {
				t.Fatalf("Error() payload missing error object: %v", payload)
			}
			if got, _ := rawErr["provider"].(string); got != "gemini" {
				t.Fatalf("Error().error.provider = %q, want %q", got, "gemini")
			}
		},
	)
}

func TestModelCooldownErrorDoesNotExposePreviousRequestCause(t *testing.T) {
	t.Parallel()

	cause := errors.New("prior-client-private-diagnostic")
	cooldown := newModelCooldownErrorWithCause("test-model", "gemini", time.Minute, cause)
	var payload struct {
		Error map[string]any `json:"error"`
	}
	if err := json.Unmarshal([]byte(cooldown.Error()), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Error["message"] != "All credentials for model test-model are cooling down via provider gemini" {
		t.Fatalf("cooldown message contains previous request data: %v", payload.Error["message"])
	}
	if _, exposed := payload.Error["last_upstream_error"]; exposed {
		t.Fatal("cooldown response contains previous request diagnostic")
	}
	if !errors.Is(cooldown, cause) {
		t.Fatal("cooldown error lost its internal cause")
	}
}

func TestSelectorPick_AllTransientUnavailableReturnsAuthUnavailable(t *testing.T) {
	t.Parallel()

	model := "test-model"
	next := time.Now().Add(time.Minute)
	auths := []*Auth{
		{
			ID: "a",
			ModelStates: map[string]*ModelState{
				model: {
					Status:         StatusError,
					Unavailable:    true,
					NextRetryAfter: next,
				},
			},
		},
		{
			ID: "b",
			ModelStates: map[string]*ModelState{
				model: {
					Status:         StatusError,
					Unavailable:    true,
					NextRetryAfter: next,
				},
			},
		},
	}

	selector := &FillFirstSelector{}
	_, err := selector.Pick(context.Background(), "gemini", model, cliproxyexecutor.Options{}, auths)
	if _, ok := errors.AsType[*modelCooldownError](err); ok {
		t.Fatalf("Pick() error = %T, do not report a transient error as quota cooldown", err)
	}
	var authErr *Error
	if !errors.As(err, &authErr) || authErr.Code != "auth_unavailable" {
		t.Fatalf("Pick() error = %#v, want auth_unavailable", err)
	}
}

func TestIsAuthBlockedForModel_TransientUnavailableUsesOtherBlockReason(t *testing.T) {
	t.Parallel()

	now := time.Now()
	next := now.Add(time.Minute)
	tests := []struct {
		name  string
		auth  *Auth
		model string
	}{
		{
			name: "model state",
			auth: &Auth{
				ModelStates: map[string]*ModelState{
					"test-model": {
						Status:         StatusError,
						Unavailable:    true,
						NextRetryAfter: next,
					},
				},
			},
			model: "test-model",
		},
		{
			name: "auth state",
			auth: &Auth{
				Status:         StatusError,
				Unavailable:    true,
				NextRetryAfter: next,
			},
		},
	}

	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				blocked, reason, retryAt := isAuthBlockedForModel(test.auth, test.model, now)
				if !blocked || reason != blockReasonOther || !retryAt.Equal(next) {
					t.Fatalf(
						"isAuthBlockedForModel() = (%v, %v, %v), want (true, %v, %v)", blocked, reason, retryAt,
						blockReasonOther, next,
					)
				}
			},
		)
	}
}

func TestSessionAffinitySelector_PicksStickyAuth(t *testing.T) {
	t.Parallel()

	selector := NewSessionAffinitySelectorWithConfig(
		SessionAffinityConfig{
			Fallback: &RoundRobinSelector{},
			TTL:      time.Hour,
		},
	)
	defer selector.Stop()

	auths := []*Auth{{ID: "a"}, {ID: "b"}}
	opts := cliproxyexecutor.Options{Headers: http.Header{"X-Session-ID": {"sess-1"}}}

	first, err := selector.Pick(context.Background(), "gemini", "m1", opts, auths)
	if err != nil {
		t.Fatalf("first Pick() error = %v", err)
	}
	second, err := selector.Pick(context.Background(), "gemini", "m1", opts, auths)
	if err != nil {
		t.Fatalf("second Pick() error = %v", err)
	}
	if first == nil || second == nil {
		t.Fatal("expected non-nil auths")
	}
	if first.ID != second.ID {
		t.Fatalf("sticky session mismatch: %q vs %q", first.ID, second.ID)
	}
}

func TestSessionAffinitySelector_RespectsAvailablePriority(t *testing.T) {
	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
		Fallback: &FillFirstSelector{},
		TTL:      time.Hour,
	})
	defer selector.Stop()

	const model = "model-a"
	low := &Auth{ID: "low", Provider: "gemini", Attributes: map[string]string{"priority": "0"}}
	high := &Auth{ID: "high", Provider: "gemini", Attributes: map[string]string{"priority": "10"}}
	auths := []*Auth{low, high}
	opts := cliproxyexecutor.Options{
		Headers:  http.Header{"X-Session-ID": {"sess-priority"}},
		Metadata: map[string]any{cliproxyexecutor.SessionAffinityModelMetadataKey: model},
	}
	selector.Remember("gemini", opts, low)

	pick := func(want string) {
		t.Helper()
		selected, err := selector.Pick(context.Background(), "gemini", model, opts, auths)
		if err != nil {
			t.Fatalf("Pick() error = %v", err)
		}
		if selected == nil {
			t.Fatal("Pick() returned no auth")
		}
		if selected.ID != want {
			t.Fatalf("Pick() = auth %s, want auth %s", selected.ID, want)
		}
	}

	pick("high")
	high.ModelStates = map[string]*ModelState{
		model: {
			Status:         StatusError,
			Unavailable:    true,
			NextRetryAfter: time.Now().Add(time.Hour),
			Quota:          QuotaState{Exceeded: true, NextRecoverAt: time.Now().Add(time.Hour)},
		},
	}
	pick("low")
	high.ModelStates = nil
	pick("high")
}

func TestSessionAffinitySelector_SkipsStickyAuthCoolingDownForModel(t *testing.T) {
	selector := NewSessionAffinitySelectorWithConfig(
		SessionAffinityConfig{
			Fallback: &FillFirstSelector{},
			TTL:      time.Hour,
		},
	)
	defer selector.Stop()

	const model = "model-a"
	auths := []*Auth{
		{
			ID:       "a",
			Provider: "gemini",
			ModelStates: map[string]*ModelState{
				model: {
					Status:         StatusError,
					Unavailable:    true,
					NextRetryAfter: time.Now().Add(time.Hour),
					Quota: QuotaState{
						Exceeded:      true,
						NextRecoverAt: time.Now().Add(time.Hour),
					},
				},
			},
		},
		{ID: "b", Provider: "gemini"},
	}
	opts := cliproxyexecutor.Options{Headers: http.Header{"X-Session-ID": {"sess-cooldown"}}}
	selector.Remember("gemini", opts, auths[0])

	selected, errPick := selector.Pick(context.Background(), "gemini", model, opts, auths)
	if errPick != nil {
		t.Fatalf("Pick() error = %v", errPick)
	}
	if selected == nil || selected.ID != "b" {
		t.Fatalf("Pick() = %#v, want non-cooling auth b", selected)
	}
}

func TestHeaderValueIgnoreCase(t *testing.T) {
	t.Parallel()

	headers := http.Header{"X-Session-ID": {"sess-1"}}
	if got := headerValueIgnoreCase(headers, "X-Session-ID"); got != "sess-1" {
		t.Fatalf("headerValueIgnoreCase() = %q, want %q", got, "sess-1")
	}
}

func TestExtractSessionID_ClaudeCodeSessionIDPriorityOverHeader(t *testing.T) {
	t.Parallel()

	headers := http.Header{"X-Session-ID": {"header-session"}}
	body := []byte(`{"metadata":{"user_id":"{\"session_id\":\"claude-session-1\"}"}}`)
	if got := ExtractSessionID(headers, body, nil); got != "claude:claude-session-1" {
		t.Fatalf("ExtractSessionID() = %q, want %q", got, "claude:claude-session-1")
	}
}

func TestExtractSessionID_ClaudeCodeSuffixSessionID(t *testing.T) {
	t.Parallel()

	body := []byte(`{"metadata":{"user_id":"user_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa_account_x_session_9fbc3f7a-390a-4acf-aecf-11b2ed8d511e"}}`)
	if got := ExtractSessionID(nil, body, nil); got != "claude:9fbc3f7a-390a-4acf-aecf-11b2ed8d511e" {
		t.Fatalf("ExtractSessionID() = %q, want %q", got, "claude:9fbc3f7a-390a-4acf-aecf-11b2ed8d511e")
	}
}

func TestExtractSessionID_CodexSessionIDHeader(t *testing.T) {
	t.Parallel()

	headers := http.Header{"Session_id": {"codex-session-1"}}
	if got := ExtractSessionID(headers, nil, nil); got != "codex-session-1" {
		t.Fatalf("ExtractSessionID() = %q, want %q", got, "codex-session-1")
	}
}

func TestExtractSessionID_CodexSessionIDPriorityOverClientRequestID(t *testing.T) {
	t.Parallel()

	headers := http.Header{"Session_id": {"codex-session-1"}, "X-Client-Request-Id": {"client-session-1"}}
	if got := ExtractSessionID(headers, nil, nil); got != "codex-session-1" {
		t.Fatalf("ExtractSessionID() = %q, want %q", got, "codex-session-1")
	}
}

func TestExtractSessionID_CodexHyphenSessionIDHeader(t *testing.T) {
	t.Parallel()

	headers := http.Header{"Session-Id": {"codex-session-2"}}
	if got := ExtractSessionID(headers, nil, nil); got != "codex-session-2" {
		t.Fatalf("ExtractSessionID() = %q, want %q", got, "codex-session-2")
	}
}

func TestExtractSessionID_CodexConversationIDHeader(t *testing.T) {
	t.Parallel()

	headers := http.Header{"Conversation_id": {"codex-conversation-1"}}
	if got := ExtractSessionID(headers, nil, nil); got != "codex-conversation-1" {
		t.Fatalf("ExtractSessionID() = %q, want %q", got, "codex-conversation-1")
	}
}

func TestExtractSessionID_ClientRequestIDPriorityOverCodexPromptCacheKey(t *testing.T) {
	t.Parallel()

	headers := http.Header{"X-Client-Request-Id": {"client-session-1"}}
	body := []byte(`{"prompt_cache_key":"codex-cache-1"}`)
	if got := ExtractSessionID(headers, body, nil); got != "client-session-1" {
		t.Fatalf("ExtractSessionID() = %q, want %q", got, "client-session-1")
	}
}

func TestExtractSessionID_CodexTurnMetadataPromptCacheKeyBody(t *testing.T) {
	t.Parallel()

	body := []byte(`{"client_metadata":{"x-codex-turn-metadata":"{\"prompt_cache_key\":\"turn-cache-1\",\"window_id\":\"window-1\"}"}}`)
	if got := ExtractSessionID(nil, body, nil); got != "turn-cache-1" {
		t.Fatalf("ExtractSessionID() = %q, want %q", got, "turn-cache-1")
	}
}

func TestSessionAffinitySelector_UsesCodexSessionID(t *testing.T) {
	t.Parallel()

	selector := NewSessionAffinitySelectorWithConfig(
		SessionAffinityConfig{
			Fallback: &RoundRobinSelector{},
			TTL:      time.Hour,
		},
	)
	defer selector.Stop()

	auths := []*Auth{{ID: "a"}, {ID: "b"}}
	opts := cliproxyexecutor.Options{Headers: http.Header{"Session_id": {"codex-session-1"}}}

	first, err := selector.Pick(context.Background(), "codex", "m1", opts, auths)
	if err != nil {
		t.Fatalf("first Pick() error = %v", err)
	}
	second, err := selector.Pick(context.Background(), "codex", "m1", opts, auths)
	if err != nil {
		t.Fatalf("second Pick() error = %v", err)
	}
	if first == nil || second == nil || first.ID != second.ID {
		t.Fatalf("expected Session_id stickiness, got %v and %v", first, second)
	}
}

func TestSessionAffinitySelector_UsesCodexPromptCacheKeyBody(t *testing.T) {
	t.Parallel()

	selector := NewSessionAffinitySelectorWithConfig(
		SessionAffinityConfig{
			Fallback: &RoundRobinSelector{},
			TTL:      time.Hour,
		},
	)
	defer selector.Stop()

	auths := []*Auth{{ID: "a"}, {ID: "b"}}
	opts := cliproxyexecutor.Options{OriginalRequest: []byte(`{"prompt_cache_key":"codex-cache-1"}`)}

	first, err := selector.Pick(context.Background(), "codex", "m1", opts, auths)
	if err != nil {
		t.Fatalf("first Pick() error = %v", err)
	}
	second, err := selector.Pick(context.Background(), "codex", "m1", opts, auths)
	if err != nil {
		t.Fatalf("second Pick() error = %v", err)
	}
	if first == nil || second == nil || first.ID != second.ID {
		t.Fatalf("expected prompt_cache_key stickiness, got %v and %v", first, second)
	}
}

func TestSessionAffinitySelector_CodexSessionIDPriorityOverPromptCacheKey(t *testing.T) {
	t.Parallel()

	selector := NewSessionAffinitySelectorWithConfig(
		SessionAffinityConfig{
			Fallback: &RoundRobinSelector{},
			TTL:      time.Hour,
		},
	)
	defer selector.Stop()

	auths := []*Auth{{ID: "a"}, {ID: "b"}}
	selector.Remember(
		"codex", cliproxyexecutor.Options{Headers: http.Header{"Session_id": {"codex-client-session"}}}, auths[0],
	)

	opts := cliproxyexecutor.Options{
		Headers:         http.Header{"Session_id": {"codex-client-session"}},
		OriginalRequest: []byte(`{"prompt_cache_key":"codex-cache-1"}`),
	}
	sticky := selector.PickSticky("codex", opts, auths)
	if sticky == nil || sticky.ID != "a" {
		t.Fatalf("PickSticky() = %v, want auth a", sticky)
	}
}

func TestSessionAffinitySelector_CodexSessionIDPriorityOverAmpThreadID(t *testing.T) {
	t.Parallel()

	selector := NewSessionAffinitySelectorWithConfig(
		SessionAffinityConfig{
			Fallback: &RoundRobinSelector{},
			TTL:      time.Hour,
		},
	)
	defer selector.Stop()

	auths := []*Auth{{ID: "a"}, {ID: "b"}}
	selector.Remember(
		"codex", cliproxyexecutor.Options{Headers: http.Header{"Session_id": {"codex-session-1"}}}, auths[0],
	)

	opts := cliproxyexecutor.Options{
		Headers: http.Header{
			"Session_id": {"codex-session-1"}, "X-Amp-Thread-Id": {"amp-session-1"},
		},
	}
	sticky := selector.PickSticky("codex", opts, auths)
	if sticky == nil || sticky.ID != "a" {
		t.Fatalf("PickSticky() = %v, want auth a", sticky)
	}
}

func TestSessionAffinitySelector_UsesAmpThreadID(t *testing.T) {
	t.Parallel()

	selector := NewSessionAffinitySelectorWithConfig(
		SessionAffinityConfig{
			Fallback: &RoundRobinSelector{},
			TTL:      time.Hour,
		},
	)
	defer selector.Stop()

	auths := []*Auth{{ID: "a"}, {ID: "b"}}
	opts := cliproxyexecutor.Options{Headers: http.Header{"X-Amp-Thread-Id": {"thread-1"}}}

	first, err := selector.Pick(context.Background(), "codex", "m1", opts, auths)
	if err != nil {
		t.Fatalf("first Pick() error = %v", err)
	}
	second, err := selector.Pick(context.Background(), "codex", "m1", opts, auths)
	if err != nil {
		t.Fatalf("second Pick() error = %v", err)
	}
	if first == nil || second == nil || first.ID != second.ID {
		t.Fatalf("expected X-Amp-Thread-Id stickiness, got %v and %v", first, second)
	}
}

func TestSessionAffinitySelector_UsesExecutionSessionMetadata(t *testing.T) {
	t.Parallel()

	selector := NewSessionAffinitySelectorWithConfig(
		SessionAffinityConfig{
			Fallback: &RoundRobinSelector{},
			TTL:      time.Hour,
		},
	)
	defer selector.Stop()

	auths := []*Auth{{ID: "a"}, {ID: "b"}}
	opts := cliproxyexecutor.Options{Metadata: map[string]any{cliproxyexecutor.ExecutionSessionMetadataKey: "exec-1"}}

	first, err := selector.Pick(context.Background(), "gemini", "m1", opts, auths)
	if err != nil {
		t.Fatalf("first Pick() error = %v", err)
	}
	second, err := selector.Pick(context.Background(), "gemini", "m1", opts, auths)
	if err != nil {
		t.Fatalf("second Pick() error = %v", err)
	}
	if first == nil || second == nil || first.ID != second.ID {
		t.Fatalf("expected metadata session stickiness, got %v and %v", first, second)
	}
}

func TestIsAuthBlockedForModel_UnavailableWithoutNextRetryIsBlocked(t *testing.T) {
	t.Parallel()

	now := time.Now()
	model := "test-model"
	auth := &Auth{
		ID: "a",
		ModelStates: map[string]*ModelState{
			model: {
				Status:      StatusActive,
				Unavailable: true,
				Quota: QuotaState{
					Exceeded: true,
				},
			},
		},
	}

	blocked, reason, next := isAuthBlockedForModel(auth, model, now)
	if !blocked {
		t.Fatal("blocked = false, want authoritative block")
	}
	if reason != blockReasonOther {
		t.Fatalf("reason = %v, want %v", reason, blockReasonOther)
	}
	if !next.IsZero() {
		t.Fatalf("next = %v, want zero", next)
	}
}

func TestFillFirstSelectorPick_ThinkingSuffixFallsBackToBaseModelState(t *testing.T) {
	t.Parallel()

	selector := &FillFirstSelector{}
	now := time.Now()

	baseModel := "test-model"
	requestedModel := "test-model(high)"

	high := &Auth{
		ID:         "high",
		Attributes: map[string]string{"priority": "10"},
		ModelStates: map[string]*ModelState{
			baseModel: {
				Status:         StatusActive,
				Unavailable:    true,
				NextRetryAfter: now.Add(30 * time.Minute),
				Quota: QuotaState{
					Exceeded: true,
				},
			},
		},
	}
	low := &Auth{
		ID:         "low",
		Attributes: map[string]string{"priority": "0"},
	}

	got, err := selector.Pick(
		context.Background(), "mixed", requestedModel, cliproxyexecutor.Options{}, []*Auth{high, low},
	)
	if err != nil {
		t.Fatalf("Pick() error = %v", err)
	}
	if got == nil {
		t.Fatalf("Pick() auth = nil")
	}
	if got.ID != "low" {
		t.Fatalf("Pick() auth.ID = %q, want %q", got.ID, "low")
	}
}

func TestRoundRobinSelectorPick_ThinkingSuffixSharesCursor(t *testing.T) {
	t.Parallel()

	selector := &RoundRobinSelector{}
	auths := []*Auth{
		{ID: "b"},
		{ID: "a"},
	}

	first, err := selector.Pick(context.Background(), "gemini", "test-model(high)", cliproxyexecutor.Options{}, auths)
	if err != nil {
		t.Fatalf("Pick() first error = %v", err)
	}
	second, err := selector.Pick(context.Background(), "gemini", "test-model(low)", cliproxyexecutor.Options{}, auths)
	if err != nil {
		t.Fatalf("Pick() second error = %v", err)
	}
	if first == nil || second == nil {
		t.Fatalf("Pick() returned nil auth")
	}
	if first.ID != "a" {
		t.Fatalf("Pick() first auth.ID = %q, want %q", first.ID, "a")
	}
	if second.ID != "b" {
		t.Fatalf("Pick() second auth.ID = %q, want %q", second.ID, "b")
	}
}

func TestRoundRobinSelectorPick_CursorKeyCap(t *testing.T) {
	t.Parallel()

	selector := &RoundRobinSelector{maxKeys: 2}
	auths := []*Auth{{ID: "a"}}

	_, _ = selector.Pick(context.Background(), "gemini", "m1", cliproxyexecutor.Options{}, auths)
	_, _ = selector.Pick(context.Background(), "gemini", "m2", cliproxyexecutor.Options{}, auths)
	_, _ = selector.Pick(context.Background(), "gemini", "m3", cliproxyexecutor.Options{}, auths)

	selector.mu.Lock()
	defer selector.mu.Unlock()

	if selector.cursors == nil {
		t.Fatalf("selector.cursors = nil")
	}
	if len(selector.cursors) != 1 {
		t.Fatalf("len(selector.cursors) = %d, want %d", len(selector.cursors), 1)
	}
	if _, ok := selector.cursors["gemini:m3"]; !ok {
		t.Fatalf("selector.cursors missing key %q", "gemini:m3")
	}
}

func TestRoundRobinSelectorPick_GeminiCLICredentialGrouping(t *testing.T) {
	t.Parallel()

	selector := &RoundRobinSelector{}

	// Simulate two gemini-cli credentials, each with multiple projects:
	// Credential A (parent = "cred-a.json") has 3 projects
	// Credential B (parent = "cred-b.json") has 2 projects
	auths := []*Auth{
		{ID: "cred-a.json::proj-a1", Attributes: map[string]string{"gemini_virtual_parent": "cred-a.json"}},
		{ID: "cred-a.json::proj-a2", Attributes: map[string]string{"gemini_virtual_parent": "cred-a.json"}},
		{ID: "cred-a.json::proj-a3", Attributes: map[string]string{"gemini_virtual_parent": "cred-a.json"}},
		{ID: "cred-b.json::proj-b1", Attributes: map[string]string{"gemini_virtual_parent": "cred-b.json"}},
		{ID: "cred-b.json::proj-b2", Attributes: map[string]string{"gemini_virtual_parent": "cred-b.json"}},
	}

	// Two-level round-robin: consecutive picks must alternate between credentials.
	// Credential group order is randomized, but within each call the group cursor
	// advances by 1, so consecutive picks should cycle through different parents.
	picks := make([]string, 6)
	parents := make([]string, 6)
	for i := range 6 {
		got, err := selector.Pick(
			context.Background(), "gemini-cli", "gemini-2.5-pro", cliproxyexecutor.Options{}, auths,
		)
		if err != nil {
			t.Fatalf("Pick() #%d error = %v", i, err)
		}
		if got == nil {
			t.Fatalf("Pick() #%d auth = nil", i)
		}
		picks[i] = got.ID
		parents[i] = got.Attributes["gemini_virtual_parent"]
	}

	// Verify property: consecutive picks must alternate between credential groups.
	for i := 1; i < len(parents); i++ {
		if parents[i] == parents[i-1] {
			t.Fatalf(
				"Pick() #%d and #%d both from same parent %q (IDs: %q, %q); expected alternating credentials",
				i-1, i, parents[i], picks[i-1], picks[i],
			)
		}
	}

	// Verify property: each credential's projects are picked in sequence (round-robin within group).
	credPicks := map[string][]string{}
	for i, id := range picks {
		credPicks[parents[i]] = append(credPicks[parents[i]], id)
	}
	for parent, ids := range credPicks {
		for i := 1; i < len(ids); i++ {
			if ids[i] == ids[i-1] {
				t.Fatalf("Credential %q picked same project %q twice in a row", parent, ids[i])
			}
		}
	}
}

func TestRoundRobinSelectorPick_SingleParentFallsBackToFlat(t *testing.T) {
	t.Parallel()

	selector := &RoundRobinSelector{}

	// All auths from the same parent - should fall back to flat round-robin
	// because there's only one credential group (no benefit from two-level).
	auths := []*Auth{
		{ID: "cred-a.json::proj-a1", Attributes: map[string]string{"gemini_virtual_parent": "cred-a.json"}},
		{ID: "cred-a.json::proj-a2", Attributes: map[string]string{"gemini_virtual_parent": "cred-a.json"}},
		{ID: "cred-a.json::proj-a3", Attributes: map[string]string{"gemini_virtual_parent": "cred-a.json"}},
	}

	// With single parent group, parentOrder has length 1, so it uses flat round-robin.
	// Sorted by ID: proj-a1, proj-a2, proj-a3
	want := []string{
		"cred-a.json::proj-a1",
		"cred-a.json::proj-a2",
		"cred-a.json::proj-a3",
		"cred-a.json::proj-a1",
	}

	for i, expectedID := range want {
		got, err := selector.Pick(
			context.Background(), "gemini-cli", "gemini-2.5-pro", cliproxyexecutor.Options{}, auths,
		)
		if err != nil {
			t.Fatalf("Pick() #%d error = %v", i, err)
		}
		if got == nil {
			t.Fatalf("Pick() #%d auth = nil", i)
		}
		if got.ID != expectedID {
			t.Fatalf("Pick() #%d auth.ID = %q, want %q", i, got.ID, expectedID)
		}
	}
}

func TestRoundRobinSelectorPick_MixedVirtualAndNonVirtualFallsBackToFlat(t *testing.T) {
	t.Parallel()

	selector := &RoundRobinSelector{}

	// Mix of virtual and non-virtual auths (e.g., a regular gemini-cli auth without projects
	// alongside virtual ones). Should fall back to flat round-robin.
	auths := []*Auth{
		{ID: "cred-a.json::proj-a1", Attributes: map[string]string{"gemini_virtual_parent": "cred-a.json"}},
		{ID: "cred-regular.json"}, // no gemini_virtual_parent
	}

	// groupByVirtualParent returns nil when any auth lacks the attribute,
	// so flat round-robin is used. Sorted by ID: cred-a.json::proj-a1, cred-regular.json
	want := []string{
		"cred-a.json::proj-a1",
		"cred-regular.json",
		"cred-a.json::proj-a1",
	}

	for i, expectedID := range want {
		got, err := selector.Pick(context.Background(), "gemini-cli", "", cliproxyexecutor.Options{}, auths)
		if err != nil {
			t.Fatalf("Pick() #%d error = %v", i, err)
		}
		if got == nil {
			t.Fatalf("Pick() #%d auth = nil", i)
		}
		if got.ID != expectedID {
			t.Fatalf("Pick() #%d auth.ID = %q, want %q", i, got.ID, expectedID)
		}
	}
}

type selectorStopProbe struct {
	mu    sync.Mutex
	stops int
}

func (*selectorStopProbe) Pick(
	context.Context,
	string,
	string,
	cliproxyexecutor.Options,
	[]*Auth,
) (*Auth, error) {
	return nil, nil
}

func (s *selectorStopProbe) Stop() {
	s.mu.Lock()
	s.stops++
	s.mu.Unlock()
}

func (s *selectorStopProbe) stopCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stops
}

type uncomparableSelector struct {
	values []string
}

func (uncomparableSelector) Pick(
	context.Context,
	string,
	string,
	cliproxyexecutor.Options,
	[]*Auth,
) (*Auth, error) {
	return nil, nil
}

func TestManagerSetSelectorUncomparableTypes(t *testing.T) {
	t.Parallel()
	manager := NewManager(nil, nil, nil)
	manager.SetSelector(uncomparableSelector{values: []string{"first"}})
	manager.SetSelector(uncomparableSelector{values: []string{"second"}})
	manager.SetSelector(nil)
}

func TestManagerSetSelectorSameInstanceDoesNotStop(t *testing.T) {
	t.Parallel()
	selector := &selectorStopProbe{}
	manager := NewManager(nil, selector, nil)

	manager.SetSelector(selector)

	if stops := selector.stopCount(); stops != 0 {
		t.Fatalf("Stop() calls = %d, want 0", stops)
	}
	if manager.Selector() != selector {
		t.Fatalf("Selector() = %#v, want original selector", manager.Selector())
	}
}

func TestManagerSetSelectorConcurrent(t *testing.T) {
	t.Parallel()
	manager := NewManager(nil, nil, nil)
	var workers sync.WaitGroup
	for range 20 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for range 10 {
				manager.SetSelector(&selectorStopProbe{})
			}
		}()
	}
	workers.Wait()
	if manager.Selector() == nil {
		t.Fatal("Selector() = nil after concurrent replacements")
	}
}
