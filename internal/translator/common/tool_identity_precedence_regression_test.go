package common_test

import (
	"context"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/common"
	_ "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/openai/openai/responses"
	sdk "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestResponsesIdentitySDKSameSourceFirstDeclaration(t *testing.T) {
	for _, declarations := range []string{
		`[{"type":"namespace","name":"ns","tools":[{"type":"custom","name":"exec","description":"first"}]},{"` +
			`type":"function","name":"ns__exec","description":"second","parameters":{"type":"object"}}]`,
		`[{"type":"function","name":"ns__exec","description":"first","parameter` +
			`s":{"type":"object"}},{"type":"namespace","name":"ns","tools":[{"type"` +
			`:"custom","name":"exec","description":"second"}]}]`,
	} {
		body := []byte(
			`{"tools":` + declarations + (`,"input":[{"type":"additional_tools","tools":[{"type":"custom","name":` +
				`"ns__exec","description":"additional"}]},{"role":"user","content":"cal` +
				`l"}]}`),
		)
		identity := common.NewResponsesToolIdentity(body)
		winning := identity.Declarations()
		if len(winning) != 1 || gjson.Get(winning[0].Raw, "description").String() != "first" {
			t.Fatalf("identity winner changed: %#v", winning)
		}
		out, err := sdk.TranslateRequestChecked(
			context.Background(), sdk.FormatOpenAIResponse, sdk.FormatOpenAI, "fixture", body, false,
		)
		if err != nil {
			t.Fatal(err)
		}
		if len(gjson.GetBytes(out, "tools").Array()) != 1 || gjson.GetBytes(
			out, "tools.0.function.description",
		).String() != "first" {
			t.Fatalf("consumer changed winner: %s", out)
		}
		var state any
		back := sdk.TranslateNonStream(
			context.Background(), sdk.FormatOpenAI, sdk.FormatOpenAIResponse, "fixture", body, nil,
			[]byte((`{"id":"fixture","choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call-a","` +
				`type":"function","function":{"name":"ns__exec","arguments":"{\"input\":\"command\"}"}}]},"` +
				`finish_reason":"tool_calls"}]}`)),
			&state,
		)
		found := false
		for _, item := range gjson.GetBytes(back, "output").Array() {
			if item.Get("call_id").String() != "call-a" {
				continue
			}
			found = true
			if winning[0].Kind == common.ResponsesCustomTool {
				if item.Get("type").String() != "custom_tool_call" || item.Get("name").String() != "exec" ||
					item.Get("namespace").String() != "ns" {
					t.Fatalf("custom identity reverse changed: %s", back)
				}
			} else if item.Get("type").String() != "function_call" || item.Get("name").String() != "ns__exec" {
				t.Fatalf("flat identity reverse changed: %s", back)
			}
		}
		if !found {
			t.Fatalf("tool identity missing: %s", back)
		}
	}
}
