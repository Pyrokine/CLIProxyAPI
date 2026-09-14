package modelcatalog

import (
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
)

func (p Price) EffectiveStatus() PriceStatus {
	status := PriceStatus(strings.ToLower(strings.TrimSpace(string(p.Status))))
	if status == "" {
		return PriceStatusComplete
	}
	return status
}

func (p Price) Automatic() bool {
	switch p.EffectiveStatus() {
	case PriceStatusComplete:
		return true
	case PriceStatusPartial:
		return p.AutoApply
	default:
		return false
	}
}

func (p Price) CacheReadRate() (float64, bool) {
	if p.CacheRead != nil {
		return *p.CacheRead, true
	}
	return p.Cache, true
}

func (p Price) CacheWriteRate() (float64, bool) {
	if p.CacheWrite == nil {
		return 0, false
	}
	return *p.CacheWrite, true
}

func (p Price) CacheWrite5mRate() (float64, bool) {
	if p.CacheWrite5m == nil {
		return 0, false
	}
	return *p.CacheWrite5m, true
}

func (p Price) CacheWrite1hRate() (float64, bool) {
	if p.CacheWrite1h == nil {
		return 0, false
	}
	return *p.CacheWrite1h, true
}

func (p Price) HasReferenceTokenRates() bool {
	return p.Prompt != 0 || p.Completion != 0 || p.Cache != 0 ||
		p.CacheRead != nil || p.CacheWrite != nil || p.CacheWrite5m != nil || p.CacheWrite1h != nil
}

func (p Price) EffectiveAt(at time.Time) bool {
	if at.IsZero() {
		return true
	}
	if from, ok := parsePriceBoundary(p.EffectiveFrom, false); ok && at.Before(from) {
		return false
	}
	if until, ok := parsePriceBoundary(p.EffectiveUntil, true); ok && !at.Before(until) {
		return false
	}
	return true
}

func (p Price) RatesAt(at time.Time) Price {
	if at.IsZero() {
		return p
	}
	for _, band := range p.UTCRateBands {
		if !band.matches(at.UTC()) {
			continue
		}
		p.Prompt = band.Prompt
		p.Completion = band.Completion
		p.Cache = band.Cache
		p.CacheRead = cloneFloat64(band.CacheRead)
		p.CacheWrite = cloneFloat64(band.CacheWrite)
		p.CacheWrite5m = cloneFloat64(band.CacheWrite5m)
		p.CacheWrite1h = cloneFloat64(band.CacheWrite1h)
		return p
	}
	return p
}

func (p Price) validate() error {
	status := p.EffectiveStatus()
	switch status {
	case PriceStatusComplete, PriceStatusPartial, PriceStatusUnpriced:
	default:
		return fmt.Errorf("invalid price status %q", p.Status)
	}
	currency := strings.ToUpper(strings.TrimSpace(p.Currency))
	if currency != "" && currency != "USD" {
		return fmt.Errorf("unsupported price currency %q: usage aggregation requires USD", p.Currency)
	}
	if err := validateTokenRates(
		tokenRates{
			Prompt:       p.Prompt,
			Completion:   p.Completion,
			Cache:        p.Cache,
			CacheRead:    p.CacheRead,
			CacheWrite:   p.CacheWrite,
			CacheWrite5m: p.CacheWrite5m,
			CacheWrite1h: p.CacheWrite1h,
		},
	); err != nil {
		return err
	}
	intervalsByWeekday := make(map[time.Weekday][]priceBandInterval)
	for index, band := range p.UTCRateBands {
		startMinute, endMinute, weekdays, err := band.validate()
		if err != nil {
			return fmt.Errorf("utc_rate_bands[%d]: %w", index, err)
		}
		for _, weekday := range weekdays {
			for _, existing := range intervalsByWeekday[weekday] {
				if startMinute < existing.end && existing.start < endMinute {
					return fmt.Errorf("utc_rate_bands[%d]: overlaps another band on %s", index, weekday)
				}
			}
			intervalsByWeekday[weekday] = append(
				intervalsByWeekday[weekday], priceBandInterval{start: startMinute, end: endMinute},
			)
		}
	}
	for _, boundary := range []struct {
		name  string
		value string
	}{
		{name: "effective_from", value: p.EffectiveFrom},
		{name: "effective_until", value: p.EffectiveUntil},
	} {
		if strings.TrimSpace(boundary.value) == "" {
			continue
		}
		if _, ok := parsePriceBoundary(boundary.value, boundary.name == "effective_until"); !ok {
			return fmt.Errorf("invalid %s %q", boundary.name, boundary.value)
		}
	}
	return nil
}

type priceBandInterval struct {
	start int
	end   int
}

