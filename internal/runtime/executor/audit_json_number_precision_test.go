package executor

import (
	"bytes"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestAuditColonSpacingPreservesJSONNumberLiterals(t *testing.T) {
	for _, literal := range []string{
		"9007199254740993", "-9007199254740993", "18446744073709551615", "0.123456789012345678901",
		"1.234567890123456789e+100", "42", `"9007199254740993"`,
	} {
		t.Run(
			literal, func(t *testing.T) {
				payload := []byte(
					`{"value":` +
						literal +
						`,"nested":[{"value":` +
						literal +
						(`}],"text":"keep: spacing and ` +
							`\\\"quotes"}`),
				)
				output := ensureColonSpacedJSON(payload)
				if got := gjson.GetBytes(output, "value").Raw; got != literal {
					t.Fatalf("value=%s, want exact %s; output=%s", got, literal, output)
				}
				if got := gjson.GetBytes(output, "nested.0.value").Raw; got != literal {
					t.Fatalf("nested value=%s, want %s", got, literal)
				}
				if !bytes.Contains(output, []byte(`"value": `)) || strings.Contains(string(output), "\n") {
					t.Fatalf("colon formatting changed: %s", output)
				}
				if gjson.GetBytes(output, "text").String() != gjson.GetBytes(payload, "text").String() {
					t.Fatalf("string changed: %s", output)
				}
			},
		)
	}
	for _, payload := range []string{"", " ", `{"value":1} {"value":2}`, `{"value":`, "data: [DONE]\n\n"} {
		if output := ensureColonSpacedJSON([]byte(payload)); string(output) != payload {
			t.Fatalf("non-JSON changed: %q", output)
		}
	}
}
