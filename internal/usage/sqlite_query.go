// Last compiled: 2026-05-07
// Author: pyro

package usage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	// dayBucketThreshold splits hour_bucket vs day_bucket. Beyond ~90 days the
	// hour grid bloats (one year = 8760 buckets per dimension) and slows
	// queries while adding zero usable detail at typical chart resolutions.
	dayBucketThreshold = 90 * 24 * time.Hour

	nsPerDay  = int64(24 * 3600 * 1_000_000_000)
	nsPerHour = int64(3600 * 1_000_000_000)

	dayRollupWatermarkKey = "day_bucket_rollup_watermark_ns"

	// costMicroPerUSD converts cost_micro back to USD for response payloads.
	costMicroPerUSD = 1_000_000.0
)

// resolvedFilter is the SQL-ready form of one EventFilters dimension.
//
// active indicates the user supplied a non-empty filter string.
// matchedAny tracks whether at least one supplied name was found in the dim
// table — when active && !matchedAny the query MUST return empty rather than
// silently degrading to "all rows".
//
// includesNone is the "(none)" sentinel: by_api_key surfaces a "(none)"
// bucket for requests that have no API key (OAuth-style flows), and the
// FilterBar lets users tick that bucket. When the box is ticked the filter
// must resolve to "api_key_id IS NULL", which a regular IN-list cannot
// express because SQL's NULL semantics make `col IN (NULL)` always false.
type resolvedFilter struct {
	active       bool
	matchedAny   bool
	ids          []int64
	includesNone bool
}

// noneSentinel is the magic filter string that maps to SQL NULL. The
// frontend produces it for the "(none)" bucket in by_api_key — see the
// LEFT JOIN COALESCE clause in queryByAPIKey.
const noneSentinel = "(none)"

// resolvedCredentialFilter is the credential-dimension counterpart that also
// carries the provider scope. v2 filter strings can be either bare "source"
// (matches the source under any provider — used by legacy frontends and by
// rows where provider is NULL) or "provider:source" (matches the source only
// under that provider — used by the v2 FilterBar dropdown to disambiguate
// the same OAuth email across claude/codex/gemini).
type resolvedCredentialFilter struct {
	active     bool
	matchedAny bool
	pairs      []credentialPair
}

// credentialPair represents one entry in a credential filter. providerID
// being NullInt64{Valid:false} means "any provider" — i.e. legacy bare-source
// tokens that should match every provider speaking through that credential.
type credentialPair struct {
	credentialID int64
	providerID   sql.NullInt64
}

// resolveNamesToIDs translates a comma-separated user filter into a slice of
// dim_* row ids. Names that don't exist are dropped (still recorded as
// "active without match" via matchedAny). The "(none)" sentinel is special:
// it resolves to "this dimension is NULL" without touching the dim table,
// so by_api_key's (none) bucket can be selected/excluded as a real filter
// value.
func resolveNamesToIDs(ctx context.Context, db *sql.DB, table, raw string) (resolvedFilter, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return resolvedFilter{}, nil
	}
	rf := resolvedFilter{active: true}
	seen := make(map[string]struct{})
	for _, part := range strings.Split(raw, ",") {
		name := strings.TrimSpace(part)
		if name == "" {
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}

		if name == noneSentinel {
			rf.includesNone = true
			rf.matchedAny = true
			continue
		}

		var id int64
		err := db.QueryRowContext(ctx, "SELECT id FROM "+table+" WHERE name = ?", name).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return resolvedFilter{}, fmt.Errorf("resolve %s.name=%q: %w", table, name, err)
		}
		rf.ids = append(rf.ids, id)
		rf.matchedAny = true
	}
	return rf, nil
}

// resolveCredentialFilter parses the credential filter string into a list of
// (provider, credential) id pairs. The string is comma-separated; each token
// is either "source" (bare, matches any provider — preserves the v1 contract
// for un-redeployed frontends and for legacy rows where provider_id is NULL)
// or "provider:source" (precise, matches only that provider's view of the
// credential).
//
// Tokens that fail to resolve in dim_credential or dim_provider are dropped
// silently — same fail-closed contract as resolveNamesToIDs: when a filter
// is active but no token matched, the caller must short-circuit to empty
// rather than degrade to "all rows".
func resolveCredentialFilter(ctx context.Context, db *sql.DB, raw string) (resolvedCredentialFilter, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return resolvedCredentialFilter{}, nil
	}
	rf := resolvedCredentialFilter{active: true}
	seen := make(map[string]struct{})
	for _, part := range strings.Split(raw, ",") {
		token := strings.TrimSpace(part)
		if token == "" {
			continue
		}
		if _, dup := seen[token]; dup {
			continue
		}
		seen[token] = struct{}{}

		providerName, credName := splitCredentialToken(token)

		var credID int64
		err := db.QueryRowContext(ctx, "SELECT id FROM dim_credential WHERE name = ?", credName).Scan(&credID)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return resolvedCredentialFilter{}, fmt.Errorf("resolve credential %q: %w", credName, err)
		}

		pair := credentialPair{credentialID: credID}
		if providerName != "" {
			var providerID int64
			err := db.QueryRowContext(ctx, "SELECT id FROM dim_provider WHERE name = ?", providerName).Scan(&providerID)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return resolvedCredentialFilter{}, fmt.Errorf("resolve provider %q: %w", providerName, err)
			}
			pair.providerID = sql.NullInt64{Int64: providerID, Valid: true}
		}
		rf.pairs = append(rf.pairs, pair)
		rf.matchedAny = true
	}
	return rf, nil
}

// splitCredentialToken pulls the optional "provider:" prefix off a credential
// filter token. The first colon is the separator; emails (the typical source
// value) never contain a colon, so a bare-source token like "user@host.com"
// stays in the credName branch. The provider half must be a known short name
// (claude, codex, gemini, ...) whose presence triggers the disambiguation,
// and we keep the test cheap by lower-bounding the remaining "source" half
// at one character so accidental "::source" or "provider:" inputs degrade
// gracefully to "match nothing".
func splitCredentialToken(token string) (provider, credential string) {
	idx := strings.Index(token, ":")
	if idx <= 0 || idx >= len(token)-1 {
		return "", token
	}
	return token[:idx], token[idx+1:]
}

