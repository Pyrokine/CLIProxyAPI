package quota

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

const CodexResetCreditProbePurpose = "codex_reset_credit"

type codexResetCreditProbeKey struct{}

// WithCodexResetCreditProbe 使原生采集器只查询本次重置资格所需的数据
func WithCodexResetCreditProbe(ctx context.Context) context.Context {
	return context.WithValue(ctx, codexResetCreditProbeKey{}, true)
}

func IsCodexResetCreditProbe(ctx context.Context) bool {
	value, _ := ctx.Value(codexResetCreditProbeKey{}).(bool)
	return value
}

type codexResetCreditCount struct {
	state string
	value float64
}

type codexResetCreditSummary struct {
	count              codexResetCreditCount
	creditAvailability string
}

// CodexResetCreditAvailability 按本次计数和明确的期限证据判断资格，不从明细条数推算持有次数
func CodexResetCreditAvailability(data []byte, now time.Time) (string, error) {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(data, &payload); err != nil || payload == nil {
		return "", fmt.Errorf("invalid Codex reset credit response")
	}
	for _, field := range []string{"rate_limit_reset_credits_error", "rateLimitResetCreditsError"} {
		if value := bytes.TrimSpace(payload[field]); len(value) > 0 && string(value) != "null" && string(value) != `""` {
			return "", fmt.Errorf("codex reset credit query failed")
		}
	}
	usage := parseCodexResetCreditSummary(codexResetCreditField(payload, "rate_limit_reset_credits", "rateLimitResetCredits"), now)
	details := parseCodexResetCreditSummary(codexResetCreditField(payload, "rate_limit_reset_credits_details", "rateLimitResetCreditsDetails"), now)
	if usage.count.state == "conflict" || details.count.state == "conflict" ||
		(usage.count.state == "known" && details.count.state == "known" && usage.count.value != details.count.value) {
		return "conflict", nil
	}
	if usage.count.state == "invalid" || details.count.state == "invalid" {
		return "invalid", nil
	}
	count := usage.count
	if count.state != "known" {
		count = details.count
	}
	if count.state != "known" {
		return "missing", nil
	}
	if count.value == 0 {
		return "none", nil
	}
	if usage.creditAvailability == "invalid" || details.creditAvailability == "invalid" {
		return "invalid", nil
	}
	if usage.creditAvailability == "conflict" || details.creditAvailability == "conflict" {
		return "conflict", nil
	}
	return "available", nil
}

func codexResetCreditField(record map[string]json.RawMessage, snake, camel string) json.RawMessage {
	value := bytes.TrimSpace(record[snake])
	if len(value) == 0 || string(value) == "null" {
		return record[camel]
	}
	return value
}

func parseCodexResetCreditSummary(data json.RawMessage, now time.Time) codexResetCreditSummary {
	result := codexResetCreditSummary{count: codexResetCreditCount{state: "missing"}}
	data = bytes.TrimSpace(data)
	if len(data) == 0 || string(data) == "null" {
		return result
	}
	if data[0] == '"' {
		var encoded string
		if json.Unmarshal(data, &encoded) != nil {
			result.count.state = "invalid"
			return result
		}
		data = []byte(encoded)
	}
	var record map[string]json.RawMessage
	if json.Unmarshal(data, &record) != nil || record == nil {
		result.count.state = "invalid"
		return result
	}
	if len(record) > 0 {
		expected := false
		for _, field := range []string{"credits", "available_count", "availableCount", "applicable_available_count", "applicableAvailableCount"} {
			if _, ok := record[field]; ok {
				expected = true
				break
			}
		}
		if !expected {
			result.count.state = "invalid"
			return result
		}
	}
	for _, field := range []string{"available_count", "availableCount"} {
		value, exists := record[field]
		if !exists {
			continue
		}
		count, valid := parseCodexResetCreditCount(value)
		if !valid {
			result.count.state = "invalid"
			break
		}
		if result.count.state == "known" && result.count.value != count {
			result.count.state = "conflict"
			break
		}
		result.count = codexResetCreditCount{state: "known", value: count}
	}
	if credits, exists := record["credits"]; exists {
		var records []json.RawMessage
		if bytes.Equal(bytes.TrimSpace(credits), []byte("null")) || json.Unmarshal(credits, &records) != nil {
			result.count.state = "invalid"
			return result
		}
		available, unavailable := false, false
		for _, raw := range records {
			var credit map[string]json.RawMessage
			if json.Unmarshal(raw, &credit) != nil || credit == nil {
				result.count.state = "invalid"
				return result
			}
			resetType, valid := codexResetCreditTextAliases(credit, "reset_type", "resetType")
			if !valid {
				result.count.state = "invalid"
				return result
			}
			if resetType != "codex_rate_limits" {
				continue
			}
			status, valid := codexResetCreditTextAliases(credit, "status", "status")
			if !valid {
				result.count.state = "invalid"
				return result
			}
			if status != "available" {
				unavailable = true
				continue
			}
			_, hasSnakeExpiry := credit["expires_at"]
			_, hasCamelExpiry := credit["expiresAt"]
			if !hasSnakeExpiry && !hasCamelExpiry {
				available = true
				continue
			}
			expiry, valid := codexResetCreditTextAliases(credit, "expires_at", "expiresAt")
			expiresAt, err := time.Parse(time.RFC3339Nano, expiry)
			if !valid || err != nil {
				result.creditAvailability = "invalid"
				continue
			}
			if expiresAt.After(now) {
				available = true
			} else {
				unavailable = true
			}
		}
		if result.creditAvailability == "" && unavailable && !available {
			result.creditAvailability = "conflict"
		}
	}
	return result
}

func parseCodexResetCreditCount(data json.RawMessage) (float64, bool) {
	var value any
	if json.Unmarshal(data, &value) != nil {
		return 0, false
	}
	var count float64
	switch typed := value.(type) {
	case float64:
		count = typed
	case string:
		raw := strings.TrimSpace(typed)
		if raw == "" || strings.IndexFunc(raw, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
			return 0, false
		}
		var err error
		count, err = strconv.ParseFloat(raw, 64)
		if err != nil {
			return 0, false
		}
	default:
		return 0, false
	}
	return count, !math.IsNaN(count) && !math.IsInf(count, 0) && count >= 0 && count <= 9007199254740991 && math.Trunc(count) == count
}

func codexResetCreditTextAliases(record map[string]json.RawMessage, snake, camel string) (string, bool) {
	result := ""
	for _, field := range []string{snake, camel} {
		raw, exists := record[field]
		if !exists {
			continue
		}
		var text string
		if json.Unmarshal(raw, &text) != nil {
			var number float64
			if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &number) != nil || math.IsInf(number, 0) || math.IsNaN(number) {
				return "", false
			}
			text = strconv.FormatFloat(number, 'f', -1, 64)
		}
		text = strings.TrimSpace(text)
		if text == "" || result != "" && text != result {
			return "", false
		}
		result = text
	}
	return result, result != ""
}
