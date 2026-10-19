package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

const xaiBillingSchemaVersion = 1

type xaiQuotaPayload struct {
	SchemaVersion   int                      `json:"schema_version"`
	Source          string                   `json:"source"`
	BillingFormat   string                   `json:"billing_format"`
	ValueSource     map[string]string        `json:"value_source"`
	Billing         json.RawMessage          `json:"billing"`
	Normalized      xaiNormalizedBilling     `json:"normalized"`
	AuxiliaryErrors []xaiQuotaAuxiliaryError `json:"auxiliary_errors,omitempty"`
}

type xaiQuotaAuxiliaryError struct {
	Source string `json:"source"`
	Code   string `json:"code"`
	Status int    `json:"status,omitempty"`
}

type xaiNormalizedBilling struct {
	PlanCode        *string             `json:"plan_code"`
	PlanName        *string             `json:"plan_name"`
	PlanTier        *string             `json:"plan_tier"`
	MonthlyLimit    *float64            `json:"monthly_limit"`
	Used            *float64            `json:"used"`
	IncludedUsed    *float64            `json:"included_used"`
	Remaining       *float64            `json:"remaining"`
	OnDemandEnabled *bool               `json:"on_demand_enabled"`
	OnDemandCap     *float64            `json:"on_demand_cap"`
	OnDemandUsed    *float64            `json:"on_demand_used"`
	PrepaidBalance  *float64            `json:"prepaid_balance"`
	UsagePercent    *float64            `json:"usage_percent"`
	UsagePeriod     xaiNormalizedPeriod `json:"usage_period"`
	BillingPeriod   xaiNormalizedPeriod `json:"billing_period"`
	Weekly          xaiNormalizedWindow `json:"weekly"`
	Monthly         xaiNormalizedWindow `json:"monthly"`
	ProductUsage    []xaiProductUsage   `json:"product_usage"`
	AmountUnit      string              `json:"amount_unit"`
	Currency        *string             `json:"currency"`
	SyncedAt        string              `json:"synced_at"`
}

type xaiNormalizedPeriod struct {
	Type    string  `json:"type"`
	RawType *string `json:"raw_type"`
	Start   *string `json:"start"`
	End     *string `json:"end"`
}

type xaiNormalizedWindow struct {
	UsagePercent *float64 `json:"usage_percent"`
	PeriodType   string   `json:"period_type"`
	PeriodStart  *string  `json:"period_start"`
	PeriodEnd    *string  `json:"period_end"`
}

type xaiProductUsage struct {
	Product      string   `json:"product"`
	UsagePercent *float64 `json:"usage_percent"`
}

type xaiBillingValueSource struct {
	name   string
	values map[string]any
}