// inClause builds an " AND col IN (?,?)" fragment plus its args. An active
// filter with zero matched ids returns "AND 1=0" so callers fail closed; an
// inactive filter contributes nothing.
//
// "(none)" support: when the filter includes the sentinel, the clause OR-s
// `col IS NULL` against the IN-list. SQL NULL semantics force this OR form:
// `col IN (NULL, 1, 2)` is always FALSE for NULL rows, so the only way to
// match NULLs alongside concrete ids is `col IS NULL OR col IN (...)`.
func inClause(col string, rf resolvedFilter) (string, []any) {
	if !rf.active {
		return "", nil
	}
	if !rf.includesNone && len(rf.ids) == 0 {
		return " AND 1=0", nil
	}
	if rf.includesNone && len(rf.ids) == 0 {
		return " AND " + col + " IS NULL", nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(rf.ids)), ",")
	args := make([]any, len(rf.ids))
	for i, id := range rf.ids {
		args[i] = id
	}
	if rf.includesNone {
		return " AND (" + col + " IS NULL OR " + col + " IN (" + placeholders + "))", args
	}
	return " AND " + col + " IN (" + placeholders + ")", args
}

// credentialClause builds the OR-of-pairs SQL fragment for the credential
// filter. credCol/provCol are the qualified column names (e.g. "b.credential_id",
// "b.provider_id") so the same generator works in JOINed and bare-table queries.
//
// Each pair becomes either:
//
//	(credential_id = ? AND provider_id = ?)   — precise (provider:source)
//	credential_id = ?                          — wildcard (bare source)
//
// joined with OR. An active filter with zero matched pairs returns "AND 1=0"
// so callers fail closed.
func credentialClause(credCol, provCol string, rf resolvedCredentialFilter) (string, []any) {
	if !rf.active {
		return "", nil
	}
	if len(rf.pairs) == 0 {
		return " AND 1=0", nil
	}
	var ors []string
	args := make([]any, 0, len(rf.pairs)*2)
	for _, p := range rf.pairs {
		if p.providerID.Valid {
			ors = append(ors, "("+credCol+" = ? AND "+provCol+" = ?)")
			args = append(args, p.credentialID, p.providerID.Int64)
		} else {
			ors = append(ors, credCol+" = ?")
			args = append(args, p.credentialID)
		}
	}
	return " AND (" + strings.Join(ors, " OR ") + ")", args
}

// floorToHour rounds t down to the start of its hour in UTC. Zero time is
// returned unchanged so callers can pass time.Time{} as a "no lower bound"
// sentinel through buildFilterClause without it drifting to 1970.
func floorToHour(t time.Time) time.Time {
	if t.IsZero() {
		return t
	}
	return time.Unix(0, (t.UTC().UnixNano()/nsPerHour)*nsPerHour).UTC()
}

// buildFilterClause assembles the full " WHERE ts ≥ ? AND ts ≤ ? AND col IN (...) ..."
// fragment shared between hour_bucket queries and events-table queries. tsCol
// is the timestamp column name in the target table (bucket_ts_ns or ts_ns).
// prefix is "" for unaliased queries, "b." for ones that join the bucket
// table under the b alias. modelF/apiKeyF use the simple IN-list filter;
// credF carries the (provider, source) pair structure introduced in v2.
func buildFilterClause(
	from, to time.Time,
	tsCol, prefix string,
	modelF, apiKeyF resolvedFilter,
	credF resolvedCredentialFilter,
) (string, []any) {
	var clauses []string
	var args []any
	if !from.IsZero() {
		clauses = append(clauses, tsCol+" >= ?")
		args = append(args, from.UTC().UnixNano())
	}
	if !to.IsZero() {
		clauses = append(clauses, tsCol+" <= ?")
		args = append(args, to.UTC().UnixNano())
	}
	if part, a := inClause(prefix+"model_id", modelF); part != "" {
		clauses = append(clauses, strings.TrimPrefix(part, " AND "))
		args = append(args, a...)
	}
	if part, a := credentialClause(prefix+"credential_id", prefix+"provider_id", credF); part != "" {
		clauses = append(clauses, strings.TrimPrefix(part, " AND "))
		args = append(args, a...)
	}
	if part, a := inClause(prefix+"api_key_id", apiKeyF); part != "" {
		clauses = append(clauses, strings.TrimPrefix(part, " AND "))
		args = append(args, a...)
	}
	if len(clauses) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(clauses, " AND "), args
}

