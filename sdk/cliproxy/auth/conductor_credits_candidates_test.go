package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestFindAllAntigravityCreditsCandidateAuths_PrefersKnownCreditsThenUnknown(t *testing.T) {
	m := &Manager{
		auths: map[string]*Auth{
			"zz-credits": {ID: "zz-credits", Provider: "antigravity"},
			"aa-unknown": {ID: "aa-unknown", Provider: "antigravity"},
			"mm-no":      {ID: "mm-no", Provider: "antigravity"},
		},
		executors: map[string]ProviderExecutor{
			"antigravity": schedulerTestExecutor{},
		},
	}

	SetAntigravityCreditsHint(
		"zz-credits", AntigravityCreditsHint{
			Known:     true,
			Available: true,
			UpdatedAt: time.Now(),
		},
	)
	SetAntigravityCreditsHint(
		"mm-no", AntigravityCreditsHint{
			Known:     true,
			Available: false,
			UpdatedAt: time.Now(),
		},
	)

	opts := cliproxyexecutor.Options{}

	candidates := m.findAllAntigravityCreditsCandidateAuths("claude-sonnet-4-6", opts)
	if len(candidates) != 2 {
		t.Fatalf("candidates len = %d, want 2", len(candidates))
	}
	if candidates[0].auth.ID != "zz-credits" {
		t.Fatalf("candidates[0].auth.ID = %q, want %q", candidates[0].auth.ID, "zz-credits")
	}
	if candidates[1].auth.ID != "aa-unknown" {
		t.Fatalf("candidates[1].auth.ID = %q, want %q", candidates[1].auth.ID, "aa-unknown")
	}

	nonClaude := m.findAllAntigravityCreditsCandidateAuths("gemini-3-flash", opts)
	if len(nonClaude) != 0 {
		t.Fatalf("nonClaude len = %d, want 0", len(nonClaude))
	}

	pinnedOpts := cliproxyexecutor.Options{
		Metadata: map[string]any{cliproxyexecutor.PinnedAuthMetadataKey: "aa-unknown"},
	}
	pinned := m.findAllAntigravityCreditsCandidateAuths("claude-sonnet-4-6", pinnedOpts)
	if len(pinned) != 1 {
		t.Fatalf("pinned len = %d, want 1", len(pinned))
	}
	if pinned[0].auth.ID != "aa-unknown" {
		t.Fatalf("pinned[0].auth.ID = %q, want %q", pinned[0].auth.ID, "aa-unknown")
	}
}

func TestAntigravityCreditsContext1MSkipsLowContextCredential(t *testing.T) {
	const (
		model      = "claude-context-credits-model"
		lowAuthID  = "credits-context-low"
		highAuthID = "credits-context-high"
	)
	tests := []struct {
		name   string
		stream bool
	}{
		{name: "execute"},
		{name: "stream", stream: true},
	}
	for _, testCase := range tests {
		t.Run(
			testCase.name, func(t *testing.T) {
				manager := NewManager(nil, &RoundRobinSelector{}, nil)
				executor := &context1MTrackingExecutor{
					id: "antigravity",
				}
				manager.RegisterExecutor(executor)
				registerContext1MTestAuthForModel(t, manager, lowAuthID, "antigravity", model, 200_000)
				registerContext1MTestAuthForModel(
					t, manager, highAuthID, "antigravity", model, context1MMinimumTokens,
				)
				SetAntigravityCreditsHint(
					lowAuthID, AntigravityCreditsHint{
						Known: true, Available: true, UpdatedAt: time.Now(),
					},
				)

				request := cliproxyexecutor.Request{Model: model + "[1m]"}
				if testCase.stream {
					_, err, ok := manager.tryAntigravityCreditsExecuteStream(
						context.Background(), request, cliproxyexecutor.Options{},
					)
					if err != nil || !ok {
						t.Fatalf("tryAntigravityCreditsExecuteStream = (%v, %t), want success", err, ok)
					}
				} else {
					_, err, ok := manager.tryAntigravityCreditsExecute(
						context.Background(), request, cliproxyexecutor.Options{},
					)
					if err != nil || !ok {
						t.Fatalf("tryAntigravityCreditsExecute = (%v, %t), want success", err, ok)
					}
				}
				if got := executor.executedAuthIDs(); len(got) != 1 || got[0] != highAuthID {
					t.Fatalf("upstream executions = %v, want [%s]", got, highAuthID)
				}
			},
		)
	}
}

type creditsContextTransportProvider struct{ http.RoundTripper }

func (p creditsContextTransportProvider) RoundTripperFor(*Auth) http.RoundTripper {
	return p.RoundTripper
}

func TestPreparedCreditsAndHomeContextsUseConsumerTransportKey(t *testing.T) {
	transport := &http.Transport{}
	manager := NewManager(nil, nil, nil)
	manager.SetRoundTripperProvider(creditsContextTransportProvider{transport})
	auth := &Auth{ID: "prepared-transport-context", Provider: "test", Status: StatusActive}
	t.Run("credits", func(t *testing.T) {
		ctx, _, _ := manager.prepareAntigravityCreditsExecution(context.Background(), auth, "test-model", cliproxyexecutor.Options{})
		if util.RoundTripperValue(ctx) != transport {
			t.Fatal("credits context did not publish the consumer transport key")
		}
	})
	t.Run("home", func(t *testing.T) {
		attempt, err := manager.prepareHomeAttempt(context.Background(), &HomeDispatchSelection{Provider: "test", Executor: schedulerTestExecutor{}}, auth,
			cliproxyexecutor.Request{Model: "test-model"}, cliproxyexecutor.Options{}, "test-model")
		if err != nil {
			t.Fatal(err)
		}
		if util.RoundTripperValue(attempt.ctx) != transport {
			t.Fatal("Home context did not publish the consumer transport key")
		}
	})
}

