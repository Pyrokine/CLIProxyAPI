package usage

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

// Parser describes one importable on-disk format. ParseAny tries Detect on
// each parser in priority order; the first match wins.
type Parser interface {
	Detect(data []byte) bool
	Parse(data []byte) ([]FlatDetail, error)
}

// flatDetailsParser handles the v2 backend payloads that share a {date,
// details:[FlatDetail]} envelope: today.json AND detail/YYYY-MM-DD.json.
// They differ only in optional saved_at, which we ignore.
type flatDetailsParser struct{}

type flatDetailsEnvelope struct {
	Version int          `json:"version"`
	Date    string       `json:"date"`
	Details []FlatDetail `json:"details"`
}

func (flatDetailsParser) Detect(data []byte) bool {
	var probe struct {
		Details json.RawMessage `json:"details"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return false
	}
	// json.RawMessage of `null` is the 4-byte literal — also count as miss.
	return len(probe.Details) > 0 && !bytes.Equal(probe.Details, []byte("null"))
}

func (flatDetailsParser) Parse(data []byte) ([]FlatDetail, error) {
	var env flatDetailsEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("parse flat details envelope: %w", err)
	}
	return env.Details, nil
}

// statisticsSnapshotParser handles every v1-shaped payload: usage-statistics.json,
// usage-archive-*.json, /usage/export, /usage/import (all wrap StatisticsSnapshot).
type statisticsSnapshotParser struct{}

type snapshotEnvelope struct {
	Version int                `json:"version"`
	Usage   StatisticsSnapshot `json:"usage"`
}

func (statisticsSnapshotParser) Detect(data []byte) bool {
	var probe struct {
		Usage struct {
			APIs json.RawMessage `json:"apis"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return false
	}
	return len(probe.Usage.APIs) > 0 && !bytes.Equal(probe.Usage.APIs, []byte("null"))
}

func (statisticsSnapshotParser) Parse(data []byte) ([]FlatDetail, error) {
	var env snapshotEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("parse snapshot envelope: %w", err)
	}
	return flattenSnapshot(env.Usage), nil
}

// flatArrayParser handles a bare JSON array of FlatDetail records, which is
// the simplest format the frontend "export to JSON" button could produce.
type flatArrayParser struct{}

func (flatArrayParser) Detect(data []byte) bool {
	trimmed := bytes.TrimLeft(data, " \t\r\n")
	return len(trimmed) > 0 && trimmed[0] == '['
}

func (flatArrayParser) Parse(data []byte) ([]FlatDetail, error) {
	var details []FlatDetail
	if err := json.Unmarshal(data, &details); err != nil {
		return nil, fmt.Errorf("parse flat array: %w", err)
	}
	return details, nil
}

// csvParser handles the events CSV the frontend produces from the request
// events tab. raw_api_key is preferred when an authorized export includes it.
// Masked api_key values are display labels rather than stable identities, so
// masked-only CSVs leave APIKey empty instead of creating false dimensions.
type csvParser struct{}

// csvRequiredHeaders is the minimum header set the parser needs to recover a
// FlatDetail. raw_api_key + auth_index are optional (legacy CSVs lack them).
var csvRequiredHeaders = []string{
	"timestamp", "model", "source", "api_key", "result",
	"input_tokens", "output_tokens", "total_tokens",
}

func (csvParser) Detect(data []byte) bool {
	// First non-empty line must contain every required header.
	r := csvReaderFor(data)
	header, err := r.Read()
	if err != nil {
		return false
	}
	have := make(map[string]struct{}, len(header))
	for _, col := range header {
		have[strings.ToLower(strings.TrimSpace(col))] = struct{}{}
	}
	for _, want := range csvRequiredHeaders {
		if _, ok := have[want]; !ok {
			return false
		}
	}
	return true
}

