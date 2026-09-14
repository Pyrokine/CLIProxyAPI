package quota

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"
)

// schedulingStatus describes whether cached quota data can influence routing.
type schedulingStatus string

const (
	SchedulingStatusFresh   schedulingStatus = "fresh"
	SchedulingStatusStale   schedulingStatus = "stale"
	SchedulingStatusUnknown schedulingStatus = "unknown"
)

// schedulingSnapshot is the provider-neutral quota signal used by one routing decision.
type schedulingSnapshot struct {
	Status     schedulingStatus
	Headroom   float64
	ResetAt    *time.Time
	EntryKey   string
	ObservedAt *time.Time
}

type schedulingEntrySnapshot struct {
	key        string
	status     Status
	freshness  Freshness
	disabled   bool
	data       json.RawMessage
	observedAt *time.Time
}

type quotaWindow struct {
	headroom float64
	resetAt  *time.Time
}

// SchedulingSnapshots returns immutable signals for the requested auth indexes.
func (s *Scheduler) SchedulingSnapshots(
	credType CredentialType,
	authIndexes []string,
	model string,
	now time.Time,
) map[string]schedulingSnapshot {
	requested := make(map[string]struct{}, len(authIndexes))
	for _, authIndex := range authIndexes {
		authIndex = strings.TrimSpace(authIndex)
		if authIndex != "" {
			requested[authIndex] = struct{}{}
		}
	}
	out := make(map[string]schedulingSnapshot, len(requested))
	if s == nil || len(requested) == 0 {
		return out
	}
	if now.IsZero() {
		now = time.Now()
	}

	entries := make(map[string][]schedulingEntrySnapshot, len(requested))
	s.mu.RLock()
	for key, entry := range s.entries {
		if entry == nil {
			continue
		}
		entry.mu.RLock()
		authIndex := strings.TrimSpace(entry.AuthIndex)
		_, wanted := requested[authIndex]
		if entry.Type != credType || !wanted {
			entry.mu.RUnlock()
			continue
		}
		snapshot := schedulingEntrySnapshot{
			key:       key,
			status:    entry.Status,
			freshness: entry.Freshness,
			disabled:  entry.Disabled,
			data:      append(json.RawMessage(nil), entry.Data...),
		}
		if entry.ObservedAt != nil {
			observedAt := *entry.ObservedAt
			snapshot.observedAt = &observedAt
		}
		entry.mu.RUnlock()
		entries[authIndex] = append(entries[authIndex], snapshot)
	}
	s.mu.RUnlock()

	for authIndex := range requested {
		candidates := entries[authIndex]
		if len(candidates) == 0 {
			out[authIndex] = schedulingSnapshot{Status: SchedulingStatusUnknown}
			continue
		}
		result := schedulingSnapshot{Status: SchedulingStatusStale}
		var selected *quotaWindow
		for _, candidate := range candidates {
			if candidate.status != StatusSuccess || candidate.freshness != FreshnessFresh ||
				candidate.disabled || len(candidate.data) == 0 {
				continue
			}
			window, errParse := parseSchedulingWindow(credType, candidate.data, model, now)
			if errParse != nil {
				continue
			}
			if selected == nil || window.headroom < selected.headroom ||
				(window.headroom == selected.headroom && resetBefore(window.resetAt, selected.resetAt)) {
				selected = &window
				result.EntryKey = candidate.key
				result.ObservedAt = cloneTime(candidate.observedAt)
			}
		}
		if selected != nil {
			result.Status = SchedulingStatusFresh
			result.Headroom = selected.headroom
			result.ResetAt = cloneTime(selected.resetAt)
		}
		out[authIndex] = result
	}
	return out
}

func parseSchedulingWindow(
	credType CredentialType,
	data []byte,
	model string,
	now time.Time,
) (quotaWindow, error) {
	value, errDecode := decodeQuotaJSON(data)
	if errDecode != nil {
		return quotaWindow{}, errDecode
	}
	switch credType {
	case TypeClaude:
		return parseClaudeSchedulingWindow(value, model)
	case TypeCodex:
		return parseCodexSchedulingWindow(value, model, now)
	case TypeGeminiCli, TypeAntigravity:
		return parseGeminiSchedulingWindow(value, model)
	case TypeKimi:
		return parseKimiSchedulingWindow(value, now)
	case TypeXAI:
		return parseXAISchedulingWindow(value, model)
	default:
		return quotaWindow{}, fmt.Errorf("quota scheduling is not supported for type %s", credType)
	}
}

