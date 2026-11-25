package management

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/watcher/synthesizer"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/gin-gonic/gin"
)

func credentialRepairRequest(t *testing.T, handler gin.HandlerFunc, method, path, body string, want int) []byte {
	t.Helper()
	response := runConfigListRequest(handler, method, path, body)
	if response.Code != want {
		t.Fatalf("%s %s: HTTP %d, want %d: %s", method, path, response.Code, want, response.Body)
	}
	return response.Body.Bytes()
}

func TestCredentialRepairPatchRetainsEntryOnEmptyKey(t *testing.T) {
	for _, resource := range []struct {
		name  string
		post  func(*Handler, *gin.Context)
		patch func(*Handler, *gin.Context)
		count func(*config.Config) int
	}{
		{
			"gemini", (*Handler).PostGeminiKey, (*Handler).PatchGeminiKey,
			func(cfg *config.Config) int { return len(cfg.GeminiKey) },
		},
		{
			"interactions", (*Handler).PostInteractionsKey, (*Handler).PatchInteractionsKey,
			func(cfg *config.Config) int { return len(cfg.InteractionsKey) },
		},
		{
			"vertex", (*Handler).PostVertexCompatKey, (*Handler).PatchVertexCompatKey,
			func(cfg *config.Config) int { return len(cfg.VertexCompatAPIKey) },
		},
	} {
		t.Run(
			resource.name, func(t *testing.T) {
				h := newConfigListTestHandler(t, &config.Config{})
				path := "/" + resource.name + "-api-key"
				post := func(c *gin.Context) { resource.post(h, c) }
				patch := func(c *gin.Context) { resource.patch(h, c) }
				credentialRepairRequest(
					t, post, http.MethodPost, path, `{"api-key":"synthetic-credential"}`, http.StatusCreated,
				)
				credentialRepairRequest(
					t, patch, http.MethodPatch, path, `{"index":0,"keep-entry":true,"value":{"api-key":"  "}}`,
					http.StatusBadRequest,
				)
				cfg, err := config.LoadConfig(h.configFilePath)
				if err != nil || resource.count(cfg) != 1 {
					t.Fatalf("failed form save removed persisted credential: %v", err)
				}
				credentialRepairRequest(
					t, patch, http.MethodPatch, path, `{"index":0,"value":{"api-key":""}}`, http.StatusOK,
				)
				if resource.count(h.configSnapshot()) != 0 {
					t.Fatal("legacy empty-key deletion no longer removes the entry")
				}
			},
		)
	}
}

func TestCredentialRepairVertexClearAndNameOnlyModelsRoundTrip(t *testing.T) {
	h := newConfigListTestHandler(t, &config.Config{})
	const path = "/vertex-api-key"
	credentialRepairRequest(
		t, h.PostVertexCompatKey, http.MethodPost, path, `{
		"api-key":"synthetic-vertex-key","base-url":"https://gateway.example.invalid/v1",
		"interactions":false,"disable-cooling":false,"request-retry":0,"weight":0,
		"excluded-models":["private-*"],"models":[
			{"name":" Name Only ","display-name":"Display","force-mapping":true},
			{"name":"Same Name","alias":"Same Name"},
			{"name":"Upstream","alias":" Client "},
			{"alias":"no-upstream"}
		]}`, http.StatusCreated,
	)
	credentialRepairRequest(
		t, h.PatchVertexCompatKey, http.MethodPatch, path,
		`{"index":0,"keep-entry":true,"value":{"clear-base-url":true,"base-url":"https://other.example.invalid"}}`,
		http.StatusBadRequest,
	)
	credentialRepairRequest(
		t, h.PatchVertexCompatKey, http.MethodPatch, path,
		`{"index":0,"keep-entry":true,"value":{"clear-base-url":true}}`, http.StatusOK,
	)
	cfg, err := config.LoadConfig(h.configFilePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.VertexCompatAPIKey) != 1 {
		t.Fatalf("persisted Vertex entries = %d, want one", len(cfg.VertexCompatAPIKey))
	}
	entry := cfg.VertexCompatAPIKey[0]
	if entry.BaseURL != "" || entry.APIKey != "synthetic-vertex-key" ||
		entry.Interactions == nil || *entry.Interactions || entry.DisableCooling == nil || *entry.DisableCooling ||
		entry.Weight == nil || *entry.Weight != 0 || entry.RequestRetry == nil || *entry.RequestRetry != 0 ||
		len(entry.ExcludedModels) != 1 || entry.ExcludedModels[0] != "private-*" {
		t.Fatalf("reset changed credential fields: %+v", entry)
	}
	if len(entry.Models) != 3 || entry.Models[0].Name != "Name Only" || entry.Models[0].Alias != "Name Only" ||
		entry.Models[0].DisplayName != "Display" || !entry.Models[0].ForceMapping || entry.Models[1].Alias != "Same Name" ||
		entry.Models[2].Alias != "Client" {
		t.Fatalf("stored model mappings = %+v", entry.Models)
	}
	// The opt-in edit does not change the old empty-URL deletion contract.
	credentialRepairRequest(
		t, h.PatchVertexCompatKey, http.MethodPatch, path,
		`{"index":0,"value":{"base-url":""}}`, http.StatusOK,
	)
	if len(h.configSnapshot().VertexCompatAPIKey) != 0 {
		t.Fatal("legacy empty-URL deletion retained an entry")
	}
}

