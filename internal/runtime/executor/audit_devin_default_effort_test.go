package executor

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor/helps"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestAuditDevinDefaultEffortAtExecutionEntry(t *testing.T) {
	for _, test := range []struct {
		name       string
		model      string
		generation string
		format     translator.Format
		payload    string
		wantUID    string
		wantMax    int64
	}{
		{name: "SWE 2 bare", model: "devin/swe-2", wantUID: "swe-2"},
		{name: "SWE 2 unqualified", model: "swe-2", wantUID: "swe-2"},
		{name: "SWE 1.7 bare", model: "devin/swe-1-7", wantUID: "swe-1-7"},
		{name: "SWE lightning bare", model: "devin/swe-2-lightning", wantUID: "swe-2-lightning"},
		{name: "SWE priority without effort", model: "devin/swe-2-priority", wantUID: "swe-2-priority"},
		{
			name: "SWE body effort", model: "devin/swe-2", generation: `,"generation_config":{"thinking_level":"low"}`,
			wantUID: "swe-2-medium",
		},
		{
			name: "SWE body none", model: "devin/swe-2", generation: `,"generation_config":{"thinking_level":"none"}`,
			wantUID: "swe-2-high",
		},
		{
			name: "SWE body budget", model: "devin/swe-2",
			generation: `,"generation_config":{"thinking_config":{"thinking_budget":40000}}`, wantUID: "swe-2-max",
		},
		{name: "SWE suffix effort", model: "devin/swe-2-high", wantUID: "swe-2-high"},
		{name: "SWE colon effort", model: "devin/swe-1-7:medium", wantUID: "swe-1-7-medium"},
		{
			name: "SWE suffix overrides body", model: "devin/swe-2(max)",
			generation: `,"generation_config":{"thinking_level":"medium"}`, wantUID: "swe-2-max",
		},
		{name: "Sonnet exact bare", model: "devin/claude-sonnet-4-5", wantUID: "MODEL_PRIVATE_2"},
		{name: "Sonnet dot and case", model: "DEVIN/CLAUDE-SONNET-4.5", wantUID: "MODEL_PRIVATE_2"},
		{name: "Sonnet explicit none", model: "devin/claude-sonnet-4-5(none)", wantUID: "MODEL_PRIVATE_2"},
		{name: "Sonnet explicit effort", model: "devin/claude-sonnet-4-5:high", wantUID: "MODEL_PRIVATE_3"},
		{
			name: "Sonnet body effort", model: "devin/claude-sonnet-4-5",
			generation: `,"generation_config":{"thinking_level":"high"}`, wantUID: "MODEL_PRIVATE_3",
		},
		{
			name: "Sonnet body none", model: "devin/claude-sonnet-4-5",
			generation: `,"generation_config":{"thinking_level":"none"}`, wantUID: "MODEL_PRIVATE_2",
		},
		{
			name: "Sonnet body budget", model: "devin/claude-sonnet-4-5",
			generation: `,"generation_config":{"thinking_config":{"thinking_budget":4096}}`, wantUID: "MODEL_PRIVATE_3",
		},
		{
			name: "Sonnet body zero budget", model: "devin/claude-sonnet-4-5",
			generation: `,"generation_config":{"thinking_config":{"thinking_budget":0}}`, wantUID: "MODEL_PRIVATE_2",
		},
		{
			name: "Sonnet dot body effort", model: "DEVIN/CLAUDE-SONNET-4.5",
			generation: `,"generation_config":{"thinking_level":"high"}`, wantUID: "MODEL_PRIVATE_3",
		},
		{
			name: "Sonnet dot body none", model: "Devin/Claude-Sonnet-4.5",
			generation: `,"generation_config":{"thinking_level":"none"}`, wantUID: "MODEL_PRIVATE_2",
		},
		{
			name: "Sonnet dot body budget", model: "DEVIN/CLAUDE-SONNET-4.5",
			generation: `,"generation_config":{"thinking_config":{"thinking_budget":4096}}`, wantUID: "MODEL_PRIVATE_3",
		},
		{name: "GLM default", model: "devin/glm-5-2", wantUID: "glm-5-2"},
		{
			name: "GLM body high", model: "devin/glm-5-2", generation: `,"generation_config":{"thinking_level":"high"}`,
			wantUID: "glm-5-2",
		},
		{
			name: "GLM body none", model: "devin/glm-5-2", generation: `,"generation_config":{"thinking_level":"none"}`,
			wantUID: "glm-5-2-none",
		},
		{
			name: "GLM body zero budget", model: "devin/glm-5-2",
			generation: `,"generation_config":{"thinking_config":{"thinking_budget":0}}`, wantUID: "glm-5-2-none",
		},
		{
			name: "GLM camel case zero budget", model: "devin/glm-5-2",
			generation: `,"generationConfig":{"thinkingConfig":{"thinkingBudget":0}}`, wantUID: "glm-5-2-none",
		},
		{
			name: "GLM explicit level overrides zero budget", model: "devin/glm-5-2",
			generation: `,"generation_config":{"thinking_level":"max","thinking_config":{"thinking_budget":0}}`,
			wantUID:    "glm-5-2-max",
		},
		{name: "GLM million default", model: "devin/glm-5-2-1m", wantUID: "glm-5-2-1m"},
		{
			name: "GLM million body none", model: "devin/glm-5-2-1m",
			generation: `,"generation_config":{"thinking_level":"none"}`, wantUID: "glm-5-2-none-1m",
		},
		{
			name: "GLM million body max", model: "devin/glm-5-2-1m",
			generation: `,"generation_config":{"thinking_level":"max"}`, wantUID: "glm-5-2-max-1m",
		},
		{
			name: "SWE legacy fast body", model: "devin/swe-1-6",
			generation: `,"generation_config":{"thinking_level":"fast"}`, wantUID: "swe-1-6-fast",
		},
		{name: "SWE legacy fast UID", model: "devin/swe-1-6-fast", wantUID: "swe-1-6-fast"},
		{
			name: "Opus legacy thinking body", model: "devin/claude-opus-4-6",
			generation: `,"generation_config":{"thinking_level":"high"}`, wantUID: "claude-opus-4-6-thinking",
		},
		{
			name: "Sonnet legacy thinking body", model: "devin/claude-sonnet-4-6",
			generation: `,"generation_config":{"thinking_level":"high"}`, wantUID: "claude-sonnet-4-6-thinking",
		},
		{
			name: "Opus million legacy thinking body", model: "devin/claude-opus-4-6-1m",
			generation: `,"generation_config":{"thinking_level":"high"}`, wantUID: "claude-opus-4-6-thinking-1m",
		},
		{
			name: "Sonnet million legacy thinking body", model: "devin/claude-sonnet-4-6-1m",
			generation: `,"generation_config":{"thinking_level":"high"}`, wantUID: "claude-sonnet-4-6-thinking-1m",
		},
		{
			name: "Opus legacy thinking UID", model: "devin/claude-opus-4-6-thinking",
			wantUID: "claude-opus-4-6-thinking",
		},
		{
			name: "Sonnet million legacy thinking UID", model: "devin/claude-sonnet-4-6-thinking-1m",
			wantUID: "claude-sonnet-4-6-thinking-1m",
		},
		{name: "legacy GPT default", model: "devin/MODEL_GPT_5_2", wantUID: "MODEL_GPT_5_2_LOW"},
		{
			name: "legacy GPT none", model: "devin/MODEL_GPT_5_2",
			generation: `,"generation_config":{"thinking_level":"none"}`, wantUID: "MODEL_GPT_5_2_NONE",
		},
		{
			name: "legacy GPT medium", model: "devin/MODEL_GPT_5_2",
			generation: `,"generation_config":{"thinking_level":"medium"}`, wantUID: "MODEL_GPT_5_2_MEDIUM",
		},
		{
			name: "legacy GPT high", model: "devin/MODEL_GPT_5_2",
			generation: `,"generation_config":{"thinking_level":"high"}`, wantUID: "MODEL_GPT_5_2_HIGH",
		},
		{
			name: "legacy GPT xhigh", model: "devin/MODEL_GPT_5_2",
			generation: `,"generation_config":{"thinking_level":"xhigh"}`, wantUID: "MODEL_GPT_5_2_XHIGH",
		},
		{
			name: "legacy GPT UID preserves effort", model: "devin/MODEL_GPT_5_2_XHIGH",
			generation: `,"generation_config":{"thinking_level":"none"}`, wantUID: "MODEL_GPT_5_2_XHIGH",
		},
		{
			name: "legacy Gemini default", model: "devin/MODEL_GOOGLE_GEMINI_3_0_FLASH",
			wantUID: "MODEL_GOOGLE_GEMINI_3_0_FLASH_HIGH",
		},
		{
			name: "legacy Gemini minimal", model: "devin/MODEL_GOOGLE_GEMINI_3_0_FLASH",
			generation: `,"generation_config":{"thinking_level":"minimal"}`,
			wantUID:    "MODEL_GOOGLE_GEMINI_3_0_FLASH_MINIMAL",
		},
		{
			name: "legacy Gemini low", model: "devin/MODEL_GOOGLE_GEMINI_3_0_FLASH",
			generation: `,"generation_config":{"thinking_level":"low"}`, wantUID: "MODEL_GOOGLE_GEMINI_3_0_FLASH_LOW",
		},
		{
			name: "legacy Gemini medium", model: "devin/MODEL_GOOGLE_GEMINI_3_0_FLASH",
			generation: `,"generation_config":{"thinking_level":"medium"}`,
			wantUID:    "MODEL_GOOGLE_GEMINI_3_0_FLASH_MEDIUM",
		},
		{
			name: "legacy Gemini high", model: "devin/MODEL_GOOGLE_GEMINI_3_0_FLASH",
			generation: `,"generation_config":{"thinking_level":"high"}`, wantUID: "MODEL_GOOGLE_GEMINI_3_0_FLASH_HIGH",
		},
		{name: "legacy Opus default", model: "devin/MODEL_CLAUDE_4_5_OPUS", wantUID: "MODEL_CLAUDE_4_5_OPUS"},
		{
			name: "legacy Opus high", model: "devin/MODEL_CLAUDE_4_5_OPUS",
			generation: `,"generation_config":{"thinking_level":"high"}`, wantUID: "MODEL_CLAUDE_4_5_OPUS_THINKING",
		},
		{
			name: "legacy Opus thinking UID", model: "devin/MODEL_CLAUDE_4_5_OPUS_THINKING",
			wantUID: "MODEL_CLAUDE_4_5_OPUS_THINKING",
		},
		{name: "legacy exact private UID", model: "devin/MODEL_PRIVATE_12", wantUID: "MODEL_PRIVATE_12"},
		{
			name: "explicit Sonnet catalog limit", model: "devin/claude-sonnet-4-5",
			generation: `,"generation_config":{"max_output_tokens":` +
				`128000}`, wantUID: "MODEL_PRIVATE_2", wantMax: 128000,
		},
		{
			name: "legacy GPT completion limit", model: "devin/MODEL_GPT_5_2",
			generation: `,"generation_config":{"max_output_tokens":128000}`, wantUID: "MODEL_GPT_5_2_LOW",
		},
		{
			name: "OpenAI Sonnet high", model: "devin/claude-sonnet-4-5", format: translator.FormatOpenAI,
			payload: `{"messages":[{"role":"user","content":"synthetic"}],"reasoning_effort":"high"}`,
			wantUID: "MODEL_PRIVATE_3",
		},
		{
			name: "OpenAI GLM none", model: "devin/glm-5-2", format: translator.FormatOpenAI,
			payload: `{"messages":[{"role":"user","content":"synthetic"}],"reasoning_effort":"none"}`,
			wantUID: "glm-5-2-none",
		},
		{
			name: "OpenAI GPT xhigh", model: "devin/MODEL_GPT_5_2", format: translator.FormatOpenAI,
			payload: `{"messages":[{"role":"user","content":"synthetic"}],"reasoning_effort":"xhigh"}`,
			wantUID: "MODEL_GPT_5_2_XHIGH",
		},
		{
			name: "Responses Sonnet dot high", model: "DEVIN/CLAUDE-SONNET-4.5", format: translator.FormatCodex,
			payload: `{"input":[{"role":"user","content":"synthetic"}],"reasoning":{"effort":"high"}}`,
			wantUID: "MODEL_PRIVATE_3",
		},
		{
			name: "Responses GLM none", model: "devin/glm-5-2-1m", format: translator.FormatCodex,
			payload: `{"input":[{"role":"user","content":"synthetic"}],"reasoning":{"effort":"none"}}`,
			wantUID: "glm-5-2-none-1m",
		},
		{
			name: "Responses GPT xhigh", model: "devin/MODEL_GPT_5_2", format: translator.FormatCodex,
			payload: `{"input":[{"role":"user","content":"synthetic"}],"reasoning":{"effort":"xhigh"}}`,
			wantUID: "MODEL_GPT_5_2_XHIGH",
		},
		{
			name: "Claude Sonnet budget", model: "devin/claude-sonnet-4-5", format: translator.FormatClaude,
			payload: `{"messages":[{"role":"user","content":"synthetic"}],"max_tokens":64000` +
				`,"thinking":{"type":"enabled","budget_tokens":4096}}`,
			wantUID: "MODEL_PRIVATE_3",
		},
		{
			name: "Claude GLM none", model: "devin/glm-5-2", format: translator.FormatClaude,
			payload: `{"messages":[{"role":"user","content":"synthetic"}],"max_tokens":` +
				`64000,"thinking":{"type":` +
				`"disabled"}}`,
			wantUID: "glm-5-2-none",
		},
		{
			name: "Claude GPT budget", model: "devin/MODEL_GPT_5_2", format: translator.FormatClaude,
			payload: `{"messages":[{"role":"user","content":"synthetic"}],"max_tokens":64000` +
				`,"thinking":{"type":"enabled","budget_tokens":20000}}`,
			wantUID: "MODEL_GPT_5_2_HIGH",
		},
		{
			name: "Responses direct GLM none", model: "devin/glm-5-2", format: translator.FormatOpenAIResponse,
			payload: `{"input":[{"role":"user","content":"synthetic"}],"reasoning":{"effort":"none"}}`,
			wantUID: "glm-5-2-none",
		},
		{
			name: "Responses latest effort update", model: "devin/MODEL_GPT_5_2", format: translator.FormatCodex,
			payload: `{"input":[{"role":"user","content":"synthetic"},{"type":"configuration` +
				`_update","reasoning":{"effort":"low"}},{"type":"configuration_update",` +
				`"reasoning":{"effort":"xhigh"}}],"reasoning":{"effort":"medium"}}`,
			wantUID: "MODEL_GPT_5_2_XHIGH",
		},
		{
			name: "Responses explicit update none", model: "devin/glm-5-2", format: translator.FormatCodex,
			payload: `{"input":[{"role":"user","content":"synthetic"},{"type":"configuration` +
				`_update","reasoning":{"effort":"none"}}],"reasoning":{"effort":"max"}}`,
			wantUID: "glm-5-2-none",
		},
		{
			name: "Responses suffix overrides update", model: "devin/MODEL_GPT_5_2(low)",
			format: translator.FormatCodex,
			payload: `{"input":[{"role":"user","content":"synthetic"},{"type":` +
				`"configuration_update","reasoning":{"effort":` +
				`"xhigh"}}]}`,
			wantUID: "MODEL_GPT_5_2_LOW",
		},
		{name: "Sonnet arbitrary prefix rejected", model: "devin/custom-sonnet-4-5"},
		{name: "Sonnet arbitrary substring rejected", model: "devin/custom-claude-sonnet-4-5-model"},
	} {
		for _, streaming := range []bool{false, true} {
			t.Run(
				fmt.Sprintf("%s/stream=%t", test.name, streaming), func(t *testing.T) {
					calls := 0
					ctx := util.WithRoundTripper(
						t.Context(), devinTestRoundTripper(
							func(request *http.Request) (*http.Response, error) {
								calls++
								if request.URL.Host != "devin.synthetic.invalid" {
									t.Fatalf("unexpected synthetic request host %q", request.URL.Host)
								}
								body, err := io.ReadAll(request.Body)
								if err != nil || len(body) < 5 {
									t.Fatalf("Connect frame length=%d error=%v", len(body), err)
								}
								_, _, err = helps.FinalizeDevinPayload(
									body[5:], func(view []byte) ([]byte, error) {
										if uid := gjson.GetBytes(view, "model").String(); uid != test.wantUID {
											t.Errorf("wire UID=%q, want %q", uid, test.wantUID)
										}
										wantMax := test.wantMax
										if wantMax == 0 {
											wantMax = 64000
										}
										if maxTokens := gjson.GetBytes(
											view, "completion_config.max_tokens",
										).Int(); maxTokens != wantMax {
											t.Errorf("wire max_tokens=%d, want %d", maxTokens, wantMax)
										}
										return view, nil
									},
								)
								if err != nil {
									t.Fatal(err)
								}
								wire := helps.WrapConnectEnvelope(
									testDevinBytesField(
										nil, 3, []byte("synthetic answer"),
									),
								)
								wire = append(
									wire,
									helps.WrapConnectEnvelopeWithFlag(
										helps.ConnectFlagEndStream,
										[]byte(`{}`),
									)...,
								)
								return &http.Response{
									StatusCode: http.StatusOK,
									Header:     http.Header{"Content-Type": {"application/connect+proto"}},
									Body:       io.NopCloser(strings.NewReader(string(wire))), Request: request,
								}, nil
							},
						),
					)
					credential := &auth.Auth{
						ID: t.Name(), Provider: "devin", Attributes: map[string]string{
							"api_key": "synthetic-session", "base_url": "https://devin.synthetic.invalid",
						},
					}
					payload := []byte(test.payload)
					format := test.format
					if format == "" {
						format = translator.FormatInteractions
						payload = []byte(
							`{"input":[{"type":"user_input","content":"synthetic"}]` + test.generation + `}`,
						)
					}
					err := auditFinalUsageCall(
						ctx,
						NewDevinExecutor(&config.Config{}),
						credential,
						core.Request{Model: test.model, Payload: payload},
						core.Options{
							SourceFormat:   format,
							ResponseFormat: translator.FormatInteractions,
						},
						streaming,
					)
					if test.wantUID == "" {
						status, ok := err.(interface{ StatusCode() int })
						if !ok || status.StatusCode() != http.StatusBadRequest || calls != 0 {
							t.Fatalf("unknown model error=%v calls=%d, want 400 before dispatch", err, calls)
						}
					} else if err != nil || calls != 1 {
						t.Fatalf("error=%v calls=%d, want one synthetic dispatch", err, calls)
					}
				},
			)
		}
	}
}