func decodeQuotaJSON(data []byte) (any, error) {
	value, errDecode := decodeSingleQuotaJSON(bytes.NewReader(data))
	if errDecode != nil {
		return nil, errDecode
	}
	for range 2 {
		encoded, ok := value.(string)
		if !ok {
			break
		}
		value, errDecode = decodeSingleQuotaJSON(strings.NewReader(encoded))
		if errDecode != nil {
			return nil, errDecode
		}
	}
	return value, nil
}

func decodeSingleQuotaJSON(reader io.Reader) (any, error) {
	decoder := json.NewDecoder(reader)
	decoder.UseNumber()
	var value any
	if errDecode := decoder.Decode(&value); errDecode != nil {
		return nil, errDecode
	}
	var trailing any
	if errTrailing := decoder.Decode(&trailing); !errors.Is(errTrailing, io.EOF) {
		if errTrailing == nil {
			return nil, errors.New("quota payload contains multiple JSON values")
		}
		return nil, errTrailing
	}
	return value, nil
}

func parseClaudeSchedulingWindow(value any, model string) (quotaWindow, error) {
	root := quotaMap(value)
	usage := quotaMap(quotaNestedValue(root["usage"]))
	if usage == nil {
		return quotaWindow{}, errors.New("claude quota usage is missing")
	}
	windows := make([]quotaWindow, 0, 4)
	appendClaudeWindow := func(key string) {
		window := quotaMap(usage[key])
		used, okUsed := quotaNumber(window["utilization"])
		if !okUsed {
			return
		}
		headroom, okHeadroom := headroomFromPercent(used)
		if !okHeadroom {
			return
		}
		windows = append(
			windows, quotaWindow{
				headroom: headroom,
				resetAt:  quotaAbsoluteTime(window["resets_at"]),
			},
		)
	}
	appendClaudeWindow("five_hour")
	appendClaudeWindow("seven_day")
	modelKey := strings.ToLower(strings.TrimSpace(model))
	switch {
	case strings.Contains(modelKey, "opus"):
		appendClaudeWindow("seven_day_opus")
	case strings.Contains(modelKey, "sonnet"):
		appendClaudeWindow("seven_day_sonnet")
	case strings.Contains(modelKey, "fable"):
		appendClaudeWindow("iguana_necktie")
		for _, rawLimit := range quotaSlice(usage["limits"]) {
			limit := quotaMap(rawLimit)
			scope := quotaMap(limit["scope"])
			scopeModel := quotaMap(scope["model"])
			displayName := strings.ToLower(quotaString(firstQuotaValue(scopeModel, "display_name", "displayName")))
			if displayName != "fable" && displayName != "fable 5" {
				continue
			}
			used, okUsed := quotaNumber(limit["percent"])
			if !okUsed {
				continue
			}
			if headroom, ok := headroomFromPercent(used); ok {
				windows = append(
					windows, quotaWindow{
						headroom: headroom,
						resetAt:  quotaAbsoluteTime(limit["resets_at"]),
					},
				)
			}
		}
	}
	return strictestQuotaWindow(windows)
}

func parseCodexSchedulingWindow(value any, model string, now time.Time) (quotaWindow, error) {
	root := quotaMap(value)
	windows := make([]quotaWindow, 0, 6)
	appendCodexRateLimitWindows(&windows, quotaMap(firstQuotaValue(root, "rate_limit", "rateLimit")), now)
	additional := quotaSlice(firstQuotaValue(root, "additional_rate_limits", "additionalRateLimits"))
	for _, raw := range additional {
		entry := quotaMap(raw)
		name := quotaString(firstQuotaValue(entry, "limit_name", "limitName", "metered_feature", "meteredFeature"))
		if name == "" || !ModelScopeMatches(name, model) {
			continue
		}
		appendCodexRateLimitWindows(&windows, quotaMap(firstQuotaValue(entry, "rate_limit", "rateLimit")), now)
	}
	if len(windows) == 0 {
		usage := quotaMap(root["completions_usage"])
		used, okUsed := quotaNumber(firstQuotaValue(usage, "premium_completions_used", "completions_used"))
		limit, okLimit := quotaNumber(firstQuotaValue(usage, "premium_completions_limit", "completions_limit"))
		if okUsed && okLimit {
			if headroom, ok := headroomFromAmounts(limit-used, limit); ok {
				windows = append(
					windows, quotaWindow{headroom: headroom, resetAt: quotaAbsoluteTime(usage["reset_date"])},
				)
			}
		}
	}
	return strictestQuotaWindow(windows)
}

