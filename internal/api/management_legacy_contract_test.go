package api

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	proxyconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/tidwall/gjson"
	"gopkg.in/yaml.v3"
)

func TestManagementV0RoutesShareAccessControl(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "test-password")
	for _, tc := range []struct {
		name                                   string
		enabled, home, authorized, remote, tls bool
		allowRemote                            bool
		want                                   int
	}{
		{name: "authorized", enabled: true, authorized: true, want: http.StatusOK},
		{name: "missing key", enabled: true, want: http.StatusUnauthorized},
		{name: "disabled", authorized: true, want: http.StatusNotFound},
		{name: "home local authorized", enabled: true, home: true, authorized: true, want: http.StatusOK},
		{name: "home missing key", enabled: true, home: true, want: http.StatusUnauthorized},
		{
			name: "home remote denied", enabled: true, home: true, authorized: true, remote: true, tls: true,
			want: http.StatusUnauthorized,
		},
		{
			name: "home remote TLS authorized", enabled: true, home: true, authorized: true, remote: true,
			tls: true, allowRemote: true, want: http.StatusOK,
		},
		{
			name: "home remote plaintext", enabled: true, home: true, authorized: true, remote: true,
			allowRemote: true, want: http.StatusMisdirectedRequest,
		},
		{
			name: "home remote TLS missing key", enabled: true, home: true, remote: true, tls: true,
			allowRemote: true, want: http.StatusUnauthorized,
		},
	} {
		t.Run(
			tc.name, func(t *testing.T) {
				server := newTestServer(t)
				cfg := *server.configSnapshot()
				cfg.Home.Enabled = tc.home
				cfg.TLS.RequireForAuth = true
				cfg.RemoteManagement.AllowRemote = tc.allowRemote
				cfg.Plugins.Dir = t.TempDir()
				var item proxyconfig.PluginInstanceConfig
				if err := yaml.Unmarshal([]byte("enabled: false\npriority: 7\n"), &item); err != nil {
					t.Fatal(err)
				}
				cfg.Plugins.Configs = map[string]proxyconfig.PluginInstanceConfig{"test-plugin": item}
				server.mgmt.SetConfig(&cfg)
				server.UpdateClients(&cfg)
				server.managementRoutesEnabled.store(tc.enabled)
				for _, path := range []string{
					"/v0/management/config", "/v0/management/plugins",
					"/v0/management/plugins/test-plugin/config", "/v0/management/max-retry-credentials",
					"/v0/management/usage/storage-upgrade",
				} {
					req := httptest.NewRequest(http.MethodGet, path, nil)
					req.RemoteAddr = "127.0.0.1:1234"
					if tc.remote {
						req.RemoteAddr = "203.0.113.20:1234"
					}
					if tc.tls {
						req.TLS = &tls.ConnectionState{}
					}
					if tc.authorized {
						req.Header.Set("Authorization", "Bearer test-password")
					}
					response := httptest.NewRecorder()
					server.engine.ServeHTTP(response, req)
					if response.Code != tc.want {
						t.Fatalf("%s: status=%d want=%d body=%s", path, response.Code, tc.want, response.Body.String())
					}
				}
				if tc.home {
					response := httptest.NewRecorder()
					server.engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/management.html", nil))
					if response.Code != http.StatusNotFound {
						t.Fatalf("Home control panel status=%d, want 404", response.Code)
					}
				}
			},
		)
	}
}

