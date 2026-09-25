package management

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"reflect"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/gin-gonic/gin"
)

func TestConcurrentCredentialOverridePatchesPreserveOtherFields(t *testing.T) {
	h := newConfigListTestHandler(t, &config.Config{CodexKey: []config.CodexKey{{
		APIKey: "synthetic-config-key", BaseURL: "https://example.test", DisableCooling: new(true), RequestRetry: new(3),
	}}})
	start := make(chan struct{})
	results := make(chan int, 2)
	for _, fields := range []string{`"disable-cooling":null`, `"request-retry":0`} {
		go func() {
			<-start
			response := runConfigListRequest(h.PatchCodexKey, http.MethodPatch, "/codex-api-key", `{"index":0,"value":{`+fields+`}}`)
			results <- response.Code
		}()
	}
	close(start)
	codes := []int{<-results, <-results}
	for _, code := range codes {
		if code != http.StatusOK {
			t.Fatalf("PATCH status=%d", code)
		}
	}
	data, err := os.ReadFile(h.configFilePath)
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := config.ParseConfigBytes(data)
	if err != nil {
		t.Fatal(err)
	}
	for _, cfg := range []*config.Config{h.configSnapshot(), reloaded} {
		entry := cfg.CodexKey[0]
		if entry.DisableCooling != nil || entry.RequestRetry == nil || *entry.RequestRetry != 0 || entry.APIKey != "synthetic-config-key" {
			t.Fatalf("concurrent patches lost an override: %+v", entry)
		}
	}
}

