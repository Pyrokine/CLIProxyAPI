// Last compiled: 2026-05-07
// Author: pyro

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
// events tab. Frontend writes both `api_key` (masked, e.g. "sk-***-abc") and
// `raw_api_key` (full secret) plus `auth_index` since R-547 — the parser
// prefers raw_api_key when present so re-imports dedup correctly against the
// originals; falls back to the masked value for legacy CSVs (in which case
// fingerprint won't align with unmasked events).
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
		idx[strings.ToLower(strings.TrimSpace(col))] = i
	}
	get := func(row []string, name string) string {
		i, ok := idx[name]
		if !ok || i >= len(row) {
			return ""
		}
		return strings.TrimSpace(row[i])
	}
	parseInt := func(s string) int64 {
		if s == "" {
			return 0
		}
		n, _ := strconv.ParseInt(s, 10, 64)
		return n
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
		// R-547:优先 raw_api_key 列(R-547 后 frontend 导出的 CSV 含此列),
		// 缺失时退回 api_key(可能是 masked,fingerprint 一致性会打折)。
		apiKey := get(row, "raw_api_key")
		if apiKey == "" {
			apiKey = get(row, "api_key")
		}
		details = append(
			details, FlatDetail{
				Timestamp: ts,
				Model:     get(row, "model"),
				Source:    get(row, "source"),
				AuthIndex: get(row, "auth_index"),
				APIKey:    apiKey,
				Failed:    strings.EqualFold(get(row, "result"), "failed"),
				Tokens: tokenStats{
					InputTokens:     parseInt(get(row, "input_tokens")),
					OutputTokens:    parseInt(get(row, "output_tokens")),
					ReasoningTokens: parseInt(get(row, "reasoning_tokens")),
					CachedTokens:    parseInt(get(row, "cached_tokens")),
					TotalTokens:     parseInt(get(row, "total_tokens")),
				},
			},
		)
	}
	return details, nil
}

func csvReaderFor(data []byte) *csv.Reader {
	r := csv.NewReader(bytes.NewReader(data))
	r.FieldsPerRecord = -1 // tolerate mixed-width rows from hand-edited files
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
// Returns the matched parser type for diagnostics in the caller's logs.
func ParseAny(data []byte) ([]FlatDetail, string, error) {
	for _, p := range allParsers {
		if p.Detect(data) {
			details, err := p.Parse(data)
			if err != nil {
				return nil, fmt.Sprintf("%T", p), err
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
				out = append(
					out, FlatDetail{
						Timestamp: d.Timestamp,
						Model:     cleanModel,
						Source:    normaliseDimName(d.Source, ""),
						AuthIndex: normaliseDimName(d.AuthIndex, ""),
						APIKey:    cleanAPI,
						Tokens:    normaliseTokenStats(d.Tokens),
						Failed:    d.Failed,
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

func importDayRange(details []FlatDetail) (int64, int64, bool) {
	if len(details) == 0 {
		return 0, 0, false
	}
	earliest := details[0].Timestamp.UTC().Truncate(24 * time.Hour).UnixNano()
	latest := earliest
	for i := 1; i < len(details); i++ {
		dayNs := details[i].Timestamp.UTC().Truncate(24 * time.Hour).UnixNano()
		if dayNs < earliest {
			earliest = dayNs
		}
		if dayNs > latest {
			latest = dayNs
		}
	}
	return earliest, latest, true
}

// ImportBytes is the synchronous entry point used by HTTP handlers. It parses
// the payload, then imports every detail in a single transaction so the
// caller sees consistent counts on success.
func ImportBytes(ctx context.Context, store *Store, priceFn PriceFunc, data []byte) (ImportResult, error) {
	if store == nil {
		return ImportResult{}, errors.New("usage: import on nil store")
	}
	details, format, err := ParseAny(data)
	if err != nil {
		return ImportResult{}, err
	}
	earliestDayNs, latestDayNs, hasDayRange := importDayRange(details)
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
	const maxImportBytes = 100 << 20
	if info.Size() > maxImportBytes {
		return ImportResult{}, fmt.Errorf(
			"read %s: file too large (%d bytes > %d bytes limit)", path, info.Size(), maxImportBytes,
		)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ImportResult{}, fmt.Errorf("read %s: %w", path, err)
	}
	return ImportBytes(ctx, store, priceFn, data)
}

// importDetails persists a slice of FlatDetail in chunked transactions. We
// chunk because a single 60MB file can hold hundreds of thousands of records,
// and one giant tx would block writers on the WAL for too long.
func importDetails(ctx context.Context, store *Store, priceFn PriceFunc, details []FlatDetail) (
	added, skipped int,
	err error,
) {
	const chunkSize = 1000

	for start := 0; start < len(details); start += chunkSize {
		end := start + chunkSize
		if end > len(details) {
			end = len(details)
		}
		a, s, err := importChunk(ctx, store, priceFn, details[start:end])
		if err != nil {
			return added, skipped, err
		}
		added += a
		skipped += s
	}
	return added, skipped, nil
}

func importChunk(ctx context.Context, store *Store, priceFn PriceFunc, details []FlatDetail) (
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
	for i := range details {
		ok, err := persistEvent(ctx, tx, store, &details[i], priceFn)
		if err != nil {
			return added, skipped, fmt.Errorf("import event[%d]: %w", i, err)
		}
		if ok {
			added++
		} else {
			skipped++
		}
	}
	if err := tx.Commit(); err != nil {
		return added, skipped, fmt.Errorf("commit import tx: %w", err)
	}
	committed = true
	return added, skipped, nil
}
