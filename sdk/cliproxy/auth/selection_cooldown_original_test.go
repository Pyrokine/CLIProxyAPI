package auth

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestOriginalSelectionCooldownRouteNames(t *testing.T) {
	const model = "client-opus(high)"
	next := time.Now().Add(time.Hour)
	auth := &Auth{
		ID: "cooling-auth", Unavailable: true, NextRetryAfter: next,
		StateEffectScope: core.StateEffectScopeCredential, StateEffectKey: "cooling-auth",
		Quota:       QuotaState{Exceeded: true, NextRecoverAt: next},
		ModelStates: map[string]*ModelState{"other-model": {Status: StatusActive}},
	}
	for _, sample := range []struct {
		name     string
		selector Selector
	}{
		{"round-robin", &RoundRobinSelector{}},
		{"weighted-round-robin", &WeightedRoundRobinSelector{}},
		{"fill-first", &FillFirstSelector{}},
	} {
		t.Run(
			sample.name, func(t *testing.T) {
				_, err := sample.selector.Pick(t.Context(), "mixed", model, core.Options{}, []*Auth{auth})
				var cooldown *modelCooldownError
				if !errors.As(
					err, &cooldown,
				) || cooldown.model != model || cooldown.resetIn <= 0 || cooldown.resetIn > time.Hour {
					t.Fatalf("route cooldown=%+v error=%v", cooldown, err)
				}
			},
		)
	}
}

func TestOriginalSelectionCooldownCausePriority(t *testing.T) {
	const model = "gpt-5.6-sol"
	now := time.Now()
	older, newer, next := now.Add(-10*time.Minute), now.Add(-10*time.Second), now.Add(time.Minute)
	overloaded := `{"type":"error","code":"server_is_overloaded","message":"Our servers a` +
		`re currently overloaded. Please try again later.","sequence_number":0}`
	for _, sample := range []struct {
		name    string
		auths   []*Auth
		want    string
		cooling bool
	}{
		{
			"upstream_error", []*Auth{
				{
					ID: "codex-auth-1", Provider: "codex", Unavailable: true,
					NextRetryAfter: next, Status: StatusError, StatusMessage: overloaded,
					Quota:     QuotaState{Exceeded: true, NextRecoverAt: next},
					LastError: &Error{Code: "auth_unavailable", Message: overloaded, HTTPStatus: 503},
					ModelStates: map[string]*ModelState{
						model: {
							Status: StatusError, Unavailable: true, NextRetryAfter: next,
							StatusMessage: overloaded, Quota: QuotaState{Exceeded: true, NextRecoverAt: next},
							LastError: &Error{Code: "auth_unavailable", Message: overloaded, HTTPStatus: 503},
						},
					},
				},
			}, overloaded, true,
		},
		{
			"latest_candidate", []*Auth{
				{
					ID: "codex-auth-1", Provider: "codex", Unavailable: true, Status: StatusError,
					StatusMessage: "older error: quota exceeded", UpdatedAt: older,
					ModelStates: map[string]*ModelState{
						model: {
							Status: StatusError, Unavailable: true,
							StatusMessage: "older error: quota exceeded", UpdatedAt: older,
						},
					},
				},
				{
					ID: "codex-auth-2", Provider: "codex", Unavailable: true, Status: StatusError,
					StatusMessage: "newer error: server is overloaded", UpdatedAt: newer,
					ModelStates: map[string]*ModelState{
						model: {
							Status: StatusError, Unavailable: true,
							StatusMessage: "newer error: server is overloaded", UpdatedAt: newer,
						},
					},
				},
			}, "newer error: server is overloaded", false,
		},
		{
			"model_before_global", []*Auth{
				{
					ID: "codex-auth-1", Provider: "codex", Unavailable: true,
					Status: StatusError, StatusMessage: "global error: unrelated failure", UpdatedAt: now,
					ModelStates: map[string]*ModelState{
						model: {
							Status: StatusError, Unavailable: true,
							StatusMessage: "model error: model rate limited", UpdatedAt: older,
						},
					},
				},
			}, "model error: model rate limited", false,
		},
		{
			"global_without_model", []*Auth{
				{
					ID: "codex-auth-1", Provider: "codex", Unavailable: true,
					Status: StatusError, StatusMessage: "global error: credential exhausted", UpdatedAt: now,
				},
			}, "global error: credential exhausted", false,
		},
		{
			"cross_candidate_model", []*Auth{
				{
					ID: "codex-auth-A", Provider: "codex", Unavailable: true, Status: StatusError,
					StatusMessage: "older global status A", UpdatedAt: older,
					ModelStates: map[string]*ModelState{
						model: {
							Status: StatusError, Unavailable: true,
							StatusMessage: "model error on candidate A: quota exceeded", UpdatedAt: older,
						},
					},
				},
				{
					ID: "codex-auth-B", Provider: "codex", Unavailable: true, Status: StatusError,
					StatusMessage: "global auth error on candidate B: connection reset", UpdatedAt: newer,
				},
			}, "model error on candidate A: quota exceeded", false,
		},
		{
			"mixed_shards", []*Auth{
				{
					ID: "auth-codex", Provider: "codex", Unavailable: true, Status: StatusError,
					StatusMessage: "older codex global status", UpdatedAt: older,
					ModelStates: map[string]*ModelState{
						model: {
							Status: StatusError, Unavailable: true,
							StatusMessage: "codex model error: rate limited", UpdatedAt: older,
						},
					},
				},
				{
					ID: "auth-gemini", Provider: "gemini", Unavailable: true, Status: StatusError,
					StatusMessage: "gemini global error: network timeout", UpdatedAt: newer,
				},
			}, "codex model error: rate limited", false,
		},
	} {
		t.Run(
			sample.name, func(t *testing.T) {
				manager := NewManager(nil, &RoundRobinSelector{}, nil)
				manager.SetHomeClientProvider(func() HomeClient { return nil })
				providers := []string{"codex"}
				if sample.name == "mixed_shards" {
					providers = append(providers, "gemini")
				}
				for _, provider := range providers {
					manager.RegisterExecutor(&requestScopedTestExecutor{provider: provider})
				}
				for _, auth := range sample.auths {
					registerProjectionTestClient(t, auth.ID, auth.Provider, &registry.ModelInfo{ID: model})
					if _, err := manager.Register(WithSkipPersist(t.Context()), auth); err != nil {
						t.Fatal(err)
					}
				}
				var err error
				if len(providers) > 1 {
					_, _, _, err = manager.pickNextMixed(t.Context(), providers, model, core.Options{}, nil)
				} else {
					_, _, err = manager.PickNext(t.Context(), providers[0], model, core.Options{}, nil)
				}
				cause := errors.Unwrap(err)
				var upstream *Error
				message := ""
				if errors.As(cause, &upstream) {
					message = upstream.Message
				} else if cause != nil {
					message = cause.Error()
				}
				if cause == nil || message != sample.want {
					t.Fatalf("selection cause=%v error=%v, want %q", cause, err, sample.want)
				}
				if sample.cooling &&
					(upstream == nil || upstream.Code != "auth_unavailable" || upstream.HTTPStatus != 503) {
					t.Fatalf("upstream error identity=%+v", upstream)
				}
				if strings.Contains(err.Error(), sample.want) {
					t.Fatal("selection display exposed the original upstream error body")
				}
				if sample.cooling {
					var cooldown *modelCooldownError
					if !errors.As(
						err, &cooldown,
					) || cooldown.model != model || cooldown.resetIn <= 0 || cooldown.resetIn > time.Minute {
						t.Fatalf("model cooldown=%+v error=%v", cooldown, err)
					}
				}
			},
		)
	}
}