func (csvParser) Parse(data []byte) ([]FlatDetail, error) {
	r := csvReaderFor(data)
	header, err := r.Read()
	if err != nil {
		return nil, fmt.Errorf("parse csv header: %w", err)
	}
	idx := make(map[string]int, len(header))
	for i, col := range header {
		name := strings.ToLower(strings.TrimSpace(col))
		if name == "" {
			return nil, fmt.Errorf("parse csv header: empty column at position %d", i+1)
		}
		if _, exists := idx[name]; exists {
			return nil, fmt.Errorf("parse csv header: duplicate column %q", name)
		}
		idx[name] = i
	}
	get := func(row []string, name string) string {
		i, ok := idx[name]
		if !ok || i >= len(row) {
			return ""
		}
		return strings.TrimSpace(row[i])
	}
	parseInt := func(lineNo int, row []string, name string, required bool) (int64, error) {
		raw := get(row, name)
		if raw == "" {
			if required {
				return 0, fmt.Errorf("parse csv line %d %s: empty integer", lineNo, name)
			}
			return 0, nil
		}
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("parse csv line %d %s %q: %w", lineNo, name, raw, err)
		}
		if n < 0 {
			return 0, fmt.Errorf("parse csv line %d %s: negative value %d", lineNo, name, n)
		}
		return n, nil
	}
	parseThinking := func(lineNo int, row []string) (*Thinking, error) {
		intensity := get(row, "thinking_intensity")
		mode := get(row, "thinking_mode")
		level := get(row, "thinking_level")
		budgetText := get(row, "thinking_budget")
		var budget *int
		if budgetText != "" {
			n, err := strconv.Atoi(budgetText)
			if err != nil {
				return nil, fmt.Errorf("parse csv line %d thinking_budget %q: %w", lineNo, budgetText, err)
			}
			if n < 0 {
				return nil, fmt.Errorf("parse csv line %d thinking_budget: negative value %d", lineNo, n)
			}
			budget = &n
		}
		return cloneThinking(&Thinking{Intensity: intensity, Mode: mode, Level: level, Budget: budget}), nil
	}

	var details []FlatDetail
	for lineNo := 2; ; lineNo++ {
		row, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("parse csv line %d: %w", lineNo, err)
		}
		ts, err := time.Parse(time.RFC3339, get(row, "timestamp"))
		if err != nil {
			// Frontend writes ISO-8601 with offset; tolerate the date-only and
			// space-separated variants users hand-edit in spreadsheet apps.
			ts, err = time.Parse("2006-01-02T15:04:05", get(row, "timestamp"))
			if err != nil {
				return nil, fmt.Errorf("parse csv line %d timestamp %q: %w", lineNo, get(row, "timestamp"), err)
			}
		}
		apiKey := get(row, "raw_api_key")
		if apiKey == "" {
			candidate := get(row, "api_key")
			if !isMaskedAPIKey(candidate) {
				apiKey = candidate
			}
		}
		result := strings.ToLower(get(row, "result"))
		if result != "success" && result != "failed" {
			return nil, fmt.Errorf("parse csv line %d result %q: want success or failed", lineNo, result)
		}
		latencyMs, err := parseInt(lineNo, row, "latency_ms", false)
		if err != nil {
			return nil, err
		}
		inputTokens, err := parseInt(lineNo, row, "input_tokens", true)
		if err != nil {
			return nil, err
		}
		outputTokens, err := parseInt(lineNo, row, "output_tokens", true)
		if err != nil {
			return nil, err
		}
		reasoningTokens, err := parseInt(lineNo, row, "reasoning_tokens", false)
		if err != nil {
			return nil, err
		}
		cachedTokens, err := parseInt(lineNo, row, "cached_tokens", false)
		if err != nil {
			return nil, err
		}
		totalTokens, err := parseInt(lineNo, row, "total_tokens", true)
		if err != nil {
			return nil, err
		}
		thinking, err := parseThinking(lineNo, row)
		if err != nil {
			return nil, err
		}
		requestServiceTier := get(row, "request_service_tier")
		if requestServiceTier == "" {
			requestServiceTier = get(row, "service_tier")
		}
		details = append(
			details, FlatDetail{
				Timestamp:           ts,
				Model:               get(row, "model"),
				Source:              get(row, "source"),
				AuthIndex:           get(row, "auth_index"),
				Provider:            get(row, "provider"),
				APIKey:              apiKey,
				LatencyMs:           latencyMs,
				ReasoningEffort:     get(row, "reasoning_effort"),
				ServiceTier:         requestServiceTier,
				RequestServiceTier:  requestServiceTier,
				ResponseServiceTier: get(row, "response_service_tier"),
				ExecutorType:        get(row, "executor_type"),
				Thinking:            thinking,
				Failed:              result == "failed",
				Tokens: tokenStats{
					InputTokens:     inputTokens,
					OutputTokens:    outputTokens,
					ReasoningTokens: reasoningTokens,
					CachedTokens:    cachedTokens,
					TotalTokens:     totalTokens,
				},
			},
		)
	}
	return details, nil
}

