package helps

import (
	"fmt"
	"math"
	"strings"

	"google.golang.org/protobuf/encoding/protowire"
)

// DevinUsage captures token accounting and request metadata from response field 7.
// Presence flags distinguish an explicit zero from an absent metric.
type DevinUsage struct {
	PromptTokens            int64             `json:"prompt_tokens"`
	CompletionTokens        int64             `json:"completion_tokens"`
	CachedTokens            int64             `json:"cached_tokens"`
	CacheWriteTokens        int64             `json:"cache_write_tokens,omitempty"`
	StatusCode              uint64            `json:"status_code,omitempty"`
	RequestID               string            `json:"request_id,omitempty"`
	ModelName               string            `json:"model_name,omitempty"`
	Headers                 map[string]string `json:"headers,omitempty"`
	PromptTokensPresent     bool              `json:"-"`
	CompletionTokensPresent bool              `json:"-"`
	CachedTokensPresent     bool              `json:"-"`
	CacheWriteTokensPresent bool              `json:"-"`
}

// InputTokens returns uncached plus cached input tokens.
func (usage *DevinUsage) InputTokens() int64 {
	if usage == nil {
		return 0
	}
	return usage.PromptTokens + usage.CachedTokens
}

// TotalTokens returns input plus completion tokens.
func (usage *DevinUsage) TotalTokens() int64 {
	if usage == nil {
		return 0
	}
	return usage.InputTokens() + usage.CompletionTokens
}

// FillMissingTokenMetrics copies only absent token metrics from fallback.
func (usage *DevinUsage) FillMissingTokenMetrics(fallback *DevinUsage) {
	if usage == nil || fallback == nil {
		return
	}
	if !usage.PromptTokensPresent && fallback.PromptTokensPresent {
		usage.PromptTokens = fallback.PromptTokens
		usage.PromptTokensPresent = true
	}
	if !usage.CompletionTokensPresent && fallback.CompletionTokensPresent {
		usage.CompletionTokens = fallback.CompletionTokens
		usage.CompletionTokensPresent = true
	}
	if !usage.CachedTokensPresent && fallback.CachedTokensPresent {
		usage.CachedTokens = fallback.CachedTokens
		usage.CachedTokensPresent = true
	}
	if !usage.CacheWriteTokensPresent && fallback.CacheWriteTokensPresent {
		usage.CacheWriteTokens = fallback.CacheWriteTokens
		usage.CacheWriteTokensPresent = true
	}
}

func mergeDevinUsage(dst, src *DevinUsage) {
	if dst == nil || src == nil {
		return
	}
	if src.PromptTokensPresent {
		dst.PromptTokens = src.PromptTokens
		dst.PromptTokensPresent = true
	}
	if src.CompletionTokensPresent {
		dst.CompletionTokens = src.CompletionTokens
		dst.CompletionTokensPresent = true
	}
	if src.CachedTokensPresent {
		dst.CachedTokens = src.CachedTokens
		dst.CachedTokensPresent = true
	}
	if src.CacheWriteTokensPresent {
		dst.CacheWriteTokens = src.CacheWriteTokens
		dst.CacheWriteTokensPresent = true
	}
	if src.StatusCode != 0 {
		dst.StatusCode = src.StatusCode
	}
	if src.RequestID != "" {
		dst.RequestID = src.RequestID
	}
	if src.ModelName != "" {
		dst.ModelName = src.ModelName
	}
	if len(src.Headers) > 0 {
		if dst.Headers == nil {
			dst.Headers = make(map[string]string, len(src.Headers))
		}
		for key, value := range src.Headers {
			dst.Headers[key] = value
		}
	}
}

