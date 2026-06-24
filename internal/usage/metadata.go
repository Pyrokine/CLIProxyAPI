package usage

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
)

type eventMetadata struct {
	LatencyMs         int64       `json:"latency_ms,omitempty"`
	RequestID         string      `json:"request_id,omitempty"`
	TimeToFirstByteMs int64       `json:"time_to_first_byte_ms,omitempty"`
	TotalDurationMs   int64       `json:"total_duration_ms,omitempty"`
	Completed         bool        `json:"completed"`
	ReasoningEffort   string      `json:"reasoning_effort,omitempty"`
	ServiceTier       string      `json:"service_tier,omitempty"`
	ExecutorType      string      `json:"executor_type,omitempty"`
	Thinking          *Thinking   `json:"thinking,omitempty"`
	ResponseHeaders   http.Header `json:"response_headers,omitempty"`
}

func encodeEventMetadata(d *FlatDetail) (any, error) {
	if d == nil {
		return nil, nil
	}
	meta := eventMetadata{
		LatencyMs:         d.LatencyMs,
		RequestID:         strings.TrimSpace(d.RequestID),
		TimeToFirstByteMs: d.TimeToFirstByteMs,
		TotalDurationMs:   d.TotalDurationMs,
		Completed:         d.Completed,
		ReasoningEffort:   strings.TrimSpace(d.ReasoningEffort),
		ServiceTier:       strings.TrimSpace(d.ServiceTier),
		ExecutorType:      strings.TrimSpace(d.ExecutorType),
		Thinking:          cloneThinking(d.Thinking),
		ResponseHeaders:   sanitizeResponseHeaders(d.ResponseHeaders),
	}
	if meta.LatencyMs <= 0 && meta.RequestID == "" && meta.TimeToFirstByteMs <= 0 && meta.TotalDurationMs <= 0 && !meta.Completed && meta.ReasoningEffort == "" && meta.ServiceTier == "" && meta.ExecutorType == "" && meta.Thinking == nil && len(meta.ResponseHeaders) == 0 {
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
	d.ServiceTier = strings.TrimSpace(meta.ServiceTier)
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
