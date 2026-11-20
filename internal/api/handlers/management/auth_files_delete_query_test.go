package management

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/quota"
	sdkauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/auth"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestDeleteAuthFilesConsumesRepeatedQueryNamesWithExistingBatchSemantics(t *testing.T) {
	for _, name := range []string{
		"query_batch", "query_partial", "json_precedence", "single_query", "all_confirmation",
	} {
		t.Run(
			name, func(t *testing.T) {
				dir := t.TempDir()
				store := sdkauth.NewFileTokenStore()
				store.SetBaseDir(dir)
				manager := coreauth.NewManager(store, nil, nil)
				h := &Handler{cfg: &config.Config{AuthDir: dir}, tokenStore: store, authManager: manager}
				engine := resourceMutationEngine(t, h)
				engine.POST("/auth-files", h.UploadAuthFile)
				engine.DELETE("/auth-files", h.DeleteAuthFile)
				for _, file := range []string{"first.json", "second.json", "third.json"} {
					callResourceMutation(
						t, engine, http.MethodPost, "/auth-files?name="+file,
						`{"type":"codex","access_token":"synthetic-delete-query"}`, 200, "succeeded",
						"credential.persisted", "credential.published",
					)
				}
				path, body := "/auth-files?name=first.json&name=second.json", ""
				status, outcome := 200, "succeeded"
				removed := []string{"first.json", "second.json"}
				switch name {
				case "query_partial":
					path, status, outcome = "/auth-files?name=first.json&name=missing.json", 207, "partial"
					removed = []string{"first.json"}
				case "json_precedence":
					body, removed = `{"names":["third.json"]}`, []string{"third.json"}
				case "single_query":
					path, removed = "/auth-files?name=first.json", []string{"first.json"}
				case "all_confirmation":
					path, status, outcome,
						removed = "/auth-files?all=true&name=first.json&name=second.json", 400, "uncertain", nil
				}
				var kinds []string
				if len(removed) > 0 {
					kinds = []string{"credential.deleted"}
				}
				callResourceMutation(t, engine, http.MethodDelete, path, body, status, outcome, kinds...)
				for _, file := range []string{"first.json", "second.json", "third.json"} {
					deleted := false
					for _, target := range removed {
						deleted = deleted || file == target
					}
					_, err := os.Stat(filepath.Join(dir, file))
					_, registered := manager.GetByID(file)
					if deleted != errors.Is(err, os.ErrNotExist) || registered == deleted {
						t.Fatalf("file=%s deleted=%t stat=%v registered=%t", file, deleted, err, registered)
					}
				}
			},
		)
	}
}

