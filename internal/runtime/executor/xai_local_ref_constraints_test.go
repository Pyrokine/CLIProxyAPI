package executor

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestXAIRequestPreparationPreservesReferencedConstraints(t *testing.T) {
	for _, tc := range []struct {
		name   string
		schema string
		paths  map[string]string
	}{
		{"required-intersection", `{"$ref":"#/$defs/Base","required":["b"],"$defs":{"Base":{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"string"}},"required":["a"]}}}`,
			map[string]string{"type": "object", "required.0": "a", "allOf.0.required.0": "b"}},
		{"property-intersection", `{"type":"object","properties":{"value":{"$ref":"#/$defs/Value","minimum":8}},"$defs":{"Value":{"type":"number","minimum":10}}}`,
			map[string]string{"properties.value.minimum": "10", "properties.value.allOf.0.minimum": "8"}},
		{"recursive-object", `{"$ref":"#/$defs/Node","$defs":{"Node":{"type":"object","properties":{"name":{"type":"string"},"child":{"$ref":"#/$defs/Node"}},"required":["name"],"additionalProperties":false}}}`,
			map[string]string{"type": "object", "required.0": "name", "additionalProperties": "false", "properties.child.$ref": "#/$defs/Node", "$defs.Node.required.0": "name", "$defs.Node.additionalProperties": "false"}},
		{"root-recursion", `{"type":"object","properties":{"name":{"type":"string"},"child":{"$ref":"#"}},"required":["name"],"additionalProperties":false}`,
			map[string]string{"type": "object", "properties.child.$ref": "#", "required.0": "name", "additionalProperties": "false"}},
	} {
		for _, folded := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/folded=%t", tc.name, folded), func(t *testing.T) {
				tools := `{"type":"function","name":"lookup","parameters":` + tc.schema + `}`
				if folded {
					children := []string{tools}
					for i := range xaiMaxTools {
						children = append(children, fmt.Sprintf(`{"type":"function","name":"extra_%d","parameters":{"type":"object"}}`, i))
					}
					tools = `{"type":"namespace","name":"fixture","tools":[` + strings.Join(children, ",") + `]}`
				}
				payload := []byte(`{"model":"grok-4.6","input":"hi","tools":[` + tools + `]}`)
				req := cliproxyexecutor.Request{Model: "grok-4.6", Payload: payload}
				opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse}
				httpPrepared, err := newXAIExecutor(&config.Config{}).prepareResponsesRequest(req, opts, true)
				if err != nil {
					t.Fatal(err)
				}
				wsPrepared, err := newXAIWebsocketsExecutor(&config.Config{}).prepareResponsesWebsocketRequest(context.Background(), nil, req, opts)
				if err != nil {
					t.Fatal(err)
				}
				for name, body := range map[string][]byte{"HTTP": httpPrepared.body, "WebSocket": wsPrepared.body} {
					schema := gjson.GetBytes(body, "tools.0.parameters")
					if folded {
						description := gjson.GetBytes(body, "tools.0.description").String()
						_, parameters, found := strings.Cut(description, "  Parameters: ")
						if !found {
							t.Fatalf("%s dispatcher lost child parameters", name)
						}
						schema = gjson.Parse(strings.SplitN(parameters, "\n", 2)[0])
					}
					for path, want := range tc.paths {
						if got := schema.Get(path).String(); got != want {
							t.Errorf("%s %s=%s, want %s; schema=%s", name, path, got, want, schema.Raw)
						}
					}
				}
			})
		}
	}
}

func TestXAIRequestPreparationRejectsUnresolvedLocalReferences(t *testing.T) {
	for _, folded := range []bool{false, true} {
		tools := `{"type":"function","name":"lookup","parameters":{"type":"object","properties":{"value":{"$ref":"#/$defs/Missing"}}}}`
		if folded {
			children := []string{tools}
			for i := range xaiMaxTools {
				children = append(children, fmt.Sprintf(`{"type":"function","name":"extra_%d","parameters":{"type":"object"}}`, i))
			}
			tools = `{"type":"namespace","name":"fixture","tools":[` + strings.Join(children, ",") + `]}`
		}
		payload := []byte(`{"model":"grok-4.6","input":"hi","tools":[` + tools + `]}`)
		req := cliproxyexecutor.Request{Model: "grok-4.6", Payload: payload}
		opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse}
		_, httpErr := newXAIExecutor(&config.Config{}).prepareResponsesRequest(req, opts, true)
		_, wsErr := newXAIWebsocketsExecutor(&config.Config{}).prepareResponsesWebsocketRequest(context.Background(), nil, req, opts)
		for name, err := range map[string]error{"HTTP": httpErr, "WebSocket": wsErr} {
			status, ok := err.(interface{ StatusCode() int })
			if !ok || status.StatusCode() != http.StatusBadRequest || !strings.Contains(err.Error(), "unresolved local reference") {
				t.Errorf("%s folded=%t error=%v, want unresolved reference status 400", name, folded, err)
			}
		}
	}
}
