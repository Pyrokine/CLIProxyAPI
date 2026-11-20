package tui

import (
	"errors"
	"strings"
	"testing"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

var _ tea.Model = (*app)(nil)

func TestTUIV2ViewAndResizeContract(tb *testing.T) {
	root := newApp(&Client{baseURL: "http://127.0.0.1:8317"}, "fixture-management-password", nil)
	view := root.View()
	if !view.AltScreen || !strings.Contains(view.Content, t("auth_gate_title")) ||
		strings.Contains(view.Content, "fixture-management-password") {
		tb.Fatalf("authentication view lost alternate screen or password masking: %q", view.Content)
	}
	root.authenticated = true
	if view := root.View(); !view.AltScreen || view.Content != t("initializing_tui") {
		tb.Fatalf("initial view = %#v", view)
	}

	for _, size := range []tea.WindowSizeMsg{{Width: 120, Height: 32}, {Width: 80, Height: 24}} {
		root.Update(size)
		for i, control := range []*viewport.Model{
			&root.dashboard.viewport, &root.config.viewport, &root.auth.viewport, &root.keys.viewport,
			&root.oauth.viewport, &root.usage.viewport, &root.logs.viewport,
		} {
			if control.Width() != size.Width || control.Height() != size.Height-4 {
				tb.Fatalf(
					"tab %d viewport = %dx%d, want %dx%d", i, control.Width(), control.Height(),
					size.Width, size.Height-4,
				)
			}
		}
		if root.authInput.Width() != size.Width-6 || root.auth.editInput.Width() != size.Width-20 ||
			root.keys.editInput.Width() != size.Width-16 || root.oauth.callbackInput.Width() != size.Width-16 {
			tb.Fatal("resize did not reach text input controls")
		}
		usage := map[string]any{
			"usage": map[string]any{
				"total_requests": float64(17), "total_tokens": float64(42),
				"requests_by_hour": map[string]any{"09": float64(5), "10": float64(12)},
				"tokens_by_hour":   map[string]any{"09": float64(42)},
			},
		}
		for _, msg := range []tea.Msg{
			dashboardDataMsg{config: map[string]any{}, usage: usage},
			configDataMsg{config: map[string]any{"proxy-url": "fixture-proxy"}},
			authFilesMsg{files: []map[string]any{{"name": "fixture.json"}}},
			keysDataMsg{apiKeys: []string{"fixture-access-key"}}, usageDataMsg{usage: usage},
			logsPollMsg{lines: []string{"[info] fixture log"}},
		} {
			root.Update(msg)
		}
		root.oauth.state, root.oauth.message, root.oauth.err = oauthIdle, "", nil
		for tab, marker := range []string{
			"17", "fixture-proxy", "fixture.json", maskKey("fixture-access-key"),
			"Gemini CLI", "17", "fixture log",
		} {
			root.activeTab = tab
			view := root.View()
			if !view.AltScreen || !strings.Contains(view.Content, marker) ||
				lipgloss.Height(view.Content) != size.Height-2 {
				tb.Fatalf("tab %d view lost its content or layout: %q", tab, view.Content)
			}
			if size.Width == 120 {
				tb.Logf("tab %d (%dx%d):\n%s", tab, size.Width, size.Height, view.Content)
			}
		}
		failure := errors.New("fixture view failure")
		for _, msg := range []tea.Msg{
			dashboardDataMsg{err: failure}, configDataMsg{err: failure}, authFilesMsg{err: failure},
			keysDataMsg{err: failure}, usageDataMsg{err: failure}, logsPollMsg{err: failure},
			oauthStartMsg{err: failure},
		} {
			root.Update(msg)
		}
		for tab := tabDashboard; tab <= tabLogs; tab++ {
			root.activeTab = tab
			view := root.View()
			if !view.AltScreen || !strings.Contains(view.Content, failure.Error()) ||
				!strings.Contains(view.Content, root.tabs[tab]) ||
				lipgloss.Height(view.Content) != size.Height-2 {
				tb.Fatalf("tab %d view lost its error, title or layout: %q", tab, view.Content)
			}
			if size.Width == 120 {
				tb.Logf("tab %d (%dx%d):\n%s", tab, size.Width, size.Height, view.Content)
			}
		}
	}
}

func TestTUIV2KeyPressAndSpaceContract(t *testing.T) {
	root := newApp(nil, "", nil)
	root.authenticated = true
	root.Update(tea.WindowSizeMsg{Width: 120, Height: 32})
	root.initialized = [7]bool{true, true, true, true, true, true, true}
	root.Update(tea.KeyReleaseMsg{Code: tea.KeyTab})
	if root.activeTab != tabDashboard {
		t.Fatal("key release switched tabs")
	}
	root.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	if root.activeTab != tabConfig {
		t.Fatal("Tab did not select Config")
	}
	root.Update(tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
	if root.activeTab != tabDashboard {
		t.Fatal("Shift+Tab did not restore Dashboard")
	}

	root.activeTab = tabAuthFiles
	root.auth.files = []map[string]any{{"name": "fixture.json"}}
	root.Update(tea.KeyPressMsg{Code: tea.KeySpace, Text: " "})
	if root.auth.expanded != 0 {
		t.Fatal("Space did not expand the selected credential")
	}
	root.Update(tea.KeyPressMsg{Code: tea.KeySpace, Text: " "})
	if root.auth.expanded != -1 {
		t.Fatal("Space did not collapse the selected credential")
	}
	for _, key := range []tea.KeyPressMsg{
		{Code: 'r', Text: "R", ShiftedCode: 'R', Mod: tea.ModShift},
		{Code: 'r', Mod: tea.ModCtrl},
	} {
		if _, command := root.Update(key); command == nil {
			t.Fatalf("credential shortcut %q returned no command", key.String())
		}
	}

	root.activeTab = tabConfig
	root.config.fields = []configField{{apiPath: "debug", kind: "bool", value: "false"}}
	_, command := root.Update(tea.KeyPressMsg{Code: tea.KeySpace, Text: " "})
	if command == nil || root.config.editing {
		t.Fatal("Space did not create the boolean toggle command")
	}
	_, command = root.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if command == nil {
		t.Fatal("Ctrl+C did not return Quit")
	}
	if _, ok := command().(tea.QuitMsg); !ok {
		t.Fatal("Ctrl+C returned a different command")
	}
}

func TestTUIV2BorderedCardDimensions(t *testing.T) {
	root := newApp(&Client{baseURL: "http://127.0.0.1:8317"}, "", nil)
	root.authenticated = true
	root.Update(tea.WindowSizeMsg{Width: 120, Height: 32})
	usage := map[string]any{"usage": map[string]any{"total_requests": float64(17), "total_tokens": float64(42)}}
	root.Update(dashboardDataMsg{config: map[string]any{}, usage: usage})
	root.Update(usageDataMsg{usage: usage})
	for _, test := range []struct {
		name    string
		content string
		height  int
	}{
		{"dashboard", root.dashboard.content, 4},
		{"usage", root.usage.renderContent(), 5},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				lines := strings.Split(test.content, "\n")
				for i, line := range lines {
					if !strings.Contains(line, "╭") {
						continue
					}
					if got, want := lipgloss.Width(line), 4*(28+2)+3; got != want {
						t.Fatalf("card row width = %d, want %d", got, want)
					}
					if i+test.height > len(lines) || strings.Count(lines[i+test.height-1], "╰") != 4 {
						t.Fatalf("card row did not preserve its %d-line frame: %s", test.height, test.content)
					}
					return
				}
				t.Fatal("card row is missing")
			},
		)
	}
}