func appendCodexRateLimitWindows(dst *[]quotaWindow, limit map[string]any, now time.Time) {
	if limit == nil {
		return
	}
	allowed, allowedKnown := quotaBool(limit["allowed"])
	limitReached, limitReachedKnown := quotaBool(firstQuotaValue(limit, "limit_reached", "limitReached"))
	if allowedKnown && !allowed || limitReachedKnown && limitReached {
		*dst = append(*dst, quotaWindow{headroom: 0})
		return
	}
	for _, names := range [][2]string{{"primary_window", "primaryWindow"}, {"secondary_window", "secondaryWindow"}} {
		window := quotaMap(firstQuotaValue(limit, names[0], names[1]))
		if window == nil {
			continue
		}
		windowAllowed, windowAllowedKnown := quotaBool(window["allowed"])
		if !windowAllowedKnown {
			windowAllowed, windowAllowedKnown = allowed, allowedKnown
		}
		windowReached, windowReachedKnown := quotaBool(firstQuotaValue(window, "limit_reached", "limitReached"))
		if !windowReachedKnown {
			windowReached, windowReachedKnown = limitReached, limitReachedKnown
		}
		headroom := 0.0
		known := false
		if windowAllowedKnown && !windowAllowed || windowReachedKnown && windowReached {
			known = true
		} else if used, okUsed := quotaNumber(firstQuotaValue(window, "used_percent", "usedPercent")); okUsed {
			headroom, known = headroomFromPercent(used)
		}
		if !known {
			continue
		}
		resetAt := quotaAbsoluteTime(firstQuotaValue(window, "reset_at", "resetAt"))
		if resetAt == nil {
			if seconds, okSeconds := quotaNumber(
				firstQuotaValue(
					window, "reset_after_seconds", "resetAfterSeconds",
				),
			); okSeconds && seconds >= 0 {
				reset := now.Add(time.Duration(seconds * float64(time.Second)))
				resetAt = &reset
			}
		}
		*dst = append(*dst, quotaWindow{headroom: headroom, resetAt: resetAt})
	}
}

func parseGeminiSchedulingWindow(value any, model string) (quotaWindow, error) {
	root := quotaMap(value)
	quotaValue := quotaNestedValue(root["quota"])
	quota := quotaMap(quotaValue)
	if quota == nil {
		return quotaWindow{}, errors.New("gemini quota is missing")
	}
	windows := make([]quotaWindow, 0)
	buckets := quotaSlice(quota["buckets"])
	for _, raw := range buckets {
		bucket := quotaMap(raw)
		remaining, okRemaining := quotaNumber(firstQuotaValue(bucket, "remainingFraction", "remaining_fraction"))
		if !okRemaining || remaining < 0 || remaining > 1 {
			continue
		}
		modelID := quotaString(firstQuotaValue(bucket, "modelId", "model_id"))
		if modelID != "" && !ModelScopeMatches(modelID, model) {
			continue
		}
		windows = append(
			windows, quotaWindow{
				headroom: remaining,
				resetAt:  quotaAbsoluteTime(firstQuotaValue(bucket, "resetTime", "reset_time")),
			},
		)
	}
	if len(windows) == 0 && len(buckets) == 0 {
		for _, raw := range quotaSlice(firstQuotaValue(quota, "userCredits", "user_credits")) {
			credit := quotaMap(raw)
			remaining, okRemaining := quotaNumber(firstQuotaValue(credit, "remainingValue", "remaining_value"))
			total, okTotal := quotaNumber(firstQuotaValue(credit, "totalValue", "total_value"))
			if !okRemaining || !okTotal {
				continue
			}
			headroom, okHeadroom := headroomFromAmounts(remaining, total)
			if !okHeadroom {
				continue
			}
			windows = append(
				windows, quotaWindow{
					headroom: headroom,
					resetAt:  quotaAbsoluteTime(firstQuotaValue(credit, "resetTime", "reset_time")),
				},
			)
		}
	}
	return strictestQuotaWindow(windows)
}