func TestCredentialRepairRejectsDuplicateEditWithoutDroppingSibling(t *testing.T) {
	for _, resource := range []struct {
		name, second, duplicate, distinct string
		post, patch                       func(*Handler, *gin.Context)
	}{
		{
			"gemini", `"prefix":"two"`, `"prefix":" /two/ "`, `"prefix":"three"`, (*Handler).PostGeminiKey,
			(*Handler).PatchGeminiKey,
		},
		{
			"interactions", `"prefix":"two"`, `"prefix":" /two/ "`, `"prefix":"three"`, (*Handler).PostInteractionsKey,
			(*Handler).PatchInteractionsKey,
		},
		{
			"vertex", `"base-url":"https://second.example.invalid"`, `"base-url":" https://second.example.invalid "`,
			`"clear-base-url":true`, (*Handler).PostVertexCompatKey, (*Handler).PatchVertexCompatKey,
		},
	} {
		t.Run(
			resource.name, func(t *testing.T) {
				h := newConfigListTestHandler(t, &config.Config{})
				resourceKey := resource.name + "-api-key"
				path := "/" + resourceKey
				post := func(c *gin.Context) { resource.post(h, c) }
				patch := func(c *gin.Context) { resource.patch(h, c) }
				credentialRepairRequest(
					t, post, http.MethodPost, path,
					`{"api-key":"synthetic-shared-key","base-url":"https://first.example.invalid","prefix":"one","priority":10}`,
					http.StatusCreated,
				)
				second := resource.second
				if resource.name != "vertex" {
					second += `,"base-url":"https://first.example.invalid"`
				}
				credentialRepairRequest(
					t, post, http.MethodPost, path,
					`{"api-key":"synthetic-shared-key","priority":20,`+second+`}`, http.StatusCreated,
				)
				credentialRepairRequest(
					t, patch, http.MethodPatch, path,
					`{"index":0,"keep-entry":true,"value":{"priority":30,`+resource.duplicate+`}}`, http.StatusConflict,
				)
				if rows := credentialConfigRows(
					t, h, resourceKey,
				); len(rows) != 2 || rows[0].Priority != 10 || rows[1].Priority != 20 {
					t.Fatalf("duplicate write changed existing rows: %+v", rows)
				}
				credentialRepairRequest(
					t, patch, http.MethodPatch, path,
					`{"index":0,"keep-entry":true,"value":{"priority":40,`+resource.distinct+`}}`, http.StatusOK,
				)
				if rows := credentialConfigRows(
					t, h, resourceKey,
				); len(rows) != 2 || rows[0].Priority != 40 || rows[1].Priority != 20 {
					t.Fatalf("distinct write did not retain both records: %+v", rows)
				}
			},
		)
	}
}

type credentialRepairRow struct {
	EntryRef string `json:"entry-ref"`
	Prefix   string `json:"prefix"`
	Priority int    `json:"priority"`
}