func normalizeXAIBilling(body []byte, billingFormat string, syncedAt time.Time) (*xaiQuotaPayload, error) {
	root, err := xaiBillingObject(body)
	if err != nil {
		return nil, err
	}
	billingFormat = strings.TrimSpace(strings.ToLower(billingFormat))
	if billingFormat == "" {
		billingFormat = "legacy"
	}
	config := xaiBillingMap(root["config"])
	sources := make([]xaiBillingValueSource, 0, 2)
	if config != nil {
		sources = append(sources, xaiBillingValueSource{name: "billing_" + billingFormat + ".config", values: config})
	}
	sources = append(sources, xaiBillingValueSource{name: "billing_" + billingFormat, values: root})

	valueSource := make(map[string]string)
	planCode, planCodeSource := xaiBillingPlanValue(sources, false)
	planName, planNameSource := xaiBillingPlanValue(sources, true)
	planTier, planTierSource := xaiBillingString(sources, "subscriptionTier", "subscription_tier")
	monthlyLimit, monthlyLimitSource := xaiBillingNumber(sources, "monthlyLimit", "monthly_limit")
	includedUsed, includedUsedSource := xaiBillingNumber(sources, "includedUsed", "included_used", "used")
	monthlyUsed, monthlyUsedSource := xaiBillingNumber(sources, "used")
	totalUsed, totalUsedSource := xaiBillingNumber(sources, "totalUsed", "total_used")
	onDemandCap, onDemandCapSource := xaiBillingNumber(
		sources,
		"onDemandCap",
		"on_demand_cap",
		"maxAmountPerMonth",
		"max_amount_per_month",
	)
	onDemandUsed, onDemandUsedSource := xaiBillingNumber(sources, "onDemandUsed", "on_demand_used")
	prepaidBalance, prepaidBalanceSource := xaiBillingNumber(sources, "prepaidBalance", "prepaid_balance")
	usagePercent, usagePercentSource := xaiBillingNumber(sources, "creditUsagePercent", "credit_usage_percent")
	weeklyPercent, weeklyPercentSource := usagePercent, usagePercentSource
	onDemandEnabled, onDemandEnabledSource := xaiBillingBool(sources, "onDemandEnabled", "on_demand_enabled")
	remaining, remainingSource := xaiBillingNumber(sources, "remaining", "remainingCredits", "remaining_credits")

	used := totalUsed
	usedSource := totalUsedSource
	if used == nil && includedUsed != nil && onDemandUsed != nil {
		value := *includedUsed + *onDemandUsed
		if !math.IsInf(value, 0) && !math.IsNaN(value) {
			used = &value
			usedSource = "derived:included_used+on_demand_used"
		}
	} else if used == nil && includedUsed != nil {
		value := *includedUsed
		used = &value
		usedSource = includedUsedSource
	}
	if remaining == nil && monthlyLimit != nil && includedUsed != nil {
		value := *monthlyLimit - *includedUsed
		if !math.IsInf(value, 0) && !math.IsNaN(value) {
			value = math.Max(0, value)
			remaining = &value
			remainingSource = "derived:monthly_limit-included_used"
		}
	}
	if usagePercent == nil {
		switch {
		case monthlyLimit != nil && *monthlyLimit > 0 && includedUsed != nil:
			usagePercent = xaiBillingPercent(includedUsed, monthlyLimit)
			if usagePercent != nil {
				usagePercentSource = "derived:included_used/monthly_limit"
			}
		case onDemandCap != nil && *onDemandCap > 0 && onDemandUsed != nil:
			usagePercent = xaiBillingPercent(onDemandUsed, onDemandCap)
			if usagePercent != nil {
				usagePercentSource = "derived:on_demand_used/on_demand_cap"
			}
		}
	}

	currentPeriod, currentPeriodSource := xaiBillingRecord(sources, "currentPeriod", "current_period")
	usagePeriod := xaiNormalizedPeriod{Type: "unknown"}
	if currentPeriod != nil {
		rawType := xaiBillingStringValue(currentPeriod["type"])
		usagePeriod = xaiNormalizedPeriod{
			Type:    xaiBillingPeriodType(rawType),
			RawType: rawType,
			Start:   xaiBillingStringValue(currentPeriod["start"]),
			End:     xaiBillingStringValue(currentPeriod["end"]),
		}
		valueSource["usage_period"] = currentPeriodSource
	}
	billingStart, billingStartSource := xaiBillingString(sources, "billingPeriodStart", "billing_period_start")
	billingEnd, billingEndSource := xaiBillingString(sources, "billingPeriodEnd", "billing_period_end")
	billingPeriod := xaiNormalizedPeriod{
		Type:  "monthly",
		Start: billingStart,
		End:   billingEnd,
	}

	amountUnit := "unknown"
	if billingFormat == "credits" {
		amountUnit = "credits"
		valueSource["amount_unit"] = "request:format=credits"
	} else if unit, unitSource := xaiBillingString(sources, "amountUnit", "amount_unit", "unit"); unit != nil {
		amountUnit = xaiBillingAmountUnit(*unit)
		if amountUnit != "unknown" {
			valueSource["amount_unit"] = unitSource
		}
	}
	currency, currencySource := xaiBillingString(sources, "currency", "currencyCode", "currency_code")
	currency = xaiBillingCurrency(currency)
	productUsage := xaiBillingProductUsage(sources)
	if planCode == nil && planName == nil && planTier == nil && monthlyLimit == nil && used == nil &&
		includedUsed == nil && remaining == nil && onDemandEnabled == nil && onDemandCap == nil &&
		onDemandUsed == nil && prepaidBalance == nil && usagePercent == nil && currentPeriod == nil &&
		billingStart == nil && billingEnd == nil && len(productUsage) == 0 {
		return nil, fmt.Errorf("payload contains no billing fields")
	}

	weekly := xaiNormalizedWindow{PeriodType: "unknown"}
	monthly := xaiNormalizedWindow{PeriodType: "unknown"}
	if billingFormat == "credits" {
		weekly = xaiNormalizedWindow{
			UsagePercent: weeklyPercent,
			PeriodType:   usagePeriod.Type,
			PeriodStart:  usagePeriod.Start,
			PeriodEnd:    usagePeriod.End,
		}
		xaiSetBillingSource(valueSource, "weekly.usage_percent", weeklyPercent, weeklyPercentSource)
		if usagePeriod.RawType != nil {
			valueSource["weekly.period_type"] = currentPeriodSource + ".type"
		}
		xaiSetBillingSource(valueSource, "weekly.period_start", weekly.PeriodStart, currentPeriodSource+".start")
		xaiSetBillingSource(valueSource, "weekly.period_end", weekly.PeriodEnd, currentPeriodSource+".end")
	} else {
		if monthlyUsed != nil || monthlyLimit != nil || billingStart != nil || billingEnd != nil {
			monthly.PeriodType = "monthly"
			valueSource["monthly.period_type"] = "derived:billing_" + billingFormat
		}
		monthly.PeriodStart, monthly.PeriodEnd = billingStart, billingEnd
		monthly.UsagePercent = xaiBillingPercent(monthlyUsed, monthlyLimit)
		if monthly.UsagePercent != nil {
			valueSource["monthly.usage_percent"] = "derived:" + monthlyUsedSource + "/" + monthlyLimitSource
		}
		xaiSetBillingSource(valueSource, "monthly.period_start", monthly.PeriodStart, billingStartSource)
		xaiSetBillingSource(valueSource, "monthly.period_end", monthly.PeriodEnd, billingEndSource)
	}

	xaiSetBillingSource(valueSource, "plan_code", planCode, planCodeSource)
	xaiSetBillingSource(valueSource, "plan_name", planName, planNameSource)
	xaiSetBillingSource(valueSource, "plan_tier", planTier, planTierSource)
	xaiSetBillingSource(valueSource, "monthly_limit", monthlyLimit, monthlyLimitSource)
	xaiSetBillingSource(valueSource, "used", used, usedSource)
	xaiSetBillingSource(valueSource, "included_used", includedUsed, includedUsedSource)
	xaiSetBillingSource(valueSource, "remaining", remaining, remainingSource)
	xaiSetBillingSource(valueSource, "on_demand_enabled", onDemandEnabled, onDemandEnabledSource)
	xaiSetBillingSource(valueSource, "on_demand_cap", onDemandCap, onDemandCapSource)
	xaiSetBillingSource(valueSource, "on_demand_used", onDemandUsed, onDemandUsedSource)
	xaiSetBillingSource(valueSource, "prepaid_balance", prepaidBalance, prepaidBalanceSource)
	xaiSetBillingSource(valueSource, "usage_percent", usagePercent, usagePercentSource)
	xaiSetBillingSource(valueSource, "billing_period_start", billingStart, billingStartSource)
	xaiSetBillingSource(valueSource, "billing_period_end", billingEnd, billingEndSource)
	xaiSetBillingSource(valueSource, "currency", currency, currencySource)

	return &xaiQuotaPayload{
		SchemaVersion: xaiBillingSchemaVersion,
		Source:        "xai_build_billing",
		BillingFormat: billingFormat,
		ValueSource:   valueSource,
		Billing:       append(json.RawMessage(nil), body...),
		Normalized: xaiNormalizedBilling{
			PlanCode:        planCode,
			PlanName:        planName,
			PlanTier:        planTier,
			MonthlyLimit:    monthlyLimit,
			Used:            used,
			IncludedUsed:    includedUsed,
			Remaining:       remaining,
			OnDemandEnabled: onDemandEnabled,
			OnDemandCap:     onDemandCap,
			OnDemandUsed:    onDemandUsed,
			PrepaidBalance:  prepaidBalance,
			UsagePercent:    usagePercent,
			UsagePeriod:     usagePeriod,
			BillingPeriod:   billingPeriod,
			Weekly:          weekly,
			Monthly:         monthly,
			ProductUsage:    productUsage,
			AmountUnit:      amountUnit,
			Currency:        currency,
			SyncedAt:        syncedAt.UTC().Format(time.RFC3339Nano),
		},
	}, nil
}

