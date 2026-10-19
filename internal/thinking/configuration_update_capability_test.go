package thinking_test

import (
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking"
	_ "github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking/provider/claude"
	_ "github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking/provider/codex"
	_ "github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking/provider/openai"
	_ "github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking/provider/xai"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
	"github.com/tidwall/gjson"
)

func TestResponsesConfigurationUpdatesRequireModelSupport(t *testing.T) {
	body := []byte(`{"model":"test-model","reasoning":{"effort":"high","summary":"auto"},"input":[{"type":"message","role":"user","content":"hello"},{"type":"configuration_update","reasoning":{"effort":"low"}}]}`)
	model := &registry.ModelInfo{
		ID: "test-model", Type: "codex", Thinking: &registry.ThinkingSupport{Levels: []string{"low", "high"}},
		SupportConfigurationUpdate: true,
	}
	out, err := thinking.ApplyThinkingWithModelInfoAndSummary(body, body, "test-model", "codex", "codex", "codex", model, thinking.SummaryConfig{})
	if err != nil {
		t.Fatalf("supported updates: %v", err)
	}
	if string(out) != string(body) {
		t.Errorf("supported model lost native configuration updates: %s", out)
	}
	model.SupportConfigurationUpdate = false
	out, err = thinking.ApplyThinkingWithModelInfoAndSummary(body, body, "test-model", "codex", "codex", "codex", model, thinking.SummaryConfig{})
	if err != nil {
		t.Fatalf("unsupported updates: %v", err)
	}
	if items := gjson.GetBytes(out, "input").Array(); len(items) != 1 || items[0].Get("type").String() != "message" {
		t.Errorf("unsupported update left in request: %s", out)
	}
	if got := gjson.GetBytes(out, "reasoning.effort").String(); got != "low" {
		t.Errorf("final source effort = %q, want low; request=%s", got, out)
	}
}

func TestUnsupportedResponsesUpdatesPreserveOtherInputFields(t *testing.T) {
	body := []byte(`{"input":[{"type":"configuration_update","tools":[]},{"type":"function_call_output","call_id":"call_1","output":"ok"}]}`)
	model := &registry.ModelInfo{ID: "test-model", Type: "codex", UserDefined: true}
	out, err := thinking.ApplyThinkingWithModelInfoAndSummary(body, body, "test-model", "codex", "codex", "codex", model, thinking.SummaryConfig{})
	if err != nil {
		t.Fatalf("unsupported updates: %v", err)
	}
	items := gjson.GetBytes(out, "input").Array()
	if len(items) != 1 || items[0].Get("call_id").String() != "call_1" {
		t.Errorf("unrelated input changed: %s", out)
	}
}

