package tui

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestAuthTabUppercaseRefreshesCredentialsAndLowercaseReloads(t *testing.T) {
	var posts atomic.Int32
	var gets atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v0/management/auth-files/refresh":
			posts.Add(1)
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && r.URL.Path == "/v0/management/auth-files":
			gets.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"files":[]}`))
		default:
			t.Fatalf("unexpected request = %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	client := &Client{baseURL: server.URL, http: server.Client()}
	model := newAuthTabModel(client)
	_, refreshCmd := model.handleNormalInput(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'R'}})
	if refreshCmd == nil {
		t.Fatal("uppercase R returned no command")
	}
	refreshMsg, ok := refreshCmd().(authActionMsg)
	if !ok || refreshMsg.err != nil {
		t.Fatalf("uppercase R result = %#v", refreshMsg)
	}
	if posts.Load() != 1 || gets.Load() != 0 {
		t.Fatalf("after uppercase R posts=%d gets=%d", posts.Load(), gets.Load())
	}

	_, reloadCmd := model.handleNormalInput(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	if reloadCmd == nil {
		t.Fatal("lowercase r returned no command")
	}
	reloadMsg, ok := reloadCmd().(authFilesMsg)
	if !ok || reloadMsg.err != nil {
		t.Fatalf("lowercase r result = %#v", reloadMsg)
	}
	if posts.Load() != 1 || gets.Load() != 1 {
		t.Fatalf("after lowercase r posts=%d gets=%d", posts.Load(), gets.Load())
	}
}
