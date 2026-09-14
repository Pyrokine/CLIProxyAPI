package auth

import (
	"context"
	"testing"
	"time"

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