// Query implements /usage/summary against the bucket tables. Routing rule:
// span ≤ 90d → hour_bucket; span > 90d → day_bucket. granularity controls
// time-series granularity but never falls back to scanning events.
func (s *Store) Query(
	ctx context.Context,
	from, to time.Time,
	filters EventFilters,
	granularity string,
	includeGroups bool,
) (QueryResult, error) {
	if s == nil || s.db == nil {
		return QueryResult{}, fmt.Errorf("usage: query on nil store")
	}
	if !from.Before(to) {
		return emptyQueryResult(), nil
	}

	useDayBucket := to.Sub(from) > dayBucketThreshold
	table := "hour_bucket"
	if useDayBucket {
		table = "day_bucket"
	}

	modelF, err := resolveNamesToIDs(ctx, s.db, "dim_model", filters.Model)
	if err != nil {
		return QueryResult{}, err
	}
	credF, err := resolveCredentialFilter(ctx, s.db, filters.Source)
	if err != nil {
		return QueryResult{}, err
	}
	apiKeyF, err := resolveNamesToIDs(ctx, s.db, "dim_api_key", filters.APIKey)
	if err != nil {
		return QueryResult{}, err
	}

	// 0-match short-circuit: a user-supplied filter must NEVER match more than
	// the user asked for.
	if (modelF.active && !modelF.matchedAny) ||
		(credF.active && !credF.matchedAny) ||
		(apiKeyF.active && !apiKeyF.matchedAny) {
		return emptyQueryResult(), nil
	}

	// Bucket rows are keyed by their start instant (hour/day boundary). A
	// non-aligned from like 10:15 would otherwise exclude the 10:00 bucket
	// entirely, dropping 10:15-10:59 events that actually live in it. Floor
	// both ends to the bucket grain so any bucket overlapping [from, to] is
	// included. The trade-off (a sliver of out-of-window events at the edges)
	// is inherent to bucket aggregation and far less surprising than silent
	// undercounting.
	grainNs := nsPerHour
	if useDayBucket {
		grainNs = nsPerDay
	}
	fromNs := (from.UTC().UnixNano() / grainNs) * grainNs
	toNs := (to.UTC().UnixNano() / grainNs) * grainNs

	timeExpr := "bucket_ts_ns"
	if !useDayBucket && granularity == "daily" {
		timeExpr = fmt.Sprintf("(bucket_ts_ns / %d) * %d", nsPerDay, nsPerDay)
	}

	result := emptyQueryResult()
	if err := s.queryTotals(ctx, table, fromNs, toNs, modelF, credF, apiKeyF, &result); err != nil {
		return QueryResult{}, err
	}
	if err := s.queryByModel(ctx, table, fromNs, toNs, modelF, credF, apiKeyF, &result); err != nil {
		return QueryResult{}, err
	}
	if err := s.queryByCredential(ctx, table, fromNs, toNs, modelF, credF, apiKeyF, &result); err != nil {
		return QueryResult{}, err
	}
	if err := s.queryByAPIKey(ctx, table, fromNs, toNs, modelF, credF, apiKeyF, &result); err != nil {
		return QueryResult{}, err
	}
	if err := s.queryTimeSeries(ctx, table, fromNs, toNs, modelF, credF, apiKeyF, timeExpr, &result); err != nil {
		return QueryResult{}, err
	}
	if includeGroups {
		if err := s.queryTimeSeriesByModel(
			ctx, table, fromNs, toNs, modelF, credF, apiKeyF, timeExpr, &result,
		); err != nil {
			return QueryResult{}, err
		}
		if err := s.queryTimeSeriesByCredential(
			ctx, table, fromNs, toNs, modelF, credF, apiKeyF, timeExpr, &result,
		); err != nil {
			return QueryResult{}, err
		}
		if err := s.queryTimeSeriesByAPIKey(
			ctx, table, fromNs, toNs, modelF, credF, apiKeyF, timeExpr, &result,
		); err != nil {
			return QueryResult{}, err
		}
	}

	// R-443:把 [fromNs, toNs] 区间内每个桶都填齐,缺数据的桶补零值,这样
	// 前端在"选 30 天但只有 7 天数据"的情况下能渲染成连续的 0 曲线而不是
	// 断线连接到首个非零点。step 取决于 useDayBucket / granularity:
	// hour_bucket+default → 1h;hour_bucket+daily → 1d;day_bucket → 1d。
	stepNs := nsPerHour
	if useDayBucket || granularity == "daily" {
		stepNs = nsPerDay
	}
	padTimeSeries(&result, fromNs, toNs, stepNs)

	return result, nil
}

// padTimeSeries 把 from/to 内的所有桶补齐为零值;TimeSeries 与所有
// TimeSeriesBy* map 都会被处理。零值桶用 timePoint{Time: bucket-start}
// 填充,前端依据 Time 字段对齐 X 轴。
func padTimeSeries(result *QueryResult, fromNs, toNs, stepNs int64) {
	if result == nil || stepNs <= 0 || fromNs > toNs {
		return
	}
	fromNs = (fromNs / stepNs) * stepNs
	toNs = (toNs / stepNs) * stepNs

	pad := func(series []timePoint) []timePoint {
		existing := make(map[int64]timePoint, len(series))
		for _, p := range series {
			t, err := time.Parse(time.RFC3339, p.Time)
			if err != nil {
				continue
			}
			existing[t.UTC().UnixNano()] = p
		}
		bucketCount := (toNs-fromNs)/stepNs + 1
		out := make([]timePoint, 0, bucketCount)
		for ts := fromNs; ts <= toNs; ts += stepNs {
			if p, ok := existing[ts]; ok {
				out = append(out, p)
				continue
			}
			out = append(out, timePoint{Time: time.Unix(0, ts).UTC().Format(time.RFC3339)})
		}
		return out
	}

	result.TimeSeries = pad(result.TimeSeries)
	for k, v := range result.TimeSeriesByModel {
		result.TimeSeriesByModel[k] = pad(v)
	}
	for k, v := range result.TimeSeriesByCredential {
		result.TimeSeriesByCredential[k] = pad(v)
	}
	for k, v := range result.TimeSeriesByAPIKey {
		result.TimeSeriesByAPIKey[k] = pad(v)
	}
}

// emptyQueryResult mimics summary.go's "make-all-maps" initialisation so the
// JSON response never contains nil maps, which would serialise as `null` and
// confuse the frontend's `Object.keys(...)` checks.
func emptyQueryResult() QueryResult {
	return QueryResult{
		ByModel:                make(map[string]*summaryModelStats),
		ByCredential:           make(map[string]*summaryCredentialStats),
		ByAPIKey:               make(map[string]*summaryAPIKeyStats),
		TimeSeriesByModel:      make(map[string][]timePoint),
		TimeSeriesByCredential: make(map[string][]timePoint),
		TimeSeriesByAPIKey:     make(map[string][]timePoint),
	}
}

// composeFilterSQL returns the WHERE-tail "AND ..." string and accumulated args
// for a typical bucket query (model / credential / api_key). prefix is "" for
// unaliased queries (queryTotals, queryTimeSeries on hour_bucket directly) or
// "b." for JOINed queries that alias the bucket table to b.
func composeFilterSQL(
	modelF resolvedFilter,
	credF resolvedCredentialFilter,
	apiKeyF resolvedFilter,
	prefix string,
) (string, []any) {
	var sb strings.Builder
	args := make([]any, 0, len(modelF.ids)+len(credF.pairs)*2+len(apiKeyF.ids))
	if part, a := inClause(prefix+"model_id", modelF); part != "" {
		sb.WriteString(part)
		args = append(args, a...)
	}
	if part, a := credentialClause(prefix+"credential_id", prefix+"provider_id", credF); part != "" {
		sb.WriteString(part)
		args = append(args, a...)
	}
	if part, a := inClause(prefix+"api_key_id", apiKeyF); part != "" {
		sb.WriteString(part)
		args = append(args, a...)
	}
	return sb.String(), args
}

