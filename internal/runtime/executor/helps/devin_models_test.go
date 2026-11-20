package helps

import "testing"

func TestResolveDevinChatModelUsesCatalogCapabilities(t *testing.T) {
	tests := []struct {
		name     string
		model    string
		level    string
		budget   int
		wantUID  string
		wantBase string
	}{
		{name: "default", wantUID: "swe-2", wantBase: "devin/swe-2"},
		{name: "namespace", model: "devin/swe-2", wantUID: "swe-2", wantBase: "devin/swe-2"},
		{name: "SWE 1.7 bare default", model: "devin/swe-1-7", wantUID: "swe-1-7", wantBase: "devin/swe-1-7"},
		{
			name: "SWE lightning bare default", model: "devin/swe-2-lightning", wantUID: "swe-2-lightning",
			wantBase: "devin/swe-2-lightning",
		},
		{
			name: "priority without effort", model: "devin/swe-2-priority", wantUID: "swe-2-priority",
			wantBase: "devin/swe-2",
		},
		{name: "body effort", model: "devin/swe-2", level: "medium", wantUID: "swe-2-medium", wantBase: "devin/swe-2"},
		{
			name: "SWE flash effort", model: "devin/swe-2-flash(high)", wantUID: "swe-2-flash-high",
			wantBase: "devin/swe-2-flash",
		},
		{
			name: "Fable 5.2 effort", model: "devin/claude-fable-5-2(xhigh)", wantUID: "claude-fable-5-2-xhigh",
			wantBase: "devin/claude-fable-5-2",
		},
		{
			name: "Gemini Carbon effort", model: "devin/gemini-4-carbon(high)", wantUID: "gemini-4-carbon-high",
			wantBase: "devin/gemini-4-carbon",
		},
		{
			name: "GPT 6.1 Terra effort", model: "devin/gpt-6.1-terra(high)", wantUID: "gpt-6-1-terra-high",
			wantBase: "devin/gpt-6-1-terra",
		},
		{name: "body budget", model: "devin/swe-2", budget: 40000, wantUID: "swe-2-max", wantBase: "devin/swe-2"},
		{
			name: "explicit none uses supported default", model: "devin/swe-2", level: "none", wantUID: "swe-2-high",
			wantBase: "devin/swe-2",
		},
		{
			name: "unsupported explicit low clamps", model: "devin/swe-2-low", wantUID: "swe-2-medium",
			wantBase: "devin/swe-2",
		},
		{
			name: "explicit xhigh tie clamps upward", model: "devin/swe-2-xhigh", wantUID: "swe-2-max",
			wantBase: "devin/swe-2",
		},
		{
			name: "parenthesized suffix wins", model: "devin/swe-2-low(max)", wantUID: "swe-2-max",
			wantBase: "devin/swe-2",
		},
		{name: "colon suffix", model: "devin/swe-2:low", wantUID: "swe-2-medium", wantBase: "devin/swe-2"},
		{
			name: "priority preserves clamped effort", model: "devin/swe-2-low-priority",
			wantUID: "swe-2-medium-priority", wantBase: "devin/swe-2",
		},
		{
			name: "gpt dot normalization", model: "devin/gpt-5.6-sol", wantUID: "gpt-5-6-sol-low",
			wantBase: "devin/gpt-5-6-sol",
		},
		{
			name: "supported none", model: "devin/gpt-5-6-sol-none", wantUID: "gpt-5-6-sol-none",
			wantBase: "devin/gpt-5-6-sol",
		},
		{
			name: "unsupported fast does not bypass catalog", model: "devin/gpt-5-6-sol-fast",
			wantUID: "gpt-5-6-sol-low", wantBase: "devin/gpt-5-6-sol",
		},
		{
			name: "numeric suffix", model: "devin/gpt-5-6-sol(20000)", wantUID: "gpt-5-6-sol-high",
			wantBase: "devin/gpt-5-6-sol",
		},
		{
			name: "non-thinking model stays bare", model: "devin/kimi-k2-7-high", wantUID: "kimi-k2-7",
			wantBase: "devin/kimi-k2-7",
		},
		{name: "slow model", model: "devin/swe-1-6-slow", wantUID: "swe-1-6-slow", wantBase: "devin/swe-1-6-slow"},
		{
			name: "slow model without namespace", model: "swe-1-6-slow", wantUID: "swe-1-6-slow",
			wantBase: "devin/swe-1-6-slow",
		},
		{
			name: "slow model low effort", model: "devin/swe-1-6-slow:low", wantUID: "swe-1-6-slow",
			wantBase: "devin/swe-1-6-slow",
		},
		{
			name: "slow model high effort", model: "devin/swe-1-6-slow:high", wantUID: "swe-1-6-slow",
			wantBase: "devin/swe-1-6-slow",
		},
		{
			name: "gemini alias", model: "devin/gemini-3-flash", wantUID: "gemini-3-8-flash-high",
			wantBase: "devin/gemini-3-8-flash",
		},
		{
			name: "private Haiku alias", model: "devin/claude-haiku-4-5", wantUID: "MODEL_PRIVATE_11",
			wantBase: "devin/claude-haiku-4-5",
		},
		{
			name: "private Sonnet thinking alias", model: "devin/claude-sonnet-4-5(high)", wantUID: "MODEL_PRIVATE_3",
			wantBase: "devin/claude-sonnet-4-5",
		},
		{
			name: "private GPT 4.1 alias", model: "devin/gpt-4.1", wantUID: "MODEL_CHAT_GPT_4_1_2025_04_14",
			wantBase: "devin/gpt-4-1",
		},
	}
	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				uid, model, err := ResolveDevinChatModel(test.model, test.level, test.budget)
				if err != nil {
					t.Fatalf("ResolveDevinChatModel(): %v", err)
				}
				if uid != test.wantUID {
					t.Fatalf("UID = %q, want %q", uid, test.wantUID)
				}
				if model == nil || model.ID != test.wantBase || model.MaxCompletionTokens <= 0 {
					t.Fatalf("catalog model = %+v, want %q with completion limit", model, test.wantBase)
				}
			},
		)
	}
}

