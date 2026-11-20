package management

import (
	"encoding/json"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
)

func TestGetAPIKeysSearchReturnsOnlyExistingMaskedReferences(t *testing.T) {
	keys := []string{"abcd-first-wxyz", "abcd-second-wxyz", "other-CASE-3333"}
	h := &Handler{cfg: &config.Config{APIKeys: keys, APIKeyAliases: map[string]string{keys[1]: "Alpha_Search"}}}
	before := maskAPIKeyList(keys)
	for _, entry := range []struct {
		name, query string
		requested   bool
		indexes     []int
	}{
		{"legacy", "", false, nil},
		{"full", keys[0], true, []int{0}},
		{"fragment", "SECOND", true, []int{1}},
		{"saved_alias_does_not_match", "alpha_search", true, nil},
		{"trimmed_fragment", "  second  ", true, []int{1}},
		{"empty", "", true, []int{0, 1, 2}},
		{"none", "no-matching-synthetic-key", true, nil},
	} {
		t.Run(
			entry.name, func(t *testing.T) {
				path := "/api-keys"
				if entry.requested {
					path += "?api_key_search=" + url.QueryEscape(entry.query)
				}
				c, response := newArtifactCacheTestContext(http.MethodGet, path, "")
				h.GetAPIKeys(c)
				var body map[string]json.RawMessage
				if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &body) != nil {
					t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
				}
				var listed []string
				if err := json.Unmarshal(body["api-keys"], &listed); err != nil || !reflect.DeepEqual(listed, before) {
					t.Fatalf("original reference list changed: %v err=%v", listed, err)
				}
				matches, exists := body["api-key-matches"]
				if exists != entry.requested {
					t.Fatalf("optional matches existence=%t", exists)
				}
				if exists {
					var actual []string
					if err := json.Unmarshal(
						matches, &actual,
					); err != nil || actual == nil || len(actual) != len(entry.indexes) {
						t.Fatalf("match array=%s err=%v", matches, err)
					}
					for i, index := range entry.indexes {
						if actual[i] != before[index] {
							t.Fatalf("match %q is not existing reference %q", actual[i], before[index])
						}
					}
				}
				for _, key := range keys {
					if strings.Contains(response.Body.String(), key) {
						t.Fatal("key query disclosed original key")
					}
				}
			},
		)
	}
}

func TestAPIKeySearchParameterUsesExistingQueryRedaction(t *testing.T) {
	masked := util.MaskSensitiveQuery("api_key_search=synthetic-secret-fragment&keep=visible")
	if strings.Contains(masked, "synthetic-secret-fragment") || !strings.Contains(masked, "keep=visible") {
		t.Fatalf("search query was not safely redacted: %q", masked)
	}
}