func TestTUIV2FocusedInputsAcceptTextAndPaste(t *testing.T) {
	for _, test := range []struct {
		name  string
		setup func(*app)
		value func(*app) string
	}{
		{"password", func(a *app) {}, func(a *app) string { return a.authInput.Value() }},
		{
			"config", func(a *app) {
				a.authenticated, a.activeTab = true, tabConfig
				a.config.fields = []configField{{apiPath: "proxy-url", kind: "string"}}
				a.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
			}, func(a *app) string { return a.config.textInput.Value() },
		},
		{
			"credential", func(a *app) {
				a.authenticated, a.activeTab = true, tabAuthFiles
				a.auth.files = []map[string]any{{"name": "fixture.json"}}
				a.Update(tea.KeyPressMsg{Code: '1', Text: "1"})
			}, func(a *app) string { return a.auth.editInput.Value() },
		},
		{
			"new key", func(a *app) {
				a.authenticated, a.activeTab = true, tabAPIKeys
				a.Update(tea.KeyPressMsg{Code: 'a', Text: "a"})
			}, func(a *app) string { return a.keys.editInput.Value() },
		},
		{
			"existing key", func(a *app) {
				a.authenticated, a.activeTab = true, tabAPIKeys
				a.keys.keys = []string{""}
				a.Update(tea.KeyPressMsg{Code: 'e', Text: "e"})
			}, func(a *app) string { return a.keys.editInput.Value() },
		},
		{
			"callback", func(a *app) {
				a.authenticated, a.activeTab = true, tabOAuth
				a.oauth.state, a.oauth.inputActive = oauthRemote, true
				a.oauth.callbackInput.Focus()
			}, func(a *app) string { return a.oauth.callbackInput.Value() },
		},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				root := newApp(nil, "", nil)
				root.Update(tea.WindowSizeMsg{Width: 120, Height: 32})
				test.setup(&root)
				tab := root.activeTab
				for _, msg := range []tea.Msg{
					tea.KeyPressMsg{Code: 'x', Text: "x"},
					tea.KeyPressMsg{Code: '界', Text: "界"},
					tea.PasteMsg{Content: " qL/fixture?code=abc&state=one"},
					tea.KeyReleaseMsg{Code: 'x', Text: "x"},
				} {
					model, _ := root.Update(msg)
					if model != &root || root.activeTab != tab {
						t.Fatal("input changed the root model or active tab")
					}
				}
				if got, want := test.value(&root), "x界 qL/fixture?code=abc&state=one"; got != want {
					t.Fatalf("input = %q, want %q", got, want)
				}
				if test.name == "password" && strings.Contains(root.View().Content, test.value(&root)) {
					t.Fatal("password input was displayed in plaintext")
				}
			},
		)
	}
}