func (p *xaiQuotaPayload) retainWeeklyBilling() {
	if p == nil {
		return
	}
	p.Normalized.Monthly = xaiNormalizedWindow{PeriodType: "unknown"}
	p.Normalized.MonthlyLimit = nil
	p.Normalized.Used = nil
	p.Normalized.IncludedUsed = nil
	p.Normalized.Remaining = nil
	p.Normalized.BillingPeriod = xaiNormalizedPeriod{Type: "unknown"}
	p.Normalized.AmountUnit = "unknown"
	p.Normalized.Currency = nil
	p.Normalized.UsagePercent = p.Normalized.Weekly.UsagePercent
	for _, field := range []string{
		"monthly_limit", "used", "included_used", "remaining", "billing_period_start", "billing_period_end",
		"amount_unit", "currency", "monthly.usage_percent", "monthly.period_type", "monthly.period_start", "monthly.period_end",
		"usage_percent",
	} {
		delete(p.ValueSource, field)
	}
	if source := p.ValueSource["weekly.usage_percent"]; source != "" {
		p.ValueSource["usage_percent"] = source
	}
}

func (p *xaiQuotaPayload) applyMonthlyBilling(body []byte, syncedAt time.Time) error {
	monthly, err := normalizeXAIBilling(body, "legacy", syncedAt)
	if err != nil {
		return err
	}
	if monthly.Normalized.Monthly.PeriodType != "monthly" {
		return fmt.Errorf("monthly billing response has no monthly fields")
	}
	if p.Normalized.PlanCode == nil {
		p.Normalized.PlanCode = monthly.Normalized.PlanCode
		xaiSetBillingSource(p.ValueSource, "plan_code", p.Normalized.PlanCode, monthly.ValueSource["plan_code"])
	}
	if p.Normalized.PlanName == nil {
		p.Normalized.PlanName = monthly.Normalized.PlanName
		xaiSetBillingSource(p.ValueSource, "plan_name", p.Normalized.PlanName, monthly.ValueSource["plan_name"])
	}
	if p.Normalized.PlanTier == nil {
		p.Normalized.PlanTier = monthly.Normalized.PlanTier
		xaiSetBillingSource(p.ValueSource, "plan_tier", p.Normalized.PlanTier, monthly.ValueSource["plan_tier"])
	}
	p.Normalized.Monthly = monthly.Normalized.Monthly
	p.Normalized.MonthlyLimit = monthly.Normalized.MonthlyLimit
	p.Normalized.Used = monthly.Normalized.Used
	p.Normalized.IncludedUsed = monthly.Normalized.IncludedUsed
	p.Normalized.Remaining = monthly.Normalized.Remaining
	p.Normalized.BillingPeriod = monthly.Normalized.BillingPeriod
	p.Normalized.AmountUnit = monthly.Normalized.AmountUnit
	p.Normalized.Currency = monthly.Normalized.Currency
	for _, field := range []string{
		"monthly_limit", "used", "included_used", "remaining", "billing_period_start", "billing_period_end",
		"amount_unit", "currency", "monthly.usage_percent", "monthly.period_type", "monthly.period_start", "monthly.period_end",
	} {
		if source := monthly.ValueSource[field]; source != "" {
			p.ValueSource[field] = source
		}
	}
	return nil
}

