package config

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
)

func TestCredentialOverridesSurviveConfigClone(t *testing.T) {
	for _, provider := range []string{
		"claude-api-key", "codex-api-key", "xai-api-key", "gemini-api-key", "interactions-api-key", "vertex-api-key", "openai-compatibility",
	} {
		for _, tc := range []struct {
			name    string
			fields  string
			present bool
			cooling bool
			retry   float64
		}{
			{name: "missing"},
			{name: "null", fields: `,"disable-cooling":null,"request-retry":null`},
			{name: "false_zero", fields: `,"disable-cooling":false,"request-retry":0`, present: true},
			{name: "true_inherit", fields: `,"disable-cooling":true,"request-retry":-1`, present: true, cooling: true, retry: -1},
			{name: "false_positive", fields: `,"disable-cooling":false,"request-retry":2`, present: true, retry: 2},
		} {
			t.Run(provider+"/"+tc.name, func(t *testing.T) {
				var cfg Config
				input := fmt.Sprintf(`{"%s":[{"api-key":"synthetic-provider-key","name":"example"%s}]}`, provider, tc.fields)
				if err := json.Unmarshal([]byte(input), &cfg); err != nil {
					t.Fatal(err)
				}
				cloned, err := Clone(&cfg)
				if err != nil {
					t.Fatal(err)
				}
				if tc.present {
					field := map[string]string{
						"claude-api-key": "ClaudeKey", "codex-api-key": "CodexKey", "xai-api-key": "XAIKey",
						"gemini-api-key": "GeminiKey", "interactions-api-key": "InteractionsKey",
						"vertex-api-key": "VertexCompatAPIKey", "openai-compatibility": "OpenAICompatibility",
					}[provider]
					original := reflect.ValueOf(&cfg).Elem().FieldByName(field).Index(0)
					copy := reflect.ValueOf(cloned).Elem().FieldByName(field).Index(0)
					for _, name := range []string{"DisableCooling", "RequestRetry"} {
						left, right := original.FieldByName(name), copy.FieldByName(name)
						if left.IsNil() || right.IsNil() || left.Pointer() == right.Pointer() {
							t.Fatalf("%s was lost or shares storage after Clone", name)
						}
					}
				}
				for _, value := range []*Config{&cfg, cloned} {
					data, err := json.Marshal(value)
					if err != nil {
						t.Fatal(err)
					}
					var decoded map[string]json.RawMessage
					if err := json.Unmarshal(data, &decoded); err != nil {
						t.Fatal(err)
					}
					var entries []map[string]any
					if err := json.Unmarshal(decoded[provider], &entries); err != nil {
						t.Fatal(err)
					}
					if len(entries) != 1 {
						t.Fatalf("entries=%v, want one", entries)
					}
					cooling, hasCooling := entries[0]["disable-cooling"]
					retry, hasRetry := entries[0]["request-retry"]
					if hasCooling != tc.present || hasRetry != tc.present ||
						(tc.present && (cooling != tc.cooling || retry != tc.retry)) {
						t.Fatalf("cooling=(%v,%t) retry=(%v,%t), want present=%t cooling=%t retry=%v", cooling, hasCooling, retry, hasRetry, tc.present, tc.cooling, tc.retry)
					}
				}
			})
		}
	}
}
