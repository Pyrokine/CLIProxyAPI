package management

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/gin-gonic/gin"
)

func TestManagementLookupRetainsLegacyAPIKeyIndexReferences(t *testing.T) {
	for _, entry := range []struct{ provider, prefix, compat string }{
		{"gemini", "gemini-api-key", ""},
		{"claude", "claude-api-key", ""},
		{"codex", "codex-api-key", ""},
		{"custom", "openai-compatibility", "Custom"},
	} {
		t.Run(
			entry.provider, func(t *testing.T) {
				manager := coreauth.NewManager(nil, nil, nil)
				auth, err := manager.Register(
					t.Context(), &coreauth.Auth{
						ID: entry.provider + "-identity", Provider: entry.provider, Status: coreauth.StatusActive,
						Attributes: map[string]string{
							"runtime_only": "true", "api_key": "synthetic-key",
							"base_url": "https://synthetic.invalid/v1",
							"source":   "synthetic-first-source", "compat_name": entry.compat,
						},
					},
				)
				if err != nil {
					t.Fatal(err)
				}
				sum := sha256.Sum256([]byte(entry.prefix + ":https://synthetic.invalid/v1+synthetic-key"))
				legacyIndex := hex.EncodeToString(sum[:8])
				if auth.Index == "" || auth.Index == legacyIndex {
					t.Fatal("fixture must retain a distinct current source-aware index")
				}
				h := &Handler{authManager: manager}
				for _, suffix := range []string{"", "&page=1&page_size=1"} {
					t.Run(
						"list"+suffix, func(t *testing.T) {
							rec := httptest.NewRecorder()
							ctx, _ := gin.CreateTestContext(rec)
							ctx.Request = httptest.NewRequest(
								http.MethodGet, "/auth-files?name="+auth.ID+"&auth_index="+legacyIndex+suffix, nil,
							)
							h.ListAuthFiles(ctx)
							var payload struct {
								Files []struct {
									ID        string `json:"id"`
									AuthIndex string `json:"auth_index"`
								} `json:"files"`
							}
							if err := json.Unmarshal(
								rec.Body.Bytes(), &payload,
							); err != nil || rec.Code != http.StatusOK || len(payload.Files) != 1 {
								t.Fatalf("legacy list: status=%d body=%s error=%v", rec.Code, rec.Body.String(), err)
							}
							if payload.Files[0].ID != auth.ID || payload.Files[0].AuthIndex != auth.Index {
								t.Fatalf("legacy lookup changed the published identity: %+v", payload.Files[0])
							}
						},
					)
				}
				if selected := h.authByIndex(legacyIndex); selected == nil || selected.ID != auth.ID ||
					selected.Index != auth.Index {
					t.Fatalf("legacy credential consumer selected %+v", selected)
				}
				for _, request := range []struct {
					path, body string
					handle     func(*gin.Context)
				}{
					{
						"/auth-files", `{"auth-index":"` + legacyIndex + `","prefix":"legacy-reference"}`,
						h.PatchAuthFileFields,
					},
					{
						"/auth-files/status", `{"auth_index":"` + legacyIndex + `","disabled":true}`,
						h.PatchAuthFileStatus,
					},
				} {
					rec := httptest.NewRecorder()
					ctx, _ := gin.CreateTestContext(rec)
					ctx.Request = httptest.NewRequest(http.MethodPatch, request.path, strings.NewReader(request.body))
					ctx.Request.Header.Set("Content-Type", "application/json")
					request.handle(ctx)
					if rec.Code != http.StatusOK {
						t.Fatalf("legacy mutation %s: status=%d body=%s", request.path, rec.Code, rec.Body.String())
					}
				}
				current, _ := manager.GetByID(auth.ID)
				if current.Index != auth.Index || !current.Disabled || current.Prefix != "legacy-reference" {
					t.Fatalf("legacy mutation lost identity or changes: %+v", current)
				}
			},
		)
	}
}

