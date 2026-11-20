package auth

import (
	"regexp"
	"strings"

	"github.com/tidwall/gjson"
)

var (
	upstreamSummaryUserinfo = regexp.MustCompile(`(?i)((?:[A-Za-z0-9.+_\-]+:)?//)(?:[^:\s/@]+:[^@\s]+|[^@\s/]+)@`)
	upstreamSummaryQuery    = regexp.MustCompile(
		`(?i)([?&][A-Za-z0-9_.-]*(?:key|token|secret|password|auth|sig|signature)=)[^&\s,\r\n;]+`,
	)
	upstreamSummaryCookie        = regexp.MustCompile(`(?i)\b(?:set-)?cookie\s*:[^\r\n]+`)
	upstreamSummaryAuthorization = regexp.MustCompile(`(?i)\bauthorization\s*[:=]\s*[^\r\n]+`)
	upstreamSummaryNaturalSecret = regexp.MustCompile(
		`(?i)\b([A-Za-z0-9_.-]*(?:api[ _-]?key|access[ _-]?token|client[ _-]?secret|private[ _-]?key|secret[ ` +
			`_-]?key|password|secret|token|credentials?|sessionid))\s*(?:(?:is|was|provided|used)?\s*[:= ]\s*` +
			`|\s+is\s+|\s+was\s+|\s+provided\s+|\s+)(?:"(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*'|(?:[^\r\n;,|]+?(?:` +
			`\s+(?:and|with|for|via)\s+|[,;|]|\r|\n|$)|[^\r\n;,|]+))`,
	)
	upstreamSummaryAssignment = regexp.MustCompile(
		`(?i)(['"]?[A-Za-z0-9_.-]*(?:key|token|secret|password|credential|credentials|bearer|sessionid|auth|` +
			`signature|sig)['"]?\s*[=:]\s*)(?:"(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*'|(?:[^\r\n;,|]+?(?:\s+(?:` +
			`and|with|for|via)\s+|[,;|]|\r|\n|$)|[^\r\n;,|]+))`,
	)
	upstreamSummaryInvalidToken = regexp.MustCompile(
		`(?i)\b(invalid|bad|expired|unknown)\s+(?:api\s+key|access\s+token|refresh\s+token|token|key|secret|` +
			`password|credentials?|bearer)\s*(?:[:= ]\s*)?(?:"(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*'|[^\s,\r\n;]+` +
			`)`,
	)
	upstreamSummaryBareKey     = regexp.MustCompile(`\b(?:sk-[A-Za-z0-9._~+/=-]{6,}|ghp_[A-Za-z0-9._~+/=-]{6,})\b`)
	upstreamSummaryBearer      = regexp.MustCompile(`(?i)\b(?:bearer|basic)\s+[A-Za-z0-9._~+/=-]+`)
	upstreamSummaryQuotedPath  = regexp.MustCompile("\"/[^\"\r\n]+\"|'/[^'\r\n]+'|`/[^`\r\n]+`")
	upstreamSummaryWindowsPath = regexp.MustCompile(
		`(?i)\b[A-Za-z]:\\[^\r\n:,;'"<>]+|\\\\[^\r\n:,;'"<>]+\\[^\r\n:,;'"<>]+`,
	)
	upstreamSummaryPathConnector = regexp.MustCompile(`(?i)\s+(?:to|from|into|onto|for|via|with|and)\s+/`)
	upstreamSummaryUnixPath      = regexp.MustCompile(
		`(^|[\s(\[{<"';,=])(/(?:[^/\s\r\n"',;?#()<>{}\[\]]+(?:\s+[^/\s\r\n"',` +
			`;?#()<>{}\[\]]+)*/)*[^/:\s\r\n"',;?#()<>{}\[\]]+(?::[^/:\s\r\n"',;?#()` +
			`<>{}\[\]]+)?)`,
	)
	upstreamSummaryFilePath = regexp.MustCompile(
		"(^|[\\s\"'`(\\[,;=])(/[^\\s:\\r\\n\"'`,;\\])>]+(?:\\s+[^\\s:\\r\\n\"'`" +
			",;\\])>]+)*\\.(?:json|yaml|yml|key|pem|txt|log|toml|conf|env|crt|cer))",
	)
)