func parseDevinUsageField(data []byte) (*DevinUsage, error) {
	usage := &DevinUsage{}
	for position := 0; position < len(data); {
		number, wireType, consumed := protowire.ConsumeTag(data[position:])
		if consumed <= 0 {
			return nil, protowire.ParseError(consumed)
		}
		position += consumed

		switch number {
		case 2, 3, 4, 5, 6:
			if wireType != protowire.VarintType {
				return nil, fmt.Errorf("usage field %d has wire type %d, want varint", number, wireType)
			}
			value, size := protowire.ConsumeVarint(data[position:])
			if size <= 0 {
				return nil, protowire.ParseError(size)
			}
			position += size
			switch number {
			case 2:
				converted, err := devinTokenCount(value)
				if err != nil {
					return nil, fmt.Errorf("usage prompt tokens: %w", err)
				}
				if usage.PromptTokens > math.MaxInt64-converted {
					return nil, fmt.Errorf("usage prompt token count overflows int64")
				}
				usage.PromptTokens += converted
				usage.PromptTokensPresent = true
			case 4:
				converted, err := devinTokenCount(value)
				if err != nil {
					return nil, fmt.Errorf("usage cache write tokens: %w", err)
				}
				if usage.CacheWriteTokens > math.MaxInt64-converted {
					return nil, fmt.Errorf("usage cache write token count overflows int64")
				}
				usage.CacheWriteTokens += converted
				usage.CacheWriteTokensPresent = true
			case 3:
				converted, err := devinTokenCount(value)
				if err != nil {
					return nil, fmt.Errorf("usage completion tokens: %w", err)
				}
				usage.CompletionTokens = converted
				usage.CompletionTokensPresent = true
			case 5:
				converted, err := devinTokenCount(value)
				if err != nil {
					return nil, fmt.Errorf("usage cached tokens: %w", err)
				}
				usage.CachedTokens = converted
				usage.CachedTokensPresent = true
			case 6:
				usage.StatusCode = value
			}
		case 8, 9:
			if wireType != protowire.BytesType {
				return nil, fmt.Errorf("usage field %d has wire type %d, want bytes", number, wireType)
			}
			value, size := protowire.ConsumeBytes(data[position:])
			if size <= 0 {
				return nil, protowire.ParseError(size)
			}
			position += size
			if number == 9 {
				usage.ModelName = string(value)
				continue
			}

			key, headerValue, err := parseDevinHeaderField(value)
			if err != nil {
				if len(value) > 0 && isPrintableASCII(value) && usage.RequestID == "" {
					usage.RequestID = string(value)
					continue
				}
				return nil, fmt.Errorf("parse usage header: %w", err)
			}
			if key == "" {
				if len(value) > 0 && isPrintableASCII(value) && usage.RequestID == "" {
					usage.RequestID = string(value)
				}
				continue
			}
			if usage.Headers == nil {
				usage.Headers = make(map[string]string)
			}
			usage.Headers[key] = headerValue
			if (strings.EqualFold(key, "x-request-id") || strings.EqualFold(key, "request-id")) &&
				headerValue != "" {
				usage.RequestID = headerValue
			}
		default:
			size := protowire.ConsumeFieldValue(number, wireType, data[position:])
			if size <= 0 {
				return nil, protowire.ParseError(size)
			}
			position += size
		}
	}
	return usage, nil
}

func devinTokenCount(value uint64) (int64, error) {
	if value > math.MaxInt64 {
		return 0, fmt.Errorf("token count %d exceeds int64", value)
	}
	return int64(value), nil
}

func parseDevinHeaderField(data []byte) (string, string, error) {
	var key string
	var value string
	for position := 0; position < len(data); {
		number, wireType, consumed := protowire.ConsumeTag(data[position:])
		if consumed <= 0 {
			return "", "", protowire.ParseError(consumed)
		}
		position += consumed
		if number == 1 || number == 2 {
			if wireType != protowire.BytesType {
				return "", "", fmt.Errorf("header field %d has wire type %d, want bytes", number, wireType)
			}
			bytesValue, size := protowire.ConsumeBytes(data[position:])
			if size <= 0 {
				return "", "", protowire.ParseError(size)
			}
			position += size
			if number == 1 {
				key = string(bytesValue)
			} else {
				value = string(bytesValue)
			}
			continue
		}

		size := protowire.ConsumeFieldValue(number, wireType, data[position:])
		if size <= 0 {
			return "", "", protowire.ParseError(size)
		}
		position += size
	}
	return key, value, nil
}

func isPrintableASCII(value []byte) bool {
	for _, character := range value {
		if character < 32 || character > 126 {
			return false
		}
	}
	return true
}

// ParseDevinResponseDimensionGroups decodes token metrics from response field 28.
func ParseDevinResponseDimensionGroups(groups ...[]byte) (*DevinUsage, error) {
	usage := &DevinUsage{}
	for index, group := range groups {
		if len(group) == 0 {
			continue
		}
		unwrapped, err := unwrapDevinDimensionGroup(group)
		if err != nil {
			return nil, fmt.Errorf("dimension group %d: %w", index, err)
		}
		title, metrics, err := parseDevinDimensionGroup(unwrapped)
		if err != nil {
			return nil, fmt.Errorf("dimension group %d: %w", index, err)
		}
		if !strings.EqualFold(title, "Token Usage") {
			continue
		}
		for key, metric := range metrics {
			count, err := devinDimensionTokenCount(metric)
			if err != nil {
				return nil, fmt.Errorf("dimension metric %q: %w", key, err)
			}
			switch key {
			case "input_tokens":
				usage.PromptTokens = count
				usage.PromptTokensPresent = true
			case "output_tokens":
				usage.CompletionTokens = count
				usage.CompletionTokensPresent = true
			case "cached_input_tokens":
				usage.CachedTokens = count
				usage.CachedTokensPresent = true
			}
		}
	}
	return usage, nil
}