func TestManagementLegacyIndexCollisionsRequireExactIdentity(t *testing.T) {
	for _, delimiterCollision := range []bool{false, true} {
		name := "same key from different sources"
		if delimiterCollision {
			name = "legacy plus delimiter"
		}
		t.Run(
			name, func(t *testing.T) {
				manager := coreauth.NewManager(nil, nil, nil)
				var auths []*coreauth.Auth
				for _, id := range []string{"first", "second"} {
					baseURL, key := "https://synthetic.invalid/v1", "synthetic-key"
					if delimiterCollision {
						baseURL, key = "https://synthetic.invalid/v1+", "synthetic-key"
						if id == "second" {
							baseURL, key = "https://synthetic.invalid/v1", "+synthetic-key"
						}
					}
					auth, err := manager.Register(
						t.Context(), &coreauth.Auth{
							ID: id, Provider: "gemini", Status: coreauth.StatusActive,
							Attributes: map[string]string{
								"runtime_only": "true", "api_key": key, "base_url": baseURL,
								"source": "synthetic-source-" + id,
							},
						},
					)
					if err != nil {
						t.Fatal(err)
					}
					auths = append(auths, auth)
				}
				if auths[0].Index == auths[1].Index {
					t.Fatal("current indexes must preserve source and tuple boundaries")
				}
				sum := sha256.Sum256(
					[]byte("gemini-api-key:" + auths[0].Attributes["base_url"] + "+" + auths[0].Attributes["api_key"]),
				)
				legacyIndex := hex.EncodeToString(sum[:8])
				h := &Handler{authManager: manager}
				if selected := h.authByIndex(legacyIndex); selected != nil {
					t.Fatalf("ambiguous legacy reference selected %q", selected.ID)
				}
				for _, request := range []struct {
					body string
					code int
				}{
					{`{"auth_index":"` + legacyIndex + `","disabled":true}`, http.StatusNotFound},
					{`{"name":"first","auth_index":"` + legacyIndex + `","disabled":true}`, http.StatusOK},
				} {
					rec := httptest.NewRecorder()
					ctx, _ := gin.CreateTestContext(rec)
					ctx.Request = httptest.NewRequest(
						http.MethodPatch, "/auth-files/status", strings.NewReader(request.body),
					)
					ctx.Request.Header.Set("Content-Type", "application/json")
					h.PatchAuthFileStatus(ctx)
					if rec.Code != request.code {
						t.Fatalf("identity disambiguation: status=%d body=%s", rec.Code, rec.Body.String())
					}
				}
				first, _ := manager.GetByID("first")
				second, _ := manager.GetByID("second")
				if !first.Disabled || second.Disabled || first.Index != auths[0].Index ||
					second.Index != auths[1].Index {
					t.Fatalf("legacy reference changed the wrong identity: first=%+v second=%+v", first, second)
				}
				for _, auth := range auths {
					if selected := h.authByIndex(auth.Index); selected == nil || selected.ID != auth.ID {
						t.Fatalf("current exact index lost identity %q", auth.ID)
					}
				}
			},
		)
	}
}

func TestManagementLegacyReferencePreservesPublishedIndexPriority(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	sum := sha256.Sum256([]byte("gemini-api-key:https://synthetic.invalid/v1+synthetic-key"))
	legacyIndex := hex.EncodeToString(sum[:8])
	for _, auth := range []*coreauth.Auth{
		{
			ID: "derived", Provider: "gemini", Status: coreauth.StatusActive, Attributes: map[string]string{
				"runtime_only": "true", "api_key": "synthetic-key", "base_url": "https://synthetic.invalid/v1",
				"source": "synthetic-source",
			},
		},
		{
			ID: "published", Index: legacyIndex, Provider: "gemini", Status: coreauth.StatusActive,
			Attributes: map[string]string{"runtime_only": "true"},
		},
	} {
		if _, err := manager.Register(t.Context(), auth); err != nil {
			t.Fatal(err)
		}
	}
	h := &Handler{authManager: manager}
	if selected := h.authByIndex(legacyIndex); selected == nil || selected.ID != "published" {
		t.Fatalf("legacy alias overrode a published index: %+v", selected)
	}
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/auth-files?auth_index="+legacyIndex, nil)
	h.ListAuthFiles(ctx)
	var payload struct {
		Files []struct {
			ID string `json:"id"`
		} `json:"files"`
	}
	if err := json.Unmarshal(
		rec.Body.Bytes(), &payload,
	); err != nil || rec.Code != http.StatusOK || len(payload.Files) != 1 || payload.Files[0].ID != "published" {
		t.Fatalf("published index list: status=%d body=%s error=%v", rec.Code, rec.Body.String(), err)
	}
}