func (s *Store) queryTotals(
	ctx context.Context,
	table string,
	fromNs, toNs int64,
	modelF resolvedFilter, credF resolvedCredentialFilter, apiKeyF resolvedFilter,
	result *QueryResult,
) error {
	filterSQL, filterArgs := composeFilterSQL(modelF, credF, apiKeyF, "")

	q := "SELECT " +
		"COALESCE(SUM(requests),0), COALESCE(SUM(success),0), COALESCE(SUM(failure),0), " +
		"COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0), COALESCE(SUM(reasoning_tokens),0), " +
		"COALESCE(SUM(cached_tokens),0), COALESCE(SUM(total_tokens),0), COALESCE(SUM(cost_micro),0) " +
		"FROM " + table + " WHERE bucket_ts_ns >= ? AND bucket_ts_ns <= ?" + filterSQL

	args := append([]any{fromNs, toNs}, filterArgs...)
	row := s.db.QueryRowContext(ctx, q, args...)

	var totals summaryTotals
	var costMicro int64
	if err := row.Scan(
		&totals.Requests, &totals.Success, &totals.Failure,
		&totals.Tokens.InputTokens, &totals.Tokens.OutputTokens, &totals.Tokens.ReasoningTokens,
		&totals.Tokens.CachedTokens, &totals.Tokens.TotalTokens,
		&costMicro,
	); err != nil {
		return fmt.Errorf("queryTotals: %w", err)
	}
	totals.Cost = float64(costMicro) / costMicroPerUSD
	result.Totals = totals
	return nil
}

func (s *Store) queryByModel(
	ctx context.Context,
	table string,
	fromNs, toNs int64,
	modelF resolvedFilter, credF resolvedCredentialFilter, apiKeyF resolvedFilter,
	result *QueryResult,
) error {
	filterSQL, filterArgs := composeFilterSQL(modelF, credF, apiKeyF, "b.")

	q := "SELECT m.name, " +
		"SUM(b.requests), SUM(b.success), SUM(b.failure), " +
		"SUM(b.input_tokens), SUM(b.output_tokens), SUM(b.reasoning_tokens), SUM(b.cached_tokens), SUM(b.total_tokens), " +
		"SUM(b.cost_micro) " +
		"FROM " + table + " b JOIN dim_model m ON m.id = b.model_id " +
		"WHERE b.bucket_ts_ns >= ? AND b.bucket_ts_ns <= ?" + filterSQL +
		" GROUP BY m.id, m.name"

	args := append([]any{fromNs, toNs}, filterArgs...)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("queryByModel: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var name string
		var ms summaryModelStats
		var costMicro int64
		if err := rows.Scan(
			&name,
			&ms.Requests, &ms.Success, &ms.Failure,
			&ms.Tokens.InputTokens, &ms.Tokens.OutputTokens, &ms.Tokens.ReasoningTokens,
			&ms.Tokens.CachedTokens, &ms.Tokens.TotalTokens,
			&costMicro,
		); err != nil {
			return fmt.Errorf("queryByModel scan: %w", err)
		}
		ms.Cost = float64(costMicro) / costMicroPerUSD
		result.ByModel[name] = &ms
	}
	return rows.Err()
}

func (s *Store) queryByCredential(
	ctx context.Context,
	table string,
	fromNs, toNs int64,
	modelF resolvedFilter, credF resolvedCredentialFilter, apiKeyF resolvedFilter,
	result *QueryResult,
) error {
	filterSQL, filterArgs := composeFilterSQL(modelF, credF, apiKeyF, "b.")

	// (provider, credential) is the v2 dedup grouping: same OAuth email under
	// claude vs codex used to collapse onto one row in dim_credential, hiding
	// the upstream-vendor breakdown the dashboard now wants to surface. LEFT
	// JOIN on dim_provider keeps legacy rows (provider_id IS NULL) in the
	// result with provider name "".
	q := "SELECT c.name, COALESCE(p.name, '') AS provider_name, SUM(b.success), SUM(b.failure) " +
		"FROM " + table + " b JOIN dim_credential c ON c.id = b.credential_id " +
		"LEFT JOIN dim_provider p ON p.id = b.provider_id " +
		"WHERE b.bucket_ts_ns >= ? AND b.bucket_ts_ns <= ? AND b.credential_id IS NOT NULL" + filterSQL +
		" GROUP BY c.id, c.name, p.id, provider_name"

	args := append([]any{fromNs, toNs}, filterArgs...)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("queryByCredential: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var credName, providerName string
		var cs summaryCredentialStats
		if err := rows.Scan(&credName, &providerName, &cs.Success, &cs.Failure); err != nil {
			return fmt.Errorf("queryByCredential scan: %w", err)
		}
		cs.Source = credName
		cs.Provider = providerName
		result.ByCredential[credentialMapKey(providerName, credName)] = &cs
	}
	return rows.Err()
}

// credentialMapKey derives the JSON map key for the by_credential dimension.
// When the row has a known provider the key is "provider:source" so the same
// email under claude and codex remain distinct entries. Legacy rows (provider
// blank because the v1/v2 JSON imports never carried it) keep the bare source
// as their key — that path preserves alias-dictionary lookups for any frontend
// build that hasn't been redeployed alongside this backend.
func credentialMapKey(provider, source string) string {
	if provider == "" {
		return source
	}
	return provider + ":" + source
}

func (s *Store) queryByAPIKey(
	ctx context.Context,
	table string,
	fromNs, toNs int64,
	modelF resolvedFilter, credF resolvedCredentialFilter, apiKeyF resolvedFilter,
	result *QueryResult,
) error {
	filterSQL, filterArgs := composeFilterSQL(modelF, credF, apiKeyF, "b.")

	// LEFT JOIN + COALESCE so OAuth-style requests (api_key_id IS NULL) land
	// in a "(none)" bucket. Without this, by_api_key silently drops half the
	// dataset and the dimension chart's totals diverge from the headline
	// totals. When apiKeyF is active the filter clause "AND b.api_key_id IN (...)"
	// excludes NULLs naturally, so explicit api_key filters never see "(none)".
	q := "SELECT COALESCE(k.name, '(none)') AS api_key_name, " +
		"SUM(b.requests), SUM(b.success), SUM(b.failure), " +
		"SUM(b.input_tokens), SUM(b.output_tokens), SUM(b.reasoning_tokens), SUM(b.cached_tokens), SUM(b.total_tokens), " +
		"SUM(b.cost_micro) " +
		"FROM " + table + " b LEFT JOIN dim_api_key k ON k.id = b.api_key_id " +
		"WHERE b.bucket_ts_ns >= ? AND b.bucket_ts_ns <= ?" + filterSQL +
		" GROUP BY api_key_name"

	args := append([]any{fromNs, toNs}, filterArgs...)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("queryByAPIKey: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var name string
		var aks summaryAPIKeyStats
		var costMicro int64
		if err := rows.Scan(
			&name,
			&aks.Requests, &aks.Success, &aks.Failure,
			&aks.Tokens.InputTokens, &aks.Tokens.OutputTokens, &aks.Tokens.ReasoningTokens,
			&aks.Tokens.CachedTokens, &aks.Tokens.TotalTokens,
			&costMicro,
		); err != nil {
			return fmt.Errorf("queryByAPIKey scan: %w", err)
		}
		aks.Cost = float64(costMicro) / costMicroPerUSD
		result.ByAPIKey[name] = &aks
	}
	return rows.Err()
}

