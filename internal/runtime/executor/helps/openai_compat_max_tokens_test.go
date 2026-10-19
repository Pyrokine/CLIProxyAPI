package helps

import (
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/tidwall/gjson"
)

func TestShouldUseMaxCompletionTokensForModel(t *testing.T) {
	compat := &config.OpenAICompatibility{Models: []config.OpenAICompatibilityModel{
		{Name: "upstream-a", Alias: "shared", UseMaxCompletionTokens: true},
		{Name: "upstream-b", Alias: "shared"},
	}}
	for _, tc := range []struct {
		name      string
		upstream  string
		requested string
		want      bool
	}{
		{name: "resolved model takes precedence", upstream: "upstream-b", requested: "shared", want: false},
		{name: "alias fallback", upstream: "missing", requested: "shared", want: true},
		{name: "case insensitive", upstream: "UPSTREAM-A", want: true},
		{name: "unknown model", upstream: "missing", requested: "missing", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ShouldUseMaxCompletionTokensForModel(compat, tc.upstream, tc.requested); got != tc.want {
				t.Fatalf("preference = %t, want %t", got, tc.want)
			}
		})
	}
}

func TestNormalizeOpenAIMaxTokens(t *testing.T) {
	for _, tc := range []struct {
		name     string
		payload  string
		useMCT   bool
		wantKey  string
		omitKey  string
		wantJSON string
	}{
		{name: "enabled with legacy field", payload: `{"max_tokens":128,"model":"upstream"}`, useMCT: true,
			wantKey: "max_completion_tokens", omitKey: "max_tokens", wantJSON: "128"},
		{name: "disabled with new field", payload: `{"max_completion_tokens":256,"model":"upstream"}`,
			wantKey: "max_tokens", omitKey: "max_completion_tokens", wantJSON: "256"},
		{name: "enabled prefers existing new field", payload: `{"max_tokens":128,"max_completion_tokens":256}`, useMCT: true,
			wantKey: "max_completion_tokens", omitKey: "max_tokens", wantJSON: "256"},
		{name: "disabled prefers existing legacy field", payload: `{"max_tokens":128,"max_completion_tokens":256}`,
			wantKey: "max_tokens", omitKey: "max_completion_tokens", wantJSON: "128"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := NormalizeOpenAIMaxTokens([]byte(tc.payload), tc.useMCT)
			if err != nil {
				t.Fatal(err)
			}
			if got := gjson.GetBytes(out, tc.wantKey); got.Raw != tc.wantJSON {
				t.Fatalf("%s = %s, want %s in %s", tc.wantKey, got.Raw, tc.wantJSON, out)
			}
			if gjson.GetBytes(out, tc.omitKey).Exists() {
				t.Fatalf("%s should be removed from %s", tc.omitKey, out)
			}
		})
	}
}
