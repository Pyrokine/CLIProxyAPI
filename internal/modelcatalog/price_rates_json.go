package modelcatalog

import (
	"bytes"
	"encoding/json"
	"reflect"
)

// Equal compares the persisted price, including absent and explicitly zero rates.
func (p Price) Equal(other Price) bool {
	if reflect.DeepEqual(p, other) {
		return true
	}
	left, leftErr := json.Marshal(p)
	right, rightErr := json.Marshal(other)
	return leftErr == nil && rightErr == nil && bytes.Equal(left, right)
}

func (p *Price) UnmarshalJSON(data []byte) error {
	type priceJSON Price
	var value priceJSON
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	*p = Price(value)
	present := func(key string) bool {
		raw, exists := fields[key]
		return exists && !bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
	}
	p.promptPresent = present("prompt") && p.Prompt == 0
	p.completionPresent = present("completion") && p.Completion == 0
	p.cachePresent = present("cache") && p.Cache == 0
	p.requestScopedPresent = present("request_scoped") && !p.RequestScoped
	p.autoApplyPresent = present("auto_apply") && !p.AutoApply
	return nil
}

func (p Price) MarshalJSON() ([]byte, error) {
	type priceJSON Price
	var prompt, completion, cache *float64
	if p.PromptRateKnown() {
		prompt = &p.Prompt
	}
	if p.CompletionRateKnown() {
		completion = &p.Completion
	}
	if _, known := p.CacheReadRate(); p.CacheRead == nil && known || p.cachePresent {
		cache = &p.Cache
	}
	var requestScoped, autoApply *bool
	if p.RequestScoped || p.requestScopedPresent {
		requestScoped = &p.RequestScoped
	}
	if p.AutoApply || p.autoApplyPresent {
		autoApply = &p.AutoApply
	}
	return json.Marshal(
		struct {
			*priceJSON
			Prompt        *float64 `json:"prompt,omitempty"`
			Completion    *float64 `json:"completion,omitempty"`
			Cache         *float64 `json:"cache,omitempty"`
			RequestScoped *bool    `json:"request_scoped,omitempty"`
			AutoApply     *bool    `json:"auto_apply,omitempty"`
		}{
			priceJSON: (*priceJSON)(&p), Prompt: prompt, Completion: completion, Cache: cache,
			RequestScoped: requestScoped, AutoApply: autoApply,
		},
	)
}

func (p Price) PromptRateKnown() bool {
	return p.promptPresent || p.Prompt != 0
}

func (p Price) CompletionRateKnown() bool {
	return p.completionPresent || p.Completion != 0
}
