package management

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/gin-gonic/gin"
)

func TestAuthFilesPaginationPreservesLegacyListAndFilters(t *testing.T) {
	dir := t.TempDir()
	manager := coreauth.NewManager(nil, nil, nil)
	for i, name := range []string{"zeta.json", "alpha.json", "beta.json"} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(`{"type":"codex"}`), 0o600); err != nil {
			t.Fatalf("create credential fixture: %v", err)
		}
		if _, err := manager.Register(context.Background(), &coreauth.Auth{
			ID: fmt.Sprintf("auth-%d", i), Provider: "codex", FileName: name,
			Attributes: map[string]string{"path": path}, Metadata: map[string]any{"type": "codex"},
		}); err != nil {
			t.Fatalf("register credential fixture: %v", err)
		}
	}
	h := &Handler{authManager: manager}
	testAuthFileListPages(t, h, true)
	testAuthFileListPages(t, &Handler{cfg: &config.Config{AuthDir: dir}}, false)
}

func testAuthFileListPages(t *testing.T, h *Handler, manager bool) {
	t.Helper()
	get := func(query string) (int, map[string]json.RawMessage) {
		t.Helper()
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/auth-files"+query, nil)
		h.ListAuthFiles(ctx)
		var response map[string]json.RawMessage
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatalf("decode auth files response: %v", err)
		}
		return recorder.Code, response
	}
	for _, tc := range []struct {
		query    string
		code     int
		name     string
		hasMore  bool
		allFiles bool
	}{
		{query: "?page=1&page_size=1", code: http.StatusOK, name: "alpha.json", hasMore: true},
		{query: "?page=2&page_size=1", code: http.StatusOK, name: "beta.json", hasMore: true},
		{query: "?page=3&page_size=1", code: http.StatusOK, name: "zeta.json", hasMore: false},
		{query: "?page=99999999&page_size=1", code: http.StatusOK, hasMore: false},
		{query: fmt.Sprintf("?page=2&page_size=%d", int(^uint(0)>>1)), code: http.StatusOK, hasMore: false},
		{query: "", code: http.StatusOK, allFiles: true},
		{query: "?page=0", code: http.StatusBadRequest},
		{query: "?page=", code: http.StatusBadRequest},
		{query: "?page_size=0", code: http.StatusBadRequest},
		{query: "?page_size=no", code: http.StatusBadRequest},
		{query: "?page=9223372036854775808", code: http.StatusBadRequest},
	} {
		t.Run(fmt.Sprintf("manager=%t,%s", manager, tc.query), func(t *testing.T) {
			code, response := get(tc.query)
			if code != tc.code {
				t.Fatalf("status %d, response %s", code, response)
			}
			if code != http.StatusOK {
				return
			}
			var files []struct {
				Name string `json:"name"`
			}
			if err := json.Unmarshal(response["files"], &files); err != nil {
				t.Fatalf("decode files: %v", err)
			}
			if tc.allFiles {
				if len(files) != 3 || response["total"] != nil || response["page"] != nil {
					t.Fatalf("legacy response changed: %s", response)
				}
				return
			}
			if len(files) != 0 && (len(files) != 1 || files[0].Name != tc.name) ||
				(tc.name != "" && len(files) != 1) || string(response["total"]) != "3" ||
				string(response["has_more"]) != fmt.Sprint(tc.hasMore) {
				t.Fatalf("page %q = %s", tc.query, response)
			}
		})
	}
	code, filtered := get("?page=1&page_size=1&name=beta.json")
	if code != http.StatusOK || string(filtered["total"]) != "1" || !strings.Contains(string(filtered["files"]), "beta.json") {
		t.Fatalf("pagination filter response = %d, %s", code, filtered)
	}
	if !manager {
		code, indexed := get("?page=1&page_size=1&auth_index=unknown")
		if code != http.StatusOK || string(indexed["total"]) != "0" || string(indexed["files"]) != "[]" {
			t.Fatalf("disk auth_index response = %d, %s", code, indexed)
		}
		return
	}
	var files []struct {
		Index string `json:"auth_index"`
	}
	if err := json.Unmarshal(filtered["files"], &files); err != nil || len(files) != 1 || files[0].Index == "" {
		t.Fatalf("missing auth_index in filtered response: %s, %v", filtered, err)
	}
	code, indexed := get("?page=1&page_size=1&auth_index=" + url.QueryEscape(files[0].Index))
	if code != http.StatusOK || string(indexed["total"]) != "1" || !strings.Contains(string(indexed["files"]), "beta.json") {
		t.Fatalf("auth_index response = %d, %s", code, indexed)
	}
}
