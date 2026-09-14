package usage

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

const (
	maxEventMetadataBytes       = 64 << 10
	maxEventMetadataStringBytes = 512
	maxEventHeaderBytes         = 8 << 10
	maxEventHeaderNameBytes     = 256
	maxEventHeaderValueBytes    = 512
	maxEventHeaderNames         = 64
	maxEventHeaderValues        = 16
)

type eventMetadata struct {
	LatencyMs           int64                     `json:"latency_ms,omitempty"`
	RequestID           string                    `json:"request_id,omitempty"`
	TimeToFirstByteMs   int64                     `json:"time_to_first_byte_ms,omitempty"`
	TotalDurationMs     int64                     `json:"total_duration_ms,omitempty"`
	Completed           bool                      `json:"completed"`
	Generate            *bool                     `json:"generate,omitempty"`
	ClientIP            string                    `json:"client_ip,omitempty"`
	XForwardedFor       string                    `json:"x_forwarded_for,omitempty"`
	UserAgent           string                    `json:"user_agent,omitempty"`
	AccountingVersion   int                       `json:"accounting_version,omitempty"`
	TokenBreakdown      *coreusage.TokenBreakdown `json:"token_breakdown,omitempty"`
	ReasoningEffort     string                    `json:"reasoning_effort,omitempty"`
	ServiceTier         string                    `json:"service_tier,omitempty"`
	RequestServiceTier  string                    `json:"request_service_tier,omitempty"`
	ResponseServiceTier string                    `json:"response_service_tier,omitempty"`
	ExecutorType        string                    `json:"executor_type,omitempty"`
	Thinking            *Thinking                 `json:"thinking,omitempty"`
	ResponseHeaders     http.Header               `json:"response_headers,omitempty"`
	Truncated           bool                      `json:"truncated,omitempty"`
}

func validEventTokenBreakdown(d *FlatDetail) (*coreusage.TokenBreakdown, error) {
	if d == nil {
		return nil, nil
	}
	if d.TokenBreakdown.Valid() {
		breakdown := d.TokenBreakdown
		return &breakdown, nil
	}
	if d.AccountingVersion != 0 {
		return nil, errors.New("usage: invalid token breakdown metadata")
	}
	return nil, nil
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
	tokenBreakdown, err := validEventTokenBreakdown(d)
	if err != nil {
		return nil, err
	}
	accountingVersion := d.AccountingVersion
	if tokenBreakdown != nil {
		accountingVersion = coreusage.TokenAccountingSchemaVersion
	}
	generate := true
	if d.generateRecorded {
		generate = d.Generate
	}
	meta := eventMetadata{
		LatencyMs:           d.LatencyMs,
		RequestID:           strings.TrimSpace(d.RequestID),
		TimeToFirstByteMs:   d.TimeToFirstByteMs,
		TotalDurationMs:     d.TotalDurationMs,
		Completed:           d.Completed,
		Generate:            &generate,
		ClientIP:            strings.TrimSpace(d.ClientIP),
		XForwardedFor:       strings.TrimSpace(d.XForwardedFor),
		UserAgent:           strings.TrimSpace(d.UserAgent),
		AccountingVersion:   accountingVersion,
		TokenBreakdown:      tokenBreakdown,
		ReasoningEffort:     strings.TrimSpace(d.ReasoningEffort),
		ServiceTier:         requestServiceTier,
		RequestServiceTier:  requestServiceTier,
		ResponseServiceTier: strings.TrimSpace(d.ResponseServiceTier),
		ExecutorType:        strings.TrimSpace(d.ExecutorType),
		Thinking:            cloneThinking(d.Thinking),
		ResponseHeaders:     responseHeaders,
		Truncated:           d.MetadataTruncated,
	}
	boundEventMetadata(&meta)
	if meta.LatencyMs <= 0 && meta.RequestID == "" && meta.TimeToFirstByteMs <= 0 && meta.TotalDurationMs <= 0 && !meta.Completed && meta.Generate != nil && *meta.Generate && meta.ClientIP == "" && meta.XForwardedFor == "" && meta.UserAgent == "" && meta.AccountingVersion == 0 && meta.TokenBreakdown == nil && meta.ReasoningEffort == "" && meta.ServiceTier == "" && meta.ResponseServiceTier == "" && meta.ExecutorType == "" && meta.Thinking == nil && len(meta.ResponseHeaders) == 0 && !meta.Truncated {
		return nil, nil
	}
	raw, err := json.Marshal(meta)
	if err != nil {
		return nil, fmt.Errorf("encode event metadata: %w", err)
	}
	if len(raw) > maxEventMetadataBytes {
		meta.ResponseHeaders = nil
		meta.Truncated = true
		raw, err = json.Marshal(meta)
		if err != nil {
			return nil, fmt.Errorf("encode bounded event metadata: %w", err)
		}
	}
	if len(raw) > maxEventMetadataBytes {
		return nil, errors.New("usage: bounded event metadata exceeds size limit")
	}
	return string(raw), nil
}

