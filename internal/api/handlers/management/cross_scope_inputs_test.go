package management

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/watcher/synthesizer"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/gin-gonic/gin"
)

func TestDeleteCredentialIndexRequiresACompleteInteger(t *testing.T) {
	for _, index := range []string{"0tail", "0.5", "0 1", "9999999999999999999999999999", "-1", "0"} {
		t.Run(
			index, func(t *testing.T) {
				h := newConfigListTestHandler(
					t, &config.Config{
						MetaKey: []config.MetaKey{
							{APIKey: "synthetic-first", BaseURL: "https://api.meta.ai/v1"},
							{APIKey: "synthetic-second", BaseURL: "https://api.meta.ai/v1"},
						},
					},
				)
				before, err := os.ReadFile(h.configFilePath)
				if err != nil {
					t.Fatal(err)
				}
				original := append([]config.MetaKey(nil), h.configSnapshot().MetaKey...)
				response := runConfigListRequest(
					h.DeleteMetaKey, http.MethodDelete, "/meta-api-key?index="+url.QueryEscape(index), "",
				)
				if index == "0" {
					if response.Code != 200 || len(h.configSnapshot().MetaKey) != 1 ||
						h.configSnapshot().MetaKey[0].APIKey != "synthetic-second" {
						t.Fatalf(
							"valid delete status=%d remaining=%+v",
							response.Code,
							h.configSnapshot().MetaKey,
						)
					}
					return
				}
				if response.Code != 400 || !reflect.DeepEqual(h.configSnapshot().MetaKey, original) {
					t.Fatalf(
						"invalid delete status=%d remaining=%+v",
						response.Code,
						h.configSnapshot().MetaKey,
					)
				}
				after, err := os.ReadFile(h.configFilePath)
				if err != nil || string(after) != string(before) {
					t.Fatalf("invalid index changed config=%q err=%v", after, err)
				}
			},
		)
	}
}

func TestOAuthMapWrapperRequiresExplicitItemsBeforeMutation(t *testing.T) {
	for _, tc := range []struct {
		body  string
		valid bool
		clear bool
	}{
		{body: `{"codex":true}`},
		{body: `{"codex":"invalid"}`},
		{body: `{"unknown":{"nested":true}}`},
		{body: `{"items":{"codex":["new-model"]}}`, valid: true},
		{body: `{"codex":["new-model"]}`, valid: true},
		{body: `{}`, valid: true, clear: true},
		{body: `{"items":null}`, valid: true, clear: true},
	} {
		t.Run(
			tc.body, func(t *testing.T) {
				beforeMap := map[string][]string{"codex": {"original-model"}}
				h := newConfigListTestHandler(t, &config.Config{OAuthExcludedModels: beforeMap})
				before, err := os.ReadFile(h.configFilePath)
				if err != nil {
					t.Fatal(err)
				}
				response := runConfigListRequest(
					h.PutOAuthExcludedModels, http.MethodPut, "/oauth-excluded-models", tc.body,
				)
				if !tc.valid {
					after, err := os.ReadFile(h.configFilePath)
					if response.Code != 400 || err != nil || string(before) != string(after) ||
						!reflect.DeepEqual(
							h.configSnapshot().OAuthExcludedModels, beforeMap,
						) {
						t.Fatalf(
							"rejected wrapper status=%d config=%+v err=%v", response.Code,
							h.configSnapshot().OAuthExcludedModels, err,
						)
					}
					return
				}
				if response.Code != 200 {
					t.Fatalf("valid wrapper status=%d body=%s", response.Code, response.Body.String())
				}
				if tc.clear {
					if len(h.configSnapshot().OAuthExcludedModels) != 0 {
						t.Fatal("explicit clear did not clear the map")
					}
				} else if !reflect.DeepEqual(h.configSnapshot().OAuthExcludedModels["codex"], []string{"new-model"}) {
					t.Fatalf("map=%+v", h.configSnapshot().OAuthExcludedModels)
				}
			},
		)
	}
}

func TestPatchMetaEmptyBaseURLRestoresDefaultWithoutDeletingCredential(t *testing.T) {
	for _, value := range []string{`""`, `"   "`, `"https://api.meta.ai/v1"`, `null`} {
		t.Run(
			value, func(t *testing.T) {
				h := newConfigListTestHandler(
					t, &config.Config{
						MetaKey: []config.MetaKey{
							{
								APIKey: "synthetic-meta-key", BaseURL: "https://custom.example/v1",
							},
						},
					},
				)
				body := `{"index":0,"value":{"base-url":` + value + `,"priority":7,"weight":9}}`
				response := runConfigListRequest(h.PatchMetaKey, http.MethodPatch, "/meta-api-key", body)
				if response.Code != 200 || len(h.configSnapshot().MetaKey) != 1 {
					t.Fatalf("patch status=%d body=%s", response.Code, response.Body.String())
				}
				got := h.configSnapshot().MetaKey[0]
				wantBase := "https://api.meta.ai/v1"
				if value == "null" {
					wantBase = "https://custom.example/v1"
				}
				if got.APIKey != "synthetic-meta-key" || got.BaseURL != wantBase || got.Priority != 7 ||
					got.Weight == nil ||
					*got.Weight != 9 {
					t.Fatalf("patched credential=%+v", got)
				}
				loaded, err := config.LoadConfig(h.configFilePath)
				if err != nil || len(loaded.MetaKey) != 1 || loaded.MetaKey[0].BaseURL != wantBase {
					t.Fatalf("reloaded Meta=%+v err=%v", loaded, err)
				}
			},
		)
	}
}