// noinspection GoTypeAssertionOnErrors
func TestAuditDevinGenerationConstraintsAtExecutionEntry(t *testing.T) {
	for _, test := range []struct {
		name          string
		fields        string
		format        translator.Format
		payload       string
		reject        string
		wantTopP      float64
		nativeDefault bool
	}{
		{name: "omitted", wantTopP: 0.95},
		{name: "top p value", fields: `,"top_p":0.63`, wantTopP: 0.63},
		{name: "top p zero", fields: `,"top_p":0`, wantTopP: 0},
		{name: "top p one", fields: `,"top_p":1`, wantTopP: 1},
		{name: "top p camel", fields: `,"topP":0.42`, wantTopP: 0.42},
		{
			name: "camel generation config", format: translator.FormatInteractions,
			payload: `{"input":[{"type":"user_input","content":"synthetic"}],"generationConf` +
				`ig":{"temperature":0,"maxOutputTokens":17,"topP":0.24}}`,
			wantTopP: 0.24,
		},
		{name: "explicit zero precedes native default", fields: `,"top_p":0`, wantTopP: 0, nativeDefault: true},
		{name: "camel zero precedes native default", fields: `,"topP":0`, wantTopP: 0, nativeDefault: true},
		{name: "native default when omitted", wantTopP: 0.7, nativeDefault: true},
		{name: "top p null", fields: `,"top_p":null`, reject: "top_p"},
		{name: "top p empty", fields: `,"top_p":""`, reject: "top_p"},
		{name: "top p negative", fields: `,"top_p":-0.1`, reject: "top_p"},
		{name: "top p over one", fields: `,"top_p":1.1`, reject: "top_p"},
		{name: "presence", fields: `,"presence_penalty":0.5`, reject: "presence_penalty"},
		{name: "presence zero", fields: `,"presence_penalty":0`, reject: "presence_penalty"},
		{name: "presence null", fields: `,"presence_penalty":null`, reject: "presence_penalty"},
		{name: "presence empty", fields: `,"presence_penalty":""`, reject: "presence_penalty"},
		{name: "frequency", fields: `,"frequency_penalty":0.5`, reject: "frequency_penalty"},
		{name: "frequency zero", fields: `,"frequency_penalty":0`, reject: "frequency_penalty"},
		{name: "frequency null", fields: `,"frequency_penalty":null`, reject: "frequency_penalty"},
		{name: "frequency empty", fields: `,"frequency_penalty":""`, reject: "frequency_penalty"},
		{name: "stop", fields: `,"stop_sequences":["END"]`, reject: "stop_sequences"},
		{name: "stop empty array", fields: `,"stop_sequences":[]`, reject: "stop_sequences"},
		{name: "stop null", fields: `,"stop_sequences":null`, reject: "stop_sequences"},
		{name: "stop empty string", fields: `,"stop_sequences":""`, reject: "stop_sequences"},
		{name: "presence camel", fields: `,"presencePenalty":0`, reject: "presencePenalty"},
		{name: "frequency camel", fields: `,"frequencyPenalty":0`, reject: "frequencyPenalty"},
		{name: "stop camel", fields: `,"stopSequences":[]`, reject: "stopSequences"},
		{
			name: "OpenAI top p", format: translator.FormatOpenAI,
			payload: `{"messages":[{"role":"user","content":"synthetic"}],"temperature":0,` +
				`"max_tokens":17,"top_p":` +
				`0.36}`,
			wantTopP: 0.36,
		},
		{
			name: "Claude stop", format: translator.FormatClaude,
			payload: `{"messages":[{"role":"user","content":"synthetic"}],"max_tokens":17,"stop_sequences":["END"]}`,
			reject:  "stop_sequences",
		},
	} {
		for _, streaming := range []bool{false, true} {
			t.Run(
				fmt.Sprintf("%s/stream=%t", test.name, streaming), func(t *testing.T) {
					calls := 0
					ctx := util.WithRoundTripper(
						t.Context(), devinTestRoundTripper(
							func(request *http.Request) (*http.Response, error) {
								calls++
								if request.URL.Host != "devin.synthetic.invalid" {
									t.Fatalf("unexpected host %q", request.URL.Host)
								}
								body, err := io.ReadAll(request.Body)
								if err != nil || len(body) < 5 {
									t.Fatalf("invalid Connect frame: length=%d error=%v", len(body), err)
								}
								_, _, err = helps.FinalizeDevinPayload(
									body[5:], func(view []byte) ([]byte, error) {
										value := gjson.GetBytes(view, "completion_config.top_p")
										if !value.Exists() {
											t.Error("native top_p field is absent")
										}
										got := value.Float()
										if delta := got - test.wantTopP; delta < -0.000001 || delta > 0.000001 {
											t.Errorf("wire top_p=%v, want %v", got, test.wantTopP)
										}
										if gjson.GetBytes(
											view, "completion_config.max_tokens",
										).Int() != 17 || gjson.GetBytes(
											view, "completion_config.temperature",
										).Float() != 0 {
											t.Errorf("supported generation fields changed: %s", view)
										}
										return view, nil
									},
								)
								if err != nil {
									t.Fatal(err)
								}
								wire := helps.WrapConnectEnvelope(
									testDevinBytesField(
										nil, 3, []byte("synthetic answer"),
									),
								)
								wire = append(
									wire,
									helps.WrapConnectEnvelopeWithFlag(
										helps.ConnectFlagEndStream,
										[]byte(`{}`),
									)...,
								)
								return &http.Response{
									StatusCode: http.StatusOK,
									Header:     http.Header{"Content-Type": {"application/connect+proto"}},
									Body:       io.NopCloser(strings.NewReader(string(wire))), Request: request,
								}, nil
							},
						),
					)
					cfg := &config.Config{}
					if test.nativeDefault {
						cfg.Payload.Default = []config.PayloadRule{
							{
								Models: []config.PayloadModelRule{
									{
										Name: "devin/swe-2", Protocol: "devin",
									},
								}, Params: map[string]any{"completion_config.top_p": 0.7},
							},
						}
					}
					payload := []byte(test.payload)
					format := test.format
					if format == "" {
						format = translator.FormatInteractions
						payload = []byte(
							`{"input":[{"type":"user_input","content":"synthetic"}],` +
								`"generation_config":{"temperature":0,"max_output_tokens":` +
								`17` +
								test.fields +
								`}}`,
						)
					}
					credential := &auth.Auth{
						ID: t.Name(), Provider: "devin", Attributes: map[string]string{
							"api_key": "synthetic-session", "base_url": "https://devin.synthetic.invalid",
						},
					}
					err := auditFinalUsageCall(
						ctx,
						NewDevinExecutor(cfg),
						credential,
						core.Request{Model: "devin/swe-2", Payload: payload},
						core.Options{
							SourceFormat:   format,
							ResponseFormat: translator.FormatInteractions,
						},
						streaming,
					)
					if test.reject != "" {
						status, ok := err.(statusErr)
						if !ok || status.code != http.StatusBadRequest || !status.requestScoped ||
							status.dispatchPhase != core.DispatchPhaseNotSent ||
							!strings.Contains(
								status.Error(), test.reject,
							) ||
							calls != 0 {
							t.Fatalf(
								"error=%v calls=%d, want named request-scoped 400 before dispatch",
								err,
								calls,
							)
						}
					} else if err != nil || calls != 1 {
						t.Fatalf("error=%v calls=%d, want one dispatch", err, calls)
					}
				},
			)
		}
	}
}