func TestResolveDevinChatModelRejectsUnknownModel(t *testing.T) {
	if _, _, err := ResolveDevinChatModel("devin/not-a-model-high", "", 0); err == nil {
		t.Fatal("ResolveDevinChatModel(unknown) error = nil")
	}
}

func TestNormalizeThinkingLevel(t *testing.T) {
	tests := []struct {
		level  string
		budget int
		want   string
	}{
		{level: "off", want: "none"},
		{level: "adaptive", want: "high"},
		{level: "8192", want: "medium"},
		{budget: 4096, want: "low"},
		{budget: 16384, want: "medium"},
		{budget: 32768, want: "high"},
		{budget: 32769, want: "max"},
	}
	for _, test := range tests {
		if got := normalizeThinkingLevel(test.level, test.budget); got != test.want {
			t.Fatalf("NormalizeThinkingLevel(%q, %d) = %q, want %q", test.level, test.budget, got, test.want)
		}
	}
}

func TestHasDevinEffortSuffix(t *testing.T) {
	for _, model := range []string{"swe-2-high", "swe-2-low-priority", "gpt-5-6-sol-none", "swe-1-6-slow"} {
		if !hasDevinEffortSuffix(model) {
			t.Fatalf("HasDevinEffortSuffix(%q) = false", model)
		}
	}
	if hasDevinEffortSuffix("swe-2") {
		t.Fatal("HasDevinEffortSuffix(swe-2) = true")
	}
}