func validateTokenRates(rates tokenRates) error {
	values := []struct {
		name  string
		value float64
	}{
		{name: "prompt", value: rates.Prompt},
		{name: "completion", value: rates.Completion},
		{name: "cache", value: rates.Cache},
	}
	if rates.CacheRead != nil {
		values = append(
			values, struct {
				name  string
				value float64
			}{name: "cache_read", value: *rates.CacheRead},
		)
	}
	for _, rate := range []struct {
		name  string
		value *float64
	}{
		{name: "cache_write", value: rates.CacheWrite},
		{name: "cache_write_5m", value: rates.CacheWrite5m},
		{name: "cache_write_1h", value: rates.CacheWrite1h},
	} {
		if rate.value == nil {
			continue
		}
		values = append(
			values, struct {
				name  string
				value float64
			}{name: rate.name, value: *rate.value},
		)
	}
	for _, item := range values {
		if math.IsNaN(item.value) || math.IsInf(item.value, 0) || item.value < 0 {
			return fmt.Errorf("invalid %s price", item.name)
		}
	}
	return nil
}

func (b utcPriceBand) validate() (int, int, []time.Weekday, error) {
	if len(b.Weekdays) == 0 {
		return 0, 0, nil, fmt.Errorf("weekdays must not be empty")
	}
	weekdays := make([]time.Weekday, 0, len(b.Weekdays))
	seen := make(map[time.Weekday]struct{}, len(b.Weekdays))
	for _, raw := range b.Weekdays {
		weekday, ok := parseWeekday(raw)
		if !ok {
			return 0, 0, nil, fmt.Errorf("invalid weekday %q", raw)
		}
		if _, exists := seen[weekday]; exists {
			continue
		}
		seen[weekday] = struct{}{}
		weekdays = append(weekdays, weekday)
	}
	startMinute, ok := parseUTCTimeOfDay(b.Start)
	if !ok {
		return 0, 0, nil, fmt.Errorf("invalid start %q", b.Start)
	}
	endMinute, ok := parseUTCTimeOfDay(b.End)
	if !ok {
		return 0, 0, nil, fmt.Errorf("invalid end %q", b.End)
	}
	if startMinute >= endMinute {
		return 0, 0, nil, fmt.Errorf("start must precede end")
	}
	if err := validateTokenRates(b.tokenRates); err != nil {
		return 0, 0, nil, err
	}
	return startMinute, endMinute, weekdays, nil
}

func (b utcPriceBand) matches(at time.Time) bool {
	startMinute, endMinute, weekdays, err := b.validate()
	if err != nil {
		return false
	}
	matchesWeekday := slices.Contains(weekdays, at.Weekday())
	if !matchesWeekday {
		return false
	}
	minute := at.Hour()*60 + at.Minute()
	return minute >= startMinute && minute < endMinute
}

func parseWeekday(value string) (time.Weekday, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "sunday":
		return time.Sunday, true
	case "monday":
		return time.Monday, true
	case "tuesday":
		return time.Tuesday, true
	case "wednesday":
		return time.Wednesday, true
	case "thursday":
		return time.Thursday, true
	case "friday":
		return time.Friday, true
	case "saturday":
		return time.Saturday, true
	default:
		return 0, false
	}
}

func parseUTCTimeOfDay(value string) (int, bool) {
	parsed, err := time.Parse("15:04", strings.TrimSpace(value))
	if err != nil {
		return 0, false
	}
	return parsed.Hour()*60 + parsed.Minute(), true
}

