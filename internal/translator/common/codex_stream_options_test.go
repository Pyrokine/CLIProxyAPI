package common_test

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/common"
)

func TestNormalizeCodexStreamOptionsPreservesUnrelatedRequest(t *testing.T) {
	cases := []struct{ name, input, want string }{
		{"absent", "", ""},
		{"include-usage", `{"include_usage":true}`, ""},
		{"unknown", `{"include_usage":false,"unknown":{"value":7}}`, ""},
		{"empty", `{}`, ""},
		{"null", `null`, ""},
		{
			"declared", `{"reasoning_summary_delivery":"sequential_cutoff"}`,
			`{"reasoning_summary_delivery":"sequential_cutoff"}`,
		},
		{
			"mixed", `{"include_usage":true,"reasoning_summary_delivery":"sequential_cutoff","unknown":9}`,
			`{"reasoning_summary_delivery":"sequential_cutoff"}`,
		},
		{
			"explicit-null", `{"reasoning_summary_delivery":null,"include_usage":true}`,
			`{"reasoning_summary_delivery":null}`,
		},
		{
			"explicit-empty", `{"reasoning_summary_delivery":"","include_usage":true}`,
			`{"reasoning_summary_delivery":""}`,
		},
	}
	for _, test := range cases {
		for index, input := range []string{
			`" keep input as a string "`,
			`[{"role":"system","content":[{"type":"input_text","text":"keep system role"}]}]`,
		} {
			t.Run(
				fmt.Sprintf("%s/input-%d", test.name, index), func(t *testing.T) {
					prefix := `{"model":"administrator-model(high)","input":` +
						input +
						((`,"service_tier":` +
							`"administrator-tier` +
							`","store":true,` +
							`"stream":false,` +
							`"para`) +
							`llel_tool_calls":false,"include":["administrator-field"],"temperature"` +
							`:0.7,"max_output_tokens":9007199254740993,"metadata":{"number":1e-09,"` +
							`stream_options":{"include_usage":true}}`)
					body := []byte(prefix)
					if test.input != "" {
						body = append(body, []byte(`,"stream_options":`+test.input)...)
					}
					body = append(body, '}')
					original := append([]byte(nil), body...)
					want := prefix
					if test.want != "" {
						want += `,"stream_options":` + test.want
					}
					want += `}`
					out := common.NormalizeCodexStreamOptions(body)
					if string(out) != want {
						t.Fatalf(
							"request changed outside allowed stream options\n got: %s\nwant: %s",
							out,
							want,
						)
					}
					if !bytes.Equal(body, original) {
						t.Fatalf("input buffer changed: %s", body)
					}
					if twice := common.NormalizeCodexStreamOptions(out); !bytes.Equal(twice, out) {
						t.Fatalf("second normalization changed request: first=%s second=%s", out, twice)
					}
				},
			)
		}
	}
}