type selectionCooldownSliceError []string

func (err selectionCooldownSliceError) Error() string { return strings.Join(err, ", ") }

// noinspection GoDirectComparisonOfErrors,GoTypeAssertionOnErrors
func TestOriginalSelectionCooldownErrorConsumers(t *testing.T) {
	t.Run(
		"retry_after", func(t *testing.T) {
			err := NewModelCooldownError("gpt-5.6-sol", "codex", 15*time.Second)
			if headers := SafeResponseHeaders(err); headers.Get("Retry-After") != "15" {
				t.Fatalf("retry headers=%v", headers)
			}
		},
	)
	t.Run(
		"non_comparable_cause", func(t *testing.T) {
			cause, target := selectionCooldownSliceError{"err1", "err2"}, selectionCooldownSliceError{"err1", "err2"}
			base := &Error{Code: "auth_unavailable", Message: "no auth available"}
			wrapped := WithCause(base, cause)
			if errors.Is(wrapped, target) || !errors.Is(wrapped, base) {
				t.Fatal("non-comparable error comparison changed the base error identity")
			}
			if original, ok := errors.Unwrap(wrapped).(selectionCooldownSliceError); !ok || strings.Join(
				original, ",",
			) != "err1,err2" {
				t.Fatalf("original cause=%v", errors.Unwrap(wrapped))
			}
		},
	)
	t.Run(
		"display_and_original_cause", func(t *testing.T) {
			body := `{"type":"error","code":"server_is_overloaded","message":"Our servers a` +
				`re currently overloaded. Please try again later.","sequence_number":0}`
			cause := errors.New(body)
			wrapped := WithCause(&Error{Code: "auth_unavailable", Message: "no auth available"}, cause)
			if !errors.Is(wrapped, cause) || errors.Unwrap(wrapped) != cause {
				t.Fatal("original upstream cause changed")
			}
			if text := wrapped.Error(); !strings.Contains(
				text, "auth_unavailable: no auth available",
			) || strings.Contains(text, "Our servers are currently overloaded") {
				t.Fatalf("error display=%q", text)
			}
		},
	)
}