func unwrapDevinDimensionGroup(data []byte) ([]byte, error) {
	number, wireType, consumed := protowire.ConsumeTag(data)
	if consumed <= 0 {
		return nil, protowire.ParseError(consumed)
	}
	if number != 28 || wireType != protowire.BytesType {
		return data, nil
	}
	value, size := protowire.ConsumeBytes(data[consumed:])
	if size <= 0 {
		return nil, protowire.ParseError(size)
	}
	if consumed+size != len(data) {
		return nil, fmt.Errorf("outer field 28 contains trailing bytes")
	}
	return value, nil
}

func parseDevinDimensionGroup(data []byte) (string, map[string]float32, error) {
	var title string
	metrics := make(map[string]float32)
	for position := 0; position < len(data); {
		number, wireType, consumed := protowire.ConsumeTag(data[position:])
		if consumed <= 0 {
			return "", nil, protowire.ParseError(consumed)
		}
		position += consumed
		if number == 1 || number == 2 {
			if wireType != protowire.BytesType {
				return "", nil, fmt.Errorf("group field %d has wire type %d, want bytes", number, wireType)
			}
			value, size := protowire.ConsumeBytes(data[position:])
			if size <= 0 {
				return "", nil, protowire.ParseError(size)
			}
			position += size
			if number == 1 {
				title = string(value)
				continue
			}
			key, metric, present, err := parseDevinDimensionMetric(value)
			if err != nil {
				return "", nil, err
			}
			if key != "" && present {
				metrics[key] = metric
			}
			continue
		}

		size := protowire.ConsumeFieldValue(number, wireType, data[position:])
		if size <= 0 {
			return "", nil, protowire.ParseError(size)
		}
		position += size
	}
	return title, metrics, nil
}

func parseDevinDimensionMetric(data []byte) (string, float32, bool, error) {
	var key string
	var metric float32
	var metricPresent bool
	for position := 0; position < len(data); {
		number, wireType, consumed := protowire.ConsumeTag(data[position:])
		if consumed <= 0 {
			return "", 0, false, protowire.ParseError(consumed)
		}
		position += consumed
		switch number {
		case 5:
			if wireType != protowire.BytesType {
				return "", 0, false, fmt.Errorf("metric key has wire type %d, want bytes", wireType)
			}
			value, size := protowire.ConsumeBytes(data[position:])
			if size <= 0 {
				return "", 0, false, protowire.ParseError(size)
			}
			position += size
			key = string(value)
		case 4:
			if wireType != protowire.BytesType {
				return "", 0, false, fmt.Errorf("metric value has wire type %d, want bytes", wireType)
			}
			value, size := protowire.ConsumeBytes(data[position:])
			if size <= 0 {
				return "", 0, false, protowire.ParseError(size)
			}
			position += size
			parsed, err := parseDevinDimensionValue(value)
			if err != nil {
				return "", 0, false, err
			}
			metric = parsed
			metricPresent = true
		default:
			size := protowire.ConsumeFieldValue(number, wireType, data[position:])
			if size <= 0 {
				return "", 0, false, protowire.ParseError(size)
			}
			position += size
		}
	}
	if key != "" && !metricPresent {
		return "", 0, false, fmt.Errorf("metric %q is missing field 4", key)
	}
	return key, metric, metricPresent, nil
}

func parseDevinDimensionValue(data []byte) (float32, error) {
	var metric float32
	var found bool
	for position := 0; position < len(data); {
		number, wireType, consumed := protowire.ConsumeTag(data[position:])
		if consumed <= 0 {
			return 0, protowire.ParseError(consumed)
		}
		position += consumed
		if number == 2 {
			if wireType != protowire.Fixed32Type {
				return 0, fmt.Errorf("dimension value has wire type %d, want fixed32", wireType)
			}
			value, size := protowire.ConsumeFixed32(data[position:])
			if size <= 0 {
				return 0, protowire.ParseError(size)
			}
			position += size
			metric = math.Float32frombits(value)
			found = true
			continue
		}

		size := protowire.ConsumeFieldValue(number, wireType, data[position:])
		if size <= 0 {
			return 0, protowire.ParseError(size)
		}
		position += size
	}
	if !found {
		return 0, fmt.Errorf("dimension value is missing field 2")
	}
	return metric, nil
}

func devinDimensionTokenCount(value float32) (int64, error) {
	if float32(math.Trunc(float64(value))) != value || value < 0 || math.IsInf(float64(value), 0) || math.IsNaN(float64(value)) {
		return 0, fmt.Errorf("invalid token count %v", value)
	}
	if float64(value) > float64(math.MaxInt64) {
		return 0, fmt.Errorf("token count %v exceeds int64", value)
	}
	return int64(value), nil
}