func TestConfigurationUpdateRoutingMatrix(t *testing.T) {
	const native = `{"reasoning":{"effort":"xhigh","summary":"auto","other":7},"input":[{"type":"configuration_update","reasoning":{"effort":"low"}},{"role":"user","content":"ok"}]}`
	const nativeInput = `[{"type":"configuration_update","reasoning":{"effort":"low"}},{"role":"user","content":"ok"}]`
	const messages = `[{"role":"user","content":"ok"}]`
	tests := []struct {
		name, body, suffix, wantEffort, wantInput string
		supported, noThinking, wantSame           bool
	}{
		{name: "native baseline", body: native, supported: true, wantEffort: "xhigh", wantInput: nativeInput, wantSame: true},
		{name: "native without thinking metadata", body: native, supported: true, noThinking: true, wantEffort: "xhigh", wantInput: nativeInput, wantSame: true},
		{name: "native without effort", body: `{"reasoning":{"summary":"auto"},"input":[{"type":"configuration_update","tools":[]},{"role":"user","content":"ok"}]}`, supported: true, wantSame: true},
		{name: "native suffix preserves turn", body: native, supported: true, suffix: "high", wantEffort: "high", wantInput: nativeInput},
		{name: "native suffix without thinking", body: native, supported: true, noThinking: true, suffix: "high", wantInput: nativeInput},
		{name: "native suffix creates baseline", body: `{"reasoning":{"summary":"auto"},"input":[{"type":"configuration_update","reasoning":{"effort":"low"}},{"role":"user","content":"ok"}]}`, supported: true, suffix: "high", wantEffort: "high", wantInput: nativeInput},
		{name: "invalid suffix preserves native body", body: `{"reasoning":{"generate_summary":"auto"},"input":[{"type":"configuration_update","reasoning":{"effort":"low"}},{"role":"user","content":"ok"}]}`, supported: true, suffix: "invalid", wantInput: nativeInput, wantSame: true},
		{name: "unsupported latest update", body: `{"reasoning":{"effort":"xhigh","summary":"auto","other":7},"input":[{"role":"user","content":"first"},{"type":"configuration_update","reasoning":{"effort":"low"}},{"type":"configuration_update","reasoning":{"effort":"  "}},{"role":"assistant","content":"reply"},{"type":"configuration_update","reasoning":{"effort":"medium"}},{"type":"configuration_update","tools":[]},{"role":"user","content":"last"}]}`, wantEffort: "medium", wantInput: `[{"role":"user","content":"first"},{"role":"assistant","content":"reply"},{"role":"user","content":"last"}]`},
		{name: "unsupported ignores empty and nonstring", body: `{"reasoning":{"summary":"auto"},"input":[{"type":"configuration_update","reasoning":{"effort":"low"}},{"type":"configuration_update","reasoning":{"effort":42}},{"type":"configuration_update","reasoning":{"effort":null}},{"type":"configuration_update","reasoning":{"effort":"  "}},{"role":"user","content":"ok"}]}`, wantEffort: "low", wantInput: messages},
		{name: "unsupported suffix", body: native, suffix: "high", wantEffort: "high", wantInput: messages},
		{name: "unsupported creates baseline", body: `{"input":[{"type":"configuration_update","reasoning":{"effort":"low"}},{"role":"user","content":"ok"}]}`, wantEffort: "low", wantInput: messages},
		{name: "unsupported tools only", body: `{"reasoning":{"summary":"auto"},"input":[{"type":"configuration_update","tools":[]},{"role":"user","content":"ok"}]}`, wantInput: messages},
		{name: "unsupported tools only without thinking", body: `{"reasoning":{"summary":"auto"},"input":[{"type":"configuration_update","tools":[]},{"role":"user","content":"ok"}]}`, noThinking: true, wantInput: messages},
		{name: "unsupported without thinking preserves summary", body: native, noThinking: true, wantInput: messages},
		{name: "invalid target", body: `{"reasoning":{"effort":"xhigh"},"input":[`, wantSame: true},
		{name: "nonarray input", body: `{"reasoning":{"summary":"auto"},"input":{"type":"configuration_update","reasoning":{"effort":"low"}}}`, wantInput: `{"type":"configuration_update","reasoning":{"effort":"low"}}`, wantSame: true},
		{name: "no input introduced", body: `{"reasoning":{"summary":"auto"},"other":7}`, wantSame: true},
		{name: "only update becomes empty input", body: `{"input":[{"type":"configuration_update","reasoning":{"effort":"low"}}]}`, wantEffort: "low", wantInput: `[]`},
	}
	for _, format := range []string{"codex", "xai", "openai-response"} {
		for _, tc := range tests {
			t.Run(format+"/"+tc.name, func(t *testing.T) {
				info := &registry.ModelInfo{ID: "synthetic-responses", Type: "codex", SupportConfigurationUpdate: tc.supported}
				if !tc.noThinking {
					info.Thinking = &registry.ThinkingSupport{Levels: []string{"low", "medium", "high", "xhigh"}}
				}
				model := info.ID
				if tc.suffix != "" {
					model += "(" + tc.suffix + ")"
				}
				body := []byte(tc.body)
				out, err := thinking.ApplyThinkingWithModelInfoAndSummary(body, body, model, "openai-response", format, "codex", info, thinking.ExtractSummaryConfig(body, "codex"))
				if err != nil {
					t.Fatal(err)
				}
				if tc.wantSame && string(out) != tc.body {
					t.Fatalf("body changed: %s", out)
				}
				if gjson.ValidBytes(body) && gjson.GetBytes(out, "reasoning.effort").String() != tc.wantEffort {
					t.Errorf("effort = %q, want %q; body=%s", gjson.GetBytes(out, "reasoning.effort").String(), tc.wantEffort, out)
				}
				if tc.wantInput != "" && gjson.GetBytes(out, "input").Raw != tc.wantInput {
					t.Errorf("input = %s, want %s", gjson.GetBytes(out, "input").Raw, tc.wantInput)
				}
				for _, path := range []string{"reasoning.summary", "reasoning.other", "other"} {
					if before := gjson.GetBytes(body, path); before.Exists() && gjson.GetBytes(out, path).Raw != before.Raw {
						t.Errorf("%s changed: %s", path, out)
					}
				}
				if tc.supported && tc.suffix != "" && thinking.ExtractTranslatedReasoningEffort(out, format) != "low" {
					t.Errorf("native turn effort was overwritten: %s", out)
				}
			})
		}
	}
}