func credentialConfigRows(t *testing.T, h *Handler, resource string) []credentialRepairRow {
	t.Helper()
	data := credentialRepairRequest(t, h.GetConfig, http.MethodGet, "/config", "", http.StatusOK)
	if strings.Contains(string(data), "synthetic-shared-key") {
		t.Fatal("config readback exposed a raw API key")
	}
	var document map[string]json.RawMessage
	var rows []credentialRepairRow
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(document[resource], &rows); err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestCredentialRepairSelectedDeletePreservesSiblingAndLegacyContract(t *testing.T) {
	for _, resource := range []struct {
		name                string
		post, patch, remove func(*Handler, *gin.Context)
	}{
		{"gemini", (*Handler).PostGeminiKey, (*Handler).PatchGeminiKey, (*Handler).DeleteGeminiKey},
		{
			"interactions", (*Handler).PostInteractionsKey, (*Handler).PatchInteractionsKey,
			(*Handler).DeleteInteractionsKey,
		},
		{"claude", (*Handler).PostClaudeKey, (*Handler).PatchClaudeKey, (*Handler).DeleteClaudeKey},
		{"codex", (*Handler).PostCodexKey, (*Handler).PatchCodexKey, (*Handler).DeleteCodexKey},
		{"meta", (*Handler).PostMetaKey, (*Handler).PatchMetaKey, (*Handler).DeleteMetaKey},
		{"xai", (*Handler).PostXAIKey, (*Handler).PatchXAIKey, (*Handler).DeleteXAIKey},
	} {
		t.Run(
			resource.name, func(t *testing.T) {
				h := newConfigListTestHandler(t, &config.Config{})
				resourceKey := resource.name + "-api-key"
				path := "/" + resourceKey
				post := func(c *gin.Context) { resource.post(h, c) }
				patch := func(c *gin.Context) { resource.patch(h, c) }
				remove := func(c *gin.Context) { resource.remove(h, c) }
				for _, prefix := range []string{"first", "second"} {
					credentialRepairRequest(
						t, post, http.MethodPost, path, fmt.Sprintf(
							`{"api-key":"synthetic-shared-key","base-url":"https://shared.example.invalid","prefix":%q}`,
							prefix,
						), http.StatusCreated,
					)
				}
				if resource.name == "meta" {
					manager := coreauth.NewManager(nil, nil, nil)
					auths, err := synthesizer.NewConfigSynthesizer().Synthesize(
						&synthesizer.SynthesisContext{
							Config: h.configSnapshot(), IDGenerator: synthesizer.NewStableIDGenerator(),
							Now: time.Now(),
						},
					)
					if err != nil || len(auths) != 2 || auths[0].EnsureIndex() != auths[1].EnsureIndex() {
						t.Fatalf("fixture does not use two Meta routes with one runtime index: %v", err)
					}
					for _, auth := range auths {
						if _, err := manager.Register(context.Background(), auth); err != nil {
							t.Fatal(err)
						}
					}
					h.SetAuthManager(manager)
				}
				legacy := `{"api-key":"synthetic-shared-key","base-url":"https://shared.example.invalid"}`
				credentialRepairRequest(t, remove, http.MethodDelete, path, legacy, http.StatusBadRequest)
				rows := credentialConfigRows(t, h, resourceKey)
				if len(rows) != 2 || rows[0].EntryRef == "" || rows[1].EntryRef == "" || rows[0].EntryRef == rows[1].EntryRef {
					t.Fatalf("distinct routes lack distinct references: %+v", rows)
				}
				staleRef := fmt.Sprintf(`{"entry-ref":%q}`, rows[1].EntryRef)
				credentialRepairRequest(
					t, patch, http.MethodPatch, path, `{"index":1,"value":{"priority":14}}`, http.StatusOK,
				)
				credentialRepairRequest(t, remove, http.MethodDelete, path, staleRef, http.StatusConflict)
				rows = credentialConfigRows(t, h, resourceKey)
				if len(rows) != 2 || rows[1].Priority != 14 {
					t.Fatalf("stale deletion changed current rows: %+v", rows)
				}
				credentialRepairRequest(
					t, remove, http.MethodDelete, path, fmt.Sprintf(`{"entry-ref":%q}`, rows[1].EntryRef),
					http.StatusOK,
				)
				if rows := credentialConfigRows(t, h, resourceKey); len(rows) != 1 || rows[0].Prefix != "first" {
					t.Fatalf("selected deletion removed the wrong row: %+v", rows)
				}
				credentialRepairRequest(t, remove, http.MethodDelete, path, legacy, http.StatusOK)
				if rows := credentialConfigRows(t, h, resourceKey); len(rows) != 0 {
					t.Fatalf("legacy deletion did not remove the remaining unique row: %+v", rows)
				}
			},
		)
	}
}

func TestCredentialRepairAuthModelsSelectsExactListedIdentity(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	h := NewHandlerWithoutConfigFilePath(&config.Config{}, manager, withEnvSecret(""))
	t.Cleanup(func() { _ = h.Close() })
	const name = "shared repair.json"
	reg := registry.GetGlobalRegistry()
	for _, index := range []string{"left", "right"} {
		id := "synthetic-credential-repair-" + index
		if _, err := manager.Register(
			context.Background(), &coreauth.Auth{
				ID: id, FileName: name, Index: index, Provider: "codex",
			},
		); err != nil {
			t.Fatal(err)
		}
		reg.RegisterClient(id, "codex", []*registry.ModelInfo{{ID: index + "-model"}})
		t.Cleanup(func() { reg.UnregisterClient(id) })
	}
	path := "/auth-files/models?name=" + url.QueryEscape(name)
	assertModels := func(target, want string) {
		t.Helper()
		data := credentialRepairRequest(t, h.GetAuthFileModels, http.MethodGet, target, "", http.StatusOK)
		var response struct{ Models []struct{ ID string } }
		if err := json.Unmarshal(
			data, &response,
		); err != nil || len(response.Models) != 1 || response.Models[0].ID != want {
			t.Fatalf("model response %s, want %q: %v", data, want, err)
		}
	}
	assertModels(path+"&auth_index=right", "right-model")
	assertModels(path+"&auth_index=left", "left-model")
	assertModels("/auth-files/models?name=synthetic-credential-repair-left", "left-model")
	credentialRepairRequest(t, h.GetAuthFileModels, http.MethodGet, path, "", http.StatusConflict)
	credentialRepairRequest(t, h.GetAuthFileModels, http.MethodGet, path+"&auth_index=stale", "", http.StatusConflict)
}