// ExtractUpstreamErrorSummary 从上游 JSON 或错误文本提取经过脱敏的摘要
func ExtractUpstreamErrorSummary(raw string) string {
	raw = strings.TrimSpace(raw)
	jsonPart := raw
	if index := strings.Index(raw, ": {"); index >= 0 && index < 50 {
		jsonPart = strings.TrimSpace(raw[index+2:])
	}
	if gjson.Valid(jsonPart) {
		root := gjson.Parse(jsonPart)
		node := root.Get("error")
		var code, message string
		if node.IsObject() {
			code = strings.TrimSpace(node.Get("code").String())
			if code == "" {
				code = strings.TrimSpace(node.Get("type").String())
			}
			message = strings.TrimSpace(node.Get("message").String())
		} else if node.Type == gjson.String {
			message = strings.TrimSpace(node.String())
		}
		if code == "" && message == "" {
			code = strings.TrimSpace(root.Get("code").String())
			if code == "" {
				code = strings.TrimSpace(root.Get("type").String())
			}
			message = strings.TrimSpace(root.Get("message").String())
		}
		if code != "" && message != "" && !strings.Contains(strings.ToLower(message), strings.ToLower(code)) {
			message = code + ": " + message
		} else if message == "" {
			message = code
		}
		if message != "" {
			return SanitizeUpstreamErrorSummary(message)
		}
	}
	return SanitizeUpstreamErrorSummary(raw)
}

// SanitizeUpstreamErrorSummary 隐藏凭证与本机路径，保留最多 256 个 Unicode 字符
func SanitizeUpstreamErrorSummary(summary string) string {
	summary = strings.TrimSpace(summary)
	summary = upstreamSummaryUserinfo.ReplaceAllString(summary, "${1}[REDACTED_AUTH]@")
	summary = upstreamSummaryQuery.ReplaceAllString(summary, "${1}[REDACTED]")
	summary = upstreamSummaryQuotedPath.ReplaceAllStringFunc(
		summary, func(path string) string {
			return path[:1] + "[REDACTED_PATH]" + path[len(path)-1:]
		},
	)
	summary = upstreamSummaryWindowsPath.ReplaceAllString(summary, "[REDACTED_PATH]")

	var paths strings.Builder
	cursor := 0
	for _, match := range upstreamSummaryPathConnector.FindAllStringIndex(summary, -1) {
		paths.WriteString(sanitizeUpstreamSummaryPaths(summary[cursor:match[0]]))
		paths.WriteString(summary[match[0] : match[1]-1])
		cursor = match[1] - 1
	}
	paths.WriteString(sanitizeUpstreamSummaryPaths(summary[cursor:]))
	summary = upstreamSummaryCookie.ReplaceAllString(paths.String(), "Cookie: [REDACTED]")
	summary = upstreamSummaryAuthorization.ReplaceAllString(summary, "Authorization: [REDACTED]")
	summary = upstreamSummaryBareKey.ReplaceAllString(summary, "sk-[REDACTED]")
	summary = upstreamSummaryBearer.ReplaceAllString(summary, "Bearer [REDACTED]")
	summary = upstreamSummaryInvalidToken.ReplaceAllString(summary, "${1} token [REDACTED]")
	summary = upstreamSummaryNaturalSecret.ReplaceAllString(summary, "${1}: [REDACTED]")
	summary = upstreamSummaryAssignment.ReplaceAllString(summary, "${1}[REDACTED]")
	if runes := []rune(summary); len(runes) > 256 {
		return string(runes[:253]) + "..."
	}
	return summary
}

func sanitizeUpstreamSummaryPaths(summary string) string {
	colon := -1
	lower := strings.ToLower(summary)
	for _, signal := range []string{
		"permission denied", "no such file", "file not found", "access denied",
		"operation not permitted", "denied", "read-only", "is a directory",
		"not a directory", "cannot find", "no space", "connection refused",
		"timeout", "failed", "error", "not supported", "invalid argument",
	} {
		if index := strings.Index(lower, ": "+signal); index >= 0 && (colon < 0 || index < colon) {
			colon = index
		}
	}
	if colon < 0 {
		colon = strings.Index(summary, ": ")
	}
	if colon >= 0 {
		prefix := summary[:colon]
		for index := range prefix {
			if prefix[index] != '/' || (index > 0 && !strings.ContainsRune(" \t([{<\"'`=", rune(prefix[index-1]))) {
				continue
			}
			path := prefix[index:]
			trimmed := strings.TrimRight(path, ")]}>")
			summary = prefix[:index] + strings.Repeat(
				"[REDACTED_PATH] ", strings.Count(trimmed, " /"),
			) + "[REDACTED_PATH]" + path[len(trimmed):] + summary[colon:]
			break
		}
	}
	for {
		masked := upstreamSummaryUnixPath.ReplaceAllString(summary, "${1}[REDACTED_PATH]")
		if masked == summary {
			break
		}
		summary = masked
	}
	return upstreamSummaryFilePath.ReplaceAllString(summary, "${1}[REDACTED_PATH]")
}