func TestConfigurationUpdateSourceAcrossProtocols(t *testing.T) {
	const source = `{"reasoning":{"effort":"xhigh","summary":"auto"},"input":[{"type":"configuration_update","reasoning":{"effort":"low"}},{"role":"user","content":"ok"},{"type":"configuration_update","reasoning":{"effort":"high"}}]}`
	for _, tc := range []struct {
		name, format, model, body, path, want string
		supported                             bool
	}{
		{name: "chat", format: "openai", model: "synthetic-chat", body: `{"reasoning_effort":"medium","messages":[{"role":"user","content":"ok"}],"other":true}`, path: "reasoning_effort", want: "high"},
		{name: "chat with Responses capability", format: "openai", model: "synthetic-chat", body: `{"reasoning_effort":"medium","messages":[{"role":"user","content":"ok"}],"other":true}`, supported: true, path: "reasoning_effort", want: "high"},
		{name: "claude", format: "claude", model: "synthetic-claude", body: `{"max_tokens":4096,"thinking":{"type":"adaptive"},"output_config":{"effort":"medium"},"other":true}`, path: "output_config.effort", want: "high"},
		{name: "suffix", format: "openai", model: "synthetic-chat(low)", body: `{"reasoning_effort":"medium","messages":[{"role":"user","content":"ok"}],"other":true}`, path: "reasoning_effort", want: "low"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info := &registry.ModelInfo{ID: "synthetic", Type: tc.format, SupportConfigurationUpdate: tc.supported, Thinking: &registry.ThinkingSupport{Levels: []string{"low", "medium", "high", "xhigh"}}}
			out, err := thinking.ApplyThinkingWithModelInfoAndSummary([]byte(tc.body), []byte(source), tc.model, "openai-response", tc.format, tc.format, info, thinking.SummaryConfig{})
			if err != nil {
				t.Fatal(err)
			}
			if got := gjson.GetBytes(out, tc.path).String(); got != tc.want || !gjson.GetBytes(out, "other").Bool() {
				t.Fatalf("%s = %q, want %q with other preserved: %s", tc.path, got, tc.want, out)
			}
		})
	}
}