func TestProviderCredentialOverridesLifecycle(t *testing.T) {
	for _, provider := range []struct {
		name                          string
		post, put, patch, get, delete func(*Handler, *gin.Context)
	}{
		{"claude-api-key", (*Handler).PostClaudeKey, (*Handler).PutClaudeKeys, (*Handler).PatchClaudeKey, (*Handler).GetClaudeKeys, (*Handler).DeleteClaudeKey},
		{"codex-api-key", (*Handler).PostCodexKey, (*Handler).PutCodexKeys, (*Handler).PatchCodexKey, (*Handler).GetCodexKeys, (*Handler).DeleteCodexKey},
		{"xai-api-key", (*Handler).PostXAIKey, (*Handler).PutXAIKeys, (*Handler).PatchXAIKey, (*Handler).GetXAIKeys, (*Handler).DeleteXAIKey},
		{"gemini-api-key", (*Handler).PostGeminiKey, (*Handler).PutGeminiKeys, (*Handler).PatchGeminiKey, (*Handler).GetGeminiKeys, (*Handler).DeleteGeminiKey},
		{"interactions-api-key", (*Handler).PostInteractionsKey, (*Handler).PutInteractionsKeys, (*Handler).PatchInteractionsKey, (*Handler).GetInteractionsKeys, (*Handler).DeleteInteractionsKey},
		{"vertex-api-key", (*Handler).PostVertexCompatKey, (*Handler).PutVertexCompatKeys, (*Handler).PatchVertexCompatKey, (*Handler).GetVertexCompatKeys, (*Handler).DeleteVertexCompatKey},
		{"openai-compatibility", (*Handler).PostOpenAICompat, (*Handler).PutOpenAICompat, (*Handler).PatchOpenAICompat, (*Handler).GetOpenAICompat, (*Handler).DeleteOpenAICompat},
	} {
		t.Run(provider.name, func(t *testing.T) {
			h := newConfigListTestHandler(t, &config.Config{})
			entry := `"name":"example","api-key":"synthetic-config-key","base-url":"https://example.test"`
			created := runConfigListRequest(func(c *gin.Context) { provider.post(h, c) }, http.MethodPost, "/"+provider.name, `{`+entry+`,"disable-cooling":true,"request-retry":2}`)
			if created.Code != http.StatusCreated {
				t.Fatalf("POST: %d %s", created.Code, created.Body.String())
			}
			assertState := func(t *testing.T, cooling *bool, retry *int) {
				t.Helper()
				assertJSON := func(source string, data []byte) {
					t.Helper()
					var fields map[string]json.RawMessage
					if err := json.Unmarshal(data, &fields); err != nil {
						t.Fatal(err)
					}
					var entries []map[string]any
					if err := json.Unmarshal(fields[provider.name], &entries); err != nil {
						t.Fatal(err)
					}
					if len(entries) != 1 {
						t.Fatalf("entries=%v", entries)
					}
					got := entries[0]
					want := map[string]any{}
					if cooling != nil {
						want["disable-cooling"] = *cooling
					}
					if retry != nil {
						want["request-retry"] = float64(*retry)
					}
					for _, key := range []string{"disable-cooling", "request-retry"} {
						value, present := got[key]
						expected, wanted := want[key]
						if present != wanted || !reflect.DeepEqual(value, expected) {
							t.Fatalf("%s: %s=(%v,%t), want (%v,%t)", source, key, value, present, expected, wanted)
						}
					}
				}
				response := runConfigListRequest(func(c *gin.Context) { provider.get(h, c) }, http.MethodGet, "/"+provider.name, "")
				if response.Code != http.StatusOK {
					t.Fatalf("GET: %d %s", response.Code, response.Body.String())
				}
				assertJSON("GET", response.Body.Bytes())
				data, err := os.ReadFile(h.configFilePath)
				if err != nil {
					t.Fatal(err)
				}
				reloaded, err := config.ParseConfigBytes(data)
				if err != nil {
					t.Fatal(err)
				}
				for source, cfg := range map[string]*config.Config{"snapshot": h.configSnapshot(), "reloaded": reloaded} {
					data, err := json.Marshal(cfg)
					if err != nil {
						t.Fatal(err)
					}
					assertJSON(source, data)
				}
			}
			assertState(t, new(true), new(2))
			for _, step := range []struct {
				name, fields string
				cooling      *bool
				retry        *int
				invalid      bool
			}{
				{"missing", `"priority":7`, new(true), new(2), false},
				{"false_zero", `"disable-cooling":false,"request-retry":0`, new(false), new(0), false},
				{"invalid_boolean", `"disable-cooling":"true"`, new(false), new(0), true},
				{"invalid_integer", `"disable-cooling":true,"request-retry":1.5`, new(false), new(0), true},
				{"integer_overflow", `"request-retry":9223372036854775808`, new(false), new(0), true},
				{"integer_string", `"request-retry":"2"`, new(false), new(0), true},
				{"negative_inherit", `"request-retry":-1`, new(false), new(-1), false},
				{"clear_cooling", `"disable-cooling":null`, nil, new(-1), false},
				{"clear_retry", `"request-retry":null`, nil, nil, false},
			} {
				t.Run(step.name, func(t *testing.T) {
					before, err := os.ReadFile(h.configFilePath)
					if err != nil {
						t.Fatal(err)
					}
					response := runConfigListRequest(func(c *gin.Context) { provider.patch(h, c) }, http.MethodPatch, "/"+provider.name, `{"index":0,"value":{`+step.fields+`}}`)
					status := http.StatusOK
					if step.invalid {
						status = http.StatusBadRequest
					}
					if response.Code != status {
						t.Fatalf("PATCH: %d %s, want %d", response.Code, response.Body.String(), status)
					}
					if step.invalid {
						after, err := os.ReadFile(h.configFilePath)
						if err != nil {
							t.Fatal(err)
						}
						if !bytes.Equal(before, after) {
							t.Fatal("rejected PATCH changed persisted config")
						}
					}
					assertState(t, step.cooling, step.retry)
				})
			}
			for _, fields := range []string{`,"disable-cooling":false,"request-retry":0`, ""} {
				response := runConfigListRequest(func(c *gin.Context) { provider.put(h, c) }, http.MethodPut, "/"+provider.name, `[{`+entry+fields+`}]`)
				if response.Code != http.StatusOK {
					t.Fatalf("PUT: %d %s", response.Code, response.Body.String())
				}
				if fields == "" {
					assertState(t, nil, nil)
				} else {
					assertState(t, new(false), new(0))
				}
			}
			response := runConfigListRequest(func(c *gin.Context) { provider.delete(h, c) }, http.MethodDelete, "/"+provider.name+"?index=0", "")
			if response.Code != http.StatusOK {
				t.Fatalf("DELETE: %d %s", response.Code, response.Body.String())
			}
			response = runConfigListRequest(func(c *gin.Context) { provider.get(h, c) }, http.MethodGet, "/"+provider.name, "")
			var doc map[string][]json.RawMessage
			if err := json.Unmarshal(response.Body.Bytes(), &doc); err != nil {
				t.Fatal(err)
			}
			if len(doc[provider.name]) != 0 {
				t.Fatal("deleted entry still returned")
			}
		})
	}
}
