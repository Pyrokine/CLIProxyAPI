package gemini_test

import (
	"context"
	"testing"

	_ "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/gemini/gemini"
	sdk "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestGeminiSDKBackfillToolNameByExplicitID(t *testing.T) {
	for _, responses := range []string{
		`[{"functionResponse":{"id":"id-1","response":{"result":"second"}}},{"f` +
			`unctionResponse":{"id":"id-0","response":{"result":"first"}}}]`,
		`[{"functionResponse":{"id":"id-1","name":"tool_1","response":{"result":"second"}}},{"` +
			`functionResponse":{"id":"id-0","name":"tool_0","response":{"result":"first"}}}]`,
	} {
		body := []byte(`{"contents":[{"role":"model","parts":[{"functionCall":{"id":"id-0","na` +
			`me":"tool_0","args":{}}},{"functionCall":{"id":"id-1","name":"tool_1",` +
			`"args":{}}}]},{"role":"user","parts":` + responses + `}]}`)
		out, err := sdk.TranslateRequestChecked(
			context.Background(), sdk.FormatGemini, sdk.FormatGemini, "gemini-test", body, false,
		)
		if err != nil {
			t.Fatal(err)
		}
		for _, part := range gjson.GetBytes(out, "contents.1.parts").Array() {
			call := part.Get("functionResponse")
			want := map[string]string{"id-0": "tool_0", "id-1": "tool_1"}[call.Get("id").String()]
			if call.Get("name").String() != want {
				t.Fatalf("explicit ID matched positional name: %s", out)
			}
		}
	}
}
