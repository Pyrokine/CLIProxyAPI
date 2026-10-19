package tui

import (
	"errors"
	"testing"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
)

func TestShouldAcceptOAuthPollFiltersStaleMessages(t *testing.T) {
	msg := oauthPollMsg{state: "state-a", generation: 1, done: true, message: "ok"}

	if shouldAcceptOAuthPoll(msg, "state-a", 2, oauthRemote) {
		t.Fatal("accepted poll with stale generation")
	}
	if shouldAcceptOAuthPoll(msg, "state-b", 1, oauthRemote) {
		t.Fatal("accepted poll with mismatched state")
	}
	if shouldAcceptOAuthPoll(msg, "state-a", 1, oauthIdle) {
		t.Fatal("accepted poll while not in remote state")
	}
	if !shouldAcceptOAuthPoll(msg, "state-a", 1, oauthRemote) {
		t.Fatal("rejected valid poll message")
	}
}

func TestShouldAcceptOAuthStartFiltersStaleMessages(t *testing.T) {
	msg := oauthStartMsg{state: "state-a", generation: 1, url: "https://example.com"}
	if shouldAcceptOAuthStart(msg, 2) {
		t.Fatal("accepted start with stale generation")
	}
	if !shouldAcceptOAuthStart(msg, 1) {
		t.Fatal("rejected valid start message")
	}
}

func TestShouldFailOAuthStatusPoll(t *testing.T) {
	if shouldFailOAuthStatusPoll(4, 5) {
		t.Fatal("failed too early on transient errors")
	}
	if !shouldFailOAuthStatusPoll(5, 5) {
		t.Fatal("did not fail after max consecutive errors")
	}
	if !shouldFailOAuthStatusPoll(1, 0) {
		t.Fatal("maxErrors<=0 should fail on first error")
	}
}

func TestOAuthTabUpdateIgnoresStalePollMsg(t *testing.T) {
	m := newOAuthTabModel(nil)
	m.state = oauthRemote
	m.authState = "state-current"
	m.pollGeneration = 2
	m.ready = true
	m.viewport = viewport.New(80, 24)
	m.viewport.SetContent(m.renderContent())

	updated, cmd := m.update(
		oauthPollMsg{
			state:      "state-old",
			generation: 1,
			done:       true,
			message:    "should be ignored",
		},
	)
	if cmd != nil {
		t.Fatal("expected no command for stale poll")
	}
	if updated.state != oauthRemote {
		t.Fatalf("state = %v, want oauthRemote", updated.state)
	}
	if updated.message != "" {
		t.Fatalf("message changed by stale poll: %q", updated.message)
	}
}

func TestOAuthTabUpdateAcceptsCurrentPollMsg(t *testing.T) {
	m := newOAuthTabModel(nil)
	m.state = oauthRemote
	m.authState = "state-current"
	m.pollGeneration = 3
	m.ready = true
	m.viewport = viewport.New(80, 24)
	m.viewport.SetContent(m.renderContent())

	updated, _ := m.update(
		oauthPollMsg{
			state:      "state-current",
			generation: 3,
			done:       true,
			message:    "Authentication successful",
		},
	)
	if updated.state != oauthSuccess {
		t.Fatalf("state = %v, want oauthSuccess", updated.state)
	}
}

func TestOAuthTabEscRemoteWaitsForCancellationResult(t *testing.T) {
	m := newOAuthTabModel(nil)
	m.state = oauthRemote
	m.authState = "state-to-cancel"
	m.authURL = "https://example.com"
	m.deviceFlow = true
	m.pollGeneration = 4
	m.ready = true
	m.viewport = viewport.New(80, 24)
	m.viewport.SetContent(m.renderContent())

	updated, cmd := m.update(tea.KeyMsg{Type: tea.KeyEsc})
	if updated.state != oauthCancelling {
		t.Fatalf("state = %v, want oauthCancelling", updated.state)
	}
	if updated.pollGeneration != 5 {
		t.Fatalf("pollGeneration = %d, want 5", updated.pollGeneration)
	}
	if updated.authState != "state-to-cancel" || updated.authURL != "https://example.com" || !updated.deviceFlow {
		t.Fatalf(
			"remote fields changed before cancellation confirmation: state=%q url=%q device=%v",
			updated.authState,
			updated.authURL,
			updated.deviceFlow,
		)
	}
	if cmd == nil {
		t.Fatal("expected cancellation command")
	}
}

