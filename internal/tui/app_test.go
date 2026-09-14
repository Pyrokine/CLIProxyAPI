package tui

import (
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/charmbracelet/bubbles/viewport"
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
	root.oauth.viewport = viewport.New(80, 24)

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