func TestResolveDevinMillionContextModel(t *testing.T) {
	tests := []struct {
		name   string
		model  string
		level  string
		budget int
		want   string
	}{
		{name: "default", model: "devin/glm-5-2-1m", want: "glm-5-2-1m"},
		{name: "none", model: "devin/glm-5-2-1m", level: "none", want: "glm-5-2-none-1m"},
		{name: "max", model: "devin/glm-5-2-1m", level: "max", want: "glm-5-2-max-1m"},
		{name: "bare high variant", model: "devin/glm-5-2-1m", level: "high", want: "glm-5-2-1m"},
		{name: "budget", model: "devin/glm-5-2-1m", budget: 32769, want: "glm-5-2-max-1m"},
		{name: "none UID", model: "glm-5-2-none-1m", want: "glm-5-2-none-1m"},
		{name: "max UID", model: "devin/glm-5-2-max-1m", want: "glm-5-2-max-1m"},
		{name: "UID request effort", model: "devin/glm-5-2-max-1m", level: "none", want: "glm-5-2-none-1m"},
		{name: "UID parenthesized effort", model: "devin/glm-5-2-max-1m(none)", want: "glm-5-2-none-1m"},
		{name: "UID colon effort", model: "devin/glm-5-2-none-1m:max", want: "glm-5-2-max-1m"},
		{name: "case and spaces", model: " DEVIN/GLM-5-2-MAX-1M ", want: "glm-5-2-max-1m"},
	}
	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				uid, info, err := ResolveDevinChatModel(test.model, test.level, test.budget)
				if err != nil || uid != test.want {
					t.Fatalf(
						"ResolveDevinChatModel(%q, %q, %d) = %q, %v; want %q", test.model, test.level, test.budget, uid,
						err, test.want,
					)
				}
				if info == nil || info.ID != "devin/glm-5-2-1m" || info.MaxCompletionTokens != 64000 ||
					info.ContextLength != 1000000 {
					t.Fatalf("million-context metadata = %#v", info)
				}
				if uid != "glm-5-2-1m" && !hasDevinEffortSuffix(uid) {
					t.Fatalf("UID %q does not retain its effort", uid)
				}
				again, roundTripInfo, roundTripErr := ResolveDevinChatModel(uid, "", 0)
				if roundTripErr != nil || again != uid || roundTripInfo == nil || roundTripInfo.ID != info.ID {
					t.Fatalf("UID round trip = %q, %#v, %v; want %q", again, roundTripInfo, roundTripErr, uid)
				}
			},
		)
	}
	for _, model := range []string{"devin/not-a-model-none-1m", "devin/glm-5-3-max-1m"} {
		if uid, info, err := ResolveDevinChatModel(model, "", 0); err == nil || uid != "" || info != nil {
			t.Fatalf("unknown million-context model %q resolved to %q, %#v, %v", model, uid, info, err)
		}
	}
}

