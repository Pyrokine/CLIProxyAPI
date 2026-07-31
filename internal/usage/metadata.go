package usage

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
)

type eventMetadata struct {
	LatencyMs           int64       `json:"latency_ms,omitempty"`
	RequestID           string      `json:"request_id,omitempty"`
	TimeToFirstByteMs   int64       `json:"time_to_first_byte_ms,omitempty"`
	TotalDurationMs     int64       `json:"total_duration_ms,omitempty"`
	Completed           bool        `json:"completed"`
	ReasoningEffort     string      `json:"reasoning_effort,omitempty"`
	ServiceTier         string      `json:"service_tier,omitempty"`
	RequestServiceTier  string      `json:"request_service_tier,omitempty"`
	ResponseServiceTier string      `json:"response_service_tier,omitempty"`
	ExecutorType        string      `json:"executor_type,omitempty"`
	Thinking            *Thinking   `json:"thinking,omitempty"`
	ResponseHeaders     http.Header `json:"response_headers,omitempty"`
}

func encodeEventMetadata(d *FlatDetail) (any, error) {
	if d == nil {
		return nil, nil
	}
	requestServiceTier := strings.TrimSpace(d.RequestServiceTier)
	if requestServiceTier == "" {
		requestServiceTier = strings.TrimSpace(d.ServiceTier)
	}
	responseHeaders := sanitizeResponseHeaders(d.ResponseHeaders)
	if d.metadataTrusted {
		responseHeaders = cloneHTTPHeader(d.ResponseHeaders)
	}
	meta := eventMetadata{
		LatencyMs:           d.LatencyMs,
		RequestID:           strings.TrimSpace(d.RequestID),
		TimeToFirstByteMs:   d.TimeToFirstByteMs,
		TotalDurationMs:     d.TotalDurationMs,
		Completed:           d.Completed,
		ReasoningEffort:     strings.TrimSpace(d.ReasoningEffort),
		ServiceTier:         requestServiceTier,
		RequestServiceTier:  requestServiceTier,
		ResponseServiceTier: strings.TrimSpace(d.ResponseServiceTier),
		ExecutorType:        strings.TrimSpace(d.ExecutorType),
		Thinking:            cloneThinking(d.Thinking),
		ResponseHeaders:     responseHeaders,
	}
	if meta.LatencyMs <= 0 && meta.RequestID == "" && meta.TimeToFirstByteMs <= 0 && meta.TotalDurationMs <= 0 && !meta.Completed && meta.ReasoningEffort == "" && meta.ServiceTier == "" && meta.ResponseServiceTier == "" && meta.ExecutorType == "" && meta.Thinking == nil && len(meta.ResponseHeaders) == 0 {
		return nil, nil
	}
	raw, err := json.Marshal(meta)
	if err != nil {
		return nil, fmt.Errorf("encode event metadata: %w", err)
	}
	return string(raw), nil
}

func decodeEventMetadata(d *FlatDetail, raw sql.NullString) error {
	if d == nil || !raw.Valid || raw.String == "" {
		return nil
	}
	d.MetadataRecorded = true
	d.metadataTrusted = true
	var meta eventMetadata
	if err := json.Unmarshal([]byte(raw.String), &meta); err != nil {
		return fmt.Errorf("decode event metadata: %w", err)
	}
	d.LatencyMs = meta.LatencyMs
	d.RequestID = strings.TrimSpace(meta.RequestID)
	d.TimeToFirstByteMs = meta.TimeToFirstByteMs
	d.TotalDurationMs = meta.TotalDurationMs
	d.Completed = meta.Completed
	d.ReasoningEffort = strings.TrimSpace(meta.ReasoningEffort)
	d.RequestServiceTier = strings.TrimSpace(meta.RequestServiceTier)
	if d.RequestServiceTier == "" {
		d.RequestServiceTier = strings.TrimSpace(meta.ServiceTier)
	}
	d.ServiceTier = d.RequestServiceTier
	d.ResponseServiceTier = strings.TrimSpace(meta.ResponseServiceTier)
	d.ExecutorType = strings.TrimSpace(meta.ExecutorType)
	d.Thinking = cloneThinking(meta.Thinking)
	d.ResponseHeaders = cloneHTTPHeader(meta.ResponseHeaders)
	return nil
}

func cloneThinking(src *Thinking) *Thinking {
	if src == nil {
		return nil
	}
	dst := *src
	if src.Budget != nil {
		dst.Budget = new(int)
		*dst.Budget = *src.Budget
	}
	if dst.Intensity == "" && dst.Mode == "" && dst.Level == "" && dst.Budget == nil {
		return nil
	}
	return &dst
}

func validateSanitizedResponseHeaders(src http.Header) error {
	for key, values := range src {
		if strings.EqualFold(key, "Set-Cookie") {
			return errors.New("usage: backup response headers contain Set-Cookie")
		}
		for _, value := range values {
			if !sensitiveResponseHeader(key) {
				continue
			}
			credential := strings.TrimSpace(value)
			if strings.Contains(strings.ToLower(strings.TrimSpace(key)), "authorization") {
				parts := strings.SplitN(credential, " ", 2)
				if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" {
					return errors.New("usage: backup response headers contain an unredacted credential")
				}
				credential = strings.TrimSpace(parts[1])
			}
			if !maskedCredentialValue(credential) {
				return errors.New("usage: backup response headers contain an unredacted credential")
			}
		}
	}
	return nil
}

func sensitiveResponseHeader(key string) bool {
	lowerKey := strings.ToLower(strings.TrimSpace(key))
	return strings.Contains(lowerKey, "authorization") ||
		util.IsSensitiveFieldName(lowerKey) ||
		strings.Contains(lowerKey, "token") ||
		strings.Contains(lowerKey, "secret") ||
		strings.Contains(lowerKey, "cookie")
}

func maskedCredentialValue(value string) bool {
	if value == "***" || value == "****" {
		return true
	}
	runes := []rune(value)
	if len(runes) == 11 && string(runes[4:7]) == "..." {
		return true
	}
	return len(runes) == 12 && string(runes[4:8]) == "...."
}

func sanitizeResponseHeaders(src http.Header) http.Header {
	if len(src) == 0 {
		return nil
	}
	dst := make(http.Header, len(src))
	for key, values := range src {
		if strings.EqualFold(key, "Set-Cookie") {
			continue
		}
		clean := make([]string, 0, len(values))
		for _, value := range values {
			clean = append(clean, util.MaskSensitiveHeaderValue(key, value))
		}
		if len(clean) > 0 {
			dst[key] = clean
		}
	}
	if len(dst) == 0 {
		return nil
	}
	return dst
}

func cloneHTTPHeader(src http.Header) http.Header {
	if len(src) == 0 {
		return nil
	}
	dst := make(http.Header, len(src))
	for key, values := range src {
		dst[key] = append([]string(nil), values...)
	}
	return dst
}