func (p *xaiQuotaPayload) applySubscription(body []byte, name string) error {
	if p == nil {
		return fmt.Errorf("xai %s: quota payload is nil", name)
	}
	root, err := xaiBillingObject(body)
	if err != nil {
		return fmt.Errorf("xai %s: %w", name, err)
	}
	sources := []xaiBillingValueSource{{name: name, values: root}}
	nestedKey := "user"
	if name == "settings" {
		nestedKey = "settings"
	}
	if nested := xaiBillingMap(root[nestedKey]); nested != nil {
		sources = append([]xaiBillingValueSource{{name: name + "." + nestedKey, values: nested}}, sources...)
	}
	tier, source := xaiBillingString(sources, "subscriptionTierDisplay", "subscription_tier_display")
	if tier == nil {
		if name == "settings" && p.Normalized.PlanTier != nil {
			return nil
		}
		tier, source = xaiBillingString(sources, "subscriptionTier", "subscription_tier")
	}
	if tier != nil {
		p.Normalized.PlanTier = tier
		p.ValueSource["plan_tier"] = source
	}
	return nil
}

func (p *xaiQuotaPayload) addAuxiliaryError(source, code string, status int) {
	if p == nil {
		return
	}
	p.AuxiliaryErrors = append(
		p.AuxiliaryErrors, xaiQuotaAuxiliaryError{
			Source: strings.TrimSpace(source),
			Code:   strings.TrimSpace(code),
			Status: status,
		},
	)
}

