package helps

import (
	"bytes"
	"testing"

	"github.com/tidwall/gjson"
)

func TestNormalizeKimiResponsesInput(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input string
		order []int
	}{
		{"parallel-user", `[{"type":"function_call","call_id":"a"},{"type":"function_call","call_id":"b"},{"role":"user","content":"keep"},{"type":"function_call_output","call_id":"a","output":"first"},{"type":"function_call_output","call_id":"b","output":"second"}]`, []int{0, 1, 3, 4, 2}},
		{"parallel-developer", `[{"type":"function_call","call_id":"a"},{"type":"function_call","call_id":"b"},{"type":"function_call_output","call_id":"a","output":"first"},{"role":"developer","content":"keep"},{"type":"function_call_output","call_id":"b","output":"second"}]`, []int{0, 1, 2, 4, 3}},
		{"custom", `[{"type":"custom_tool_call","call_id":"a"},{"role":"user","content":"keep"},{"type":"custom_tool_call_output","call_id":"a","output":"raw result"}]`, []int{0, 2, 1}},
		{"duplicate-id-complete", `[{"type":"function_call","call_id":"a"},{"type":"function_call","call_id":"a"},{"role":"developer"},{"type":"function_call_output","call_id":"a","output":"one"},{"type":"function_call_output","call_id":"a","output":"two"}]`, []int{0, 1, 3, 4, 2}},
		{"duplicate-id-incomplete", `[{"type":"function_call","call_id":"a"},{"type":"function_call","call_id":"a"},{"role":"developer"},{"type":"function_call_output","call_id":"a"}]`, nil},
		{"missing-output", `[{"type":"function_call","call_id":"a"},{"type":"function_call","call_id":"b"},{"role":"user"},{"type":"function_call_output","call_id":"a"}]`, nil},
		{"empty-id", `[{"type":"function_call","call_id":""},{"role":"user"},{"type":"function_call_output","call_id":""}]`, nil},
		{"unrelated-output", `[{"type":"function_call","call_id":"a"},{"type":"function_call_output","call_id":"other"},{"role":"user"},{"type":"function_call_output","call_id":"a"}]`, []int{0, 3, 1, 2}},
		{"next-batch-boundary", `[{"type":"function_call","call_id":"a"},{"role":"user"},{"type":"function_call","call_id":"b"},{"type":"function_call_output","call_id":"a"}]`, nil},
		{"multiple-batches", `[{"type":"function_call","call_id":"a"},{"role":"developer"},{"type":"function_call_output","call_id":"a"},{"role":"assistant"},{"type":"function_call","call_id":"b"},{"role":"user"},{"type":"function_call_output","call_id":"b"}]`, []int{0, 2, 1, 3, 4, 6, 5}},
		{"contiguous", `[{"type":"function_call","call_id":"a"},{"type":"function_call_output","call_id":"a"},{"role":"user"}]`, nil},
		{"message-only", `[{"role":"user","content":"hi"}]`, nil},
		{"empty-array", `[]`, nil},
		{"string-input", `"hello"`, nil},
		{"null-input", `null`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"other":{"keep":true},"input":` + tc.input + `}`)
			original := bytes.Clone(body)
			got, err := NormalizeKimiResponsesInput(body)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(body, original) || !gjson.GetBytes(got, "other.keep").Bool() {
				t.Fatal("normalization modified input storage or unrelated fields")
			}
			if tc.order == nil {
				if !bytes.Equal(got, original) {
					t.Fatalf("unchanged input was rewritten: %s", got)
				}
			} else {
				before := gjson.GetBytes(original, "input").Array()
				after := gjson.GetBytes(got, "input").Array()
				if len(after) != len(before) {
					t.Fatalf("item count changed from %d to %d", len(before), len(after))
				}
				for i, index := range tc.order {
					if after[i].Raw != before[index].Raw {
						t.Fatalf("item %d = %s, want original item %d", i, after[i].Raw, index)
					}
				}
			}
			again, err := NormalizeKimiResponsesInput(got)
			if err != nil || !bytes.Equal(got, again) {
				t.Fatalf("normalization is not idempotent: %s, err=%v", again, err)
			}
		})
	}
	for _, body := range [][]byte{nil, []byte(`{`), []byte(`{"other":true}`)} {
		got, err := NormalizeKimiResponsesInput(body)
		if err != nil || !bytes.Equal(got, body) {
			t.Fatalf("unsupported payload changed: %q, err=%v", got, err)
		}
	}
}
