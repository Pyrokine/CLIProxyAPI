package auth

import (
	"testing"
	"time"

	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestDevinRefreshLeadRegistrationRemainsNil(t *testing.T) {
	if lead := NewDevinAuthenticator().RefreshLead(); lead != nil {
		t.Fatalf("Devin authenticator refresh lead = %v, want nil", lead)
	}
	if lead := coreauth.ProviderRefreshLead("devin", nil); lead != nil {
		t.Fatalf("registered Devin refresh lead = %v, want nil", lead)
	}
}

func TestAntigravityRefreshLeadRegistration(t *testing.T) {
	for _, lead := range []*time.Duration{
		NewAntigravityAuthenticator().RefreshLead(),
		coreauth.ProviderRefreshLead("antigravity", nil),
	} {
		if lead == nil || *lead != 50*time.Minute {
			t.Fatalf("Antigravity refresh lead=%v, want 50m", lead)
		}
		*lead = time.Second
	}
	lead := coreauth.ProviderRefreshLead("antigravity", nil)
	if lead == nil || *lead != 50*time.Minute {
		t.Fatalf("registered refresh lead aliases returned value: %v", lead)
	}
}
