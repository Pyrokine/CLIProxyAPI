package executor

import (
	"net/http"
	"strings"
	"testing"
)

func TestParseCodexEventErrorInfersStatusFromErrorType(t *testing.T) {
	tests := []struct {
		name       string
		payload    string
		wantStatus int
	}{
		{
			name:       "invalid request",
			payload:    `{"type":"response.failed","response":{"error":{"type":"bad_request_error","message":"bad input"}}}`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "cyber policy",
			payload:    `{"type":"error","error":{"type":"invalid_request","code":"cyber_policy","message":"blocked"}}`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "cyber policy overrides gateway status",
			payload: `{"type":"error","status":502,` +
				`"error":{"type":"invalid_request","code":"cyber_policy","message":"blocked"}}`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "rate limit",
			payload:    `{"type":"response.failed","response":{"error":{"type":"rate_limit_error","message":"try later"}}}`,
			wantStatus: http.StatusTooManyRequests,
		},
		{
			name:       "missing details",
			payload:    `{"type":"response.failed","response":{}}`,
			wantStatus: http.StatusBadGateway,
		},
	}
	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				err, ok := parseCodexEventError([]byte(test.payload))
				if !ok || err == nil {
					t.Fatalf("parseCodexEventError = (%v, %t), want error", err, ok)
				}
				if got := statusErrCode(err); got != test.wantStatus {
					t.Fatalf("status = %d, want %d", got, test.wantStatus)
				}
			},
		)
	}
}

func TestParseCodexEventErrorReadsNestedResponseFailure(t *testing.T) {
	payload := `{"type":"response.failed","response":{"status_code":429,` +
		`"error":{"type":"rate_limit_error","message":"try later"}}}`
	err, ok := parseCodexEventError([]byte(payload))
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
