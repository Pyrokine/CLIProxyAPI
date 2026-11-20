package executor

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/tidwall/gjson"
)

func TestXAIWebSearchAliasPreservesOriginalPositions(t *testing.T) {
	body := []byte(`{"tools":[{"type":"function","name":"web_search","parameters":{"type":"object"}}]}`)
	for _, tc := range []struct {
		name, event, path string
	}{
		{"top-level", `{"type":"response.function_call_arguments.delta","name":%q,"delta":"synthetic"}`, "name"},
		{
			"item",
			`{"type":"response.output_item.added","item":{"type":"function_call","name":%q,"call_id":"synthetic"}}`,
			"item.name",
		},
		{"item-function", `{"item":{"function":{"name":%q}}}`, "item.function.name"},
		{
			"completed",
			`{"type":"response.completed","response":{"output":[{"type":` +
				`"function_call","name":%q,"call_id":` +
				`"synthetic"}]}}`,
			"response.output.0.name",
		},
		{"output", `{"output":[{"function":{"name":%q}}]}`, "output.0.function.name"},
	} {
		for _, whitespace := range []bool{false, true} {
			t.Run(
				fmt.Sprintf("%s/whitespace=%t", tc.name, whitespace), func(t *testing.T) {
					requestNormalizer := newXAIToolIdentityNormalizer(body)
					prepared, err := requestNormalizer.normalizeDeclarations(body)
					if err != nil || requestNormalizer.clientWebSearchAlias == "" {
						t.Fatalf("request alias missing: body=%s error=%v", prepared, err)
					}
					alias := requestNormalizer.clientWebSearchAlias
					if whitespace {
						alias = " " + alias + " "
					}
					event := []byte(fmt.Sprintf(tc.event, alias))
					output := requestNormalizer.newResponseNormalizer(false).normalize(event)
					if got := gjson.GetBytes(output, tc.path).String(); got != "web_search" {
						t.Fatalf("actual response normalizer %s=%q: %s", tc.path, got, output)
					}
				},
			)
		}
	}
}

func TestXAIWebSearchAliasPreservesOriginalNamespaceBoundary(t *testing.T) {
	for _, event := range []string{
		`{"namespace":"acme","name":"clientfn_web_search"}`,
		`{"item":{"namespace":"acme","name":"clientfn_web_search"}}`,
		`{"response":{"output":[{"namespace":"acme","name":"clientfn_web_search"}]}}`,
		`{"output":[{"namespace":"acme","function":{"name":"clientfn_web_search"}}]}`,
	} {
		output, err := restoreXAIClientWebSearchName([]byte(event), "clientfn_web_search")
		if err != nil || !bytes.Equal(output, []byte(event)) {
			t.Fatalf("namespaced alias changed: %s error=%v", output, err)
		}
	}
}
