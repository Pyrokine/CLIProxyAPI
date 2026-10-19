package management

import (
	"os"
	"path/filepath"
	"testing"

	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestSortedPaginatedAuthsFiltersHiddenEntriesAndBreaksNameTies(t *testing.T) {
	dir := t.TempDir()
	sharedPath := filepath.Join(dir, "shared.json")
	if err := os.WriteFile(sharedPath, []byte(`{"type":"codex"}`), 0o600); err != nil {
		t.Fatalf("write credential fixture: %v", err)
	}
	auths := []*coreauth.Auth{
		{ID: "auth-b", FileName: "shared.json", Attributes: map[string]string{"path": sharedPath}},
		{ID: "auth-a", FileName: "shared.json", Attributes: map[string]string{"path": sharedPath}},
		{ID: "removed", FileName: "removed.json", Disabled: true, Attributes: map[string]string{"path": filepath.Join(dir, "removed.json")}},
		{ID: "missing-path", FileName: "missing-path.json"},
		{ID: "disabled-runtime", FileName: "disabled-runtime.json", Disabled: true, Attributes: map[string]string{"runtime_only": "true"}},
	}
	page, total, end := sortedPaginatedAuths(auths, "shared.json", "", authFilesPagination{enabled: true, page: 2, pageSize: 1})
	if total != 2 || end != 2 || len(page) != 1 || page[0].ID != "auth-b" {
		t.Fatalf("second shared credential page = %#v, total=%d, end=%d", page, total, end)
	}
	page, total, end = sortedPaginatedAuths(auths, "", "", authFilesPagination{enabled: true, page: 1, pageSize: 50})
	if total != 2 || end != 2 || len(page) != 2 {
		t.Fatalf("visible credentials = %#v, total=%d, end=%d", page, total, end)
	}
}