func cloneFloat64(value *float64) *float64 {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func (f *File) Validate() error {
	if f.SchemaVersion != SchemaVersion {
		return fmt.Errorf("schema_version is %d, want %d", f.SchemaVersion, SchemaVersion)
	}
	if strings.TrimSpace(f.DefaultsVersion) == "" {
		return fmt.Errorf("defaults_version must not be empty")
	}
	models := make(map[string]struct{}, len(f.Models))
	for index, model := range f.Models {
		name := normalizeModelName(model.Name)
		key := modelKey(name)
		if key == "" {
			return fmt.Errorf("models[%d].name must not be empty", index)
		}
		if _, exists := models[key]; exists {
			return fmt.Errorf("duplicate model name %q", name)
		}
		models[key] = struct{}{}
		if strings.TrimSpace(model.Provider) == "" {
			return fmt.Errorf("model %q provider must not be empty", name)
		}
		if strings.TrimSpace(model.Group) == "" {
			return fmt.Errorf("model %q group must not be empty", name)
		}
		if len(model.Aliases) > 0 || len(model.ForceMappedAliases) > 0 || model.Price != nil {
			return fmt.Errorf("model %q contains unmigrated legacy fields", name)
		}
	}
	removed := make(map[string]struct{}, len(f.RemovedDefaultModels))
	for index, name := range f.RemovedDefaultModels {
		key := modelKey(name)
		if key == "" {
			return fmt.Errorf("removed_default_models[%d] must not be empty", index)
		}
		if _, exists := removed[key]; exists {
			return fmt.Errorf("duplicate removed default model %q", name)
		}
		if _, exists := models[key]; exists {
			return fmt.Errorf("removed default model %q is still present", name)
		}
		removed[key] = struct{}{}
	}
	if err := validateOAuthAliases(f.OAuthModelAlias, models); err != nil {
		return err
	}
	priceNames := make(map[string]struct{}, len(f.Prices))
	for name, price := range f.Prices {
		key := modelKey(name)
		if key == "" {
			return fmt.Errorf("empty model price name")
		}
		if _, exists := priceNames[key]; exists {
			return fmt.Errorf("duplicate model price name %q", name)
		}
		priceNames[key] = struct{}{}
		if err := price.validate(); err != nil {
			return fmt.Errorf("price %q: %w", name, err)
		}
		from, hasFrom := parsePriceBoundary(price.EffectiveFrom, false)
		until, hasUntil := parsePriceBoundary(price.EffectiveUntil, true)
		if hasFrom && hasUntil && !from.Before(until) {
			return fmt.Errorf("price %q: effective_from must precede effective_until", name)
		}
	}
	return nil
}

func validateFileBeforeSanitize(f File) error {
	if f.SchemaVersion < 0 || f.SchemaVersion > SchemaVersion {
		return fmt.Errorf("unsupported schema_version %d", f.SchemaVersion)
	}
	models := make(map[string]struct{}, len(f.Models))
	for index, model := range f.Models {
		key := modelKey(model.Name)
		if key == "" {
			return fmt.Errorf("models[%d].name must not be empty", index)
		}
		if _, exists := models[key]; exists {
			return fmt.Errorf("duplicate model name %q", model.Name)
		}
		models[key] = struct{}{}
		if err := validateLegacyModelAliases(index, model); err != nil {
			return err
		}
	}
	if err := validateOAuthAliases(f.OAuthModelAlias, models); err != nil {
		return err
	}
	priceNames := make(map[string]struct{}, len(f.Prices))
	for name := range f.Prices {
		key := modelKey(name)
		if key == "" {
			return fmt.Errorf("empty model price name")
		}
		if _, exists := priceNames[key]; exists {
			return fmt.Errorf("duplicate model price name %q", name)
		}
		priceNames[key] = struct{}{}
	}
	return nil
}

func validateLegacyModelAliases(index int, model Model) error {
	aliases := make(map[string]struct{}, len(model.Aliases))
	for aliasIndex, alias := range model.Aliases {
		key := modelKey(alias)
		if key == "" {
			return fmt.Errorf("models[%d].aliases[%d] must not be empty", index, aliasIndex)
		}
		if key == modelKey(model.Name) {
			return fmt.Errorf("model %q alias must differ from its name", model.Name)
		}
		if _, exists := aliases[key]; exists {
			return fmt.Errorf("model %q has duplicate alias %q", model.Name, alias)
		}
		aliases[key] = struct{}{}
	}
	for _, alias := range model.ForceMappedAliases {
		if _, exists := aliases[modelKey(alias)]; !exists {
			return fmt.Errorf("model %q force-mapped alias %q is not declared", model.Name, alias)
		}
	}
	return nil
}

func validateOAuthAliases(
	aliasesByChannel map[string][]config.OAuthModelAlias,
	models map[string]struct{},
) error {
	channels := make(map[string]struct{}, len(aliasesByChannel))
	for channel, aliases := range aliasesByChannel {
		channelKey := strings.ToLower(strings.TrimSpace(channel))
		if channelKey == "" {
			return fmt.Errorf("oauth_model_alias channel must not be empty")
		}
		if _, exists := channels[channelKey]; exists {
			return fmt.Errorf("duplicate oauth_model_alias channel %q", channel)
		}
		channels[channelKey] = struct{}{}
		seenAliases := make(map[string]struct{}, len(aliases))
		for index, alias := range aliases {
			nameKey := modelKey(alias.Name)
			aliasKey := modelKey(alias.Alias)
			if nameKey == "" || aliasKey == "" {
				return fmt.Errorf("oauth_model_alias[%q][%d] name and alias must not be empty", channel, index)
			}
			if nameKey == aliasKey {
				return fmt.Errorf("oauth_model_alias[%q][%d] alias must differ from name", channel, index)
			}
			if _, exists := models[nameKey]; !exists {
				return fmt.Errorf("oauth_model_alias[%q][%d] references unknown model %q", channel, index, alias.Name)
			}
			if _, exists := seenAliases[aliasKey]; exists {
				return fmt.Errorf("oauth_model_alias channel %q has duplicate alias %q", channel, alias.Alias)
			}
			seenAliases[aliasKey] = struct{}{}
		}
	}
	return nil
}

func (f *File) Price(name string) (Price, bool) {
	if f == nil {
		return Price{}, false
	}
	return priceByModelKey(f.Prices, name)
}

func (f *File) SetPrice(name string, price Price) bool {
	if f == nil {
		return false
	}
	name = normalizeModelName(name)
	if name == "" {
		return false
	}
	if f.Prices == nil {
		f.Prices = make(map[string]Price)
	}
	deletePriceByModelName(f.Prices, name)
	setPriceByModelName(f.Prices, name, price)
	return true
}

func (f *File) DeletePrice(name string) bool {
	if f == nil {
		return false
	}
	return deletePriceByModelName(f.Prices, name)
}

func sanitizePrices(prices map[string]Price) map[string]Price {
	if len(prices) == 0 {
		return nil
	}
	names := make([]string, 0, len(prices))
	for name := range prices {
		names = append(names, name)
	}
	sort.Slice(
		names, func(i, j int) bool {
			return strings.ToLower(names[i]) < strings.ToLower(names[j])
		},
	)
	out := make(map[string]Price, len(prices))
	seen := make(map[string]struct{}, len(prices))
	for _, rawName := range names {
		name := normalizeModelName(rawName)
		key := modelKey(name)
		if key == "" {
			continue
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		price := prices[rawName]
		sanitizePrice(&price)
		out[name] = price
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func sanitizePrice(p *Price) {
	if p == nil {
		return
	}
	p.Status = PriceStatus(strings.ToLower(strings.TrimSpace(string(p.Status))))
	p.Currency = strings.ToUpper(strings.TrimSpace(p.Currency))
	p.Source = strings.TrimSpace(p.Source)
	p.CheckedAt = strings.TrimSpace(p.CheckedAt)
	p.EffectiveFrom = strings.TrimSpace(p.EffectiveFrom)
	p.EffectiveUntil = strings.TrimSpace(p.EffectiveUntil)
	p.Region = strings.TrimSpace(p.Region)
	p.ServiceTier = strings.TrimSpace(p.ServiceTier)
	p.UnpricedReason = strings.TrimSpace(p.UnpricedReason)
	p.Limitations = uniqueStrings(p.Limitations)
	p.CacheRead = cloneFloat64(p.CacheRead)
	if p.CacheRead != nil {
		p.Cache = *p.CacheRead
	}
	p.CacheWrite = cloneFloat64(p.CacheWrite)
	p.CacheWrite5m = cloneFloat64(p.CacheWrite5m)
	p.CacheWrite1h = cloneFloat64(p.CacheWrite1h)
	for i := range p.UTCRateBands {
		band := &p.UTCRateBands[i]
		weekdays := make([]string, 0, len(band.Weekdays))
		for _, weekday := range band.Weekdays {
			weekdays = append(weekdays, strings.ToLower(strings.TrimSpace(weekday)))
		}
		band.Weekdays = uniqueStrings(weekdays)
		band.Start = strings.TrimSpace(band.Start)
		band.End = strings.TrimSpace(band.End)
		band.CacheRead = cloneFloat64(band.CacheRead)
		if band.CacheRead != nil {
			band.Cache = *band.CacheRead
		}
		band.CacheWrite = cloneFloat64(band.CacheWrite)
		band.CacheWrite5m = cloneFloat64(band.CacheWrite5m)
		band.CacheWrite1h = cloneFloat64(band.CacheWrite1h)
	}
}

func priceByModelKey(prices map[string]Price, name string) (Price, bool) {
	key := modelKey(name)
	if key == "" {
		return Price{}, false
	}
	for currentName, price := range prices {
		if modelKey(currentName) == key {
			return price, true
		}
	}
	return Price{}, false
}

func setPriceByModelName(prices map[string]Price, name string, price Price) {
	if prices == nil {
		return
	}
	name = normalizeModelName(name)
	if name == "" {
		return
	}
	sanitizePrice(&price)
	prices[name] = price
}

func deletePriceByModelName(prices map[string]Price, name string) bool {
	key := modelKey(name)
	if key == "" {
		return false
	}
	removed := false
	for currentName := range prices {
		if modelKey(currentName) != key {
			continue
		}
		delete(prices, currentName)
		removed = true
	}
	return removed
}

func parsePriceBoundary(value string, endExclusive bool) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	if parsed, err := time.Parse(time.RFC3339, value); err == nil {
		return parsed, true
	}
	parsed, err := time.Parse("2006-01-02", value)
	if err != nil {
		return time.Time{}, false
	}
	if endExclusive {
		parsed = parsed.AddDate(0, 0, 1)
	}
	return parsed, true
}
