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
		{name: "default", wantUID: "swe-2-high", wantBase: "devin/swe-2"},
		{name: "namespace", model: "devin/swe-2", wantUID: "swe-2-high", wantBase: "devin/swe-2"},
		{name: "unsupported explicit low clamps", model: "devin/swe-2-low", wantUID: "swe-2-medium", wantBase: "devin/swe-2"},
		{name: "explicit xhigh tie clamps upward", model: "devin/swe-2-xhigh", wantUID: "swe-2-max", wantBase: "devin/swe-2"},
		{name: "parenthesized suffix wins", model: "devin/swe-2-low(max)", wantUID: "swe-2-max", wantBase: "devin/swe-2"},
		{name: "colon suffix", model: "devin/swe-2:low", wantUID: "swe-2-medium", wantBase: "devin/swe-2"},
		{name: "priority preserves clamped effort", model: "devin/swe-2-low-priority", wantUID: "swe-2-medium-priority", wantBase: "devin/swe-2"},
		{name: "gpt dot normalization", model: "devin/gpt-5.6-sol", wantUID: "gpt-5-6-sol-low", wantBase: "devin/gpt-5-6-sol"},
		{name: "supported none", model: "devin/gpt-5-6-sol-none", wantUID: "gpt-5-6-sol-none", wantBase: "devin/gpt-5-6-sol"},
		{name: "unsupported fast does not bypass catalog", model: "devin/gpt-5-6-sol-fast", wantUID: "gpt-5-6-sol-low", wantBase: "devin/gpt-5-6-sol"},
		{name: "numeric suffix", model: "devin/gpt-5-6-sol(20000)", wantUID: "gpt-5-6-sol-high", wantBase: "devin/gpt-5-6-sol"},
		{name: "non-thinking model stays bare", model: "devin/kimi-k2-7-high", wantUID: "kimi-k2-7", wantBase: "devin/kimi-k2-7"},
		{name: "slow model", model: "devin/swe-1-6-slow", wantUID: "swe-1-6-slow", wantBase: "devin/swe-1-6-slow"},
		{name: "slow model without namespace", model: "swe-1-6-slow", wantUID: "swe-1-6-slow", wantBase: "devin/swe-1-6-slow"},
		{name: "slow model low effort", model: "devin/swe-1-6-slow:low", wantUID: "swe-1-6-slow", wantBase: "devin/swe-1-6-slow"},
		{name: "slow model high effort", model: "devin/swe-1-6-slow:high", wantUID: "swe-1-6-slow", wantBase: "devin/swe-1-6-slow"},
		{name: "gemini alias", model: "devin/gemini-3-flash", wantUID: "gemini-3-8-flash-high", wantBase: "devin/gemini-3-8-flash"},
		{name: "private Haiku alias", model: "devin/claude-haiku-4-5", wantUID: "MODEL_PRIVATE_11", wantBase: "devin/claude-haiku-4-5"},
		{name: "private Sonnet thinking alias", model: "devin/claude-sonnet-4-5(high)", wantUID: "MODEL_PRIVATE_3", wantBase: "devin/claude-sonnet-4-5"},
		{name: "private GPT 4.1 alias", model: "devin/gpt-4.1", wantUID: "MODEL_CHAT_GPT_4_1_2025_04_14", wantBase: "devin/gpt-4-1"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
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
		})
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
		if got := NormalizeThinkingLevel(test.level, test.budget); got != test.want {
			t.Fatalf("NormalizeThinkingLevel(%q, %d) = %q, want %q", test.level, test.budget, got, test.want)
		}
	}
}

func TestHasDevinEffortSuffix(t *testing.T) {
	for _, model := range []string{"swe-2-high", "swe-2-low-priority", "gpt-5-6-sol-none", "swe-1-6-slow"} {
		if !HasDevinEffortSuffix(model) {
			t.Fatalf("HasDevinEffortSuffix(%q) = false", model)
		}
	}
	if HasDevinEffortSuffix("swe-2") {
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
		{name: "default", model: "devin/glm-5-2-1m", want: "glm-5-2-none-1m"},
		{name: "none", model: "devin/glm-5-2-1m", level: "none", want: "glm-5-2-none-1m"},
		{name: "max", model: "devin/glm-5-2-1m", level: "max", want: "glm-5-2-max-1m"},
		{name: "catalog clamps high", model: "devin/glm-5-2-1m", level: "high", want: "glm-5-2-max-1m"},
		{name: "budget", model: "devin/glm-5-2-1m", budget: 32769, want: "glm-5-2-max-1m"},
		{name: "none UID", model: "glm-5-2-none-1m", want: "glm-5-2-none-1m"},
		{name: "max UID", model: "devin/glm-5-2-max-1m", want: "glm-5-2-max-1m"},
		{name: "UID request effort", model: "devin/glm-5-2-max-1m", level: "none", want: "glm-5-2-none-1m"},
		{name: "UID parenthesized effort", model: "devin/glm-5-2-max-1m(none)", want: "glm-5-2-none-1m"},
		{name: "UID colon effort", model: "devin/glm-5-2-none-1m:max", want: "glm-5-2-max-1m"},
		{name: "case and spaces", model: " DEVIN/GLM-5-2-MAX-1M ", want: "glm-5-2-max-1m"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			uid, info, err := ResolveDevinChatModel(test.model, test.level, test.budget)
			if err != nil || uid != test.want {
				t.Fatalf("ResolveDevinChatModel(%q, %q, %d) = %q, %v; want %q", test.model, test.level, test.budget, uid, err, test.want)
			}
			if info == nil || info.ID != "devin/glm-5-2-1m" || info.MaxCompletionTokens != 64000 || info.ContextLength != 1000000 {
				t.Fatalf("million-context metadata = %#v", info)
			}
			if !HasDevinEffortSuffix(uid) {
				t.Fatalf("UID %q does not retain its effort", uid)
			}
			again, roundTripInfo, roundTripErr := ResolveDevinChatModel(uid, "", 0)
			if roundTripErr != nil || again != uid || roundTripInfo == nil || roundTripInfo.ID != info.ID {
				t.Fatalf("UID round trip = %q, %#v, %v; want %q", again, roundTripInfo, roundTripErr, uid)
			}
		})
	}
	for _, model := range []string{"devin/not-a-model-none-1m", "devin/glm-5-3-max-1m"} {
		if uid, info, err := ResolveDevinChatModel(model, "", 0); err == nil || uid != "" || info != nil {
			t.Fatalf("unknown million-context model %q resolved to %q, %#v, %v", model, uid, info, err)
		}
	}
}