// qualifyFilterCols was a v1-era helper that string-replaced bare column names
// with their "b." prefix; v2 generates the prefixed form directly via
// composeFilterSQL(prefix=...) so this helper is no longer needed.
//
// (Reserved name to keep the diff against v1 readable; remove in a follow-up
// once code-search confirms zero remaining call sites.)

func (s *Store) queryTimeSeries(
	ctx context.Context,
	table string,
	fromNs, toNs int64,
	modelF resolvedFilter, credF resolvedCredentialFilter, apiKeyF resolvedFilter,
	timeExpr string,
	result *QueryResult,
) error {
	filterSQL, filterArgs := composeFilterSQL(modelF, credF, apiKeyF, "")

	q := "SELECT " + timeExpr + " AS ts, " +
		"SUM(requests), SUM(success), SUM(failure), " +
		"SUM(input_tokens), SUM(output_tokens), SUM(reasoning_tokens), SUM(cached_tokens), SUM(total_tokens), " +
		"SUM(cost_micro) " +
		"FROM " + table + " WHERE bucket_ts_ns >= ? AND bucket_ts_ns <= ?" + filterSQL +
		" GROUP BY ts ORDER BY ts"

	args := append([]any{fromNs, toNs}, filterArgs...)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("queryTimeSeries: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var ts int64
		var tp timePoint
		var costMicro int64
		if err := rows.Scan(
			&ts,
			&tp.Requests, &tp.Success, &tp.Failure,
			&tp.Tokens.InputTokens, &tp.Tokens.OutputTokens, &tp.Tokens.ReasoningTokens,
			&tp.Tokens.CachedTokens, &tp.Tokens.TotalTokens,
			&costMicro,
		); err != nil {
			return fmt.Errorf("queryTimeSeries scan: %w", err)
		}
		tp.Time = time.Unix(0, ts).UTC().Format(time.RFC3339)
		tp.Cost = float64(costMicro) / costMicroPerUSD
		result.TimeSeries = append(result.TimeSeries, tp)
	}
	return rows.Err()
}

func (s *Store) queryTimeSeriesByModel(
	ctx context.Context,
	table string,
	fromNs, toNs int64,
	modelF resolvedFilter, credF resolvedCredentialFilter, apiKeyF resolvedFilter,
	timeExpr string,
	result *QueryResult,
) error {
	filterSQL, filterArgs := composeFilterSQL(modelF, credF, apiKeyF, "b.")
	tsExpr := strings.ReplaceAll(timeExpr, "bucket_ts_ns", "b.bucket_ts_ns")

	q := "SELECT m.name, " + tsExpr + " AS ts, " +
		"SUM(b.requests), SUM(b.success), SUM(b.failure), " +
		"SUM(b.input_tokens), SUM(b.output_tokens), SUM(b.reasoning_tokens), SUM(b.cached_tokens), SUM(b.total_tokens), " +
		"SUM(b.cost_micro) " +
		"FROM " + table + " b JOIN dim_model m ON m.id = b.model_id " +
		"WHERE b.bucket_ts_ns >= ? AND b.bucket_ts_ns <= ?" + filterSQL +
		" GROUP BY m.id, m.name, ts ORDER BY m.name, ts"

	args := append([]any{fromNs, toNs}, filterArgs...)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("queryTimeSeriesByModel: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var name string
		var ts int64
		var tp timePoint
		var costMicro int64
		if err := rows.Scan(
			&name, &ts,
			&tp.Requests, &tp.Success, &tp.Failure,
			&tp.Tokens.InputTokens, &tp.Tokens.OutputTokens, &tp.Tokens.ReasoningTokens,
			&tp.Tokens.CachedTokens, &tp.Tokens.TotalTokens,
			&costMicro,
		); err != nil {
			return fmt.Errorf("queryTimeSeriesByModel scan: %w", err)
		}
		tp.Time = time.Unix(0, ts).UTC().Format(time.RFC3339)
		tp.Cost = float64(costMicro) / costMicroPerUSD
		result.TimeSeriesByModel[name] = append(result.TimeSeriesByModel[name], tp)
	}
	return rows.Err()
}

func (s *Store) queryTimeSeriesByCredential(
	ctx context.Context,
	table string,
	fromNs, toNs int64,
	modelF resolvedFilter, credF resolvedCredentialFilter, apiKeyF resolvedFilter,
	timeExpr string,
	result *QueryResult,
) error {
	filterSQL, filterArgs := composeFilterSQL(modelF, credF, apiKeyF, "b.")
	tsExpr := strings.ReplaceAll(timeExpr, "bucket_ts_ns", "b.bucket_ts_ns")

	// cost_micro is summed even though the byCredential aggregate (totals card)
	// drops it: the chart switcher can plot cost per credential, and without
	// SUM(cost_micro) every point comes back zero while has_cost is still true,
	// drawing a flat baseline that looks like real data.
	//
	// provider participates in the GROUP BY so each (provider, credential)
	// tuple gets its own time series — keys must match queryByCredential
	// exactly so the FilterBar dropdown and the chart legend stay aligned.
	q := "SELECT c.name, COALESCE(p.name, '') AS provider_name, " + tsExpr + " AS ts, " +
		"SUM(b.requests), SUM(b.success), SUM(b.failure), " +
		"SUM(b.input_tokens), SUM(b.output_tokens), SUM(b.reasoning_tokens), SUM(b.cached_tokens), SUM(b.total_tokens), " +
		"SUM(b.cost_micro) " +
		"FROM " + table + " b JOIN dim_credential c ON c.id = b.credential_id " +
		"LEFT JOIN dim_provider p ON p.id = b.provider_id " +
		"WHERE b.bucket_ts_ns >= ? AND b.bucket_ts_ns <= ? AND b.credential_id IS NOT NULL" + filterSQL +
		" GROUP BY c.id, c.name, p.id, provider_name, ts ORDER BY c.name, provider_name, ts"

	args := append([]any{fromNs, toNs}, filterArgs...)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("queryTimeSeriesByCredential: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var credName, providerName string
		var ts int64
		var tp timePoint
		var costMicro int64
		if err := rows.Scan(
			&credName, &providerName, &ts,
			&tp.Requests, &tp.Success, &tp.Failure,
			&tp.Tokens.InputTokens, &tp.Tokens.OutputTokens, &tp.Tokens.ReasoningTokens,
			&tp.Tokens.CachedTokens, &tp.Tokens.TotalTokens,
			&costMicro,
		); err != nil {
			return fmt.Errorf("queryTimeSeriesByCredential scan: %w", err)
		}
		tp.Time = time.Unix(0, ts).UTC().Format(time.RFC3339)
		tp.Cost = float64(costMicro) / costMicroPerUSD
		key := credentialMapKey(providerName, credName)
		result.TimeSeriesByCredential[key] = append(result.TimeSeriesByCredential[key], tp)
	}
	return rows.Err()
}