func parseKimiSchedulingWindow(value any, now time.Time) (quotaWindow, error) {
	root := quotaMap(value)
	windows := make([]quotaWindow, 0)
	limits := quotaSlice(root["limits"])
	for _, raw := range limits {
		item := quotaMap(raw)
		detail := quotaMap(item["detail"])
		if detail == nil {
			detail = item
		}
		if window, ok := kimiQuotaWindow(detail, now); ok {
			windows = append(windows, window)
		}
	}
	if usage := quotaMap(root["usage"]); usage != nil {
		if window, ok := kimiQuotaWindow(usage, now); ok {
			windows = append(windows, window)
		}
	}
	if len(windows) == 0 {
		legacy := quotaSlice(value)
		if len(legacy) == 0 {
			legacy = quotaSlice(root["usages"])
		}
		for _, raw := range legacy {
			if window, ok := kimiQuotaWindow(quotaMap(raw), now); ok {
				windows = append(windows, window)
			}
		}
	}
	return strictestQuotaWindow(windows)
}

func kimiQuotaWindow(value map[string]any, now time.Time) (quotaWindow, bool) {
	if value == nil {
		return quotaWindow{}, false
	}
	limit, okLimit := quotaNumber(value["limit"])
	used, okUsed := quotaNumber(value["used"])
	if !okUsed && okLimit {
		if remaining, okRemaining := quotaNumber(value["remaining"]); okRemaining {
			used = limit - remaining
			okUsed = true
		}
	}
	if !okLimit || !okUsed {
		return quotaWindow{}, false
	}
	headroom, okHeadroom := headroomFromAmounts(limit-used, limit)
	if !okHeadroom {
		return quotaWindow{}, false
	}
	resetAt := quotaAbsoluteTime(firstQuotaValue(value, "reset_at", "resetAt", "reset_time", "resetTime"))
	if resetAt == nil {
		if seconds, okSeconds := quotaNumber(
			firstQuotaValue(
				value, "reset_in", "resetIn", "ttl",
			),
		); okSeconds && seconds >= 0 {
			reset := now.Add(time.Duration(seconds * float64(time.Second)))
			resetAt = &reset
		}
	}
	return quotaWindow{headroom: headroom, resetAt: resetAt}, true
}

func parseXAISchedulingWindow(value any, model string) (quotaWindow, error) {
	root := quotaMap(value)
	normalized := quotaMap(root["normalized"])
	if normalized == nil {
		return quotaWindow{}, errors.New("xai normalized quota is missing")
	}
	windows := make([]quotaWindow, 0, 2)
	if used, okUsed := quotaNumber(firstQuotaValue(normalized, "usage_percent", "usagePercent")); okUsed {
		if headroom, ok := headroomFromPercent(used); ok {
			windows = append(
				windows, quotaWindow{
					headroom: headroom,
					resetAt:  xaiQuotaResetAt(normalized),
				},
			)
		}
	} else {
		remaining, okRemaining := quotaNumber(normalized["remaining"])
		limit, okLimit := quotaNumber(firstQuotaValue(normalized, "monthly_limit", "monthlyLimit"))
		if okRemaining && okLimit {
			if headroom, ok := headroomFromAmounts(remaining, limit); ok {
				windows = append(windows, quotaWindow{headroom: headroom, resetAt: xaiQuotaResetAt(normalized)})
			}
		}
	}
	for _, raw := range quotaSlice(firstQuotaValue(normalized, "product_usage", "productUsage")) {
		product := quotaMap(raw)
		if !ModelScopeMatches(quotaString(product["product"]), model) {
			continue
		}
		used, okUsed := quotaNumber(firstQuotaValue(product, "usage_percent", "usagePercent"))
		if !okUsed {
			continue
		}
		if headroom, ok := headroomFromPercent(used); ok {
			windows = append(windows, quotaWindow{headroom: headroom, resetAt: xaiQuotaResetAt(normalized)})
		}
	}
	return strictestQuotaWindow(windows)
}

func xaiQuotaResetAt(normalized map[string]any) *time.Time {
	usagePeriod := quotaMap(firstQuotaValue(normalized, "usage_period", "usagePeriod"))
	if reset := quotaAbsoluteTime(usagePeriod["end"]); reset != nil {
		return reset
	}
	billingPeriod := quotaMap(firstQuotaValue(normalized, "billing_period", "billingPeriod"))
	return quotaAbsoluteTime(billingPeriod["end"])
}

