package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/api/handlers/management"
	proxyconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/quota"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
	"gopkg.in/yaml.v3"
)

func TestManagementPluginConfigReadbackRoutePreservesDirectObject(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "test-management-password")
	server := newTestServer(t)
	updated := *server.configSnapshot()
	var item proxyconfig.PluginInstanceConfig
	if err := yaml.Unmarshal(
		[]byte("priority: 7\napi-key: synthetic-plugin-secret\nopaque:\n  integer: "+
			"9007199254740993\n  mode: "+
			"retained\n"),
		&item,
	); err != nil {
		t.Fatal(err)
	}
	updated.Plugins.Configs = map[string]proxyconfig.PluginInstanceConfig{"fixture": item}
	server.mgmt.SetConfig(&updated)
	read := func(id string, authorized bool) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/v0/management/plugins/"+id+"/config", nil)
		req.RemoteAddr = "127.0.0.1:12345"
		if authorized {
			req.Header.Set("Authorization", "Bearer test-management-password")
		}
		response := httptest.NewRecorder()
		server.engine.ServeHTTP(response, req)
		return response
	}
	if response := read("fixture", false); response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated config read status=%d", response.Code)
	}
	response := read("fixture", true)
	body := response.Body.Bytes()
	if response.Code != http.StatusOK || gjson.GetBytes(body, "priority").Int() != 7 || gjson.GetBytes(
		body, "opaque.integer",
	).Raw != "9007199254740993" || gjson.GetBytes(body, "opaque.mode").String() != "retained" || gjson.GetBytes(
		body, "config",
	).Exists() {
		t.Fatalf("direct config response=%d: %s", response.Code, body)
	}
	if gjson.GetBytes(body, "enabled").Bool() || strings.Contains(
		string(body), "synthetic-plugin-secret",
	) || !strings.HasPrefix(gjson.GetBytes(body, "api-key").String(), "__CLI_PROXY_CONFIG_SECRET_") {
		t.Fatalf("enabled default or redaction changed: %s", body)
	}
	if response.Header().Get(management.MutationResultHeader) != "" {
		t.Fatal("read-only GET returned a mutation receipt")
	}
	if missing := read("missing", true); missing.Code != http.StatusNotFound || gjson.GetBytes(
		missing.Body.Bytes(), "error",
	).String() != "plugin_not_found" {
		t.Fatalf("unknown plugin response=%d: %s", missing.Code, missing.Body.String())
	}
}

func TestServerAntigravityQuotaCallbackUsesCredentialProject(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	server := newTestServer(t)
	manager := server.handlers.AuthManager
	auth, err := manager.Register(
		t.Context(), &coreauth.Auth{
			ID: "fixture-antigravity-callback", FileName: "fixture-antigravity.json", Provider: "antigravity",
			Status:   coreauth.StatusActive,
			Metadata: map[string]any{"project_id": `fixture-"project`},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	auth.EnsureIndex()
	if registered := registerAuthManagerForQuota(server.quotaScheduler, manager); registered != 1 {
		t.Fatalf("registered synthetic credentials=%d", registered)
	}
	targets, unresolved := server.quotaScheduler.ResolveRefreshTargets([]string{auth.FileName})
	if len(targets) != 1 || len(unresolved) != 0 {
		t.Fatalf("refresh targets=%+v, unresolved=%v", targets, unresolved)
	}
	const summary = `{"groups":[{"displayName":"Shared","buckets":[{"bucketId":"weekly-fixture","window":"` +
		`weekly","remainingFraction":0,"resetTime":"2026-11-13T00:00:00Z"}]}]}`
	var calls atomic.Int32
	previous := http.DefaultTransport
	http.DefaultTransport = managementPanelRoundTripper(
		func(req *http.Request) (*http.Response, error) {
			if req.URL.Hostname() != "127.0.0.1" || req.URL.Path != "/v0/management/api-call" {
				return nil, fmt.Errorf("unexpected synthetic request host or path")
			}
			var request quota.APICallRequest
			if err := json.NewDecoder(req.Body).Decode(&request); err != nil {
				return nil, err
			}
			if request.AuthIndex != auth.Index {
				return nil, fmt.Errorf("credential binding changed")
			}
			calls.Add(1)
			payload := summary
			switch {
			case strings.HasSuffix(request.URL, ":retrieveUserQuotaSummary"):
				if request.Method != http.MethodPost || gjson.Get(
					request.Data, "project",
				).String() != `fixture-"project` {
					return nil, fmt.Errorf("server callback omitted credential project")
				}
			case strings.HasSuffix(request.URL, ":loadCodeAssist"):
				payload = `{"paidTier":{"id":"g1-ultra-tier"}}`
			default:
				return nil, fmt.Errorf("unexpected synthetic upstream path")
			}
			wire := `{"status_code":200,"body":` + payload + `}`
			return &http.Response{
				StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(wire)),
				Request: req,
			}, nil
		},
	)
	t.Cleanup(func() { http.DefaultTransport = previous })
	result := server.quotaScheduler.RefreshTarget(context.Background(), targets[0])
	if result.Outcome != quota.RefreshOutcomeSucceeded || calls.Load() != 2 {
		t.Fatalf(
			"actual server quota callback outcome=%s, error=%q, calls=%d",
			result.Outcome,
			result.Error,
			calls.Load(),
		)
	}
	status := server.quotaScheduler.GetStatus()
	var payload []byte
	for _, entry := range status.Credentials {
		if entry.AuthIndex == auth.Index {
			payload = entry.Data
			if entry.Source != "antigravity_quota" || entry.ObservedAt == nil {
				t.Fatalf("quota observation=%+v", entry)
			}
		}
	}
	if gjson.GetBytes(payload, "quota.groups.0.buckets.0.window").String() != "weekly" || gjson.GetBytes(
		payload, "quota.groups.0.buckets.0.remainingFraction",
	).Raw != "0" || gjson.GetBytes(payload, "codeAssist.paidTier.id").String() != "g1-ultra-tier" {
		t.Fatalf("scheduler lost grouped quota or literal zero: %s", payload)
	}
}
