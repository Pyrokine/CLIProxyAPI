package common

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestEchoRequestFieldsPreservesExactJSONNumbers(t *testing.T) {
	request := gjson.Parse(
		"{\"tools\":[{\n\"type\":\"function\",\"name\":\"lookup\",\"parameters" +
			`":{"type":"object","properties":{"id":{"type":"integer","enum":[900719` +
			`9254740993,18446744073709551615]}}}}],"metadata":{"number":90071992547` +
			`40993},"reasoning":{"budget":9007199254740993}}`,
	)
	for _, prefix := range []string{"", "response."} {
		out := EchoRequestFields([]byte(`{}`), request, prefix)
		if strings.ContainsAny(string(out), "\n\r") {
			t.Fatalf("raw fields broke single-line SSE payload: %s", out)
		}
		for _, path := range []string{"tools", "metadata", "reasoning"} {
			if got, want := gjson.GetBytes(out, prefix+path).Raw, gjson.Get(
				request.Get(path).Raw, "@ugly",
			).Raw; got != want {
				t.Fatalf("%s changed raw JSON: got %s, want %s", prefix+path, got, want)
			}
		}
	}
}
