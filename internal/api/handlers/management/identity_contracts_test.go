package management

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/gin-gonic/gin"
)

func TestAPIKeyUsagePreservesCompatIdentityAndMaskedCollisions(t *testing.T) {
	const first = "sk-synthetic-first-secret-tail"
	const second = "sk-synthetic-second-secret-tail"
	const base = "https://synthetic.invalid"
	if util.HideAPIKey(first) != util.HideAPIKey(second) {
		t.Fatal("fixture does not exercise a display-mask collision")
	}
	manager := coreauth.NewManager(nil, nil, nil)
	for _, entry := range []struct {
		id, provider, compat, key string
		success                   bool
	}{
		{"first", "openai-compatibility", "CustomA", first, true},
		{"second", "openai-compatibility", "CustomA", second, false},
		{"same-key", "openai-compatibility", "CustomA", first, true},
		{"other-compat", "openai-compatibility", "CustomB", first, false},
		{"codex", "codex", "", first, true},
		{"gemini", "gemini", "", first, false},
	} {
		if _, err := manager.Register(
			t.Context(), &coreauth.Auth{
				ID: entry.id, Provider: entry.provider,
				Attributes: map[string]string{"api_key": entry.key, "base_url": base, "compat_name": entry.compat},
			},
		); err != nil {
			t.Fatal(err)
		}
		manager.MarkResult(
			t.Context(), coreauth.Result{
				AuthID: entry.id, Provider: entry.provider, Model: "synthetic-model", Success: entry.success,
			},
		)
	}
	h := &Handler{authManager: manager}
	var previous string
	for range 8 {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodGet, "/v0/management/api-key-usage", nil)
		h.GetAPIKeyUsage(c)
		if recorder.Code != http.StatusOK || strings.Contains(
			recorder.Body.String(), first,
		) || strings.Contains(recorder.Body.String(), second) {
			t.Fatalf("usage status=%d body=%s", recorder.Code, recorder.Body.String())
		}
		if previous != "" && previous != recorder.Body.String() {
			t.Fatal("masked collision identities changed between reads")
		}
		previous = recorder.Body.String()
		var payload map[string]map[string]apiKeyUsageEntry
		if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		//goland:noinspection SpellCheckingInspection
		if len(payload) != 4 || len(payload["customa"]) != 2 || len(payload["customb"]) != 1 ||
			len(payload["codex"]) != 1 ||
			len(payload["gemini"]) != 1 {
			t.Fatalf("business identity groups=%v", payload)
		}
		legacyKey := base + "|" + util.HideAPIKey(first)
		//goland:noinspection SpellCheckingInspection
		for _, provider := range []string{"customb", "codex", "gemini"} {
			if _, exists := payload[provider][legacyKey]; !exists {
				t.Fatalf("non-colliding public key changed for %s", provider)
			}
		}
		seenSuccess, seenFailure := false, false
		//goland:noinspection SpellCheckingInspection
		for key, entry := range payload["customa"] {
			if !strings.HasPrefix(key, legacyKey+"|") || len(entry.RecentRequests) != 24 {
				t.Fatalf("collision key=%q bucket count=%d", key, len(entry.RecentRequests))
			}
			success, failed := sumRecentRequestBuckets(entry.RecentRequests)
			switch {
			case entry.Success == 2 && entry.Failed == 0 && success == 2 && failed == 0:
				seenSuccess = true
			case entry.Success == 0 && entry.Failed == 1 && success == 0 && failed == 1:
				seenFailure = true
			default:
				t.Fatalf("distinct API keys merged or identical API keys separated: %+v", entry)
			}
		}
		if !seenSuccess || !seenFailure {
			t.Fatal("masked collision lost a credential's counters")
		}
	}
}