func (s *Store) queryTimeSeriesByAPIKey(
	ctx context.Context,
	table string,
	fromNs, toNs int64,
	modelF resolvedFilter, credF resolvedCredentialFilter, apiKeyF resolvedFilter,
	timeExpr string,
	result *QueryResult,
) error {
	filterSQL, filterArgs := composeFilterSQL(modelF, credF, apiKeyF, "b.")
	tsExpr := strings.ReplaceAll(timeExpr, "bucket_ts_ns", "b.bucket_ts_ns")

	// Same conservation logic as queryByAPIKey — LEFT JOIN with COALESCE so
	// the "by_api_key" time series sums equal the headline time_series totals.
	q := "SELECT COALESCE(k.name, '(none)') AS api_key_name, " + tsExpr + " AS ts, " +
		"SUM(b.requests), SUM(b.success), SUM(b.failure), " +
		"SUM(b.input_tokens), SUM(b.output_tokens), SUM(b.reasoning_tokens), SUM(b.cached_tokens), SUM(b.total_tokens), " +
		"SUM(b.cost_micro) " +
		"FROM " + table + " b LEFT JOIN dim_api_key k ON k.id = b.api_key_id " +
		"WHERE b.bucket_ts_ns >= ? AND b.bucket_ts_ns <= ?" + filterSQL +
		" GROUP BY api_key_name, ts ORDER BY api_key_name, ts"

	args := append([]any{fromNs, toNs}, filterArgs...)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("queryTimeSeriesByAPIKey: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var name string
		var ts int64
		var tp timePoint
		var costMicro int64
		if err := rows.Scan(
			&name, &ts,
			&tp.Requests, &tp.Success, &tp.Failure,
			&tp.Tokens.InputTokens, &tp.Tokens.OutputTokens, &tp.Tokens.ReasoningTokens,
			&tp.Tokens.CachedTokens, &tp.Tokens.TotalTokens,
			&costMicro,
		); err != nil {
			return fmt.Errorf("queryTimeSeriesByAPIKey scan: %w", err)
		}
		tp.Time = time.Unix(0, ts).UTC().Format(time.RFC3339)
		tp.Cost = float64(costMicro) / costMicroPerUSD
		result.TimeSeriesByAPIKey[name] = append(result.TimeSeriesByAPIKey[name], tp)
	}
	return rows.Err()
}

// QueryEvents serves /usage/events. Pagination is SQL-native (LIMIT/OFFSET) so
// large windows don't pull every row into memory the way DetailStore.QueryRange
// did — that "read everything then slice" pattern was the OOM accomplice in
// the JSON era and explicitly cannot be reintroduced.
func (s *Store) QueryEvents(
	ctx context.Context,
	from, to time.Time,
	filters EventFilters,
	page, pageSize int,
	sortField string,
	desc bool,
) ([]FlatDetail, int, error) {
	if s == nil || s.db == nil {
		return nil, 0, fmt.Errorf("usage: query events on nil store")
	}
	if !from.Before(to) {
		return nil, 0, nil
	}

	modelF, err := resolveNamesToIDs(ctx, s.db, "dim_model", filters.Model)
	if err != nil {
		return nil, 0, err
	}
	credF, err := resolveCredentialFilter(ctx, s.db, filters.Source)
	if err != nil {
		return nil, 0, err
	}
	apiKeyF, err := resolveNamesToIDs(ctx, s.db, "dim_api_key", filters.APIKey)
	if err != nil {
		return nil, 0, err
	}
	if (modelF.active && !modelF.matchedAny) ||
		(credF.active && !credF.matchedAny) ||
		(apiKeyF.active && !apiKeyF.matchedAny) {
		return nil, 0, nil
	}

	filterSQL, filterArgs := composeFilterSQL(modelF, credF, apiKeyF, "")

	// Status filter (success/failure/empty)
	switch filters.Status {
	case "success":
		filterSQL += " AND failed = 0"
	case "failure":
		filterSQL += " AND failed = 1"
	}

	// Search filter requires JOINs on multiple dim tables, so we only enable
	// it when the user actually typed something. Keep it server-side so the
	// frontend's free-text box still works after the JSON path is gone.
	searchSQL := ""
	var searchArgs []any
	if q := strings.TrimSpace(filters.Search); q != "" {
		searchSQL = " AND (" +
			"EXISTS (SELECT 1 FROM dim_model m WHERE m.id = events.model_id AND m.name LIKE ?) " +
			"OR EXISTS (SELECT 1 FROM dim_credential c WHERE c.id = events.credential_id AND c.name LIKE ?) " +
			"OR EXISTS (SELECT 1 FROM dim_source s WHERE s.id = events.source_id AND s.name LIKE ?) " +
			"OR EXISTS (SELECT 1 FROM dim_auth_index a WHERE a.id = events.auth_index_id AND a.name LIKE ?) " +
			"OR EXISTS (SELECT 1 FROM dim_api_key k WHERE k.id = events.api_key_id AND k.name LIKE ?)" +
			")"
		pattern := "%" + q + "%"
		searchArgs = []any{pattern, pattern, pattern, pattern, pattern}
	}

	fromNs := from.UTC().UnixNano()
	toNs := to.UTC().UnixNano()

	whereSQL := "WHERE events.ts_ns >= ? AND events.ts_ns <= ?" + filterSQL + searchSQL
	whereArgs := append([]any{fromNs, toNs}, filterArgs...)
	whereArgs = append(whereArgs, searchArgs...)

	// Total count
	countQ := "SELECT COUNT(*) FROM events " + whereSQL
	var total int
	if err := s.db.QueryRowContext(ctx, countQ, whereArgs...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count events: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}

	// Pagination + sort
	orderCol := "ts_ns"
	switch sortField {
	case "model":
		orderCol = "(SELECT name FROM dim_model WHERE id = events.model_id)"
	case "tokens":
		orderCol = "events.total_tokens"
	}
	orderDir := "ASC"
	if desc {
		orderDir = "DESC"
	}
	if pageSize <= 0 {
		pageSize = 100
	}
	if page < 1 {
		page = 1
	}
	offset := (page - 1) * pageSize

	dataQ := "SELECT events.ts_ns, " +
		"(SELECT name FROM dim_model      WHERE id = events.model_id), " +
		"(SELECT name FROM dim_source     WHERE id = events.source_id), " +
		"(SELECT name FROM dim_auth_index WHERE id = events.auth_index_id), " +
		"(SELECT name FROM dim_api_key    WHERE id = events.api_key_id), " +
		"events.input_tokens, events.output_tokens, events.reasoning_tokens, events.cached_tokens, events.total_tokens, " +
		"events.failed " +
		"FROM events " + whereSQL +
		" ORDER BY " + orderCol + " " + orderDir +
		" LIMIT ? OFFSET ?"

	dataArgs := append([]any{}, whereArgs...)
	dataArgs = append(dataArgs, pageSize, offset)

	rows, err := s.db.QueryContext(ctx, dataQ, dataArgs...)
	if err != nil {
		return nil, 0, fmt.Errorf("query events: %w", err)
	}
	defer rows.Close()

	out := make([]FlatDetail, 0, pageSize)
	for rows.Next() {
		var d FlatDetail
		var tsNs int64
		var modelName, srcName, authIdxName, apiKeyName sql.NullString
		var failedInt int64
		if err := rows.Scan(
			&tsNs,
			&modelName, &srcName, &authIdxName, &apiKeyName,
			&d.Tokens.InputTokens, &d.Tokens.OutputTokens, &d.Tokens.ReasoningTokens, &d.Tokens.CachedTokens,
			&d.Tokens.TotalTokens,
			&failedInt,
		); err != nil {
			return nil, 0, fmt.Errorf("scan event row: %w", err)
		}
		d.Timestamp = time.Unix(0, tsNs).UTC()
		d.Model = modelName.String
		d.Source = srcName.String
		d.AuthIndex = authIdxName.String
		d.APIKey = apiKeyName.String
		d.Failed = failedInt != 0
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate events: %w", err)
	}
	return out, total, nil
}