func boundEventMetadata(meta *eventMetadata) {
	if meta == nil {
		return
	}
	for _, value := range []*string{
		&meta.RequestID,
		&meta.ClientIP,
		&meta.XForwardedFor,
		&meta.UserAgent,
		&meta.ReasoningEffort,
		&meta.ServiceTier,
		&meta.RequestServiceTier,
		&meta.ResponseServiceTier,
		&meta.ExecutorType,
	} {
		bounded, truncated := truncateUTF8Bytes(*value, maxEventMetadataStringBytes)
		*value = bounded
		meta.Truncated = meta.Truncated || truncated
	}
	if meta.Thinking != nil {
		for _, value := range []*string{
			&meta.Thinking.Intensity,
			&meta.Thinking.Mode,
			&meta.Thinking.Level,
		} {
			bounded, truncated := truncateUTF8Bytes(*value, maxEventMetadataStringBytes)
			*value = bounded
			meta.Truncated = meta.Truncated || truncated
		}
	}
	boundedHeaders, headersTruncated := boundResponseHeaders(meta.ResponseHeaders)
	meta.ResponseHeaders = boundedHeaders
	meta.Truncated = meta.Truncated || headersTruncated
}

func boundResponseHeaders(src http.Header) (http.Header, bool) {
	if len(src) == 0 {
		return nil, false
	}
	keys := make([]string, 0, len(src))
	for key := range src {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	dst := make(http.Header)
	remaining := maxEventHeaderBytes
	truncated := false
	for keyIndex, key := range keys {
		if keyIndex >= maxEventHeaderNames {
			truncated = true
			break
		}
		boundedKey, keyTruncated := truncateUTF8Bytes(key, maxEventHeaderNameBytes)
		truncated = truncated || keyTruncated
		values := src[key]
		clean := make([]string, 0, min(len(values), maxEventHeaderValues))
		for valueIndex, value := range values {
			if valueIndex >= maxEventHeaderValues {
				truncated = true
				break
			}
			boundedValue, valueTruncated := truncateUTF8Bytes(value, maxEventHeaderValueBytes)
			truncated = truncated || valueTruncated
			cost := len(boundedKey) + len(boundedValue)
			if cost > remaining {
				truncated = true
				break
			}
			remaining -= cost
			clean = append(clean, boundedValue)
		}
		if len(clean) > 0 {
			dst[boundedKey] = clean
		}
		if remaining == 0 {
			truncated = true
			break
		}
	}
	if len(dst) == 0 {
		return nil, truncated
	}
	return dst, truncated
}

func truncateUTF8Bytes(value string, maxBytes int) (string, bool) {
	if maxBytes < 0 || len(value) <= maxBytes {
		return value, false
	}
	end := maxBytes
	for end > 0 && !utf8.ValidString(value[:end]) {
		end--
	}
	return value[:end], true
}

func decodeEventMetadata(d *FlatDetail, raw sql.NullString) error {
	if d == nil {
		return nil
	}
	d.Generate = true
	d.generateRecorded = false
	if !raw.Valid || raw.String == "" {
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
	d.Generate = meta.Generate == nil || *meta.Generate
	d.generateRecorded = meta.Generate != nil
	d.ClientIP = strings.TrimSpace(meta.ClientIP)
	d.XForwardedFor = strings.TrimSpace(meta.XForwardedFor)
	d.UserAgent = strings.TrimSpace(meta.UserAgent)
	d.AccountingVersion = meta.AccountingVersion
	if meta.TokenBreakdown != nil {
		if !meta.TokenBreakdown.Valid() || meta.AccountingVersion != coreusage.TokenAccountingSchemaVersion {
			return errors.New("usage: invalid token breakdown metadata")
		}
		d.TokenBreakdown = *meta.TokenBreakdown
	} else if meta.AccountingVersion != 0 {
		return errors.New("usage: accounting version has no token breakdown")
	}
	d.ReasoningEffort = strings.TrimSpace(meta.ReasoningEffort)
	d.RequestServiceTier = strings.TrimSpace(meta.RequestServiceTier)
	if d.RequestServiceTier == "" {
		d.RequestServiceTier = strings.TrimSpace(meta.ServiceTier)
	}
	d.ServiceTier = d.RequestServiceTier
	d.ResponseServiceTier = strings.TrimSpace(meta.ResponseServiceTier)
	d.ExecutorType = strings.TrimSpace(meta.ExecutorType)
	d.MetadataTruncated = meta.Truncated
	d.Thinking = cloneThinking(meta.Thinking)
	d.ResponseHeaders = cloneHTTPHeader(meta.ResponseHeaders)
	return nil
}

func decodeEventTokenBreakdown(raw sql.NullString) (*coreusage.TokenBreakdown, error) {
	if !raw.Valid || raw.String == "" {
		return nil, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw.String), &fields); err != nil {
		return nil, nil
	}
	if _, hasVersion := fields["accounting_version"]; !hasVersion {
		if _, hasBreakdown := fields["token_breakdown"]; !hasBreakdown {
			return nil, nil
		}
	}
	var detail FlatDetail
	if err := decodeEventMetadata(&detail, raw); err != nil {
		return nil, err
	}
	return validEventTokenBreakdown(&detail)
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
