package chat_completions

import (
	"fmt"
	"testing"

	"github.com/tidwall/gjson"
)

func TestCodexChatCanonicalServiceTier(t *testing.T) {
	for tier, want := range map[string]string{"fast": "priority", " PRIORITY ": "priority", " ULTRAFAST ": "ultrafast", "flex": "flex", "scale": "scale"} {
		t.Run(tier, func(t *testing.T) {
			out := ConvertOpenAIRequestToCodex("fixture-model", []byte(fmt.Sprintf(`{"messages":[{"role":"user","content":"hello"}],"service_tier":%q}`, tier)), true)
			if got := gjson.GetBytes(out, "service_tier").String(); got != want {
				t.Fatalf("service tier = %q, want %q; %s", got, want, out)
			}
		})
	}
}
