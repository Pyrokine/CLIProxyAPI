package executor

import (
	"net/http"
	"strings"
	"testing"
)

func TestParseCodexEventErrorReadsNestedResponseFailure(t *testing.T) {
	err, ok := parseCodexEventError(
		[]byte(`{"type":"response.failed","response":{"status_code":429,"error":{"type":"rate_limit_error","message":"try later"}}}`),
	)
	if !ok || err == nil {
		t.Fatalf("parseCodexEventError = (%v, %t), want error", err, ok)
	}
	if got := statusErrCode(err); got != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d", got, http.StatusTooManyRequests)
	}
	if !strings.Contains(err.Error(), "try later") {
		t.Fatalf("error = %q, want nested response error message", err.Error())
	}
}
