package helps

import (
	"bytes"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"

	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

const maxCodexAdditionalRateLimits = 8

// parseCodexQuotaEventHeaders 将 Codex 配额帧转换为有界的 HTTP 配额观察
func parseCodexQuotaEventHeaders(payload []byte) http.Header {
	if !bytes.Contains(payload, []byte(`"codex.rate_limits"`)) &&
		!bytes.Contains(payload, []byte(`"rate_limits.updated"`)) && !bytes.Contains(payload, []byte(`"error"`)) {
		return nil
	}
	eventType := gjson.GetBytes(payload, "type").String()
	if eventType != "codex.rate_limits" && eventType != "rate_limits.updated" && eventType != "error" {
		return nil
	}
	if !gjson.ValidBytes(payload) {
		return nil
	}
	root := gjson.ParseBytes(payload)
	headers := make(http.Header)
	if eventType == "error" {
		if node := root.Get("headers"); node.IsObject() {
			node.ForEach(
				func(key, value gjson.Result) bool {
					if raw := codexQuotaScalarValue(value); raw != "" {
						headers.Set(key.String(), raw)
					}
					return true
				},
			)
		}
	} else {
		addCodexQuotaRateLimitHeaders(headers, "X-Codex-", firstCodexQuotaResult(root, "rate_limits", "rateLimit"))
		addCodexQuotaRateLimitHeaders(
			headers, "X-Codex-Code-Review-",
			firstCodexQuotaResult(root, "code_review_rate_limits", "codeReviewRateLimits"),
		)
		additional := firstCodexQuotaResult(root, "additional_rate_limits", "additionalRateLimits")
		count := 0
		if additional.IsObject() || additional.IsArray() {
			additional.ForEach(
				func(key, value gjson.Result) bool {
					if count == maxCodexAdditionalRateLimits {
						return false
					}
					name := strings.TrimSpace(key.String())
					if additional.IsArray() {
						name = codexQuotaScalarValue(firstCodexQuotaResult(value, "limit_name", "limitName", "name"))
					}
					identifier := normalizeCodexQuotaHeaderIdentifier(name)
					if identifier == "" {
						return true
					}
					rateInfo := firstCodexQuotaResult(value, "rate_limit", "rateLimit")
					if !rateInfo.Exists() {
						rateInfo = value
					}
					prefix := "X-Codex-Additional-" + identifier + "-"
					if addCodexQuotaRateLimitHeaders(headers, prefix, rateInfo) {
						if validCodexQuotaEventText(name) {
							headers.Set(prefix+"Limit-Name", name)
						}
						count++
					}
					return true
				},
			)
		}
		credits := root.Get("credits")
		if credits.IsObject() {
			setCodexQuotaScalarHeader(headers, "X-Codex-Credits-Has-Credits", credits, "has_credits", "hasCredits")
			setCodexQuotaScalarHeader(headers, "X-Codex-Credits-Unlimited", credits, "unlimited")
			setCodexQuotaScalarHeader(headers, "X-Codex-Credits-Balance", credits, "balance")
		}
		if len(headers) == 0 {
			return nil
		}
		active := codexQuotaScalarValue(
			firstCodexQuotaResult(
				root, "metered_limit_name", "meteredLimitName", "limit_name", "limitName",
			),
		)
		if validCodexQuotaEventIdentifier(active) {
			headers.Set("X-Codex-Active-Limit", active)
		}
		plan := codexQuotaScalarValue(firstCodexQuotaResult(root, "plan_type", "planType"))
		if validCodexQuotaEventText(plan) {
			headers.Set("X-Codex-Plan-Type", plan)
		}
	}
	// 复用 Manager 的 allowlist、64 条上限和 512 字节值上限
	var observation cliproxyauth.QuotaState
	if !observation.ObserveResponseHeadersForProvider("codex", headers, time.Time{}) {
		return nil
	}
	for name := range headers {
		if _, retained := observation.Signals[name]; !retained {
			delete(headers, name)
		}
	}
	return headers
}

func addCodexQuotaRateLimitHeaders(headers http.Header, prefix string, rateInfo gjson.Result) bool {
	if !rateInfo.IsObject() {
		return false
	}
	changed := setCodexQuotaScalarHeader(headers, prefix+"Allowed", rateInfo, "allowed")
	changed = setCodexQuotaScalarHeader(
		headers, prefix+"Limit-Reached", rateInfo, "limit_reached", "limitReached",
	) || changed
	for _, name := range []string{"primary", "secondary"} {
		window := rateInfo.Get(name)
		used := firstCodexQuotaResult(window, "used_percent", "usedPercent")
		minutes := firstCodexQuotaResult(window, "window_minutes", "windowMinutes")
		after := firstCodexQuotaResult(window, "reset_after_seconds", "resetAfterSeconds")
		at := firstCodexQuotaResult(window, "reset_at", "resetAt")
		validAfter := validCodexQuotaNumber(after, 0, math.MaxFloat64)
		validAt := validCodexQuotaNumber(at, 1, math.MaxFloat64)
		if !validCodexQuotaNumber(used, 0, 100) || !validCodexQuotaNumber(
			minutes, 1, math.MaxFloat64,
		) || (!validAfter && !validAt) {
			continue
		}
		windowPrefix := prefix + strings.ToUpper(name[:1]) + name[1:] + "-"
		headers.Set(windowPrefix+"Used-Percent", codexQuotaScalarValue(used))
		headers.Set(windowPrefix+"Window-Minutes", codexQuotaScalarValue(minutes))
		if validAfter {
			headers.Set(windowPrefix+"Reset-After-Seconds", codexQuotaScalarValue(after))
		}
		if validAt {
			headers.Set(windowPrefix+"Reset-At", codexQuotaScalarValue(at))
		}
		changed = true
	}
	return changed
}

func validCodexQuotaNumber(value gjson.Result, minimum, maximum float64) bool {
	if value.Type != gjson.String && value.Type != gjson.Number {
		return false
	}
	number, err := strconv.ParseFloat(codexQuotaScalarValue(value), 64)
	return err == nil && !math.IsNaN(number) && number >= minimum && number <= maximum
}

func setCodexQuotaScalarHeader(headers http.Header, name string, object gjson.Result, paths ...string) bool {
	if raw := codexQuotaScalarValue(firstCodexQuotaResult(object, paths...)); raw != "" {
		headers.Set(name, raw)
		return true
	}
	return false
}

func codexQuotaScalarValue(value gjson.Result) string {
	switch value.Type {
	case gjson.String:
		return strings.TrimSpace(value.String())
	case gjson.Number, gjson.True, gjson.False:
		return value.Raw
	default:
		return ""
	}
}

func firstCodexQuotaResult(object gjson.Result, paths ...string) gjson.Result {
	for _, path := range paths {
		if value := object.Get(path); value.Exists() && value.Type != gjson.Null &&
			(value.Type != gjson.String || strings.TrimSpace(value.Str) != "") {
			return value
		}
	}
	return gjson.Result{}
}

func normalizeCodexQuotaHeaderIdentifier(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 128 {
		return ""
	}
	var identifier strings.Builder
	lastDash := false
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || char == '_' || char == '-' || char == '.' {
			identifier.WriteRune(char)
			lastDash = false
		} else if !lastDash {
			identifier.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(identifier.String(), "-_.")
}

func validCodexQuotaEventIdentifier(value string) bool {
	if !validCodexQuotaEventText(value) {
		return false
	}
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || char == '_' || char == '-' || char == '.' {
			continue
		}
		return false
	}
	return true
}

func validCodexQuotaEventText(value string) bool {
	return value != "" && len(value) <= 256 && !strings.ContainsFunc(value, unicode.IsControl)
}
