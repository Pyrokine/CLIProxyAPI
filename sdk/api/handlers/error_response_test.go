package handlers

import (
	"net/http"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestBuildErrorResponseBodyCompactsUpstreamJSONForSSE(t *testing.T) {
	body := BuildErrorResponseBody(http.StatusBadRequest, "  {\n  \"error\": {\n    \"message\": \"first\\nsecond\"\n  }\n}  ")
	if strings.Contains(string(body), "\n") || gjson.GetBytes(body, "error.message").String() != "first\nsecond" {
		t.Fatalf("upstream error JSON not compacted without changing its payload: %q", body)
	}
}

func TestBuildErrorResponseBodyClassifiesRequestTimeout(t *testing.T) {
	body := BuildErrorResponseBody(http.StatusRequestTimeout, "request body timed out")
	if gjson.GetBytes(body, "error.type").String() != "server_error" ||
		gjson.GetBytes(body, "error.code").String() != "request_timeout" {
		t.Fatalf("408 body = %s", body)
	}
}