func normalizeFlatDetailServiceTiers(detail *FlatDetail) {
	if detail == nil {
		return
	}
	detail.RequestServiceTier = strings.TrimSpace(detail.RequestServiceTier)
	if detail.RequestServiceTier == "" {
		detail.RequestServiceTier = strings.TrimSpace(detail.ServiceTier)
	}
	detail.ServiceTier = detail.RequestServiceTier
	detail.ResponseServiceTier = strings.TrimSpace(detail.ResponseServiceTier)
}

func isMaskedAPIKey(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	if value == "***" {
		return true
	}
	parts := strings.Split(value, "...")
	if len(parts) != 2 {
		return false
	}
	return (len(parts[0]) == 4 && len(parts[1]) == 4) ||
		(len(parts[0]) == 2 && len(parts[1]) == 2)
}

func csvReaderFor(data []byte) *csv.Reader {
	r := csv.NewReader(bytes.NewReader(data))
	r.TrimLeadingSpace = true
	return r
}

// allParsers is the priority-ordered detect chain. flatDetails comes before
// flatArray because a JSON array CAN be the "details" value of a backend
// envelope, and we want the envelope path to win when both match. csvParser
// is last because Detect on text data is more expensive than the JSON probes.
var allParsers = []Parser{
	flatDetailsParser{},
	statisticsSnapshotParser{},
	flatArrayParser{},
	csvParser{},
}

// ParseAny detects the format and returns the flattened FlatDetail slice.
// Encrypted backups require ParseAnyWithSecret; legacy formats remain importable
// without an encryption key.
func ParseAny(data []byte) ([]FlatDetail, string, error) {
	return ParseAnyWithSecret(data, "")
}

// ParseAnyWithSecret detects encrypted v3 backups before the legacy parser
// chain. The secret is ignored for formats that never contained encrypted keys.
func ParseAnyWithSecret(data []byte, secret string) ([]FlatDetail, string, error) {
	if IsUsageBackup(data) {
		details, err := ParseUsageBackup(data, secret)
		if err != nil {
			return nil, "usageBackupV3", err
		}
		return details, "usageBackupV3", nil
	}
	for _, p := range allParsers {
		if p.Detect(data) {
			details, err := p.Parse(data)
			if err != nil {
				return nil, fmt.Sprintf("%T", p), err
			}
			for index := range details {
				normalizeFlatDetailServiceTiers(&details[index])
			}
			return details, fmt.Sprintf("%T", p), nil
		}
	}
	return nil, "", errors.New("usage: no parser detected the input format")
}