// StartRollup launches the day-bucket rollup goroutine and returns a stop
// function. The first catch-up pass still runs synchronously before this
// function returns, but it now resumes from meta.day_bucket_rollup_watermark_ns
// instead of re-scanning every historical day on every startup. The hourly
// ticker keeps re-rolling yesterday/today in the background and advances the
// watermark as new closed days appear.
func (s *Store) StartRollup(ctx context.Context) func() {
	// Initial pass must complete before we accept queries — otherwise a
	// startup-time >90d query reads an empty day_bucket and the user sees a
	// blank summary until the goroutine catches up.
	if err := s.rollupPendingDays(ctx); err != nil {
		log.Errorf("usage: initial day rollup failed: %v", err)
	}

	stopCh := make(chan struct{})
	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)

		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()

		for {
			select {
			case <-stopCh:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := s.rollupRecentDays(ctx); err != nil {
					log.Errorf("usage: rollup recent days: %v", err)
				}
				if err := s.rollupPendingDays(ctx); err != nil {
					log.Errorf("usage: rollup pending days: %v", err)
				}
			}
		}
	}()
	return func() {
		close(stopCh)
		<-doneCh
	}
}

func (s *Store) rollupRecentDays(ctx context.Context) error {
	yesterday := time.Now().UTC().Add(-2 * time.Hour).Truncate(24 * time.Hour)
	today := time.Now().UTC().Truncate(24 * time.Hour)
	for d := yesterday; !d.After(today); d = d.Add(24 * time.Hour) {
		if err := s.RollupDay(ctx, d.UnixNano()); err != nil {
			return fmt.Errorf("rollup day %s: %w", d.Format("2006-01-02"), err)
		}
	}
	return nil
}

// rollupPendingDays catches day_bucket up to the newest hour_bucket day using a
// persisted watermark in meta, so restarts only process unseen closed days.
func (s *Store) rollupPendingDays(ctx context.Context) error {
	newestDayNs, hasData, err := s.latestHourBucketDay(ctx)
	if err != nil {
		return err
	}
	if !hasData {
		return nil
	}
	watermarkNs, err := s.getDayRollupWatermark(ctx)
	if err != nil {
		return err
	}
	startDayNs, err := s.nextPendingRollupDay(ctx, watermarkNs)
	if err != nil {
		return err
	}
	if startDayNs == 0 || startDayNs > newestDayNs {
		return nil
	}
	if err := s.rollupDayRange(ctx, startDayNs, newestDayNs); err != nil {
		return err
	}
	return s.setDayRollupWatermark(ctx, newestDayNs)
}

func (s *Store) rollupImportedDays(ctx context.Context, earliestDayNs, latestDayNs int64) error {
	if earliestDayNs == 0 || latestDayNs == 0 || earliestDayNs > latestDayNs {
		return nil
	}
	return s.rollupDayRange(ctx, earliestDayNs, latestDayNs)
}

func (s *Store) nextPendingRollupDay(ctx context.Context, watermarkNs int64) (int64, error) {
	if watermarkNs == 0 {
		startDayNs, err := s.earliestHourBucketDay(ctx)
		if err != nil {
			return 0, err
		}
		if startDayNs == 0 {
			return 0, nil
		}
		return startDayNs, nil
	}
	return watermarkNs + nsPerDay, nil
}

func (s *Store) rollupDayRange(ctx context.Context, startDayNs, endDayNs int64) error {
	for dayNs := startDayNs; dayNs <= endDayNs; dayNs += nsPerDay {
		if err := s.RollupDay(ctx, dayNs); err != nil {
			return fmt.Errorf("rollup day %s: %w", time.Unix(0, dayNs).UTC().Format("2006-01-02"), err)
		}
	}
	return nil
}

