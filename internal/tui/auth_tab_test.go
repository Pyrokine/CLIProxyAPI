package tui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func TestAuthTabUppercaseRefreshesCredentialsAndLowercaseReloads(t *testing.T) {
	var posts atomic.Int32
	var gets atomic.Int32
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodPost && r.URL.Path == "/v0/management/auth-files/refresh":
					posts.Add(1)
					var payload map[string]any
					if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
						t.Errorf("decode refresh: %v", err)
						http.Error(w, "invalid JSON", http.StatusBadRequest)
						return
					}
					if len(payload) != 2 || payload["name"] != "same.json" ||
						payload["auth_index"] != "selected-index" {
						t.Errorf("selected refresh body = %#v", payload)
						http.Error(w, "wrong refresh target", http.StatusBadRequest)
						return
					}
					w.WriteHeader(http.StatusOK)
				case r.Method == http.MethodGet && r.URL.Path == "/v0/management/auth-files":
					gets.Add(1)
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"files":[]}`))
				default:
					t.Errorf("unexpected request = %s %s", r.Method, r.URL.Path)
					http.Error(w, "unexpected request", http.StatusNotFound)
				}
			},
		),
	)
	defer server.Close()

	client := &Client{baseURL: server.URL, http: server.Client()}
	model := newAuthTabModel(client)
	model.files = []map[string]any{
		{"name": "same.json", "auth_index": "other-index"},
		{"name": "same.json", "auth_index": "selected-index"},
	}
	model.cursor = 1
	_, refreshCmd := model.handleNormalInput(tea.KeyPressMsg{Code: 'R', Text: "R"})
	if refreshCmd == nil {
		t.Fatal("uppercase R returned no command")
	}
	refreshMsg, ok := refreshCmd().(authActionMsg)
	if !ok || refreshMsg.err != nil || !strings.Contains(refreshMsg.action, "same.json") {
		t.Fatalf("uppercase R result = %#v", refreshMsg)
	}
	if posts.Load() != 1 || gets.Load() != 0 {
		t.Fatalf("after uppercase R posts=%d gets=%d", posts.Load(), gets.Load())
	}

	_, reloadCmd := model.handleNormalInput(tea.KeyPressMsg{Code: 'r', Text: "r"})
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

func TestAuthTabSelectedRefreshRequiresSelection(t *testing.T) {
	for _, files := range [][]map[string]any{nil, {{"name": "fixture.json", "auth_index": "fixture-index"}}} {
		model := newAuthTabModel(nil)
		model.files = files
		model.cursor = len(files)
		model.status = "previous outcome"
		_, cmd := model.handleNormalInput(tea.KeyPressMsg{Code: 'R', Text: "R"})
		if cmd != nil || model.status != "previous outcome" {
			t.Fatalf("without selection files=%d command=%v status=%q", len(files), cmd != nil, model.status)
		}
	}
}

func TestAuthTabSelectedRefreshCapturesIdentityAndClient(t *testing.T) {
	requests := make(chan map[string]string, 1)
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/v0/management/auth-files/refresh" {
					t.Errorf("request = %s %s", r.Method, r.URL.Path)
					http.Error(w, "unexpected request", http.StatusNotFound)
					return
				}
				var body map[string]string
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("decode refresh: %v", err)
					http.Error(w, "invalid JSON", http.StatusBadRequest)
					return
				}
				requests <- body
				w.WriteHeader(http.StatusOK)
			},
		),
	)
	defer server.Close()

	model := newAuthTabModel(&Client{baseURL: server.URL, http: server.Client()})
	model.files = []map[string]any{{"name": "selected.json", "auth_index": "selected-index"}}
	_, cmd := model.handleNormalInput(tea.KeyPressMsg{Code: 'R', Text: "R"})
	if cmd == nil {
		t.Fatal("selected refresh returned no command")
	}
	model.files[0]["name"], model.files[0]["auth_index"] = "replacement.json", "replacement-index"
	model.client = nil
	msg, ok := cmd().(authActionMsg)
	if !ok || msg.err != nil {
		t.Fatalf("selected refresh result = %#v", msg)
	}
	select {
	case body := <-requests:
		if len(body) != 2 || body["name"] != "selected.json" || body["auth_index"] != "selected-index" {
			t.Fatalf("refresh followed later selection: %#v", body)
		}
	default:
		t.Fatal("selected refresh did not request its original client")
	}
}

func TestAuthTabBulkRefreshUsesSeparateExplicitKey(tb *testing.T) {
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/v0/management/auth-files/refresh" {
					tb.Errorf("request = %s %s", r.Method, r.URL.Path)
					http.Error(w, "unexpected request", http.StatusNotFound)
					return
				}
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body) != 1 || body["all"] != true {
					tb.Errorf("explicit bulk body = %#v, error = %v", body, err)
					http.Error(w, "wrong bulk request", http.StatusBadRequest)
					return
				}
				_, _ = w.Write([]byte(`{"results":[]}`))
			},
		),
	)
	defer server.Close()

	model := newAuthTabModel(&Client{baseURL: server.URL, http: server.Client()})
	_, cmd := model.handleNormalInput(tea.KeyPressMsg{Code: 'r', Mod: tea.ModCtrl})
	if cmd == nil {
		tb.Fatal("Ctrl+R returned no bulk command")
	}
	msg, ok := cmd().(authActionMsg)
	if !ok || msg.err != nil || msg.action != t("all_credentials_refreshed") {
		tb.Fatalf("explicit bulk result = %#v", msg)
	}
}

func TestAuthTabRefreshOutcomeSurvivesAutomaticReload(t *testing.T) {
	for _, tt := range []struct {
		name string
		key  tea.KeyPressMsg
		code int
		body string
		want string
	}{
		{"selected success", tea.KeyPressMsg{Code: 'R', Text: "R"}, http.StatusOK, `{}`, "selected.json"},
		{"selected missing", tea.KeyPressMsg{Code: 'R', Text: "R"}, http.StatusNotFound, `{}`, "HTTP 404"},
		{
			"selected unavailable", tea.KeyPressMsg{Code: 'R', Text: "R"}, http.StatusConflict, `{}`,
			"HTTP 409",
		},
		{
			"selected upstream failure", tea.KeyPressMsg{Code: 'R', Text: "R"}, http.StatusBadGateway,
			`{}`, "HTTP 502",
		},
		{
			"selected timeout", tea.KeyPressMsg{Code: 'R', Text: "R"}, http.StatusGatewayTimeout, `{}`,
			"HTTP 504",
		},
		{
			"bulk partial failure", tea.KeyPressMsg{Code: 'r', Mod: tea.ModCtrl}, http.StatusOK,
			`{"results":[{"success":true},{"success":false}]}`, "1 of 2",
		},
	} {
		t.Run(
			tt.name, func(t *testing.T) {
				var posts atomic.Int32
				server := httptest.NewServer(
					http.HandlerFunc(
						func(w http.ResponseWriter, r *http.Request) {
							if r.Method == http.MethodPost && r.URL.Path == "/v0/management/auth-files/refresh" {
								posts.Add(1)
								w.WriteHeader(tt.code)
								_, _ = w.Write([]byte(tt.body))
								return
							}
							if r.Method == http.MethodGet && r.URL.Path == "/v0/management/auth-files" {
								_, _ = w.Write(
									[]byte(`{"files":[{"name":"selected.json","auth_index":"selected-index"}]}`),
								)
								return
							}
							t.Errorf("unexpected request = %s %s", r.Method, r.URL.Path)
							http.Error(w, "unexpected request", http.StatusNotFound)
						},
					),
				)
				defer server.Close()
				root := newApp(&Client{baseURL: server.URL, http: server.Client()}, "", nil)
				root.authenticated, root.activeTab = true, tabAuthFiles
				root.Update(tea.WindowSizeMsg{Width: 120, Height: 32})
				root.auth.files = []map[string]any{{"name": "selected.json", "auth_index": "selected-index"}}
				_, cmd := root.Update(tt.key)
				if cmd == nil {
					t.Fatal("refresh returned no command")
				}
				root.activeTab = tabAPIKeys
				_, reload := root.Update(cmd())
				if reload == nil {
					t.Fatal("refresh outcome did not reload the list")
				}
				root.Update(reload())
				if root.auth.err != nil || posts.Load() != 1 || !strings.Contains(root.auth.view(), tt.want) {
					t.Fatalf(
						"refresh outcome lost after reload: error=%v posts=%d view=%s", root.auth.err, posts.Load(),
						root.auth.view(),
					)
				}
				root.activeTab = tabAuthFiles
				root.Update(tea.KeyPressMsg{Code: 'r', Text: "r"})
				if root.auth.status != "" {
					t.Fatalf("manual reload kept stale status: %q", root.auth.status)
				}
			},
		)
	}
}

func TestAuthTabRefreshHelpAndUsageResourcesInBothLocales(tb *testing.T) {
	previous := currentLocale
	defer func() { currentLocale = previous }()
	for _, tt := range []struct {
		locale string
		help   []string
		usage  []string
	}{
		{
			"zh", []string{"[r] 重载列表", "[R] 刷新所选凭证", "[Ctrl+R] 刷新全部凭证"},
			[]string{"使用统计", "延迟", "首包", "总耗时"},
		},
		{
			"en", []string{"[r] Reload list", "[R] Refresh selected credential", "[Ctrl+R] Refresh all credentials"},
			[]string{"Usage", "Latency", "TTFB", "Duration"},
		},
	} {
		tb.Run(
			tt.locale, func(tb *testing.T) {
				currentLocale = tt.locale
				help := t("auth_help1") + t("auth_help2")
				for _, want := range tt.help {
					if !strings.Contains(help, want) {
						tb.Errorf("help missing %q: %s", want, help)
					}
				}
				for i, got := range []string{
					tabNames()[tabUsage], t("usage_latency"), t("usage_ttfb"), t("usage_duration"),
				} {
					if got != tt.usage[i] {
						tb.Errorf("usage resource = %q, want %q", got, tt.usage[i])
					}
				}
				if got := fmt.Sprintf(t("credentials_refreshed"), "selected.json"); !strings.Contains(
					got, "selected.json",
				) || strings.Contains(got, "%!") {
					tb.Errorf("selected refresh message = %q", got)
				}
			},
		)
	}
}