// flattenSnapshot walks a v1 nested StatisticsSnapshot and emits one
// FlatDetail per request. The apiName key in StatisticsSnapshot.APIs is
// what v2 calls "API key" — the LoggerPlugin captured record.APIKey there.
// R-458/R-459:在出口对 model/source/api_key/auth_index 做规范化,旧 JSON
// 里的 "Unknown" / "UNKNOWN" / 空白前后缀等噪声被统一,避免在 dim_* 表
// 落成多个不同 id;最终 events 写入时 sqlite_writer.Submit 还会再过一次,
// 双保险。
func flattenSnapshot(snap StatisticsSnapshot) []FlatDetail {
	var out []FlatDetail
	for apiName, api := range snap.APIs {
		for modelName, model := range api.Models {
			cleanModel := normaliseDimName(modelName, "unknown")
			cleanAPI := normaliseDimName(apiName, "")
			for _, d := range model.Details {
				requestServiceTier := strings.TrimSpace(d.RequestServiceTier)
				if requestServiceTier == "" {
					requestServiceTier = strings.TrimSpace(d.ServiceTier)
				}
				out = append(
					out, FlatDetail{
						Timestamp:           d.Timestamp,
						Model:               cleanModel,
						Source:              normaliseDimName(d.Source, ""),
						AuthIndex:           normaliseDimName(d.AuthIndex, ""),
						APIKey:              cleanAPI,
						LatencyMs:           d.LatencyMs,
						ReasoningEffort:     strings.TrimSpace(d.ReasoningEffort),
						ServiceTier:         requestServiceTier,
						RequestServiceTier:  requestServiceTier,
						ResponseServiceTier: strings.TrimSpace(d.ResponseServiceTier),
						ExecutorType:        strings.TrimSpace(d.ExecutorType),
						Thinking:            cloneThinking(d.Thinking),
						ResponseHeaders:     cloneHTTPHeader(d.ResponseHeaders),
						Tokens:              d.Tokens,
						Failed:              d.Failed,
					},
				)
			}
		}
	}
	return out
}

// normaliseDimName trims whitespace, drops surrounding quotes, and folds the
// "(none)" sentinel + several Unknown variants down to a single canonical
// form. Empty inputs return the supplied default (callers pass "unknown" for
// model fields where empty would degrade analytics, "" for source / api_key
// where empty is a valid OAuth signal).
func normaliseDimName(raw, fallback string) string {
	s := strings.TrimSpace(raw)
	// Strip a single layer of surrounding quotes — old JSON exports occasionally
	// leak "model" written as `"foo"` due to a misencoded write.
	if len(s) >= 2 && (s[0] == '"' && s[len(s)-1] == '"') {
		s = strings.TrimSpace(s[1 : len(s)-1])
	}
	if s == "" {
		return fallback
	}
	lower := strings.ToLower(s)
	if lower == "unknown" || lower == "(unknown)" || lower == "n/a" || lower == "null" {
		return "unknown"
	}
	return s
}

// ErrInvalidImport marks payload parsing and validation failures that callers
// may report as client errors. Storage and maintenance errors remain distinct.
var ErrInvalidImport = errors.New("usage: invalid import payload")

// ImportResult mirrors RequestStatistics.MergeSnapshot's MergeResult so the
// HTTP handler can keep the same response shape.
type ImportResult struct {
	Added         int64
	Skipped       int64
	Format        string
	EarliestDayNs int64
	LatestDayNs   int64
	HasDayRange   bool
}

func importDayRange(details []FlatDetail) (int64, int64, bool, error) {
	if len(details) == 0 {
		return 0, 0, false, nil
	}
	var earliest, latest int64
	for i := range details {
		if _, err := validatedTokenStats(details[i].Tokens); err != nil {
			return 0, 0, false, fmt.Errorf("event[%d]: %w", i, err)
		}
		_, _, dayNs, err := unixNanoBucketStarts(details[i].Timestamp)
		if err != nil {
			return 0, 0, false, fmt.Errorf("event[%d] timestamp: %w", i, err)
		}
		if i == 0 || dayNs < earliest {
			earliest = dayNs
		}
		if i == 0 || dayNs > latest {
			latest = dayNs
		}
	}
	return earliest, latest, true, nil
}

