package common

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestAttachToolMessageCacheControlFindsToolResultAndValidSource(t *testing.T) {
	msg := []byte(`{"role":"user","content":[{"type":"text","text":"before"},{"type":"tool_result","tool_use_id":"one","content":[]},{"type":"tool_result","tool_use_id":"two","content":[]}]}`)
	for _, source := range []string{
		`{"content":[{"cache_control":{"type":null}},{"cache_control":{"type":"ephemeral","ttl":"1h","future":true}}],"cache_control":{"type":"ephemeral","ttl":"5m"}}`,
		`{"content":{"cache_control":{"type":"ephemeral","ttl":"1h","future":true}}}`,
		`{"content":[{"cache_control":{"type":" ephemeral "}}],"cache_control":{"type":"ephemeral","ttl":"1h","future":true}}`,
	} {
		out := AttachToolMessageCacheControl(msg, gjson.Parse(source))
		if gjson.GetBytes(out, "content.1.cache_control.ttl").String() != "1h" ||
			!gjson.GetBytes(out, "content.1.cache_control.future").Bool() ||
			gjson.GetBytes(out, "content.0.cache_control").Exists() ||
			gjson.GetBytes(out, "content.2.cache_control").Exists() {
			t.Fatalf("tool result cache placement = %s", out)
		}
	}
}

func TestAttachCacheControlCopiesObject(t *testing.T) {
	source := gjson.Parse(`{"cache_control":{"type":"ephemeral","ttl":"5m"}}`)
	out := AttachCacheControl([]byte(`{"type":"text","text":"cached"}`), source)

	if got := gjson.GetBytes(out, "cache_control.type").String(); got != "ephemeral" {
		t.Fatalf("cache_control.type = %q, want ephemeral", got)
	}
	if got := gjson.GetBytes(out, "cache_control.ttl").String(); got != "5m" {
		t.Fatalf("cache_control.ttl = %q, want 5m", got)
	}
}

func TestAttachCacheControlIgnoresInvalidValues(t *testing.T) {
	for _, raw := range []string{
		`{}`, `{"cache_control":null}`, `{"cache_control":"ephemeral"}`,
		`{"cache_control":{}}`, `{"cache_control":{"type":"persistent"}}`,
		`{"cache_control":{"type":"Ephemeral"}}`, `{"cache_control":{"type":1}}`,
		`{"cache_control":{"type":null}}`, `{"cache_control":{"type":" ephemeral "}}`,
	} {
		out := AttachCacheControl([]byte(`{"type":"text","text":"fresh"}`), gjson.Parse(raw))
		if gjson.GetBytes(out, "cache_control").Exists() {
			t.Fatalf("cache_control should be absent for source %s", raw)
		}
	}
}