func TestAuditAPIKeyUsageMaskCollision(t *testing.T) {
	for _, test := range []struct {
		name        string
		left, right string
		groups      int
	}{
		{"different-short-keys", "alpha1", "bravo2", 2},
		{"different-long-keys-same-display-mask", "same-alpha-middle-tail", "same-bravo-middle-tail", 2},
		{"same-key-control", "same-shared-key-tail", "same-shared-key-tail", 1},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				if util.HideAPIKey(test.left) != util.HideAPIKey(test.right) {
					t.Fatal("original keys no longer share a display mask")
				}
				manager := coreauth.NewManager(nil, nil, nil)
				for index, key := range []string{test.left, test.right} {
					id := fmt.Sprintf("synthetic-usage-%d", index)
					if _, err := manager.Register(
						t.Context(), &coreauth.Auth{
							ID: id, Provider: "codex",
							Attributes: map[string]string{
								"api_key": key, "base_url": "https://synthetic.example.test/v1",
							},
						},
					); err != nil {
						t.Fatal(err)
					}
					manager.MarkResult(
						t.Context(),
						coreauth.Result{AuthID: id, Provider: "codex", Model: "synthetic-model", Success: true},
					)
				}
				if len(manager.List()) != 2 {
					t.Fatal("manager did not retain both auth identities")
				}
				recorder := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(recorder)
				c.Request = httptest.NewRequest(http.MethodGet, "/v0/management/api-key-usage", nil)
				(&Handler{authManager: manager}).GetAPIKeyUsage(c)
				if recorder.Code != http.StatusOK {
					t.Fatalf("usage status = %d, body = %s", recorder.Code, recorder.Body.String())
				}
				var payload map[string]map[string]apiKeyUsageEntry
				if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
					t.Fatal(err)
				}
				if len(payload["codex"]) != test.groups {
					t.Fatalf("API key groups = %d, want %d", len(payload["codex"]), test.groups)
				}
				var successes int64
				for _, entry := range payload["codex"] {
					successes += entry.Success
				}
				if successes != 2 || strings.Contains(
					recorder.Body.String(), test.left,
				) || strings.Contains(recorder.Body.String(), test.right) {
					t.Fatalf("usage totals or key privacy changed: %s", recorder.Body.String())
				}
			},
		)
	}
}

func TestAuthFileSameNameExplicitIndexAndConcurrentReads(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	for _, id := range []string{"first", "second"} {
		if _, err := manager.Register(
			t.Context(), &coreauth.Auth{
				ID: id, Index: "public-" + id, FileName: "shared.json", Provider: "qwen",
				Attributes: map[string]string{"runtime_only": "true"},
			},
		); err != nil {
			t.Fatal(err)
		}
	}
	h := &Handler{authManager: manager}
	for _, id := range []string{"first", "second"} {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodGet, "/auth-files?name=shared.json&auth_index=public-"+id, nil)
		h.ListAuthFiles(c)
		var response struct {
			Files []map[string]any `json:"files"`
		}
		if err := json.Unmarshal(
			recorder.Body.Bytes(), &response,
		); err != nil || recorder.Code != http.StatusOK || len(response.Files) != 1 || response.Files[0]["id"] != id {
			t.Fatalf(
				"same-name lookup id=%s status=%d body=%s error=%v", id, recorder.Code, recorder.Body.String(), err,
			)
		}
		recorder = httptest.NewRecorder()
		c, _ = gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(
			http.MethodPatch, "/auth-files/status",
			strings.NewReader(fmt.Sprintf(`{"name":"shared.json","auth_index":"public-%s","disabled":true}`, id)),
		)
		c.Request.Header.Set("Content-Type", "application/json")
		h.PatchAuthFileStatus(c)
		if recorder.Code != http.StatusOK {
			t.Fatalf("same-name status id=%s status=%d body=%s", id, recorder.Code, recorder.Body.String())
		}
		current, _ := manager.GetByID(id)
		if !current.Disabled || current.Index != "public-"+id {
			t.Fatalf("selected identity was not updated: %+v", current)
		}
	}
	failures := make(chan error, 32)
	var workers sync.WaitGroup
	for range 32 {
		workers.Go(
			func() {
				for range 32 {
					for _, id := range []string{"first", "second"} {
						current := h.authByIndex("public-" + id)
						if current == nil || current.ID != id || current.Index != "public-"+id {
							failures <- fmt.Errorf("explicit index read changed identity %s", id)
							return
						}
					}
				}
			},
		)
	}
	workers.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	for _, id := range []string{"first", "second"} {
		current, _ := manager.GetByID(id)
		if current.Index != "public-"+id {
			t.Fatal("concurrent reads rewrote a published explicit Index")
		}
	}
}