func TestManagementV0ConfigurationOperationsShareState(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "test-password")
	server := newTestServer(t)
	initial := *server.configSnapshot()
	initial.Port = 8317
	initial.RequestRetry = 3
	initial.MaxRetryCredentials = 3
	initial.Debug = false
	initial.APIKeys = []string{"client"}
	initial.Codex.DisableCodexCloaking = true
	initial.Plugins.Dir = t.TempDir()
	var item proxyconfig.PluginInstanceConfig
	if err := yaml.Unmarshal(
		[]byte(`enabled: false
priority: 7
opaque:
  integer: 9007199254740993
`), &item,
	); err != nil {
		t.Fatal(err)
	}
	initial.Plugins.Configs = map[string]proxyconfig.PluginInstanceConfig{"test-plugin": item}
	server.mgmt.SetConfig(&initial)
	server.UpdateClients(&initial)
	content, err := yaml.Marshal(&initial)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(server.configFilePath, content, 0o600); err != nil {
		t.Fatal(err)
	}
	request := func(method, path, body, etag string, status int) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.RemoteAddr = "127.0.0.1:1234"
		req.Header.Set("Authorization", "Bearer test-password")
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
			if path == "/v0/management/config.yaml" {
				req.Header.Set("Content-Type", "application/yaml")
			}
		}
		if etag != "" {
			req.Header.Set("If-Match", etag)
		}
		response := httptest.NewRecorder()
		server.engine.ServeHTTP(response, req)
		if response.Code != status {
			t.Fatalf("%s %s: status=%d want=%d body=%s", method, path, response.Code, status, response.Body.String())
		}
		return response
	}
	assertLimit := func(want int) {
		t.Helper()
		for _, path := range []string{"/v0/management/max-retry-credentials", "/v0/management/config"} {
			response := request(http.MethodGet, path, "", "", http.StatusOK)
			value := gjson.GetBytes(response.Body.Bytes(), "max-retry-credentials")
			if !value.Exists() || value.Int() != int64(want) {
				t.Fatalf("%s: max-retry-credentials=%s want=%d", path, value.Raw, want)
			}
		}
		var view proxyconfig.Config
		response := request(http.MethodGet, "/v0/management/config.yaml", "", "", http.StatusOK)
		if err := yaml.Unmarshal(response.Body.Bytes(), &view); err != nil {
			t.Fatal(err)
		}
		saved, err := proxyconfig.LoadConfig(server.configFilePath)
		if err != nil {
			t.Fatal(err)
		}
		current := server.configSnapshot()
		loadedWant := max(want, 0)
		if view.MaxRetryCredentials != want || saved.MaxRetryCredentials != loadedWant ||
			current.MaxRetryCredentials != want {
			t.Fatalf(
				"limit: YAML=%d loaded=%d runtime=%d want=%d loadedWant=%d",
				view.MaxRetryCredentials, saved.MaxRetryCredentials, current.MaxRetryCredentials, want, loadedWant,
			)
		}
		if !saved.Codex.DisableCodexCloaking || !current.Codex.DisableCodexCloaking ||
			saved.RequestRetry != 3 || saved.Debug {
			t.Fatal("shared update changed unrelated flat configuration")
		}
	}
	assertLimit(3)
	request(http.MethodGet, "/v0/management/plugins/test-plugin/config", "", "", http.StatusOK)
	for _, body := range []string{`{}`, `{"value":null}`, `{"value":"2"}`, `{"value":2.5}`} {
		request(http.MethodPut, "/v0/management/max-retry-credentials", body, "", http.StatusBadRequest)
	}
	unchanged, err := os.ReadFile(server.configFilePath)
	if err != nil || !bytes.Equal(unchanged, content) || server.configSnapshot().MaxRetryCredentials != 3 {
		t.Fatalf("reads or rejected writes changed configuration: %v", err)
	}
	request(http.MethodPut, "/v0/management/max-retry-credentials", `{"value":2}`, "", http.StatusOK)
	assertLimit(2)
	baseline := request(http.MethodGet, "/v0/management/config.yaml", "", "", http.StatusOK)
	if baseline.Header().Get("ETag") == "" {
		t.Fatal("YAML baseline is missing its ETag")
	}
	next := *server.configSnapshot()
	next.MaxRetryCredentials = 5
	content, err = yaml.Marshal(&next)
	if err != nil {
		t.Fatal(err)
	}
	request(http.MethodPut, "/v0/management/config.yaml", string(content), baseline.Header().Get("ETag"), http.StatusOK)
	assertLimit(5)
	for _, value := range []int{0, -1} {
		request(
			http.MethodPatch, "/v0/management/max-retry-credentials",
			fmt.Sprintf(`{"value":%d}`, value), "", http.StatusOK,
		)
		assertLimit(value)
	}
	request(http.MethodPatch, "/v0/management/plugins/test-plugin/config", `{"priority":9}`, "", http.StatusOK)
	plugin := request(http.MethodGet, "/v0/management/plugins/test-plugin/config", "", "", http.StatusOK)
	if gjson.GetBytes(plugin.Body.Bytes(), "priority").Int() != 9 ||
		gjson.GetBytes(plugin.Body.Bytes(), "opaque.integer").Raw != "9007199254740993" ||
		gjson.GetBytes(plugin.Body.Bytes(), "config").Exists() {
		t.Fatalf("plugin direct object lost shared state: %s", plugin.Body.String())
	}
	assertLimit(-1)
	request(http.MethodDelete, "/v0/management/plugins/test-plugin", "", "", http.StatusOK)
	request(http.MethodGet, "/v0/management/plugins/test-plugin/config", "", "", http.StatusNotFound)
	assertLimit(-1)
	saved, err := proxyconfig.LoadConfig(server.configFilePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := saved.Plugins.Configs["test-plugin"]; exists {
		t.Fatal("plugin deletion was not persisted")
	}
	if _, exists := server.configSnapshot().Plugins.Configs["test-plugin"]; exists {
		t.Fatal("plugin deletion was not published to the server")
	}
	request(http.MethodPut, "/v0/management/request-retry", `{"value":2}`, "", http.StatusOK)
	retry := request(http.MethodGet, "/v0/management/request-retry", "", "", http.StatusOK)
	whole := request(http.MethodGet, "/v0/management/config", "", "", http.StatusOK)
	saved, err = proxyconfig.LoadConfig(server.configFilePath)
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(retry.Body.Bytes(), "request-retry").Int() != 2 ||
		gjson.GetBytes(whole.Body.Bytes(), "request-retry").Int() != 2 ||
		saved.RequestRetry != 2 || server.configSnapshot().RequestRetry != 2 {
		t.Fatal("legacy retry write did not reach the shared configuration")
	}
}