func TestConfiguredModelsMapHighEffort(t *testing.T) {
	for _, tc := range []struct {
		name, from, to, kind, effort, want string
		levels                             []string
		wantErr                            bool
	}{
		{name: "xhigh retained", from: "openai", to: "claude", kind: "claude", effort: "xhigh", levels: []string{"high", "max", "xhigh"}, want: "xhigh"},
		{name: "xhigh prefers max", from: "openai", to: "claude", kind: "claude", effort: "xhigh", levels: []string{"high", "max"}, want: "max"},
		{name: "xhigh uses high", from: "openai", to: "claude", kind: "claude", effort: "xhigh", levels: []string{"high"}, want: "high"},
		{name: "max retained", from: "openai", to: "claude", kind: "claude", effort: "max", levels: []string{"high", "xhigh", "max"}, want: "max"},
		{name: "max prefers xhigh", from: "openai", to: "claude", kind: "claude", effort: "max", levels: []string{"high", "xhigh"}, want: "xhigh"},
		{name: "max uses high", from: "openai", to: "claude", kind: "claude", effort: "max", levels: []string{"high"}, want: "high"},
		{name: "original Responses effort", from: "openai-response", to: "claude", kind: "claude", effort: "xhigh", levels: []string{"high", "max"}, want: "max"},
		{name: "Responses to Codex", from: "openai-response", to: "codex", kind: "codex", effort: "max", levels: []string{"high", "xhigh"}, want: "xhigh"},
		{name: "OpenAI compatibility", from: "openai", to: "openai", kind: "openai-compatibility", effort: "xhigh", levels: []string{"high", "max"}, want: "max"},
		{name: "normalized source level", from: "openai", to: "claude", kind: "claude", effort: " XHIGH ", levels: []string{"high", "max"}, want: "max"},
		{name: "native OpenAI remains strict", from: "openai", to: "openai", kind: "openai", effort: "xhigh", levels: []string{"low", "medium", "high"}, wantErr: true},
		{name: "native Codex remains strict", from: "codex", to: "codex", kind: "codex", effort: "max", levels: []string{"high", "xhigh"}, wantErr: true},
		{name: "native Claude remains strict", from: "claude", to: "claude", kind: "claude", effort: "max", levels: []string{"high", "xhigh"}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info := &registry.ModelInfo{ID: "synthetic-route", Type: tc.kind, Thinking: &registry.ThinkingSupport{Levels: tc.levels}}
			body := []byte(`{"thinking":{"type":"adaptive"},"output_config":{"effort":"low"},"other":true}`)
			path := "output_config.effort"
			if tc.to == "openai" {
				body = []byte(`{"reasoning_effort":"low","messages":[],"other":true}`)
				path = "reasoning_effort"
			} else if tc.to == "codex" {
				body = []byte(`{"reasoning":{"effort":"low"},"input":[],"other":true}`)
				path = "reasoning.effort"
			}
			source := []byte(`{"reasoning_effort":"` + tc.effort + `"}`)
			if tc.from == "openai-response" || tc.from == "codex" {
				source = []byte(`{"reasoning":{"effort":"` + tc.effort + `"}}`)
			} else if tc.from == "claude" {
				source = []byte(`{"thinking":{"type":"adaptive"},"output_config":{"effort":"` + tc.effort + `"}}`)
			}
			out, err := thinking.ApplyThinkingWithModelInfoAndSummary(body, source, info.ID, tc.from, tc.to, tc.to, info, thinking.SummaryConfig{})
			if tc.wantErr {
				if err == nil {
					t.Fatalf("unsupported native effort accepted: %s", out)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := gjson.GetBytes(out, path).String(); got != tc.want || !gjson.GetBytes(out, "other").Bool() {
				t.Fatalf("effort = %q, want %q with other preserved; body=%s", got, tc.want, out)
			}
		})
	}
}

func TestConfigurationUpdateMalformedTargetDoesNotBecomeSourceRequest(t *testing.T) {
	const malformed = `{"reasoning":{"effort":"xhigh"},"input":[`
	const source = `{"reasoning":{"effort":"medium"},"input":[{"type":"configuration_update","reasoning":{"effort":"low"}},{"role":"user","content":"ok"}]}`
	info := &registry.ModelInfo{ID: "synthetic-codex", Type: "codex", Thinking: &registry.ThinkingSupport{Levels: []string{"low", "medium", "high", "xhigh"}}}
	for _, modelInfo := range []*registry.ModelInfo{info, nil} {
		for _, normalized := range []bool{false, true} {
			out, err := thinking.ApplyThinkingWithModelInfoAndSummary([]byte(malformed), []byte(source), info.ID, "codex", "codex", "codex", modelInfo, thinking.SummaryConfig{}, normalized)
			if err != nil {
				t.Fatal(err)
			}
			if string(out) != malformed {
				t.Fatalf("normalized=%v: malformed target rebuilt from separate source: %s", normalized, out)
			}
		}
	}
	for _, original := range []string{"", source} {
		for _, normalized := range []bool{false, true} {
			out, err := thinking.ApplyThinkingWithModelInfoAndSummary([]byte(malformed), []byte(original), info.ID+"(high)", "codex", "codex", "codex", info, thinking.SummaryConfig{}, normalized)
			if err != nil {
				t.Fatal(err)
			}
			if string(out) != `{"reasoning":{"effort":"high"}}` {
				t.Fatalf("normalized=%v: suffix no longer controls malformed target: %s", normalized, out)
			}
		}
	}
}

func TestConfigurationUpdateBoundAndUserDefinedCapabilities(t *testing.T) {
	const body = `{"reasoning":{"effort":"xhigh","summary":"auto"},"input":[{"type":"configuration_update","reasoning":{"effort":"low"}},{"role":"user","content":"ok"}]}`
	for _, tc := range []struct {
		name, model, want string
		info              *registry.ModelInfo
		wantUpdates       bool
	}{
		{name: "nil binding does not inherit static support", model: "gpt-6-astra", want: "low"},
		{name: "user defined unsupported", model: "synthetic", info: &registry.ModelInfo{ID: "synthetic", UserDefined: true}, want: "low"},
		{name: "user defined native", model: "synthetic", info: &registry.ModelInfo{ID: "synthetic", UserDefined: true, SupportConfigurationUpdate: true}, want: "xhigh", wantUpdates: true},
		{name: "user defined native suffix", model: "synthetic(high)", info: &registry.ModelInfo{ID: "synthetic", UserDefined: true, SupportConfigurationUpdate: true}, want: "high", wantUpdates: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := thinking.ApplyThinkingWithModelInfoAndSummary([]byte(body), []byte(body), tc.model, "codex", "codex", "codex", tc.info, thinking.ExtractSummaryConfig([]byte(body), "codex"))
			if err != nil {
				t.Fatal(err)
			}
			if gjson.GetBytes(out, "reasoning.effort").String() != tc.want || gjson.GetBytes(out, "reasoning.summary").String() != "auto" {
				t.Fatalf("bound model reasoning changed: %s", out)
			}
			if got := gjson.GetBytes(out, "input.0.type").String() == "configuration_update"; got != tc.wantUpdates {
				t.Fatalf("update present = %v, want %v; body=%s", got, tc.wantUpdates, out)
			}
			if tc.wantUpdates && gjson.GetBytes(out, "input.0.reasoning.effort").String() != "low" {
				t.Fatalf("native turn changed: %s", out)
			}
		})
	}
}