func TestDeleteAuthFileLegacyRequestShapes(t *testing.T) {
	for _, tc := range []struct {
		name, query, body string
		status            int
		removed, failed   []string
	}{
		{"json_array", "", `["first.json","second.json"]`, 200, []string{"first.json", "second.json"}, nil},
		{"json_name", "", `{"name":" first.json "}`, 200, []string{"first.json"}, nil},
		{
			"json_name_and_names", "", `{"name":"first.json","names":["second.json","first.json",""]}`, 200,
			[]string{"first.json", "second.json"}, nil,
		},
		{"json_blank_name_and_names", "", `{"name":" ","names":["third.json"]}`, 200, []string{"third.json"}, nil},
		{
			"json_array_duplicates", "", `[" first.json ","","first.json","second.json"]`, 200,
			[]string{"first.json", "second.json"}, nil,
		},
		{"json_names_duplicates", "", `{"names":["first.json","first.json",""]}`, 200, []string{"first.json"}, nil},
		{"query_duplicates", "?name=first.json&name=first.json", "", 200, []string{"first.json"}, nil},
		{
			"query_blank_entries", "?name=%20&name=first.json&name=&name=first.json", "", 200, []string{"first.json"},
			nil,
		},
		{
			"query_ignores_malformed_body", "?name=first.json&name=second.json", `{`, 200,
			[]string{"first.json", "second.json"}, nil,
		},
		{"query_single_ignores_malformed_body", "?name=first.json", `{`, 200, []string{"first.json"}, nil},
		{"json_special_name", "", `{"name":"Case+user.json"}`, 200, []string{"Case+user.json"}, nil},
		{"query_special_name", "?name=Case%2Buser.json", "", 200, []string{"Case+user.json"}, nil},
		{"array_partial", "", `["first.json","missing.json"]`, 207, []string{"first.json"}, []string{"missing.json"}},
		{
			"combined_partial", "", `{"name":"missing.json","names":["second.json"]}`, 207, []string{"second.json"},
			[]string{"missing.json"},
		},
		{
			"array_traversal", "", `["first.json","../second.json"]`, 207, []string{"first.json"},
			[]string{"../second.json"},
		},
		{
			"query_traversal", "?name=first.json&name=..%2Fsecond.json", "", 207, []string{"first.json"},
			[]string{"../second.json"},
		},
		{"json_name_missing", "", `{"name":"missing.json"}`, 404, nil, nil},
		{"json_array_single_missing", "", `["missing.json"]`, 404, nil, nil},
		{"json_names_single_missing", "", `{"names":["missing.json"]}`, 207, nil, []string{"missing.json"}},
		{"all_array_confirmation", "?all=true", `["first.json","second.json"]`, 400, nil, nil},
		{"all_object_confirmation", "?all=1", `{"name":"first.json"}`, 400, nil, nil},
		{"empty_body", "", "", 400, nil, nil},
		{"empty_array", "", `[]`, 400, nil, nil},
		{"empty_names", "", `{"names":[" ",""]}`, 400, nil, nil},
		{"malformed_json", "", `{"name":`, 400, nil, nil},
		{"array_wrong_type", "", `["first.json",2]`, 400, nil, nil},
		{"name_wrong_type", "", `{"name":["first.json"]}`, 400, nil, nil},
		{"names_wrong_type", "", `{"names":"first.json"}`, 400, nil, nil},
		{"scalar_body", "", `"first.json"`, 400, nil, nil},
		{"trailing_json", "", `{"name":"first.json"}{}`, 400, nil, nil},
	} {
		t.Run(
			tc.name, func(t *testing.T) {
				dir := t.TempDir()
				store := sdkauth.NewFileTokenStore()
				store.SetBaseDir(dir)
				manager := coreauth.NewManager(store, nil, nil)
				scheduler := quota.NewScheduler(
					quota.DefaultConfig(), func(*quota.Entry) ([]byte, error) {
						t.Fatal("deletion must not fetch provider quota")
						return nil, nil
					},
				)
				h := &Handler{
					cfg: &config.Config{AuthDir: dir}, tokenStore: store, authManager: manager,
					quotaScheduler: scheduler,
				}
				engine := resourceMutationEngine(t, h)
				engine.POST("/auth-files", h.UploadAuthFile)
				engine.DELETE("/auth-files", h.DeleteAuthFile)
				files := []string{"first.json", "second.json", "third.json", "Case+user.json"}
				for _, file := range files {
					callResourceMutation(
						t, engine, http.MethodPost, "/auth-files?name="+strings.ReplaceAll(file, "+", "%2B"),
						`{"type":"codex","access_token":"synthetic-legacy-delete"}`, 200, "succeeded",
						"credential.persisted", "credential.published",
					)
					if _, present := scheduler.GetStatus().Credentials[quota.EntryKey(
						quota.TypeCodex, file,
					)]; !present {
						t.Fatalf("uploaded file %s has no quota binding", file)
					}
				}
				request := httptest.NewRequest(
					http.MethodDelete,
					"/auth-files"+tc.query,
					strings.NewReader(tc.body),
				)
				request.Header.Set("Content-Type", "application/json")
				response := httptest.NewRecorder()
				engine.ServeHTTP(response, request)
				result := decodeMutationHeader(t, response)
				outcome := "uncertain"
				if tc.status == http.StatusOK {
					outcome = "succeeded"
				} else if len(tc.removed) > 0 && len(tc.failed) > 0 {
					outcome = "partial"
				}
				if response.Code != tc.status || result.Outcome != outcome ||
					result.ConfirmedEffect != (len(tc.removed) > 0) {
					t.Fatalf("status=%d body=%s result=%+v", response.Code, response.Body.String(), result)
				}
				var payload struct {
					Deleted int      `json:"deleted"`
					Files   []string `json:"files"`
					Failed  []struct {
						Name string `json:"name"`
					} `json:"failed"`
				}
				if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
					t.Fatal(err)
				}
				failed := make([]string, 0, len(payload.Failed))
				for _, entry := range payload.Failed {
					failed = append(failed, entry.Name)
				}
				if payload.Deleted != len(tc.removed) || !slices.Equal(
					payload.Files, tc.removed,
				) || !slices.Equal(failed, tc.failed) {
					t.Fatalf("deleted=%d files=%v failed=%v", payload.Deleted, payload.Files, failed)
				}
				for _, file := range files {
					deleted := slices.Contains(tc.removed, file)
					_, err := os.Stat(filepath.Join(dir, file))
					_, registered := manager.GetByID(file)
					_, queued := scheduler.GetStatus().Credentials[quota.EntryKey(quota.TypeCodex, file)]
					if deleted != errors.Is(err, os.ErrNotExist) || registered == deleted || queued == deleted {
						t.Fatalf(
							"file=%s deleted=%t stat=%v registered=%t queued=%t",
							file,
							deleted,
							err,
							registered,
							queued,
						)
					}
				}
			},
		)
	}
}