func TestOAuthTabEscWithActiveCallbackInputPreservesFlowUntilCancelled(t *testing.T) {
	m := newOAuthTabModel(nil)
	m.state = oauthRemote
	m.authState = "state-to-cancel"
	m.authURL = "https://example.com"
	m.deviceFlow = false
	m.inputActive = true
	m.callbackInput.Focus()
	m.callbackInput.SetValue("https://callback.example/?code=abc&state=state-to-cancel")
	m.pollGeneration = 7
	m.ready = true
	m.viewport = viewport.New(80, 24)
	m.viewport.SetContent(m.renderContent())

	updated, cmd := m.update(tea.KeyMsg{Type: tea.KeyEsc})
	if updated.state != oauthCancelling {
		t.Fatalf("state = %v, want oauthCancelling", updated.state)
	}
	if updated.pollGeneration != 8 {
		t.Fatalf("pollGeneration = %d, want 8", updated.pollGeneration)
	}
	if updated.inputActive {
		t.Fatal("inputActive still true while cancelling")
	}
	if updated.callbackInput.Value() == "" {
		t.Fatal("callback input cleared before cancellation confirmation")
	}
	if updated.authState != "state-to-cancel" || updated.authURL != "https://example.com" {
		t.Fatalf("remote fields changed: state=%q url=%q", updated.authState, updated.authURL)
	}
	if cmd == nil {
		t.Fatal("expected cancellation command")
	}
}

func TestOAuthTabStaleStartIsIgnored(t *testing.T) {
	openCalls := 0
	previousOpen := openOAuthBrowser
	openOAuthBrowser = func(string) error {
		openCalls++
		return nil
	}
	t.Cleanup(func() { openOAuthBrowser = previousOpen })

	m := newOAuthTabModel(nil)
	m.state = oauthIdle
	m.pollGeneration = 2
	m.ready = true
	m.viewport = viewport.New(80, 24)
	m.viewport.SetContent(m.renderContent())

	updated, cmd := m.update(
		oauthStartMsg{
			url:        "https://example.com",
			state:      "stale-state",
			generation: 1,
		},
	)
	if updated.state != oauthIdle {
		t.Fatalf("state = %v, want oauthIdle after stale start", updated.state)
	}
	if cmd == nil {
		t.Fatal("expected stale server session cancellation command")
	}
	if updated.authState != "" {
		t.Fatalf("stale start should not set authState, got %q", updated.authState)
	}
	if openCalls != 0 {
		t.Fatalf("stale start opened browser %d times", openCalls)
	}
}

func TestOAuthTabCurrentStartOpensBrowserAfterGenerationCheck(t *testing.T) {
	openCalls := 0
	previousOpen := openOAuthBrowser
	openOAuthBrowser = func(rawURL string) error {
		openCalls++
		if rawURL != "https://example.com" {
			t.Fatalf("browser URL = %q", rawURL)
		}
		return nil
	}
	t.Cleanup(func() { openOAuthBrowser = previousOpen })

	m := newOAuthTabModel(nil)
	m.state = oauthPending
	m.pollGeneration = 3
	m.ready = true
	m.viewport = viewport.New(80, 24)

	updated, cmd := m.update(
		oauthStartMsg{
			url:          "https://example.com",
			state:        "current-state",
			providerName: "Gemini CLI",
			providerKey:  "gemini",
			deviceFlow:   true,
			expiresIn:    60,
			generation:   3,
		},
	)
	if openCalls != 1 {
		t.Fatalf("browser opened %d times, want 1", openCalls)
	}
	if updated.state != oauthRemote || updated.authState != "current-state" || updated.providerKey != "gemini" {
		t.Fatalf("updated OAuth state = %#v", updated)
	}
	if cmd == nil {
		t.Fatal("expected status polling command")
	}
}

func TestOAuthTabCancellationConfirmationClearsRemoteState(t *testing.T) {
	m := newOAuthTabModel(nil)
	m.state = oauthCancelling
	m.authState = "state-to-cancel"
	m.authURL = "https://example.com"
	m.providerKey = "xai"
	m.deviceFlow = true
	m.pollGeneration = 5
	m.ready = true
	m.viewport = viewport.New(80, 24)

	updated, cmd := m.update(
		oauthCancelMsg{
			state: "state-to-cancel", generation: 5, cancelled: true,
		},
	)
	if cmd != nil {
		t.Fatal("successful cancellation returned a command")
	}
	if updated.state != oauthIdle {
		t.Fatalf("state = %v, want oauthIdle", updated.state)
	}
	if updated.authState != "" || updated.authURL != "" || updated.providerKey != "" || updated.deviceFlow {
		t.Fatalf("remote state was not cleared: %#v", updated)
	}
}

