package tui

import (
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"charm.land/bubbles/v2/viewport"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
)

func TestAppRoutesOAuthAsyncMessagesWhileAnotherTabIsActive(t *testing.T) {
	client, errClient := NewClient(0, "", config.TLSConfig{})
	if errClient != nil {
		t.Fatalf("NewClient() error = %v", errClient)
	}
	root := newApp(client, "", nil)
	root.authenticated = true
	root.activeTab = tabDashboard
	root.oauth.state = oauthRemote
	root.oauth.authState = "state-1"
	root.oauth.pollGeneration = 3
	root.oauth.ready = true
	root.oauth.viewport = viewport.New(viewport.WithWidth(80), viewport.WithHeight(24))

	_, cmd := root.Update(
		oauthPollMsg{
			state: "state-1", generation: 3, done: true, message: "Authentication successful",
		},
	)
	if cmd != nil {
		t.Fatal("completed OAuth message returned a command")
	}
	if root.oauth.state != oauthSuccess {
		t.Fatalf("OAuth state = %v, want oauthSuccess", root.oauth.state)
	}
}

func TestAppRoutesInactiveTabResultsAndFollowupCommands(t *testing.T) {
	failure := errors.New("fixture asynchronous failure")
	for _, test := range []struct {
		name     string
		message  tea.Msg
		accepted func(*app) bool
		followup bool
	}{
		{
			"dashboard", dashboardDataMsg{err: failure},
			func(a *app) bool { return errors.Is(a.dashboard.err, failure) }, false,
		},
		{"config", configDataMsg{err: failure}, func(a *app) bool { return errors.Is(a.config.err, failure) }, false},
		{"credentials", authFilesMsg{err: failure}, func(a *app) bool { return errors.Is(a.auth.err, failure) }, false},
		{"keys", keysDataMsg{err: failure}, func(a *app) bool { return errors.Is(a.keys.err, failure) }, false},
		{"usage", usageDataMsg{err: failure}, func(a *app) bool { return errors.Is(a.usage.err, failure) }, false},
		{
			"credential action", authActionMsg{err: failure},
			func(a *app) bool { return strings.Contains(a.auth.status, failure.Error()) }, true,
		},
		{
			"key action", keyActionMsg{err: failure},
			func(a *app) bool { return strings.Contains(a.keys.status, failure.Error()) }, true,
		},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				root := newApp(nil, "", nil)
				root.authenticated = true
				root.activeTab = tabOAuth
				root.Update(tea.WindowSizeMsg{Width: 120, Height: 32})
				_, command := root.Update(test.message)
				if !test.accepted(&root) || (command != nil) != test.followup || root.activeTab != tabOAuth {
					t.Fatalf(
						"result delivery failed: accepted=%t command=%t active=%d", test.accepted(&root),
						command != nil, root.activeTab,
					)
				}
			},
		)
	}
}