func TestNativeConfigurationUpdateEffectiveEffortLog(t *testing.T) {
	logger := log.StandardLogger()
	previousLevel := logger.GetLevel()
	previousHooks := logger.ReplaceHooks(make(log.LevelHooks))
	hook := logtest.NewLocal(logger)
	logger.SetLevel(log.DebugLevel)
	t.Cleanup(func() {
		logger.ReplaceHooks(previousHooks)
		logger.SetLevel(previousLevel)
	})
	for _, tc := range []struct {
		name, body, effort string
		baseline           thinking.Level
		config             thinking.Config
	}{
		{name: "update", body: `{"reasoning":{"effort":"high"},"input":[{"type":"configuration_update","reasoning":{"effort":"xhigh"}}]}`, effort: "xhigh", baseline: thinking.LevelHigh, config: thinking.Config{Mode: thinking.ModeLevel, Level: thinking.LevelXHigh}},
		{name: "latest", body: `{"reasoning":{"effort":"high"},"input":[{"type":"configuration_update","reasoning":{"effort":"xhigh"}},{"type":"configuration_update","reasoning":{"effort":"max"}}]}`, effort: "max", baseline: thinking.LevelHigh, config: thinking.Config{Mode: thinking.ModeLevel, Level: thinking.LevelMax}},
		{name: "invalid tail", body: `{"reasoning":{"effort":"high"},"input":[{"type":"configuration_update","reasoning":{"effort":"xhigh"}},{"type":"configuration_update","reasoning":{"effort":null}},{"type":"configuration_update","reasoning":{"effort":42}},{"type":"configuration_update","reasoning":{"effort":"  "}}]}`, effort: "xhigh", baseline: thinking.LevelHigh, config: thinking.Config{Mode: thinking.ModeLevel, Level: thinking.LevelXHigh}},
		{name: "baseline", body: `{"reasoning":{"effort":"high"},"input":[{"role":"user","content":"ok"}]}`, effort: "high", baseline: thinking.LevelHigh, config: thinking.Config{Mode: thinking.ModeLevel, Level: thinking.LevelHigh}},
		{name: "no baseline", body: `{"input":[{"type":"configuration_update","reasoning":{"effort":"xhigh"}}]}`, effort: "xhigh", config: thinking.Config{Mode: thinking.ModeLevel, Level: thinking.LevelXHigh}},
		{name: "disabled", body: `{"reasoning":{"effort":"high"},"input":[{"type":"configuration_update","reasoning":{"effort":"none"}}]}`, effort: "none", baseline: thinking.LevelHigh, config: thinking.Config{Mode: thinking.ModeNone}},
	} {
		for _, bound := range []bool{false, true} {
			t.Run(tc.name+"/"+map[bool]string{false: "registry", true: "bound"}[bound], func(t *testing.T) {
				hook.Reset()
				body := []byte(tc.body)
				var out []byte
				var err error
				if bound {
					info := registry.LookupModelInfo("gpt-6-sol", "codex")
					if info == nil || !info.SupportConfigurationUpdate {
						t.Fatal("registry model lacks native update support")
					}
					out, err = thinking.ApplyThinkingWithModelInfoAndSummary(body, body, info.ID, "codex", "codex", "codex", info, thinking.SummaryConfig{})
				} else {
					out, err = thinking.ApplyThinking(body, "gpt-6-sol", "codex", "codex", "codex")
				}
				if err != nil || string(out) != tc.body || thinking.ExtractTranslatedReasoningEffort(out, "codex") != tc.effort {
					t.Fatalf("native body or effective effort changed: %s; err=%v", out, err)
				}
				for _, message := range []string{"thinking: original config from request |", "thinking: processed config to apply |"} {
					found := false
					for _, entry := range hook.AllEntries() {
						if entry.Message != message {
							continue
						}
						found = true
						if entry.Level != log.DebugLevel || entry.Data["provider"] != "codex" || entry.Data["model"] != "gpt-6-sol" || entry.Data["level"] != tc.config.Level || entry.Data["mode"] != tc.config.Mode || entry.Data["budget"] != 0 {
							t.Errorf("unexpected fields: %v", entry.Data)
						}
						baseline, present := entry.Data["baseline_level"]
						if tc.baseline == "" && present || tc.baseline != "" && baseline != tc.baseline {
							t.Errorf("baseline = %v (present %v), want %q", baseline, present, tc.baseline)
						}
					}
					if !found {
						t.Errorf("missing native effective effort log: %s", message)
					}
				}
			})
		}
	}
}
