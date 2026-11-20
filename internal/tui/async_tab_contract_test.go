package tui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
)

func TestUsageReplySurvivesTabSwitch(t *testing.T) {
	for _, switchAway := range []bool{false, true} {
		t.Run(
			fmt.Sprintf("switch_away=%t", switchAway), func(t *testing.T) {
				server := httptest.NewServer(
					http.HandlerFunc(
						func(w http.ResponseWriter, r *http.Request) {
							if r.Method != http.MethodGet || r.URL.Path != "/v0/management/usage" {
								t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
								http.Error(w, "unexpected request", http.StatusNotFound)
								return
							}
							_, _ = w.Write([]byte(`{"usage":{"total_requests":17,"total_tokens":42}}`))
						},
					),
				)
				defer server.Close()
				client, err := NewClient(0, "fixture-management", config.TLSConfig{})
				if err != nil {
					t.Fatal(err)
				}
				client.baseURL, client.http = server.URL, server.Client()
				root := newApp(client, "", nil)
				root.authenticated = true
				root.activeTab = tabOAuth
				root.Update(tea.WindowSizeMsg{Width: 120, Height: 32})
				_, fetch := root.Update(tea.KeyPressMsg{Code: tea.KeyTab})
				if fetch == nil || root.activeTab != tabUsage {
					t.Fatal("entering Usage did not request its initial data")
				}
				if switchAway {
					root.Update(tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
				}
				root.Update(fetch())
				if switchAway {
					root.Update(tea.KeyPressMsg{Code: tea.KeyTab})
				}
				if root.activeTab != tabUsage || root.usage.err != nil || root.usage.usage == nil || !strings.Contains(
					root.usage.renderContent(), "17",
				) {
					t.Fatalf(
						"Usage reply lost: active=%d usage=%v error=%v", root.activeTab, root.usage.usage,
						root.usage.err,
					)
				}
			},
		)
	}
}

func TestKeysTabFetchesAndRendersEverySupportedProvider(t *testing.T) {
	providers := []string{
		"gemini-api-key", "claude-api-key", "codex-api-key", "vertex-api-key", "interactions-api-key", "xai-api-key",
		"openai-compatibility",
	}
	for _, failed := range append([]string{""}, providers...) {
		t.Run(
			"failed="+failed, func(t *testing.T) {
				requested := make(map[string]int)
				var requestsMu sync.Mutex
				server := httptest.NewServer(
					http.HandlerFunc(
						func(w http.ResponseWriter, r *http.Request) {
							name := strings.TrimPrefix(r.URL.Path, "/v0/management/")
							requestsMu.Lock()
							requested[name]++
							requestsMu.Unlock()
							if name == failed {
								http.Error(w, "fixture provider unavailable", http.StatusServiceUnavailable)
								return
							}
							body := map[string]any{
								name: []map[string]any{
									{
										"api-key": "fixture-private-" + name, "name": "fixture-compat",
									},
								},
							}
							if name == "api-keys" {
								body[name] = []string{"fixture-access-key"}
							}
							if err := json.NewEncoder(w).Encode(body); err != nil {
								t.Errorf("write fixture: %v", err)
							}
						},
					),
				)
				defer server.Close()
				client, err := NewClient(0, "fixture-management", config.TLSConfig{})
				if err != nil {
					t.Fatal(err)
				}
				client.baseURL, client.http = server.URL, server.Client()
				model := newKeysTabModel(client)
				model.setSize(120, 32)
				model, _ = model.update(model.init()())
				view := model.renderContent()
				if failed != "" {
					if model.err == nil || !strings.Contains(view, "fixture provider unavailable") {
						t.Fatalf(
							"provider request failure hidden: provider=%s error=%v view=%s", failed, model.err, view,
						)
					}
					return
				}
				if model.err != nil {
					t.Fatalf("fetch providers: %v", model.err)
				}
				requestsMu.Lock()
				defer requestsMu.Unlock()
				for _, name := range append([]string{"api-keys"}, providers...) {
					if requested[name] != 1 {
						t.Errorf("requests for %s=%d, want 1", name, requested[name])
					}
					if strings.Contains(view, "fixture-private-"+name) {
						t.Errorf("unmasked provider key: %s", name)
					}
				}
				if len(model.xai) != 1 || getString(model.xai[0], "api-key") != "fixture-private-xai-api-key" {
					t.Fatalf("xAI response was not consumed: %#v", model.xai)
				}
				if offset := strings.Index(view, "xAI API Keys"); offset < 0 || !strings.Contains(
					view[offset:], maskKey("fixture-private-xai-api-key"),
				) {
					t.Errorf("xAI section missing its masked key: %s", view)
				}
				for _, title := range []string{
					"Gemini API Keys", "Claude API Keys", "Codex API Keys", "Vertex API Keys", "Interactions API Keys",
					"xAI API Keys", "OpenAI Compatibility",
				} {
					if !strings.Contains(view, title) {
						t.Errorf("missing provider section %s", title)
					}
				}
			},
		)
	}
}