func TestOAuthTabCancellationRejectionResumesPolling(t *testing.T) {
	m := newOAuthTabModel(nil)
	m.state = oauthCancelling
	m.authState = "saving-state"
	m.authURL = "https://example.com"
	m.providerKey = "xai"
	m.deviceFlow = true
	m.expiresIn = 60
	m.pollGeneration = 6
	m.ready = true
	m.viewport = viewport.New(80, 24)

	updated, cmd := m.update(oauthCancelMsg{state: "saving-state", generation: 6})
	if updated.state != oauthRemote {
		t.Fatalf("state = %v, want oauthRemote", updated.state)
	}
	if updated.authState != "saving-state" || updated.authURL != "https://example.com" {
		t.Fatalf("remote state changed after rejected cancellation: %#v", updated)
	}
	if cmd == nil {
		t.Fatal("expected status polling to resume")
	}
}

func TestOAuthTabPollFailureCancelsBeforeShowingTerminalError(t *testing.T) {
	flowErr := errors.New("status polling timed out")
	m := newOAuthTabModel(nil)
	m.state = oauthRemote
	m.authState = "state-to-cancel"
	m.pollGeneration = 7
	m.ready = true
	m.viewport = viewport.New(80, 24)

	cancelling, cancelCmd := m.update(
		oauthPollMsg{
			state: "state-to-cancel", generation: 7, cancelSession: true, err: flowErr,
		},
	)
	if cancelling.state != oauthCancelling {
		t.Fatalf("poll failure state = %v, want %v", cancelling.state, oauthCancelling)
	}
	if cancelCmd == nil {
		t.Fatal("poll failure did not return a cancellation command")
	}

	updated, cmd := cancelling.update(
		oauthCancelMsg{
			state: "state-to-cancel", generation: 7, cancelled: true, flowErr: flowErr,
		},
	)
	if cmd != nil {
		t.Fatal("confirmed failure cancellation returned a command")
	}
	if updated.state != oauthError || !errors.Is(updated.err, flowErr) {
		t.Fatalf("state = %v err=%v", updated.state, updated.err)
	}
	if updated.authState != "" {
		t.Fatalf("authState = %q, want cleared", updated.authState)
	}
}

func TestOAuthTabIgnoresStaleCallbackSubmitResult(t *testing.T) {
	m := newOAuthTabModel(nil)
	m.state = oauthRemote
	m.authState = "current-state"
	m.pollGeneration = 9
	m.message = "unchanged"
	m.ready = true
	m.viewport = viewport.New(80, 24)

	updated, cmd := m.update(
		oauthCallbackSubmitMsg{
			state: "old-state", generation: 8, err: errors.New("stale error"),
		},
	)
	if cmd != nil {
		t.Fatal("stale callback result returned a command")
	}
	if updated.message != "unchanged" || updated.state != oauthRemote {
		t.Fatalf("stale callback changed model: %#v", updated)
	}
}

func TestOAuthProvidersHaveCanonicalCallbackKeys(t *testing.T) {
	providers := make(map[string]oauthProvider, len(oauthProviders))
	for _, provider := range oauthProviders {
		providers[provider.name] = provider
	}
	for _, tc := range []struct {
		name, key, path string
		device          bool
	}{
		{name: "Gemini CLI", key: "gemini", path: "gemini-cli-auth-url"},
		{name: "IFlow", key: "iflow", path: "iflow-auth-url"},
		{name: "Kimi (kimi.com)", key: "kimi", path: "kimi-auth-url", device: true},
		{name: "Kimi (kimi.ai)", key: "kimi-ai", path: "kimi-ai-auth-url", device: true},
		{name: "Meta", key: "meta", path: "meta-auth-url", device: true},
	} {
		provider, ok := providers[tc.name]
		if !ok || provider.key != tc.key || provider.apiPath != tc.path || provider.deviceFlow != tc.device {
			t.Fatalf("provider %q = %+v, want key=%q path=%q device=%t", tc.name, provider, tc.key, tc.path, tc.device)
		}
	}
}