func TestResolveDevinPublishedUIDCompatibility(t *testing.T) {
	for _, test := range []struct {
		name   string
		model  string
		level  string
		budget int
		want   string
	}{
		{name: "GLM high bare", model: "devin/glm-5-2", level: "high", want: "glm-5-2"},
		{name: "SWE 1.7 default", model: "devin/swe-1-7", want: "swe-1-7"},
		{name: "SWE 1.6 fast", model: "devin/swe-1-6:fast", want: "swe-1-6-fast"},
		{name: "Opus thinking", model: "devin/claude-opus-4-6", level: "high", want: "claude-opus-4-6-thinking"},
		{name: "Sonnet thinking", model: "devin/claude-sonnet-4-6", level: "high", want: "claude-sonnet-4-6-thinking"},
		{
			name: "Opus million thinking", model: "devin/claude-opus-4-6-1m", level: "high",
			want: "claude-opus-4-6-thinking-1m",
		},
		{
			name: "Sonnet million thinking", model: "devin/claude-sonnet-4-6-1m", level: "high",
			want: "claude-sonnet-4-6-thinking-1m",
		},
		{name: "GLM million default", model: "devin/glm-5-2-1m", want: "glm-5-2-1m"},
		{name: "GPT enum default", model: "devin/MODEL_GPT_5_2", want: "MODEL_GPT_5_2_LOW"},
		{name: "GPT enum none", model: "devin/MODEL_GPT_5_2", level: "none", want: "MODEL_GPT_5_2_NONE"},
		{name: "GPT enum medium", model: "devin/MODEL_GPT_5_2", level: "medium", want: "MODEL_GPT_5_2_MEDIUM"},
		{name: "GPT enum high", model: "devin/MODEL_GPT_5_2", level: "high", want: "MODEL_GPT_5_2_HIGH"},
		{name: "GPT enum xhigh", model: "devin/MODEL_GPT_5_2", level: "xhigh", want: "MODEL_GPT_5_2_XHIGH"},
		{
			name: "Gemini enum default", model: "devin/MODEL_GOOGLE_GEMINI_3_0_FLASH",
			want: "MODEL_GOOGLE_GEMINI_3_0_FLASH_HIGH",
		},
		{
			name: "Gemini enum minimal", model: "devin/MODEL_GOOGLE_GEMINI_3_0_FLASH", level: "minimal",
			want: "MODEL_GOOGLE_GEMINI_3_0_FLASH_MINIMAL",
		},
		{
			name: "Gemini enum low", model: "devin/MODEL_GOOGLE_GEMINI_3_0_FLASH", level: "low",
			want: "MODEL_GOOGLE_GEMINI_3_0_FLASH_LOW",
		},
		{
			name: "Gemini enum medium", model: "devin/MODEL_GOOGLE_GEMINI_3_0_FLASH", level: "medium",
			want: "MODEL_GOOGLE_GEMINI_3_0_FLASH_MEDIUM",
		},
		{
			name: "Gemini enum high", model: "devin/MODEL_GOOGLE_GEMINI_3_0_FLASH", level: "high",
			want: "MODEL_GOOGLE_GEMINI_3_0_FLASH_HIGH",
		},
		{name: "Opus enum default", model: "devin/MODEL_CLAUDE_4_5_OPUS", want: "MODEL_CLAUDE_4_5_OPUS"},
		{
			name: "Opus enum high", model: "devin/MODEL_CLAUDE_4_5_OPUS", level: "high",
			want: "MODEL_CLAUDE_4_5_OPUS_THINKING",
		},
		{name: "GLM default", model: "devin/glm-5-2", want: "glm-5-2"},
		{name: "GLM explicit none", model: "devin/glm-5-2:none", want: "glm-5-2-none"},
		{name: "GLM explicit max", model: "devin/glm-5-2(max)", want: "glm-5-2-max"},
		{name: "GPT enum numeric budget", model: "devin/MODEL_GPT_5_2", budget: 20000, want: "MODEL_GPT_5_2_HIGH"},
		{
			name: "GPT enum suffix precedence", model: "devin/MODEL_GPT_5_2_XHIGH", level: "none",
			want: "MODEL_GPT_5_2_XHIGH",
		},
		{name: "GPT enum parenthesis precedence", model: "devin/MODEL_GPT_5_2_XHIGH(none)", want: "MODEL_GPT_5_2_NONE"},
		{
			name: "Opus binary suffix precedence", model: "devin/claude-opus-4-6-thinking", level: "none",
			want: "claude-opus-4-6-thinking",
		},
		{
			name: "Opus binary parenthesis precedence", model: "devin/claude-opus-4-6-thinking(none)",
			want: "claude-opus-4-6",
		},
		{
			name: "Sonnet million suffix precedence", model: "devin/claude-sonnet-4-6-thinking-1m", level: "none",
			want: "claude-sonnet-4-6-thinking-1m",
		},
		{
			name: "Opus enum suffix precedence", model: "devin/MODEL_CLAUDE_4_5_OPUS_THINKING", level: "none",
			want: "MODEL_CLAUDE_4_5_OPUS_THINKING",
		},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				uid, info, err := ResolveDevinChatModel(test.model, test.level, test.budget)
				if err != nil || uid != test.want {
					t.Fatalf(
						"ResolveDevinChatModel(%q, %q, %d) = %q, %v; want %q", test.model, test.level, test.budget, uid,
						err, test.want,
					)
				}
				if info == nil || info.ContextLength <= 0 || info.MaxCompletionTokens != 64000 {
					t.Fatalf("published model capabilities = %#v", info)
				}
				again, _, err := ResolveDevinChatModel(uid, "", 0)
				if err != nil || again != uid {
					t.Fatalf("published UID round trip = %q, %v; want %q", again, err, uid)
				}
			},
		)
	}
	for _, uid := range []string{
		"MODEL_PRIVATE_2", "MODEL_PRIVATE_3", "MODEL_PRIVATE_11", "MODEL_PRIVATE_12", "MODEL_PRIVATE_13",
		"MODEL_PRIVATE_14", "MODEL_PRIVATE_15", "MODEL_CHAT_GPT_4_1_2025_04_14",
	} {
		if got, _, err := ResolveDevinChatModel("devin/"+uid, "", 0); err != nil || got != uid {
			t.Errorf("published private UID %q resolved to %q, %v", uid, got, err)
		}
	}
	for _, model := range []string{
		"devin/MODEL_PRIVATE_999", "devin/MODEL_GPT_5_2_CUSTOM", "devin/custom-claude-sonnet-4-5",
		"devin/prefix-claude-opus-4-6-thinking",
	} {
		if uid, info, err := ResolveDevinChatModel(model, "high", 0); err == nil || uid != "" || info != nil {
			t.Errorf("unknown model %q resolved to %q, %#v, %v", model, uid, info, err)
		}
	}
}
