package management

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	sdkAuth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/auth"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/gin-gonic/gin"
)

func TestFieldsPersistHookDoesNotBlockIndependentStatus(t *testing.T) {
	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	for _, id := range []string{"fields.json", "other.json"} {
		if _, err := manager.Register(
			context.Background(),
			&coreauth.Auth{ID: id, FileName: id, Provider: "qwen", Metadata: map[string]any{"type": "qwen"}},
		); err != nil {
			t.Fatal(err)
		}
	}
	h := NewHandler(&config.Config{}, "", manager, withTokenStore(store))
	entered, release := make(chan struct{}), make(chan struct{})
	h.SetPostAuthPersistHook(
		func(_ context.Context, auth *coreauth.Auth) error {
			if auth.ID == "fields.json" {
				close(entered)
				<-release
			}
			return nil
		},
	)
	fields, fieldsRec := newArtifactCacheTestContext(
		http.MethodPatch, "/auth-files/fields", `{"name":"fields.json","note":"updated"}`,
	)
	fieldsDone := make(chan struct{})
	go func() { defer close(fieldsDone); h.PatchAuthFileFields(fields) }()
	<-entered
	status, statusRec := newArtifactCacheTestContext(
		http.MethodPatch, "/auth-files/status", `{"name":"other.json","disabled":true}`,
	)
	statusDone := make(chan struct{})
	go func() { defer close(statusDone); h.PatchAuthFileStatus(status) }()
	blocked := false
	select {
	case <-statusDone:
	case <-time.After(time.Second):
		blocked = true
	}
	close(release)
	<-fieldsDone
	<-statusDone
	if blocked {
		t.Fatal("independent status waited for fields persist hook")
	}
	if fieldsRec.Code != http.StatusOK || statusRec.Code != http.StatusOK {
		t.Fatalf("fields=%d status=%d", fieldsRec.Code, statusRec.Code)
	}
	auth, _ := manager.GetByID("other.json")
	if auth == nil || !auth.Disabled {
		t.Fatal("independent status did not publish")
	}
}
func TestSaveTokenWithoutManagerSynthesizesPersistedHookRecord(t *testing.T) {
	dir := t.TempDir()
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: dir}, nil)
	h.tokenStore = &pluginOAuthFileStore{}
	var got *coreauth.Auth
	h.SetPostAuthPersistHook(func(_ context.Context, a *coreauth.Auth) error { got = a.Clone(); return nil })
	record := &coreauth.Auth{
		ID: "noncanonical-id", FileName: "canonical.json", Provider: "codex",
		Metadata: map[string]any{"type": "codex", "access_token": "synthetic-token", "websockets": true},
	}
	path, err := h.saveTokenRecord(context.Background(), dir, record)
	if err != nil {
		t.Fatal(err)
	}
	if got ==
		nil ||
		got.ID !=
			"canonical.json" ||
		got.FileName !=
			"canonical.json" ||
		got.Attributes["path"] !=
			filepath.Join(
				dir, "canonical.json",
			) || got.Status != coreauth.StatusActive ||
		got.Attributes["websockets"] != "true" {
		t.Fatalf("hook=%+v path=%q", got, path)
	}
}
func TestAuthFieldsRejectsSecretAlongsideSupportedField(t *testing.T) {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	claims, err := json.Marshal(
		map[string]any{
			"email":                       "user@example.com",
			"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acc-12345"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	idToken := header + "." + base64.RawURLEncoding.EncodeToString(claims) + "."
	for _, tc := range []struct{ provider, fileName, token string }{
		{"qwen", "fields.json", "replacement"},
		{"codex", "codex-test-patch-idtoken.json", idToken},
	} {
		t.Run(
			tc.provider, func(t *testing.T) {
				dir := t.TempDir()
				path := filepath.Join(dir, tc.fileName)
				store := sdkAuth.NewFileTokenStore()
				store.SetBaseDir(dir)
				manager := coreauth.NewManager(store, nil, nil)
				if _, err := manager.Register(
					t.Context(), &coreauth.Auth{
						ID: tc.fileName, FileName: tc.fileName, Provider: tc.provider,
						Attributes: map[string]string{coreauth.AttributePath: path},
						Metadata:   map[string]any{"type": tc.provider, "id_token": "original"},
					},
				); err != nil {
					t.Fatal(err)
				}
				before, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				original, _ := manager.GetByID(tc.fileName)
				h := &Handler{authManager: manager}
				for _, withNote := range []bool{false, true} {
					body := map[string]any{"name": tc.fileName, "id_token": tc.token}
					if withNote {
						body["note"] = "updated"
					}
					raw, err := json.Marshal(body)
					if err != nil {
						t.Fatal(err)
					}
					c, rec := newArtifactCacheTestContext(http.MethodPatch, "/auth-files/fields", string(raw))
					h.PatchAuthFileFields(c)
					if rec.Code != http.StatusBadRequest {
						t.Fatalf("withNote=%t status=%d body=%s", withNote, rec.Code, rec.Body.String())
					}
					current, _ := manager.GetByID(tc.fileName)
					if !reflect.DeepEqual(current, original) {
						t.Fatalf("rejected secret PATCH changed the record: %+v", current)
					}
					if after, err := os.ReadFile(path); err != nil || !bytes.Equal(before, after) {
						t.Fatalf("rejected secret PATCH changed the file: %s, %v", after, err)
					}
				}
			},
		)
	}
}
func TestAuthFieldsHeadersNoopWeightAndAliasTristate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fields.json")
	store := sdkAuth.NewFileTokenStore()
	store.SetBaseDir(dir)
	manager := coreauth.NewManager(store, nil, nil)
	record := &coreauth.Auth{
		ID: "fields.json", FileName: "fields.json", Provider: "codex",
		Attributes: map[string]string{coreauth.AttributePath: path, "header:X-Kee": "1"},
		Metadata: map[string]any{
			"type": "codex", "access_token": "synthetic-original", "headers": map[string]any{"X-Kee": "1"},
		},
	}
	if _, err := manager.Register(t.Context(), record); err != nil {
		t.Fatal(err)
	}
	h := &Handler{authManager: manager}
	for _, tc := range []struct {
		name, patch, weight string
		aliases             int
	}{
		{"headers_noop", `"headers":{},"note":"hello"`, "", 0},
		{"weight_seven", `"weight":7`, "7", 0},
		{"weight_zero", `"weight":0`, "0", 0},
		{"weight_maximum", `"weight":1000000`, "1000000", 0},
		{"weight_null", `"weight":null`, "", 0},
		{"alias_value", `"model_aliases":[{"name":"upstream","alias":"public"}]`, "", 1},
		{"alias_omitted", `"note":"aliases retained"`, "", 1},
		{"alias_empty", `"model_aliases":[]`, "", 0},
		{"alias_value_again", `"model_aliases":[{"name":"upstream","alias":"public"}]`, "", 1},
		{"alias_null", `"model_aliases":null`, "", 0},
	} {
		t.Run(
			tc.name, func(t *testing.T) {
				c, rec := newArtifactCacheTestContext(
					http.MethodPatch, "/auth-files/fields", `{"name":"fields.json",`+tc.patch+`}`,
				)
				h.PatchAuthFileFields(c)
				if rec.Code != http.StatusOK {
					t.Fatalf("patch=%s status=%d body=%s", tc.patch, rec.Code, rec.Body.String())
				}
				current, ok := manager.GetByID(record.ID)
				if !ok || current.Attributes["header:X-Kee"] != "1" ||
					coreauth.ExtractCustomHeadersFromMetadata(current.Metadata)["X-Kee"] != "1" {
					t.Fatalf("runtime headers changed: %+v", current)
				}
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var persisted map[string]any
				if err := json.Unmarshal(raw, &persisted); err != nil {
					t.Fatal(err)
				}
				if persisted["access_token"] != "synthetic-original" ||
					coreauth.ExtractCustomHeadersFromMetadata(persisted)["X-Kee"] != "1" {
					t.Fatalf("persisted unrelated fields changed: %s", raw)
				}
				reloaded, _, err := authFromFile(dir, path, raw)
				if err != nil || reloaded == nil {
					t.Fatalf("reload error=%v auth=%+v", err, reloaded)
				}
				for _, auth := range []*coreauth.Auth{current, reloaded} {
					weight, present := auth.Metadata[coreauth.AttributeWeight]
					if present != (tc.weight != "") || auth.Attributes[coreauth.AttributeWeight] != tc.weight {
						t.Fatalf("weight metadata=%#v attributes=%#v", auth.Metadata, auth.Attributes)
					}
					if present {
						encoded, err := json.Marshal(weight)
						if err != nil || string(encoded) != tc.weight {
							t.Fatalf("weight JSON=%s error=%v", encoded, err)
						}
					}
					aliases := coreauth.OAuthModelAliasesFromAttributes(auth.Attributes)
					_, present = auth.Metadata["model_aliases"]
					if len(aliases) != tc.aliases || present != (tc.aliases > 0) ||
						tc.aliases > 0 && (aliases[0].Name != "upstream" || aliases[0].Alias != "public") {
						t.Fatalf("aliases=%#v metadata=%#v", aliases, auth.Metadata)
					}
				}
			},
		)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, weight := range []string{"1000001", "1.5", "9223372036854775808", `"7"`} {
		c, rec := newArtifactCacheTestContext(
			http.MethodPatch, "/auth-files/fields", `{"name":"fields.json","weight":`+weight+`}`,
		)
		h.PatchAuthFileFields(c)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("weight=%s status=%d", weight, rec.Code)
		}
		if after, err := os.ReadFile(path); err != nil || !bytes.Equal(before, after) {
			t.Fatalf("invalid weight changed the file: %s, %v", after, err)
		}
		current, _ := manager.GetByID(record.ID)
		if _, present := current.Metadata[coreauth.AttributeWeight]; present ||
			current.Attributes[coreauth.AttributeWeight] != "" {
			t.Fatalf("invalid weight changed runtime state: %+v", current)
		}
	}
}
func TestConfigValidationRejectsInvalidPortAtHTTPEntry(t *testing.T) {
	for _, port := range []int{-1, 65536, 70000} {
		t.Run(
			strconv.Itoa(port), func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "config.yaml")
				const good = "port: 8317\n"
				for _, target := range []string{path, config.LastGoodPath(path)} {
					if err := os.WriteFile(target, []byte(good), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				h := NewHandler(&config.Config{Port: 8317}, path, nil, withEnvSecret(""))
				for _, entry := range []struct {
					method, route string
					handle        func(*gin.Context)
					status        int
				}{
					{http.MethodPost, "/v0/management/config.yaml/validate", h.ValidateConfigYAML, http.StatusOK},
					{http.MethodPut, "/v0/management/config.yaml", h.PutConfigYAML, http.StatusUnprocessableEntity},
				} {
					rec := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(rec)
					body := "port: " + strconv.Itoa(port) + "\n"
					c.Request = httptest.NewRequest(entry.method, entry.route, strings.NewReader(body))
					entry.handle(c)
					if rec.Code != entry.status {
						t.Fatalf("%s status=%d body=%s", entry.method, rec.Code, rec.Body.String())
					}
					if entry.method == http.MethodPost {
						var response struct {
							Valid  bool                    `json:"valid"`
							Errors []configValidationError `json:"errors"`
						}
						if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
							t.Fatal(err)
						}
						if response.Valid || len(response.Errors) != 1 || response.Errors[0].Field != "port" {
							t.Fatalf("invalid configuration validation result: %s", rec.Body.String())
						}
					}
					if h.configSnapshot().Port != 8317 {
						t.Fatalf("%s published the invalid port", entry.method)
					}
					for _, target := range []string{path, config.LastGoodPath(path)} {
						if data, err := os.ReadFile(target); err != nil || string(data) != good {
							t.Fatalf(
								"%s changed saved configuration: data=%q error=%v",
								entry.method,
								data,
								err,
							)
						}
					}
				}
			},
		)
	}
}