func strictestQuotaWindow(windows []quotaWindow) (quotaWindow, error) {
	if len(windows) == 0 {
		return quotaWindow{}, errors.New("quota headroom is unavailable")
	}
	selected := windows[0]
	for _, window := range windows[1:] {
		if window.headroom < selected.headroom ||
			(window.headroom == selected.headroom && resetBefore(window.resetAt, selected.resetAt)) {
			selected = window
		}
	}
	return selected, nil
}

func resetBefore(left, right *time.Time) bool {
	if left == nil {
		return false
	}
	return right == nil || left.Before(*right)
}

func headroomFromPercent(used float64) (float64, bool) {
	if math.IsNaN(used) || math.IsInf(used, 0) || used < 0 {
		return 0, false
	}
	return clampUnit(1 - used/100), true
}

func headroomFromAmounts(remaining, total float64) (float64, bool) {
	if math.IsNaN(remaining) || math.IsNaN(total) || math.IsInf(remaining, 0) || math.IsInf(total, 0) || total <= 0 {
		return 0, false
	}
	return clampUnit(remaining / total), true
}

func clampUnit(value float64) float64 {
	if value < 0 {
		return 0
	}
	if value > 1 {
		return 1
	}
	return value
}

func quotaNestedValue(value any) any {
	for range 2 {
		text, ok := value.(string)
		if !ok || strings.TrimSpace(text) == "" {
			return value
		}
		decoded, errDecode := decodeSingleQuotaJSON(strings.NewReader(text))
		if errDecode != nil {
			return value
		}
		value = decoded
	}
	return value
}

func quotaMap(value any) map[string]any {
	value = quotaNestedValue(value)
	result, _ := value.(map[string]any)
	return result
}

func quotaSlice(value any) []any {
	value = quotaNestedValue(value)
	result, _ := value.([]any)
	return result
}

func firstQuotaValue(values map[string]any, keys ...string) any {
	for _, key := range keys {
		if value, ok := values[key]; ok {
			return value
		}
	}
	return nil
}

func quotaNumber(value any) (float64, bool) {
	switch typed := value.(type) {
	case json.Number:
		parsed, err := typed.Float64()
		return parsed, err == nil && !math.IsNaN(parsed) && !math.IsInf(parsed, 0)
	case float64:
		return typed, !math.IsNaN(typed) && !math.IsInf(typed, 0)
	case float32:
		parsed := float64(typed)
		return parsed, !math.IsNaN(parsed) && !math.IsInf(parsed, 0)
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case string:
		normalized := strings.NewReplacer(",", "", "%", "", "$", "", " ", "").Replace(strings.TrimSpace(typed))
		if normalized == "" {
			return 0, false
		}
		parsed, err := strconv.ParseFloat(normalized, 64)
		return parsed, err == nil && !math.IsNaN(parsed) && !math.IsInf(parsed, 0)
	default:
		if record := quotaMap(value); record != nil {
			return quotaNumber(record["val"])
		}
		return 0, false
	}
}

func quotaBool(value any) (bool, bool) {
	switch typed := value.(type) {
	case bool:
		return typed, true
	case string:
		parsed, err := strconv.ParseBool(strings.TrimSpace(typed))
		return parsed, err == nil
	default:
		return false, false
	}
}

func quotaString(value any) string {
	text, _ := value.(string)
	return strings.TrimSpace(text)
}

func quotaAbsoluteTime(value any) *time.Time {
	if text := quotaString(value); text != "" {
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339, httpTimeLayout} {
			if parsed, err := time.Parse(layout, text); err == nil {
				parsed = parsed.UTC()
				return &parsed
			}
		}
	}
	if numeric, ok := quotaNumber(value); ok && numeric > 0 {
		seconds := numeric
		if seconds > 1e12 {
			seconds /= 1000
		}
		parsed := time.Unix(int64(seconds), int64((seconds-math.Floor(seconds))*float64(time.Second))).UTC()
		return &parsed
	}
	return nil
}

const httpTimeLayout = "Mon, 02 Jan 2006 15:04:05 GMT"

// ModelScopeMatches compares provider quota scope names with canonical model identifiers.
func ModelScopeMatches(scope, model string) bool {
	normalize := func(value string) string {
		value = strings.ToLower(strings.TrimSpace(value))
		value = strings.TrimPrefix(value, "models/")
		return value
	}
	return normalize(scope) != "" && normalize(scope) == normalize(model)
}