func xaiBillingObject(body []byte) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("parse payload: %w", err)
	}
	if encoded, ok := value.(string); ok {
		return xaiBillingObject([]byte(encoded))
	}
	root, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("payload is not an object")
	}
	return root, nil
}

func xaiBillingMap(value any) map[string]any {
	result, _ := value.(map[string]any)
	return result
}

func xaiBillingRecord(sources []xaiBillingValueSource, keys ...string) (map[string]any, string) {
	for _, source := range sources {
		for _, key := range keys {
			if value := xaiBillingMap(source.values[key]); value != nil {
				return value, source.name + "." + key
			}
		}
	}
	return nil, ""
}

func xaiBillingNumber(sources []xaiBillingValueSource, keys ...string) (*float64, string) {
	for _, source := range sources {
		for _, key := range keys {
			value, ok := source.values[key]
			if !ok {
				continue
			}
			if result := xaiBillingNumberValue(value); result != nil {
				return result, source.name + "." + key
			}
		}
	}
	return nil, ""
}

func xaiBillingNumberValue(value any) *float64 {
	switch typed := value.(type) {
	case json.Number:
		parsed, err := typed.Float64()
		if err == nil && !math.IsNaN(parsed) && !math.IsInf(parsed, 0) {
			return &parsed
		}
	case float64:
		if !math.IsNaN(typed) && !math.IsInf(typed, 0) {
			return &typed
		}
	case string:
		normalized := strings.TrimSpace(typed)
		normalized = strings.NewReplacer("$", "", ",", "", "%", "").Replace(normalized)
		if normalized == "" {
			return nil
		}
		parsed, err := strconv.ParseFloat(normalized, 64)
		if err == nil && !math.IsNaN(parsed) && !math.IsInf(parsed, 0) {
			return &parsed
		}
	case map[string]any:
		return xaiBillingNumberValue(typed["val"])
	}
	return nil
}

func xaiBillingPercent(used, limit *float64) *float64 {
	if used == nil || limit == nil || *limit <= 0 {
		return nil
	}
	percent := *used / *limit * 100
	if math.IsNaN(percent) || math.IsInf(percent, 0) {
		return nil
	}
	return &percent
}

