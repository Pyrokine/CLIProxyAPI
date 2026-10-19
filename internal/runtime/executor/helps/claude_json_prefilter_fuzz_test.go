package helps

import (
	"encoding/json"
	"strings"
	"testing"
)

func FuzzClaudeJSONPrefilterNoFalseNegatives(f *testing.F) {
	for _, seed := range []string{
		`{"system":"naming a coding session"}`,
		`{"output_config":{"format":{"schema":{"properties":{"title":{}}}}}}`,
		`{"tools":[{"cache_control":{"ttl":"1h"}}]}`,
		`{"system":"ordinary request"}`,
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		var document any
		if err := json.Unmarshal(body, &document); err != nil {
			return
		}
		var hasTitleNeedle, has1h bool
		checkString := func(value string) {
			for _, needle := range []string{
				"output_config", "naming a coding session", "Return a short title", "Write the title in the predominant language",
			} {
				if strings.Contains(value, needle) {
					hasTitleNeedle = true
				}
			}
		}
		var visit func(any)
		visit = func(value any) {
			switch typed := value.(type) {
			case map[string]any:
				for key, item := range typed {
					checkString(key)
					visit(item)
				}
			case []any:
				for _, item := range typed {
					visit(item)
				}
			case string:
				checkString(typed)
				if typed == "1h" {
					has1h = true
				}
			}
		}
		visit(document)
		if hasTitleNeedle && !jsonMayContainASCII(body,
			"output_config", "naming a coding session", "Return a short title", "Write the title in the predominant language") {
			t.Fatalf("title prefilter missed decoded JSON: %q", body)
		}
		if has1h && !jsonMayContainASCII(body, `"1h"`) {
			t.Fatalf("TTL prefilter missed decoded JSON: %q", body)
		}
	})
}