func TestConfigAuthIndexesIncludeBaseURLOnlyCredentials(t *testing.T) {
	cfg := &config.Config{
		GeminiKey: []config.GeminiKey{
			{
				BaseURL: "https://first.example/v1",
			}, {APIKey: "synthetic-gemini", BaseURL: "https://second.example/v1"},
		},
		InteractionsKey: []config.GeminiKey{
			{BaseURL: "https://first.example/v1"},
			{APIKey: "synthetic-interactions", BaseURL: "https://second.example/v1"},
		},
		ClaudeKey: []config.ClaudeKey{
			{
				BaseURL: "https://first.example/v1",
			}, {APIKey: "synthetic-claude", BaseURL: "https://second.example/v1"},
		},
		CodexKey: []config.CodexKey{
			{
				BaseURL: "https://first.example/v1",
			}, {APIKey: "synthetic-codex", BaseURL: "https://second.example/v1"},
		},
		XAIKey: []config.XAIKey{
			{
				BaseURL: "https://first.example/v1",
			}, {APIKey: "synthetic-xai", BaseURL: "https://second.example/v1"},
		},
	}
	auths, err := synthesizer.NewConfigSynthesizer().Synthesize(
		&synthesizer.SynthesisContext{
			Config: cfg, Now: time.Now(), IDGenerator: synthesizer.NewStableIDGenerator(),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(auths) != 10 {
		t.Fatalf("actual synthesized auths=%d want 10", len(auths))
	}
	manager := coreauth.NewManager(nil, nil, nil)
	for _, auth := range auths {
		if _, err := manager.Register(coreauth.WithSkipPersist(context.Background()), auth); err != nil {
			t.Fatal(err)
		}
	}
	h := &Handler{cfg: cfg, authManager: manager}
	for _, tc := range []struct {
		name    string
		handler func(*gin.Context)
	}{
		{"gemini-api-key", h.GetGeminiKeys}, {"interactions-api-key", h.GetInteractionsKeys},
		{"claude-api-key", h.GetClaudeKeys}, {"codex-api-key", h.GetCodexKeys}, {"xai-api-key", h.GetXAIKeys},
	} {
		t.Run(
			tc.name, func(t *testing.T) {
				response := runConfigListRequest(tc.handler, http.MethodGet, "/"+tc.name, "")
				var payload map[string][]struct {
					AuthIndex string `json:"auth-index"`
				}
				if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
					t.Fatal(err)
				}
				rows := payload[tc.name]
				if response.Code != 200 || len(rows) != 2 || rows[0].AuthIndex == "" ||
					rows[1].AuthIndex == "" ||
					rows[0].AuthIndex == rows[1].AuthIndex {
					t.Fatalf("actual GET rows=%+v body=%s", rows, response.Body.String())
				}
			},
		)
	}
}

func TestPluginStoreTransportFailuresDoNotExposeSignedURLsInManagement(t *testing.T) {
	const registryURL = "https://synthetic-user:synthetic-password@registry.example/" +
		"registry.json?signature=synthetic-query#synthetic-fragment"
	for _, operation := range []string{"list", "releases", "install"} {
		t.Run(
			operation, func(t *testing.T) {
				calls := 0
				client := &http.Client{
					Timeout: time.Second, Transport: releaseRoundTripper(
						func(request *http.Request) (*http.Response, error) {
							calls++
							if request.URL.Query().Get("signature") != "synthetic-query" {
								t.Error("request URL was changed instead of redacting the error")
							}
							return nil, errors.New("synthetic transport failure")
						},
					),
				}
				h := &Handler{
					cfg:                    &config.Config{Plugins: config.PluginsConfig{Dir: t.TempDir()}},
					pluginStoreRegistryURL: registryURL, pluginStoreHTTPClient: client,
				}
				engine := gin.New()
				engine.GET("/store", h.ListPluginStore)
				engine.GET("/store/:id/releases", h.ListPluginStoreReleases)
				engine.POST("/store/:id/install", h.InstallPluginFromStore)
				method, path := http.MethodGet, "/store"
				if operation == "releases" {
					path = "/store/synthetic/releases"
				}
				if operation == "install" {
					method, path = http.MethodPost, "/store/synthetic/install"
				}
				response := httptest.NewRecorder()
				engine.ServeHTTP(response, httptest.NewRequest(method, path, strings.NewReader(`{}`)))
				body := response.Body.String()
				if response.Code != 502 || calls != 1 ||
					!strings.Contains(body, "synthetic transport failure") {
					t.Fatalf("status=%d calls=%d body=%s", response.Code, calls, body)
				}
				for _, secret := range []string{
					"synthetic-user", "synthetic-password", "synthetic-query", "synthetic-fragment",
				} {
					if strings.Contains(body, secret) {
						t.Fatalf("management response exposed %s: %s", secret, body)
					}
				}
			},
		)
	}
}