// ImportBytes is the synchronous entry point for legacy, non-encrypted input.
func ImportBytes(ctx context.Context, store *Store, priceFn PriceFunc, data []byte) (ImportResult, error) {
	return ImportBytesWithSecret(ctx, store, priceFn, data, "")
}

// ImportBytesWithSecret parses encrypted backups and every legacy format, then
// imports all details in one transaction so the caller sees consistent counts.
func ImportBytesWithSecret(
	ctx context.Context,
	store *Store,
	priceFn PriceFunc,
	data []byte,
	secret string,
) (ImportResult, error) {
	if store == nil {
		return ImportResult{}, errors.New("usage: import on nil store")
	}
	details, format, err := ParseAnyWithSecret(data, secret)
	if err != nil {
		return ImportResult{}, fmt.Errorf("%w: %v", ErrInvalidImport, err)
	}
	preserveEncryptedBackup := format == "usageBackupV3"
	for i := range details {
		details[i].Source = NormalizeImportedCredentialSource(details[i].Source, preserveEncryptedBackup)
	}
	earliestDayNs, latestDayNs, hasDayRange, err := importDayRange(details)
	if err != nil {
		return ImportResult{Format: format}, fmt.Errorf("%w: %v", ErrInvalidImport, err)
	}
	added, skipped, err := importDetails(ctx, store, priceFn, details)
	if err != nil {
		return ImportResult{Format: format}, err
	}
	return ImportResult{
		Added:         int64(added),
		Skipped:       int64(skipped),
		Format:        format,
		EarliestDayNs: earliestDayNs,
		LatestDayNs:   latestDayNs,
		HasDayRange:   hasDayRange,
	}, nil
}

// ImportFile reads, parses and imports a single file. Used by migrate_legacy
// and could be exposed via a future CLI subcommand.
func ImportFile(ctx context.Context, store *Store, priceFn PriceFunc, path string) (ImportResult, error) {
	info, err := os.Stat(path)
	if err != nil {
		return ImportResult{}, fmt.Errorf("stat %s: %w", path, err)
	}
	if info.Size() > MaxPortableBackupBytes {
		return ImportResult{}, fmt.Errorf(
			"read %s: file too large (%d bytes > %d bytes limit)", path, info.Size(), MaxPortableBackupBytes,
		)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ImportResult{}, fmt.Errorf("read %s: %w", path, err)
	}
	return ImportBytes(ctx, store, priceFn, data)
}

// importDetails persists the entire payload in one transaction. Imports are
// management operations, and atomicity is more important than allowing live
// writers to interleave with a partially visible import.
func importDetails(ctx context.Context, store *Store, priceFn PriceFunc, details []FlatDetail) (
	added, skipped int,
	err error,
) {
	if len(details) == 0 {
		return 0, 0, nil
	}
	tx, err := store.DB().BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("begin import tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	dirtyDays := make(map[int64]struct{})
	for i := range details {
		ok, err := persistEvent(ctx, tx, store, &details[i], priceFn)
		if err != nil {
			return 0, 0, fmt.Errorf("import event[%d]: %w", i, err)
		}
		if ok {
			added += 1
			_, _, dayNs, bucketErr := unixNanoBucketStarts(details[i].Timestamp)
			if bucketErr != nil {
				return 0, 0, fmt.Errorf("import event[%d] timestamp: %w", i, bucketErr)
			}
			dirtyDays[dayNs] = struct{}{}
		} else {
			skipped += 1
		}
	}
	for dayNs := range dirtyDays {
		if err := store.rollupDayTx(ctx, tx, dayNs); err != nil {
			return 0, 0, fmt.Errorf(
				"rollup imported day %s: %w",
				time.Unix(0, dayNs).UTC().Format("2006-01-02"),
				err,
			)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("commit import tx: %w", err)
	}
	committed = true
	if err := store.warmDimensionCaches(ctx, details); err != nil {
		log.Warnf("usage: warm dimension caches after import: %v", err)
	}
	return added, skipped, nil
}
