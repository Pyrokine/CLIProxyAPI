package translator

import (
	"bytes"
	"testing"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestRequestEnvelopeConfigurationUpdateIntent(t *testing.T) {
	const source = `{"reasoning":{"effort":"medium"},"input":[{"type":"configuration_update","reasoning":{"effort":"low"}},{"role":"user","content":"ok"}]}`
	for _, tc := range []struct {
		name, edit                      string
		native, cross, previous, change bool
	}{
		{name: "native unchanged", native: true},
		{name: "native rewrite", native: true, edit: "rewrite", change: true},
		{name: "native delete", native: true, edit: "delete", change: true},
		{name: "native in-place rewrite", native: true, edit: "inplace", change: true},
		{name: "native unrelated change", native: true, edit: "other"},
		{name: "missing route rewrite", edit: "rewrite", change: true},
		{name: "missing route delete", edit: "delete", change: true},
		{name: "missing route in-place rewrite", edit: "inplace", change: true},
		{name: "cross protocol removes updates before normalizer", native: true, cross: true},
		{name: "earlier intent survives", native: true, previous: true, change: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			registry := NewRegistry()
			to := FormatCodex
			if tc.cross {
				to = FormatOpenAI
			}
			if tc.native {
				registry.Register(FormatOpenAIResponse, to, func(_ string, body []byte, _ bool) []byte {
					if tc.cross {
						return []byte(`{"reasoning_effort":"low","messages":[{"role":"user","content":"ok"}]}`)
					}
					return body
				}, ResponseTransform{})
			}
			hooks := summaryPluginHooks{normalizeRequest: func(body []byte) []byte {
				var out []byte
				var err error
				switch tc.edit {
				case "rewrite":
					out, err = sjson.SetBytes(body, "input.0.reasoning.effort", "high")
				case "delete":
					out, err = sjson.DeleteBytes(body, "input.0")
				case "other":
					out, err = sjson.SetBytes(body, "other", true)
				case "inplace":
					at := bytes.Index(body, []byte(`"low"`))
					if at < 0 {
						t.Fatal("normalizer did not receive update")
					}
					copy(body[at:], []byte(`"max"`))
					return body
				default:
					return body
				}
				if err != nil {
					t.Fatal(err)
				}
				return out
			}}
			body := []byte(source)
			before := bytes.Clone(body)
			out := registry.TranslateRequestEnvelope(WithPluginHooks(t.Context(), hooks), FormatOpenAIResponse, to, RequestEnvelope{
				Model: "synthetic", Body: body, ConfigurationUpdatesChanged: tc.previous,
			})
			if out.Err != nil || out.ConfigurationUpdatesChanged != tc.change {
				t.Fatalf("intent = %v, want %v; err=%v", out.ConfigurationUpdatesChanged, tc.change, out.Err)
			}
			if !bytes.Equal(body, before) {
				t.Fatal("normalizer modified caller's source buffer")
			}
			if !gjson.ValidBytes(out.Body) {
				t.Fatalf("normalizer output is not JSON: %s", out.Body)
			}
		})
	}
}

func TestRequestEnvelopeClaudeThinkingNormalizerIntent(t *testing.T) {
	const source = `{"input":[{"role":"user","content":"ok"}]}`
	const target = `{"thinking":{"type":"adaptive"},"output_config":{"effort":"low"},"messages":[{"role":"user","content":"ok"}]}`
	for _, registered := range []bool{false, true} {
		for _, edit := range []string{"unchanged", "unrelated", "visibility", "rewrite", "remove"} {
			name := "fallback/" + edit
			if registered {
				name = "registered/" + edit
			}
			t.Run(name, func(t *testing.T) {
				registry := NewRegistry()
				from := FormatClaude
				body := []byte(target)
				if registered {
					from = FormatOpenAIResponse
					body = []byte(source)
					registry.Register(from, FormatClaude, func(_ string, _ []byte, _ bool) []byte {
						return []byte(target)
					}, ResponseTransform{})
				}
				calls := 0
				hooks := summaryPluginHooks{normalizeRequest: func(body []byte) []byte {
					calls++
					var err error
					switch edit {
					case "unrelated":
						body, err = sjson.SetBytes(body, "other", true)
					case "visibility":
						body, err = sjson.SetBytes(body, "thinking.display", "summarized")
					case "rewrite":
						body, err = sjson.SetBytes(body, "output_config.effort", "high")
					case "remove":
						body, err = sjson.DeleteBytes(body, "thinking")
					}
					if err != nil {
						t.Fatal(err)
					}
					return body
				}}
				before := bytes.Clone(body)
				out := registry.TranslateRequestEnvelope(WithPluginHooks(t.Context(), hooks), from, FormatClaude,
					RequestEnvelope{Model: "synthetic", Body: body})
				want := edit == "rewrite" || edit == "remove"
				if out.Err != nil || out.ConfigurationUpdatesChanged != want {
					t.Fatalf("Claude normalizer intent = %v, want %v; error = %v", out.ConfigurationUpdatesChanged, want, out.Err)
				}
				if calls != 1 || !bytes.Equal(before, body) {
					t.Fatal("Claude normalization changed caller input or invoked the hook more than once")
				}
			})
		}
	}
}