func (s *Store) earliestHourBucketDay(ctx context.Context) (int64, error) {
	var dayNs sql.NullInt64
	if err := s.db.QueryRowContext(
		ctx,
		"SELECT MIN((bucket_ts_ns / ?) * ?) FROM hour_bucket",
		nsPerDay, nsPerDay,
	).Scan(&dayNs); err != nil {
		return 0, fmt.Errorf("query earliest day: %w", err)
	}
	if !dayNs.Valid {
		return 0, nil
	}
	return dayNs.Int64, nil
}

func (s *Store) latestHourBucketDay(ctx context.Context) (int64, bool, error) {
	var dayNs sql.NullInt64
	if err := s.db.QueryRowContext(
		ctx,
		"SELECT MAX((bucket_ts_ns / ?) * ?) FROM hour_bucket",
		nsPerDay, nsPerDay,
	).Scan(&dayNs); err != nil {
		return 0, false, fmt.Errorf("query latest day: %w", err)
	}
	if !dayNs.Valid {
		return 0, false, nil
	}
	return dayNs.Int64, true, nil
}

func (s *Store) getDayRollupWatermark(ctx context.Context) (int64, error) {
	var raw string
	err := s.db.QueryRowContext(
		ctx,
		"SELECT value FROM meta WHERE key = ?",
		dayRollupWatermarkKey,
	).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read day rollup watermark: %w", err)
	}
	parsed, parseErr := time.Parse(time.RFC3339Nano, raw)
	if parseErr == nil {
		return parsed.UTC().UnixNano(), nil
	}
	var watermarkNs int64
	if _, scanErr := fmt.Sscan(raw, &watermarkNs); scanErr != nil {
		return 0, fmt.Errorf("parse day rollup watermark %q: %w", raw, scanErr)
	}
	return watermarkNs, nil
}

func (s *Store) setDayRollupWatermark(ctx context.Context, dayNs int64) error {
	if _, err := s.db.ExecContext(
		ctx,
		"INSERT INTO meta(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value",
		dayRollupWatermarkKey,
		fmt.Sprintf("%d", dayNs),
	); err != nil {
		return fmt.Errorf("write day rollup watermark: %w", err)
	}
	return nil
}

// RollupDay recomputes day_bucket rows for the day starting at dayStartNs from
// the corresponding hour_bucket rows. Idempotent: re-running with the same day
// produces identical state, so missed runs after a crash are self-healing.
//
// The UPSERT uses "SET col = excluded.col" rather than "+= excluded.col"
// because excluded already holds the recomputed total — adding would double
// count on the second pass.
func (s *Store) RollupDay(ctx context.Context, dayStartNs int64) error {
	if s == nil || s.db == nil {
		return errors.New("usage: rollupDay on nil store")
	}
	dayEndNs := dayStartNs + nsPerDay

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin rollup tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	const aggSQL = `
SELECT model_id, credential_id, api_key_id, provider_id,
       SUM(requests), SUM(success), SUM(failure),
       SUM(input_tokens), SUM(output_tokens), SUM(reasoning_tokens), SUM(cached_tokens), SUM(total_tokens),
       SUM(cost_micro)
  FROM hour_bucket
 WHERE bucket_ts_ns >= ? AND bucket_ts_ns < ?
 GROUP BY model_id, credential_id, api_key_id, provider_id
`
	rows, err := tx.QueryContext(ctx, aggSQL, dayStartNs, dayEndNs)
	if err != nil {
		return fmt.Errorf("aggregate hour_bucket: %w", err)
	}

	type aggRow struct {
		modelID                                            int64
		credID, apiKeyID, providerID                       sql.NullInt64
		requests, success, failure                         int64
		input, output, reasoning, cached, total, costMicro int64
	}
	var aggs []aggRow
	for rows.Next() {
		var a aggRow
		if err := rows.Scan(
			&a.modelID, &a.credID, &a.apiKeyID, &a.providerID,
			&a.requests, &a.success, &a.failure,
			&a.input, &a.output, &a.reasoning, &a.cached, &a.total,
			&a.costMicro,
		); err != nil {
			rows.Close()
			return fmt.Errorf("scan aggregate: %w", err)
		}
		aggs = append(aggs, a)
	}
	rows.Close()

	// Wipe day_bucket rows for this day before re-inserting so deletions in
	// hour_bucket (e.g. from Trim) propagate.
	if _, err := tx.ExecContext(
		ctx,
		"DELETE FROM day_bucket WHERE bucket_ts_ns = ?",
		dayStartNs,
	); err != nil {
		return fmt.Errorf("clear day_bucket: %w", err)
	}

	const upsertSQL = `
INSERT INTO day_bucket (
    bucket_ts_ns, model_id, credential_id, api_key_id, provider_id,
    requests, success, failure,
    input_tokens, output_tokens, reasoning_tokens, cached_tokens, total_tokens,
    cost_micro
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(bucket_ts_ns, model_id, ifnull(credential_id, 0), ifnull(api_key_id, 0), ifnull(provider_id, 0))
DO UPDATE SET
    requests         = excluded.requests,
    success          = excluded.success,
    failure          = excluded.failure,
    input_tokens     = excluded.input_tokens,
    output_tokens    = excluded.output_tokens,
    reasoning_tokens = excluded.reasoning_tokens,
    cached_tokens    = excluded.cached_tokens,
    total_tokens     = excluded.total_tokens,
    cost_micro       = excluded.cost_micro
`
	for _, a := range aggs {
		var credAny, apiKeyAny, providerAny any
		if a.credID.Valid {
			credAny = a.credID.Int64
		}
		if a.apiKeyID.Valid {
			apiKeyAny = a.apiKeyID.Int64
		}
		if a.providerID.Valid {
			providerAny = a.providerID.Int64
		}
		if _, err := tx.ExecContext(
			ctx, upsertSQL,
			dayStartNs, a.modelID, credAny, apiKeyAny, providerAny,
			a.requests, a.success, a.failure,
			a.input, a.output, a.reasoning, a.cached, a.total,
			a.costMicro,
		); err != nil {
			return fmt.Errorf("upsert day_bucket: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit rollup tx: %w", err)
	}
	committed = true
	return nil
}
