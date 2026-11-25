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

func TestGetAPIKeysSearchMatchesOnlyVisibleFields(t *testing.T) {
	keys := []string{"abcd-first-wxyz", "abcd-second-wxyz", "other-CASE-3333"}
	h := &Handler{
		cfg: &config.Config{
			SDKConfig: config.SDKConfig{
				APIKeys: keys, APIKeyAliases: map[string]string{keys[1]: "Alpha_Search"},
			},
		},
	}
	before := maskAPIKeyList(keys)
	for _, entry := range []struct {
		name, query string
		requested   bool
		indexes     []int
	}{
		{"legacy", "", false, nil},
		{"hidden_value", keys[0], true, nil},
		{"hidden_fragment", "SECOND", true, nil},
		{"visible_fragment", "  WXYZ  ", true, []int{0, 1}},
		{"visible_reference", before[1], true, []int{1}},
		{"saved_alias", "alpha_search", true, []int{1}},
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

func TestAPIKeySearchParameterConcealsEntireValue(t *testing.T) {
	masked := util.MaskSensitiveQuery("api_key_search=synthetic-secret-fragment&keep=visible")
	want := "api_key_search=" + url.QueryEscape("***") + "&keep=visible"
	if masked != want {
		t.Fatal("search query must fully redact the search term and preserve unrelated parameters")
	}
}
