package executor

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	claudeauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/claude"
)

func claudeOAuthTestMetadata() map[string]any {
	return map[string]any{
		"account_uuid": "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
		claudeauth.DeviceIDsMetadataKey: []string{
			"0000000000000000000000000000000000000000000000000000000000000000",
		},
	}
}

func newClaudeHeaderTestRequest(t *testing.T, incoming http.Header) *http.Request {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", nil)
	if incoming != nil {
		req.Header = incoming.Clone()
	}
	return req
}

func extractTag(header, prefix string) string {
	index := strings.Index(header, prefix)
	if index < 0 {
		return ""
	}
	value := header[index+len(prefix):]
	if end := strings.IndexByte(value, ';'); end >= 0 {
		value = value[:end]
	}
	return value
}