func xaiBillingString(sources []xaiBillingValueSource, keys ...string) (*string, string) {
	for _, source := range sources {
		for _, key := range keys {
			if result := xaiBillingStringValue(source.values[key]); result != nil {
				return result, source.name + "." + key
			}
		}
	}
	return nil, ""
}

func xaiBillingStringValue(value any) *string {
	typed, ok := value.(string)
	if !ok {
		return nil
	}
	trimmed := strings.TrimSpace(typed)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}

func xaiBillingBool(sources []xaiBillingValueSource, keys ...string) (*bool, string) {
	for _, source := range sources {
		for _, key := range keys {
			value, ok := source.values[key]
			if !ok {
				continue
			}
			result, ok := value.(bool)
			if ok {
				return &result, source.name + "." + key
			}
		}
	}
	return nil, ""
}

func xaiBillingPlanValue(sources []xaiBillingValueSource, name bool) (*string, string) {
	directKeys := []string{"planCode", "plan_code"}
	nestedKeys := []string{"code", "id", "tier", "slug"}
	if name {
		directKeys = []string{"planName", "plan_name", "subscriptionName", "subscription_name"}
		nestedKeys = []string{"name", "displayName", "display_name", "label"}
	}
	if value, source := xaiBillingString(sources, directKeys...); value != nil {
		return value, source
	}
	for _, source := range sources {
		for _, key := range []string{"plan", "subscription", "membership"} {
			record := xaiBillingMap(source.values[key])
			if record == nil {
				continue
			}
			for _, nestedKey := range nestedKeys {
				if result := xaiBillingStringValue(record[nestedKey]); result != nil {
					return result, source.name + "." + key + "." + nestedKey
				}
			}
		}
	}
	return nil, ""
}

func xaiBillingProductUsage(sources []xaiBillingValueSource) []xaiProductUsage {
	for _, source := range sources {
		var raw any
		if value, ok := source.values["productUsage"]; ok {
			raw = value
		} else {
			raw = source.values["product_usage"]
		}
		values, ok := raw.([]any)
		if !ok {
			continue
		}
		result := make([]xaiProductUsage, 0, len(values))
		for index, value := range values {
			record := xaiBillingMap(value)
			if record == nil {
				continue
			}
			product := xaiBillingStringValue(record["product"])
			if product == nil {
				fallback := fmt.Sprintf("Product %d", index+1)
				product = &fallback
			}
			usagePercent := xaiBillingNumberValue(record["usagePercent"])
			if usagePercent == nil {
				usagePercent = xaiBillingNumberValue(record["usage_percent"])
			}
			usagePercent = xaiBillingNormalizePercent(usagePercent)
			result = append(result, xaiProductUsage{Product: *product, UsagePercent: usagePercent})
		}
		return result
	}
	return []xaiProductUsage{}
}

func xaiBillingNormalizePercent(value *float64) *float64 {
	if value == nil {
		return nil
	}
	if *value >= 0 && *value <= 1 {
		normalized := *value * 100
		return &normalized
	}
	return value
}

func xaiBillingPeriodType(raw *string) string {
	if raw == nil {
		return "unknown"
	}
	normalized := strings.ToLower(*raw)
	switch {
	case strings.Contains(normalized, "weekly"):
		return "weekly"
	case strings.Contains(normalized, "monthly"):
		return "monthly"
	default:
		return "unknown"
	}
}

func xaiBillingAmountUnit(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "credit", "credits":
		return "credits"
	case "cent", "cents", "currency_minor":
		return "currency_minor"
	case "currency", "currency_major", "dollar", "dollars":
		return "currency_major"
	default:
		return "unknown"
	}
}

func xaiBillingCurrency(value *string) *string {
	if value == nil {
		return nil
	}
	normalized := strings.ToUpper(strings.TrimSpace(*value))
	if len(normalized) != 3 {
		return nil
	}
	for _, char := range normalized {
		if char < 'A' || char > 'Z' {
			return nil
		}
	}
	return &normalized
}

func xaiSetBillingSource[T any](sources map[string]string, field string, value *T, source string) {
	if value != nil && source != "" {
		sources[field] = source
	}
}
