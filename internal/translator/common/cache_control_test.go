package common

import (
	"testing"

	"github.com/tidwall/gjson"
)

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
	for _, raw := range []string{`{}`, `{"cache_control":null}`, `{"cache_control":"ephemeral"}`} {
		out := AttachCacheControl([]byte(`{"type":"text","text":"fresh"}`), gjson.Parse(raw))
		if gjson.GetBytes(out, "cache_control").Exists() {
			t.Fatalf("cache_control should be absent for source %s", raw)
		}
	}
}
