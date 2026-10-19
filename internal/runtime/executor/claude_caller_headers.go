package executor

import (
	"net/http"
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func copyClaudeCallerFingerprintHeaders(dst, src http.Header) {
	if dst == nil || src == nil {
		return
	}
	for name, values := range src {
		lowerName := strings.ToLower(strings.TrimSpace(name))
		if strings.HasPrefix(lowerName, "x-claude-code-") || strings.HasPrefix(lowerName, "x-claude-remote-") {
			continue
		}
		if lowerName != "accept" && lowerName != "accept-encoding" &&
			lowerName != "user-agent" && lowerName != "x-app" &&
			lowerName != "x-client-request-id" &&
			!strings.HasPrefix(lowerName, "anthropic-") &&
			!strings.HasPrefix(lowerName, "x-stainless-") &&
			lowerName != "x-client-app" &&
			lowerName != "x-anthropic-additional-protection" {
			continue
		}
		dst.Del(name)
		for _, value := range values {
			dst.Add(name, value)
		}
	}
}

func stripKimiClaudeAttribution(body []byte) []byte {
	system := gjson.GetBytes(body, "system")
	isAttribution := func(text string) bool {
		return util.IsClaudeCodeAttributionSystemText(text)
	}
	if system.Type == gjson.String {
		if isAttribution(system.String()) {
			body, _ = sjson.DeleteBytes(body, "system")
		}
		return body
	}
	if !system.IsArray() {
		return body
	}
	kept := make([]string, 0, len(system.Array()))
	for _, part := range system.Array() {
		if part.Get("type").String() != "text" || !isAttribution(part.Get("text").String()) {
			kept = append(kept, part.Raw)
		}
	}
	if len(kept) == len(system.Array()) {
		return body
	}
	if len(kept) == 0 {
		body, _ = sjson.DeleteBytes(body, "system")
	} else {
		body, _ = sjson.SetRawBytes(body, "system", rawJSONArray(kept))
	}
	return body
}
