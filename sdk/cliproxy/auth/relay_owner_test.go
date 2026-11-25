package auth

import (
	"testing"

	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

func TestSchedulerSelectsOwnedRelayOnlyForItsClient(t *testing.T) {
	const relayID = "test-private-relay"
	scheduler := newSchedulerForTest(
		&RoundRobinSelector{}, &Auth{
			ID: relayID, Provider: "aistudio", RelayOwner: "client-a",
		},
	)
	opts := cliproxyexecutor.Options{}
	owner := coreusage.WithAPIKeyIdentity(t.Context(), "client-a")
	other := coreusage.WithAPIKeyIdentity(t.Context(), "client-b")
	for _, ctx := range []struct {
		identity string
		want     bool
	}{
		{"client-a", true},
		{"client-b", false},
		{"", false},
	} {
		request := coreusage.WithAPIKeyIdentity(t.Context(), ctx.identity)
		auth, err := scheduler.pickSingle(request, "aistudio", "", opts, nil)
		if ctx.want {
			if err != nil || auth == nil || auth.ID != relayID {
				t.Fatalf("owner selection: auth=%v err=%v", auth, err)
			}
		} else if err == nil || auth != nil {
			t.Fatal("non-owner obtained a relay from single-provider selection")
		}
	}
	providers := []string{"gemini", "aistudio"}
	if auth, _, err := scheduler.pickMixed(other, providers, "", opts, nil); err == nil || auth != nil {
		t.Fatal("non-owner obtained a relay from mixed-provider selection")
	}
	if auth, _, err := scheduler.pickMixed(
		owner, providers, "", opts, nil,
	); err != nil || auth == nil || auth.ID != relayID {
		t.Fatalf("owner mixed selection: auth=%v err=%v", auth, err)
	}
}
