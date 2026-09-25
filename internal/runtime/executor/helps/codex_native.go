package helps

import (
	"net/http"
	"strings"

	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

const (
	codexResponsesLiteHeader   = "X-OpenAI-Internal-Codex-Responses-Lite"
	codexResponsesLiteMetadata = "client_metadata.ws_request_header_x_openai_internal_codex_responses_lite"
)

// IsNativeCodexRequest reports whether a native Codex Responses-Lite request
// can be forwarded without compatibility normalization.
func IsNativeCodexRequest(body []byte, opts cliproxyexecutor.Options) bool {
	for _, format := range []sdktranslator.Format{
		opts.SourceFormat,
		cliproxyexecutor.ResponseFormatOrSource(opts),
	} {
		name := strings.TrimSpace(format.String())
		if !strings.EqualFold(name, sdktranslator.FormatCodex.String()) &&
			!strings.EqualFold(name, sdktranslator.FormatOpenAIResponse.String()) {
			return false
		}
	}
	return isCodexResponsesLiteRequest(body, opts.Headers)
}

func isCodexResponsesLiteRequest(body []byte, headers http.Header) bool {
	for key, values := range headers {
		if !strings.EqualFold(key, codexResponsesLiteHeader) {
			continue
		}
		for _, value := range values {
			if strings.EqualFold(strings.TrimSpace(value), "true") {
				return true
			}
		}
	}
	value := gjson.GetBytes(body, codexResponsesLiteMetadata)
	return value.Type == gjson.True || value.Type == gjson.String &&
		strings.EqualFold(strings.TrimSpace(value.String()), "true")
}