func TestAntigravityCreditsPublicationFollowsCredentialLifecycle(t *testing.T) {
	for _, test := range []struct {
		name     string
		accepted bool
	}{
		{name: "unchanged", accepted: true},
		{name: "label-update", accepted: true},
		{name: "token-update"},
		{name: "disabled"},
		{name: "removed"},
		{name: "recreated"},
		{name: "forgotten-and-recreated"},
		{name: "loaded-replacement"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			store := &credentialWeightValidationStore{}
			manager := NewManager(store, nil, nil)
			manager.SetHomeClientProvider(func() HomeClient { return nil })
			auth, err := manager.Register(ctx, &Auth{ID: t.Name(), Provider: "antigravity", Metadata: map[string]any{"access_token": "synthetic-access"}})
			if err != nil {
				t.Fatal(err)
			}
			defer antigravityCreditsHintByAuth.Delete(auth.ID)
			if !auth.PublishAntigravityCreditsHint(AntigravityCreditsHint{Known: true, Available: true, CreditAmount: 1500}) {
				t.Fatal("current credential publication was rejected")
			}
			switch test.name {
			case "label-update", "token-update", "disabled":
				updated := auth.Clone()
				if test.name == "label-update" {
					updated.Label = "renamed"
				}
				if test.name == "token-update" {
					updated.Metadata["access_token"] = "replacement-access"
				}
				if test.name == "disabled" {
					updated.Disabled = true
				}
				if _, err := manager.Update(ctx, updated); err != nil {
					t.Fatal(err)
				}
			case "removed", "recreated", "forgotten-and-recreated":
				if err := manager.Remove(ctx, auth.ID); err != nil {
					t.Fatal(err)
				}
				if test.name == "forgotten-and-recreated" {
					manager.ForgetRemovedRuntimeAuth(auth.ID)
				}
				if test.name != "removed" {
					recreated, err := manager.Register(WithAllowAuthRecreate(ctx), auth.Clone())
					if err != nil {
						t.Fatal(err)
					}
					current, exists := manager.GetByID(auth.ID)
					if recreated == nil || !exists || current.Generation == auth.Generation || current.Generation != recreated.Generation {
						t.Fatal("removed credential was not recreated with a new published generation")
					}
				}
			case "loaded-replacement":
				replacement := auth.Clone()
				replacement.Metadata["access_token"] = "replacement-access"
				store.auths = []*Auth{replacement}
				if err := manager.Load(ctx); err != nil {
					t.Fatal(err)
				}
				if hint, ok := GetAntigravityCreditsHint(auth.ID); ok {
					t.Fatalf("load retained an old credential balance: %+v", hint)
				}
			}
			accepted := auth.PublishAntigravityCreditsHint(AntigravityCreditsHint{Known: true, CreditAmount: 10})
			if accepted != test.accepted {
				t.Fatalf("old publication accepted=%t, want %t", accepted, test.accepted)
			}
		})
	}
}

func TestAntigravityCreditsPublicationRejectsOlderObservation(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	manager.SetHomeClientProvider(func() HomeClient { return nil })
	auth, err := manager.Register(context.Background(), &Auth{ID: t.Name(), Provider: "antigravity"})
	if err != nil {
		t.Fatal(err)
	}
	defer antigravityCreditsHintByAuth.Delete(auth.ID)
	now := time.Now()
	if !auth.PublishAntigravityCreditsHint(AntigravityCreditsHint{Known: true, Available: true, CreditAmount: 1500, UpdatedAt: now}) {
		t.Fatal("latest observation was rejected")
	}
	if auth.PublishAntigravityCreditsHint(AntigravityCreditsHint{Known: true, CreditAmount: 10, UpdatedAt: now.Add(-time.Second)}) {
		t.Fatal("older observation was accepted")
	}
	if hint, ok := GetAntigravityCreditsHint(auth.ID); !ok || hint.CreditAmount != 1500 {
		t.Fatalf("latest observation changed: %+v, present=%t", hint, ok)
	}
}

func TestAntigravityCreditsPublicationPreservesExplicitUnownedHint(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	manager.SetHomeClientProvider(func() HomeClient { return nil })
	SetAntigravityCreditsHint(t.Name(), AntigravityCreditsHint{Known: true, Available: true, CreditAmount: 1500})
	defer antigravityCreditsHintByAuth.Delete(t.Name())
	if _, err := manager.Register(context.Background(), &Auth{ID: t.Name(), Provider: "antigravity"}); err != nil {
		t.Fatal(err)
	}
	if hint, ok := GetAntigravityCreditsHint(t.Name()); !ok || hint.CreditAmount != 1500 {
		t.Fatalf("explicit pre-registration hint changed: %+v, present=%t", hint, ok)
	}
}
