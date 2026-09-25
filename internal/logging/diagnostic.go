package logging

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
)

const diagnosticLogRuneLimit = 300

var (
	diagnosticExpiredPattern       = regexp.MustCompile(`(?i)access token expired`)
	diagnosticStatusPattern        = regexp.MustCompile(`(?i)\bstatus(?:\s+code)?\s*[:=]?\s*([1-5][0-9]{2})\b`)
	diagnosticURLPattern           = regexp.MustCompile(`(?i)[a-z][a-z0-9+.-]*://[^\s"'<>]+`)
	diagnosticAssignmentPattern    = regexp.MustCompile(`(?i)["']?([a-z][a-z0-9_.%\[\]-]*(?:\s+(?:token|key|secret))?)["']?\s*[:=]\s*`)
	diagnosticValuePattern         = regexp.MustCompile(`^(?:"(?:\\.|[^"\\])*(?:"|\\?$)|'(?:\\.|[^'\\])*(?:'|\\?$)|(?i:bearer|basic)\s+[^\s,;&}\]]+|[^\s,;&}\]]+)`)
	diagnosticAuthorizationPattern = regexp.MustCompile(`(?i)\b(bearer|basic)\s+[^\s,;"'<>]+`)
)

// SafeDiagnosticForLog 返回有界、单行的诊断摘要，隐藏凭证值并保留 token 过期信号
func SafeDiagnosticForLog(message string) string {
	excerpt, truncated := diagnosticPrefix(message, 2*diagnosticLogRuneLimit)
	excerpt = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.IsSpace(r) || unicode.Is(unicode.Cf, r) {
			return ' '
		}
		return r
	}, excerpt)
	excerpt = strings.Join(strings.Fields(excerpt), " ")
	excerpt = diagnosticURLPattern.ReplaceAllStringFunc(excerpt, func(raw string) string {
		// 被截断的 URL 无法可靠区分主机名与未结束的 userinfo
		if truncated && strings.HasSuffix(excerpt, raw) {
			return "[REDACTED URL]"
		}
		parsed, err := url.Parse(raw)
		if err != nil {
			return "[REDACTED URL]"
		}
		if parsed.User != nil {
			parsed.User = url.User("[REDACTED]")
		}
		if parsed.RawQuery != "" {
			parsed.RawQuery = "[REDACTED]"
		}
		if parsed.Fragment != "" {
			parsed.Fragment = "[REDACTED]"
		}
		return parsed.String()
	})

	var masked strings.Builder
	cursor := 0
	for _, match := range diagnosticAssignmentPattern.FindAllStringSubmatchIndex(excerpt, -1) {
		if match[0] < cursor {
			continue
		}
		key := excerpt[match[2]:match[3]]
		if decoded, err := url.QueryUnescape(key); err == nil {
			key = decoded
		}
		compact := strings.ToLower(strings.NewReplacer("-", "", "_", "", " ", "").Replace(key))
		if !util.IsSensitiveFieldName(key) && compact != "privatekey" && compact != "proxyauthorization" {
			continue
		}
		value := diagnosticValuePattern.FindStringIndex(excerpt[match[1]:])
		if value == nil {
			continue
		}
		masked.WriteString(excerpt[cursor:match[1]])
		masked.WriteString(`"[REDACTED]"`)
		cursor = match[1] + value[1]
	}
	masked.WriteString(excerpt[cursor:])
	excerpt = diagnosticAuthorizationPattern.ReplaceAllString(masked.String(), "${1} [REDACTED]")
	output, outputTruncated := diagnosticPrefix(excerpt, diagnosticLogRuneLimit)
	if !diagnosticExpiredPattern.MatchString(output) && diagnosticExpiredPattern.MatchString(message) {
		const suffix = " ... access token expired"
		output, _ = diagnosticPrefix(output, diagnosticLogRuneLimit-len(suffix))
		output += suffix
	}
	if truncated || outputTruncated {
		output += "..."
	}
	return output
}

// SafeErrorDiagnostic 只提取固定错误信号，不返回原始错误文本或 URL 参数
func SafeErrorDiagnostic(err error) string {
	if err == nil {
		return ""
	}
	var parts []string
	add := func(value string) {
		if !slices.Contains(parts, value) {
			parts = append(parts, value)
		}
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		add("unexpected_EOF")
	} else if errors.Is(err, io.EOF) {
		add("EOF")
	}
	if errors.Is(err, context.Canceled) {
		add("canceled")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		add("timeout")
	}
	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() {
		add("timeout")
	}
	cause := err
	var urlError *url.Error
	if errors.As(err, &urlError) && urlError != nil {
		cause = urlError.Err
	}
	var raw string
	if cause != nil {
		raw = strings.ToLower(cause.Error())
	}
	if strings.TrimSpace(raw) == "eof" {
		add("EOF")
	}
	if strings.Contains(raw, "socks") && (strings.Contains(raw, "authentication failed") || strings.Contains(raw, "authentication required")) {
		add("proxy_authentication_failed")
	}
	for _, signal := range []struct {
		label   string
		needles []string
	}{
		{"proxy=socks", []string{"socks"}},
		{"proxy_connect_failed", []string{"proxyconnect", "proxy connect"}},
		{"dial_failed", []string{"dial ", "dial failed"}},
		{"connection_refused", []string{"connection refused"}},
		{"connection_reset", []string{"connection reset"}},
		{"connection_aborted", []string{"connection aborted"}},
		{"stream_reset", []string{"stream reset"}},
		{"network_unreachable", []string{"network is unreachable", "no route to host"}},
		{"dns_not_found", []string{"no such host"}},
		{"dns_failure", []string{"server misbehaving"}},
		{"tls_handshake_timeout", []string{"tls handshake timeout"}},
		{"timeout", []string{"i/o timeout", "deadline exceeded"}},
		{"unexpected_EOF", []string{"unexpected eof"}},
		{"tls_certificate_error", []string{"certificate"}},
		{"invalid_response_json", []string{"invalid character", "cannot unmarshal"}},
		{"oauth_error=invalid_grant", []string{"invalid_grant"}},
		{"oauth_error=refresh_token_expired", []string{"refresh_token_expired"}},
		{"oauth_error=refresh_token_revoked", []string{"refresh_token_revoked"}},
		{"oauth_error=refresh_token_reused", []string{"refresh_token_reused"}},
	} {
		for _, needle := range signal.needles {
			if strings.Contains(raw, needle) {
				add(signal.label)
				break
			}
		}
	}
	if match := diagnosticStatusPattern.FindStringSubmatch(raw); len(match) == 2 {
		add("status=" + match[1])
	}
	if len(parts) == 0 {
		return fmt.Sprintf("error_type=%T", err)
	}
	return strings.Join(parts, " ")
}

func diagnosticPrefix(value string, limit int) (string, bool) {
	count := 0
	for index := range value {
		if count == limit {
			return value[:index], true
		}
		count++
	}
	return value, false
}
