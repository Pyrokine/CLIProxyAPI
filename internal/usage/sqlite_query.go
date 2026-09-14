package usage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

const (
	maxSummaryBucketCount = 2_000

	// dayBucketThreshold switches to day buckets before an hourly series would
	// exceed maxSummaryBucketCount, keeping accepted ranges monotonic.
	dayBucketThreshold = time.Duration(maxSummaryBucketCount-1) * time.Hour

	nsPerDay       = int64(24 * 3600 * 1_000_000_000)
	nsPerHour      = int64(3600 * 1_000_000_000)
	nsPerTenMinute = int64(10 * 60 * 1_000_000_000)

	maxAuthIndexTenMinuteRange = 4 * time.Hour

	dayRollupWatermarkKey = "day_bucket_rollup_watermark_ns"

	// costMicroPerUSD converts cost_micro back to USD for response payloads.
	costMicroPerUSD = 1_000_000.0

	maxSummarySeriesCount  = 500
	maxSummaryTotalPoints  = 100_000
	maxSummaryInputRows    = 100_000
	MaxEventsPageSize      = 500
	maxEventsOffset        = 100_000
	maxEventsMetadataBytes = 8 << 20
)

var (
	ErrSummaryQueryTooLarge = errors.New("usage: summary query exceeds resource limits")
	ErrEventsPageTooDeep    = errors.New("usage: events page exceeds offset limit")
	ErrEventsPageTooLarge   = errors.New("usage: events page exceeds metadata byte limit")
	ErrTimestampOutOfRange  = errors.New("usage: timestamp is outside the UnixNano range")
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

const summaryBucketValueColumns = "model_id, credential_id, api_key_id, provider_id, " +
	"requests, success, failure, input_tokens, output_tokens, reasoning_tokens, cached_tokens, " +
	"cache_read_tokens, cache_write_tokens, total_tokens, cost_micro, " +
	"complete_requests, partial_requests, unpriced_requests, " +
	"complete_tokens, partial_tokens, unpriced_tokens, latency_ms_total, latency_samples"

func daySummaryBucketSource() string {
	dayForHour := sqliteFloorBucketExpr("h.bucket_ts_ns", nsPerDay)
	return "(SELECT d.bucket_ts_ns, " + summaryBucketValueColumns + " FROM day_bucket d " +
		"WHERE NOT EXISTS (SELECT 1 FROM dirty_day dirty WHERE dirty.day_ts_ns = d.bucket_ts_ns) " +
		"UNION ALL SELECT " + dayForHour + " AS bucket_ts_ns, " + summaryBucketValueColumns + " FROM hour_bucket h " +
		"WHERE EXISTS (SELECT 1 FROM dirty_day dirty WHERE dirty.day_ts_ns = " + dayForHour + "))"
}

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
func resolveNamesToIDs(ctx context.Context, db dbQueryer, table, raw string) (resolvedFilter, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return resolvedFilter{}, nil
	}
	rf := resolvedFilter{active: true}
	seenNames := make(map[string]struct{})
	seenIDs := make(map[int64]struct{})
	resolveName := func(name string, ids []int64, resolved bool) error {
		name = strings.TrimSpace(name)
		if name == "" {
			return nil
		}
		if _, dup := seenNames[name]; dup {
			return nil
		}
		seenNames[name] = struct{}{}

		if name == noneSentinel {
			rf.includesNone = true
			rf.matchedAny = true
		}
		if !resolved {
			var err error
			ids, err = resolveDimensionNameIDs(ctx, db, table, name)
			if err != nil {
				return err
			}
		}
		for _, id := range ids {
			if _, dup := seenIDs[id]; dup {
				continue
			}
			seenIDs[id] = struct{}{}
			rf.ids = append(rf.ids, id)
			rf.matchedAny = true
		}
		return nil
	}

	wholeIDs, err := resolveDimensionNameIDs(ctx, db, table, raw)
	if err != nil {
		return resolvedFilter{}, err
	}
	if raw == noneSentinel || len(wholeIDs) > 0 {
		if err = resolveName(raw, wholeIDs, true); err != nil {
			return resolvedFilter{}, err
		}
		return rf, nil
	}

	for part := range strings.SplitSeq(raw, ",") {
		if err = resolveName(part, nil, false); err != nil {
			return resolvedFilter{}, err
		}
	}
	return rf, nil
}

func resolveFilterNamesToIDs(
	ctx context.Context,
	db dbQueryer,
	table string,
	raw string,
	values []string,
) (resolvedFilter, error) {
	names := make([]string, 0, len(values))
	for _, value := range values {
		if name := strings.TrimSpace(value); name != "" {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return resolveNamesToIDs(ctx, db, table, raw)
	}
	if len(names) == 1 {
		if strings.TrimSpace(raw) == "" {
			raw = names[0]
		}
		return resolveNamesToIDs(ctx, db, table, raw)
	}

	rf := resolvedFilter{active: true}
	seenNames := make(map[string]struct{})
	seenIDs := make(map[int64]struct{})
	for _, name := range names {
		if _, duplicate := seenNames[name]; duplicate {
			continue
		}
		seenNames[name] = struct{}{}

		if name == noneSentinel {
			rf.includesNone = true
			rf.matchedAny = true
		}
		ids, err := resolveDimensionNameIDs(ctx, db, table, name)
		if err != nil {
			return resolvedFilter{}, err
		}
		for _, id := range ids {
			if _, duplicate := seenIDs[id]; duplicate {
				continue
			}
			seenIDs[id] = struct{}{}
			rf.ids = append(rf.ids, id)
			rf.matchedAny = true
		}
	}
	return rf, nil
}

func resolveDimensionNameIDs(ctx context.Context, db dbQueryer, table, name string) ([]int64, error) {
	if table != "dim_api_key" {
		var id int64
		err := db.QueryRowContext(ctx, "SELECT id FROM "+table+" WHERE name = ?", name).Scan(&id)
		if err == nil {
			return []int64{id}, nil
		}
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("resolve %s.name=%q: %w", table, name, err)
	}

	rows, err := db.QueryContext(ctx, "SELECT id, name FROM dim_api_key ORDER BY id")
	if err != nil {
		return nil, fmt.Errorf("resolve api key %q: %w", name, err)
	}
	defer func() { _ = rows.Close() }()

	var ids []int64
	identity := coreusage.EnsureAPIKeyIdentity(name)
	for rows.Next() {
		var candidateID int64
		var candidateName string
		if err = rows.Scan(&candidateID, &candidateName); err != nil {
			return nil, fmt.Errorf("resolve api key scan: %w", err)
		}
		trimmedCandidate := strings.TrimSpace(candidateName)
		if trimmedCandidate == name || trimmedCandidate == identity || util.HideAPIKey(trimmedCandidate) == name {
			ids = append(ids, candidateID)
		}
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("resolve api key rows: %w", err)
	}
	return ids, nil
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
func resolveCredentialFilter(ctx context.Context, db dbQueryer, raw string) (resolvedCredentialFilter, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return resolvedCredentialFilter{}, nil
	}
	rf := resolvedCredentialFilter{active: true}
	seenTokens := make(map[string]struct{})
	seenPairs := make(map[credentialPair]struct{})
	resolveToken := func(token string) (bool, error) {
		token = strings.TrimSpace(token)
		if token == "" {
			return false, nil
		}
		if _, duplicate := seenTokens[token]; duplicate {
			return false, nil
		}
		seenTokens[token] = struct{}{}

		providerName, credentialName := splitCredentialToken(token)
		var providerID sql.NullInt64
		if providerName != "" {
			var id int64
			err := db.QueryRowContext(ctx, "SELECT id FROM dim_provider WHERE name = ?", providerName).Scan(&id)
			if errors.Is(err, sql.ErrNoRows) {
				return false, nil
			}
			if err != nil {
				return false, fmt.Errorf("resolve provider %q: %w", providerName, err)
			}
			providerID = sql.NullInt64{Int64: id, Valid: true}
		}

		credentialIDs, err := resolveCredentialNameIDs(ctx, db, credentialName)
		if err != nil {
			return false, err
		}
		matched := false
		for _, credentialID := range credentialIDs {
			pair := credentialPair{credentialID: credentialID, providerID: providerID}
			if _, duplicate := seenPairs[pair]; duplicate {
				continue
			}
			seenPairs[pair] = struct{}{}
			rf.pairs = append(rf.pairs, pair)
			matched = true
		}
		if matched {
			rf.matchedAny = true
		}
		return matched, nil
	}

	for part := range strings.SplitSeq(raw, ",") {
		if _, err := resolveToken(part); err != nil {
			return resolvedCredentialFilter{}, err
		}
	}
	return rf, nil
}

func resolveCredentialNameIDs(ctx context.Context, db dbQueryer, name string) ([]int64, error) {
	name, filterKind, filterTyped := decodeCredentialFilter(name)
	rows, err := db.QueryContext(ctx, "SELECT id, name FROM dim_credential ORDER BY id")
	if err != nil {
		return nil, fmt.Errorf("resolve credential %q: %w", name, err)
	}
	defer func() { _ = rows.Close() }()

	var exactIDs []int64
	var taggedIDs []int64
	var legacyDisplayIDs []int64
	maskedName := util.HideAPIKey(name)
	for rows.Next() {
		var candidateID int64
		var candidateName string
		if err = rows.Scan(&candidateID, &candidateName); err != nil {
			return nil, fmt.Errorf("resolve credential scan: %w", err)
		}
		trimmedCandidate := strings.TrimSpace(candidateName)
		if trimmedCandidate == name && (!filterTyped || filterKind == coreusage.CredentialSourceKindAPIKey) {
			exactIDs = append(exactIDs, candidateID)
			continue
		}
		display, kind, tagged := DecodeCredentialSource(trimmedCandidate)
		if tagged {
			if filterTyped && kind != filterKind {
				continue
			}
			if display == name || (kind == coreusage.CredentialSourceKindAPIKey && display == maskedName) {
				taggedIDs = append(taggedIDs, candidateID)
			}
			continue
		}
		if filterTyped && filterKind != coreusage.CredentialSourceKindAPIKey {
			continue
		}
		if util.HideAPIKey(display) == name {
			legacyDisplayIDs = append(legacyDisplayIDs, candidateID)
		}
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("resolve credential rows: %w", err)
	}
	if len(exactIDs) > 0 {
		return append(exactIDs, taggedIDs...), nil
	}
	return append(taggedIDs, legacyDisplayIDs...), nil
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
	if strings.HasPrefix(token, credentialFilterPrefix) {
		return "", token
	}
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

func unixNanoUTC(t time.Time) (int64, error) {
	utc := t.UTC()
	ns := utc.UnixNano()
	if !time.Unix(0, ns).UTC().Equal(utc) {
		return 0, fmt.Errorf(
			"%w: %s",
			ErrTimestampOutOfRange,
			utc.Format(time.RFC3339Nano),
		)
	}
	return ns, nil
}

func floorUnixNano(ns, grain int64) (int64, error) {
	if grain <= 0 {
		return 0, errors.New("usage: timestamp bucket grain must be positive")
	}
	remainder := ns % grain
	if remainder == 0 {
		return ns, nil
	}
	if remainder > 0 {
		return ns - remainder, nil
	}
	adjustment := grain + remainder
	if ns < math.MinInt64+adjustment {
		return 0, errors.New("usage: timestamp bucket start underflows int64")
	}
	return ns - adjustment, nil
}

func unixNanoBucketStarts(t time.Time) (int64, int64, int64, error) {
	tsNs, err := unixNanoUTC(t)
	if err != nil {
		return 0, 0, 0, err
	}
	hourNs, err := floorUnixNano(tsNs, nsPerHour)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("hour bucket: %w", err)
	}
	dayNs, err := floorUnixNano(tsNs, nsPerDay)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("day bucket: %w", err)
	}
	return tsNs, hourNs, dayNs, nil
}

func sqliteFloorBucketExpr(column string, grain int64) string {
	return fmt.Sprintf(
		"CASE WHEN %[1]s >= 0 THEN (%[1]s / %[2]d) * %[2]d "+
			"ELSE (-1 - ((-1 - %[1]s) / %[2]d)) * %[2]d END",
		column,
		grain,
	)
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

// querySummary implements /usage/summary against the bucket tables. It switches from
// hour_bucket to day_bucket before the hourly grid reaches its resource limit.
// granularity controls time-series granularity but never scans events.
func (s *Store) querySummary(
	ctx context.Context,
	from, to time.Time,
	filters EventFilters,
	granularity string,
	includeGroups bool,
) (QueryResult, error) {
	return s.query(ctx, from, to, filters, granularity, includeGroups, false, "")
}

func (s *Store) queryWithAuthIndex(
	ctx context.Context,
	from, to time.Time,
	filters EventFilters,
	granularity string,
	includeGroups bool,
) (QueryResult, error) {
	return s.queryWithAuthIndexGranularity(ctx, from, to, filters, granularity, "", includeGroups)
}

func (s *Store) queryWithAuthIndexGranularity(
	ctx context.Context,
	from, to time.Time,
	filters EventFilters,
	granularity string,
	authIndexGranularity string,
	includeGroups bool,
) (QueryResult, error) {
	return s.query(ctx, from, to, filters, granularity, includeGroups, true, authIndexGranularity)
}

func (s *Store) query(
	ctx context.Context,
	from, to time.Time,
	filters EventFilters,
	granularity string,
	includeGroups bool,
	includeAuthIndex bool,
	authIndexGranularity string,
) (QueryResult, error) {
	if s == nil || s.db == nil {
		return QueryResult{}, fmt.Errorf("usage: query on nil store")
	}
	if !from.Before(to) {
		return emptyQueryResult(), nil
	}

	fromRawNs, err := unixNanoUTC(from)
	if err != nil {
		return QueryResult{}, fmt.Errorf("usage: invalid query start: %w", err)
	}
	toRawNs, err := unixNanoUTC(to)
	if err != nil {
		return QueryResult{}, fmt.Errorf("usage: invalid query end: %w", err)
	}

	useDayBucket := to.Sub(from) > dayBucketThreshold
	table := "hour_bucket"
	if useDayBucket {
		table = daySummaryBucketSource()
	}

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return QueryResult{}, fmt.Errorf("usage: begin summary transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	modelF, err := resolveNamesToIDs(ctx, tx, "dim_model", filters.Model)
	if err != nil {
		return QueryResult{}, err
	}
	credF, err := resolveCredentialFilter(ctx, tx, filters.Source)
	if err != nil {
		return QueryResult{}, err
	}
	apiKeyF, err := resolveFilterNamesToIDs(ctx, tx, "dim_api_key", filters.APIKey, filters.APIKeys)
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
	fromNs, err := floorUnixNano(fromRawNs, grainNs)
	if err != nil {
		return QueryResult{}, fmt.Errorf("usage: bucket query start: %w", err)
	}
	toNs, err := floorUnixNano(toRawNs, grainNs)
	if err != nil {
		return QueryResult{}, fmt.Errorf("usage: bucket query end: %w", err)
	}

	timeExpr := "bucket_ts_ns"
	if useDayBucket || granularity == "daily" {
		timeExpr = sqliteFloorBucketExpr("bucket_ts_ns", nsPerDay)
	}
	stepNs := nsPerHour
	if useDayBucket || granularity == "daily" {
		stepNs = nsPerDay
	}
	seriesLimit, err := summarySeriesLimit(fromNs, toNs, stepNs)
	if err != nil {
		return QueryResult{}, err
	}

	authIndexStepNs := stepNs
	authIndexFromNs := fromNs
	authIndexToNs := toNs
	authIndexSeriesLimit := seriesLimit
	if includeAuthIndex {
		switch strings.ToLower(strings.TrimSpace(authIndexGranularity)) {
		case "":
		case "10m":
			if to.Sub(from) > maxAuthIndexTenMinuteRange {
				return QueryResult{}, ErrSummaryQueryTooLarge
			}
			authIndexStepNs = nsPerTenMinute
		case "hourly":
			authIndexStepNs = nsPerHour
		case "daily":
			authIndexStepNs = nsPerDay
		default:
			return QueryResult{}, fmt.Errorf("usage: invalid auth-index granularity %q", authIndexGranularity)
		}
		authIndexFromNs, err = floorUnixNano(fromRawNs, authIndexStepNs)
		if err != nil {
			return QueryResult{}, fmt.Errorf("usage: auth-index query start: %w", err)
		}
		authIndexToNs, err = floorUnixNano(toRawNs, authIndexStepNs)
		if err != nil {
			return QueryResult{}, fmt.Errorf("usage: auth-index query end: %w", err)
		}
		authIndexSeriesLimit, err = summarySeriesLimit(authIndexFromNs, authIndexToNs, authIndexStepNs)
		if err != nil {
			return QueryResult{}, err
		}
	}
	if err = ensureSummaryInputBudget(
		ctx,
		tx,
		table,
		fromNs,
		toNs,
		modelF,
		credF,
		apiKeyF,
	); err != nil {
		return QueryResult{}, err
	}
	if includeAuthIndex {
		if err = ensureAuthIndexInputBudget(
			ctx,
			tx,
			fromRawNs,
			toRawNs,
			modelF,
			credF,
			apiKeyF,
		); err != nil {
			return QueryResult{}, err
		}
	}

	result := emptyQueryResult()
	if err := s.queryTotals(ctx, tx, table, fromNs, toNs, modelF, credF, apiKeyF, &result); err != nil {
		return QueryResult{}, err
	}
	if err := s.queryByModel(ctx, tx, table, fromNs, toNs, modelF, credF, apiKeyF, &result); err != nil {
		return QueryResult{}, err
	}
	if err := s.queryByCredential(ctx, tx, table, fromNs, toNs, modelF, credF, apiKeyF, &result); err != nil {
		return QueryResult{}, err
	}
	if err := s.queryByAPIKey(ctx, tx, table, fromNs, toNs, modelF, credF, apiKeyF, &result); err != nil {
		return QueryResult{}, err
	}
	if includeAuthIndex {
		if err := s.queryByAuthIndex(ctx, tx, fromRawNs, toRawNs, modelF, credF, apiKeyF, &result); err != nil {
			return QueryResult{}, err
		}
	}
	if summaryDimensionCount(&result) > maxSummarySeriesCount {
		return QueryResult{}, ErrSummaryQueryTooLarge
	}
	if err := s.queryTimeSeries(ctx, tx, table, fromNs, toNs, modelF, credF, apiKeyF, timeExpr, &result); err != nil {
		return QueryResult{}, err
	}
	if includeGroups {
		if err := s.queryTimeSeriesByModel(
			ctx, tx, table, fromNs, toNs, modelF, credF, apiKeyF, timeExpr, seriesLimit, &result,
		); err != nil {
			return QueryResult{}, err
		}
		if err := s.queryTimeSeriesByCredential(
			ctx, tx, table, fromNs, toNs, modelF, credF, apiKeyF, timeExpr, seriesLimit, &result,
		); err != nil {
			return QueryResult{}, err
		}
		if err := s.queryTimeSeriesByAPIKey(
			ctx, tx, table, fromNs, toNs, modelF, credF, apiKeyF, timeExpr, seriesLimit, &result,
		); err != nil {
			return QueryResult{}, err
		}
	}
	if includeAuthIndex {
		if err := s.queryTimeSeriesByAuthIndex(
			ctx,
			tx,
			fromRawNs,
			toRawNs,
			modelF,
			credF,
			apiKeyF,
			authIndexStepNs,
			authIndexSeriesLimit,
			&result,
		); err != nil {
			return QueryResult{}, err
		}
	}

	// R-443:把 [fromNs, toNs] 区间内每个桶都填齐,缺数据的桶补零值,这样
	// 前端在"选 30 天但只有 7 天数据"的情况下能渲染成连续的 0 曲线而不是
	// 断线连接到首个非零点。step 取决于 useDayBucket / granularity:
	// hour_bucket+default → 1h;hour_bucket+daily → 1d;day_bucket → 1d。
	if includeAuthIndex {
		if err := validateSummaryPointBudgetWithAuthIndex(
			&result,
			fromNs,
			toNs,
			stepNs,
			authIndexFromNs,
			authIndexToNs,
			authIndexStepNs,
		); err != nil {
			return QueryResult{}, err
		}
		authIndexSeries := result.TimeSeriesByAuthIndex
		result.TimeSeriesByAuthIndex = make(map[string][]timePoint)
		if err := padTimeSeries(&result, fromNs, toNs, stepNs); err != nil {
			return QueryResult{}, err
		}
		if err := padNamedTimeSeries(authIndexSeries, authIndexFromNs, authIndexToNs, authIndexStepNs); err != nil {
			return QueryResult{}, err
		}
		result.TimeSeriesByAuthIndex = authIndexSeries
	} else {
		if err := validateSummaryPointBudget(&result, fromNs, toNs, stepNs); err != nil {
			return QueryResult{}, err
		}
		if err := padTimeSeries(&result, fromNs, toNs, stepNs); err != nil {
			return QueryResult{}, err
		}
	}

	if err := tx.Commit(); err != nil {
		return QueryResult{}, fmt.Errorf("usage: commit summary transaction: %w", err)
	}
	committed = true
	return result, nil
}

func ensureSummaryInputBudget(
	ctx context.Context,
	queryer dbQueryer,
	table string,
	fromNs, toNs int64,
	modelF resolvedFilter,
	credF resolvedCredentialFilter,
	apiKeyF resolvedFilter,
) error {
	filterSQL, filterArgs := composeFilterSQL(modelF, credF, apiKeyF, "b.")
	query := "SELECT COUNT(*) FROM (SELECT 1 FROM " + table +
		" b WHERE b.bucket_ts_ns >= ? AND b.bucket_ts_ns <= ?" + filterSQL +
		" LIMIT ?)"
	args := append([]any{fromNs, toNs}, filterArgs...)
	args = append(args, maxSummaryInputRows+1)
	var rows int
	if err := queryer.QueryRowContext(ctx, query, args...).Scan(&rows); err != nil {
		return fmt.Errorf("usage: count summary input rows: %w", err)
	}
	if rows > maxSummaryInputRows {
		return ErrSummaryQueryTooLarge
	}
	return nil
}

func ensureAuthIndexInputBudget(
	ctx context.Context,
	queryer dbQueryer,
	fromNs, toNs int64,
	modelF resolvedFilter,
	credF resolvedCredentialFilter,
	apiKeyF resolvedFilter,
) error {
	filterSQL, filterArgs := composeFilterSQL(modelF, credF, apiKeyF, "e.")
	query := "SELECT COUNT(*) FROM (SELECT 1 FROM events e " +
		"WHERE e.ts_ns >= ? AND e.ts_ns <= ? AND e.auth_index_id IS NOT NULL" + filterSQL +
		" LIMIT ?)"
	args := append([]any{fromNs, toNs}, filterArgs...)
	args = append(args, maxSummaryInputRows+1)
	var rows int
	if err := queryer.QueryRowContext(ctx, query, args...).Scan(&rows); err != nil {
		return fmt.Errorf("usage: count auth-index summary input rows: %w", err)
	}
	if rows > maxSummaryInputRows {
		return ErrSummaryQueryTooLarge
	}
	return nil
}

func summarySeriesLimit(fromNs, toNs, stepNs int64) (int, error) {
	bucketCount, err := paddedTimeSeriesBucketCount(fromNs, toNs, stepNs)
	if err != nil {
		return 0, err
	}
	if bucketCount == 0 || bucketCount > maxSummaryBucketCount {
		return 0, ErrSummaryQueryTooLarge
	}
	limit := int(maxSummaryTotalPoints/bucketCount - 1)
	if limit < 0 {
		return 0, ErrSummaryQueryTooLarge
	}
	if limit > maxSummarySeriesCount {
		limit = maxSummarySeriesCount
	}
	return limit, nil
}

func validateSummaryPointBudget(result *QueryResult, fromNs, toNs, stepNs int64) error {
	bucketCount, err := paddedTimeSeriesBucketCount(fromNs, toNs, stepNs)
	if err != nil {
		return err
	}
	seriesCount := summaryTimeSeriesCount(result)
	if bucketCount > maxSummaryBucketCount || seriesCount > maxSummarySeriesCount {
		return ErrSummaryQueryTooLarge
	}
	if bucketCount > 0 && int64(seriesCount+1) > maxSummaryTotalPoints/bucketCount {
		return ErrSummaryQueryTooLarge
	}
	return nil
}

func validateSummaryPointBudgetWithAuthIndex(
	result *QueryResult,
	fromNs, toNs, stepNs int64,
	authIndexFromNs, authIndexToNs, authIndexStepNs int64,
) error {
	bucketCount, err := paddedTimeSeriesBucketCount(fromNs, toNs, stepNs)
	if err != nil {
		return err
	}
	authIndexBucketCount, err := paddedTimeSeriesBucketCount(
		authIndexFromNs,
		authIndexToNs,
		authIndexStepNs,
	)
	if err != nil {
		return err
	}
	regularSeriesCount := len(result.TimeSeriesByModel) + len(result.TimeSeriesByCredential) +
		len(result.TimeSeriesByAPIKey) + 1
	authIndexSeriesCount := len(result.TimeSeriesByAuthIndex)
	if bucketCount > maxSummaryBucketCount || authIndexBucketCount > maxSummaryBucketCount ||
		regularSeriesCount+authIndexSeriesCount-1 > maxSummarySeriesCount {
		return ErrSummaryQueryTooLarge
	}
	if bucketCount > 0 && int64(regularSeriesCount) > maxSummaryTotalPoints/bucketCount {
		return ErrSummaryQueryTooLarge
	}
	regularPoints := int64(regularSeriesCount) * bucketCount
	remainingPoints := int64(maxSummaryTotalPoints) - regularPoints
	if authIndexBucketCount > 0 && int64(authIndexSeriesCount) > remainingPoints/authIndexBucketCount {
		return ErrSummaryQueryTooLarge
	}
	return nil
}

func summaryDimensionCount(result *QueryResult) int {
	if result == nil {
		return 0
	}
	return len(result.ByModel) + len(result.ByCredential) + len(result.ByAPIKey) + len(result.ByAuthIndex)
}

func summaryTimeSeriesCount(result *QueryResult) int {
	if result == nil {
		return 0
	}
	return len(result.TimeSeriesByModel) + len(result.TimeSeriesByCredential) + len(result.TimeSeriesByAPIKey) +
		len(result.TimeSeriesByAuthIndex)
}

func paddedTimeSeriesBucketCount(fromNs, toNs, stepNs int64) (int64, error) {
	if stepNs <= 0 {
		return 0, errors.New("usage: time-series step must be positive")
	}
	var err error
	fromNs, err = floorUnixNano(fromNs, stepNs)
	if err != nil {
		return 0, fmt.Errorf("usage: summary start: %w", err)
	}
	toNs, err = floorUnixNano(toNs, stepNs)
	if err != nil {
		return 0, fmt.Errorf("usage: summary end: %w", err)
	}
	return timeSeriesBucketCount(fromNs, toNs, stepNs)
}

func timeSeriesBucketCount(fromNs, toNs, stepNs int64) (int64, error) {
	if stepNs <= 0 {
		return 0, errors.New("usage: time-series step must be positive")
	}
	if fromNs > toNs {
		return 0, nil
	}
	if fromNs < 0 && toNs > math.MaxInt64+fromNs {
		return 0, ErrSummaryQueryTooLarge
	}
	return (toNs-fromNs)/stepNs + 1, nil
}

// padTimeSeries 把 from/to 内的所有桶补齐为零值;TimeSeries 与所有
// TimeSeriesBy* map 都会被处理。零值桶用 timePoint{Time: bucket-start}
// 填充,前端依据 Time 字段对齐 X 轴。
func padTimeSeries(result *QueryResult, fromNs, toNs, stepNs int64) error {
	if result == nil || fromNs > toNs {
		return nil
	}
	pad, err := timeSeriesPadder(fromNs, toNs, stepNs)
	if err != nil {
		return err
	}

	result.TimeSeries = pad(result.TimeSeries)
	for _, series := range []map[string][]timePoint{
		result.TimeSeriesByModel,
		result.TimeSeriesByCredential,
		result.TimeSeriesByAPIKey,
		result.TimeSeriesByAuthIndex,
	} {
		for key, points := range series {
			series[key] = pad(points)
		}
	}
	return nil
}

func padNamedTimeSeries(series map[string][]timePoint, fromNs, toNs, stepNs int64) error {
	if fromNs > toNs {
		return nil
	}
	pad, err := timeSeriesPadder(fromNs, toNs, stepNs)
	if err != nil {
		return err
	}
	for key, points := range series {
		series[key] = pad(points)
	}
	return nil
}

func timeSeriesPadder(fromNs, toNs, stepNs int64) (func([]timePoint) []timePoint, error) {
	if stepNs <= 0 {
		return nil, errors.New("usage: time-series step must be positive")
	}
	var err error
	fromNs, err = floorUnixNano(fromNs, stepNs)
	if err != nil {
		return nil, fmt.Errorf("usage: pad time-series start: %w", err)
	}
	toNs, err = floorUnixNano(toNs, stepNs)
	if err != nil {
		return nil, fmt.Errorf("usage: pad time-series end: %w", err)
	}
	bucketCount, err := timeSeriesBucketCount(fromNs, toNs, stepNs)
	if err != nil {
		return nil, err
	}

	return func(series []timePoint) []timePoint {
		existing := make(map[int64]timePoint, len(series))
		for _, point := range series {
			timestamp, parseErr := time.Parse(time.RFC3339, point.Time)
			if parseErr == nil {
				existing[timestamp.UTC().UnixNano()] = point
			}
		}
		out := make([]timePoint, 0, bucketCount)
		for timestamp := fromNs; ; timestamp += stepNs {
			if point, ok := existing[timestamp]; ok {
				out = append(out, point)
			} else {
				out = append(out, timePoint{Time: time.Unix(0, timestamp).UTC().Format(time.RFC3339)})
			}
			if timestamp >= toNs || timestamp > math.MaxInt64-stepNs {
				break
			}
		}
		return out
	}, nil
}

// emptyQueryResult mimics summary.go's "make-all-maps" initialization so the
// JSON response never contains nil maps, which would serialize as `null` and
// confuse the frontend's `Object.keys(...)` checks.
func emptyQueryResult() QueryResult {
	return QueryResult{
		ByModel:                make(map[string]*summaryModelStats),
		ByCredential:           make(map[string]*summaryCredentialStats),
		ByAPIKey:               make(map[string]*summaryAPIKeyStats),
		ByAuthIndex:            make(map[string]*summaryAPIKeyStats),
		TimeSeriesByModel:      make(map[string][]timePoint),
		TimeSeriesByCredential: make(map[string][]timePoint),
		TimeSeriesByAPIKey:     make(map[string][]timePoint),
		TimeSeriesByAuthIndex:  make(map[string][]timePoint),
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
	queryer dbQueryer,
	table string,
	fromNs, toNs int64,
	modelF resolvedFilter, credF resolvedCredentialFilter, apiKeyF resolvedFilter,
	result *QueryResult,
) error {
	filterSQL, filterArgs := composeFilterSQL(modelF, credF, apiKeyF, "")

	q := "SELECT " +
		"COALESCE(SUM(requests),0), COALESCE(SUM(success),0), COALESCE(SUM(failure),0), " +
		"COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0), COALESCE(SUM(reasoning_tokens),0), " +
		"COALESCE(SUM(cached_tokens),0), COALESCE(SUM(cache_read_tokens),0), " +
		"COALESCE(SUM(cache_write_tokens),0), COALESCE(SUM(total_tokens),0), COALESCE(SUM(cost_micro),0), " +
		"COALESCE(SUM(complete_requests),0), COALESCE(SUM(partial_requests),0), " +
		"COALESCE(SUM(unpriced_requests),0), COALESCE(SUM(complete_tokens),0), " +
		"COALESCE(SUM(partial_tokens),0), COALESCE(SUM(unpriced_tokens),0), " +
		"COALESCE(SUM(latency_ms_total),0), COALESCE(SUM(latency_samples),0) " +
		"FROM " + table + " WHERE bucket_ts_ns >= ? AND bucket_ts_ns <= ?" + filterSQL

	args := append([]any{fromNs, toNs}, filterArgs...)
	row := queryer.QueryRowContext(ctx, q, args...)

	var totals summaryTotals
	var costMicro int64
	if err := row.Scan(
		&totals.Requests, &totals.Success, &totals.Failure,
		&totals.Tokens.InputTokens, &totals.Tokens.OutputTokens, &totals.Tokens.ReasoningTokens,
		&totals.Tokens.CachedTokens, &totals.Tokens.CacheReadTokens, &totals.Tokens.CacheWriteTokens,
		&totals.Tokens.TotalTokens,
		&costMicro,
		&totals.CostCoverage.Complete.Requests,
		&totals.CostCoverage.Partial.Requests,
		&totals.CostCoverage.Unpriced.Requests,
		&totals.CostCoverage.Complete.Tokens,
		&totals.CostCoverage.Partial.Tokens,
		&totals.CostCoverage.Unpriced.Tokens,
		&totals.Latency.TotalMs, &totals.Latency.SampleCount,
	); err != nil {
		return fmt.Errorf("queryTotals: %w", err)
	}
	totals.Cost = float64(costMicro) / costMicroPerUSD
	result.Totals = totals
	return nil
}

func (s *Store) queryByModel(
	ctx context.Context,
	queryer dbQueryer,
	table string,
	fromNs, toNs int64,
	modelF resolvedFilter, credF resolvedCredentialFilter, apiKeyF resolvedFilter,
	result *QueryResult,
) error {
	filterSQL, filterArgs := composeFilterSQL(modelF, credF, apiKeyF, "b.")

	q := "SELECT m.name, " +
		"SUM(b.requests), SUM(b.success), SUM(b.failure), " +
		"SUM(b.input_tokens), SUM(b.output_tokens), SUM(b.reasoning_tokens), SUM(b.cached_tokens), " +
		"SUM(b.cache_read_tokens), SUM(b.cache_write_tokens), SUM(b.total_tokens), " +
		"SUM(b.cost_micro), SUM(b.latency_ms_total), SUM(b.latency_samples) " +
		"FROM " + table + " b JOIN dim_model m ON m.id = b.model_id " +
		"WHERE b.bucket_ts_ns >= ? AND b.bucket_ts_ns <= ?" + filterSQL +
		" GROUP BY m.id, m.name"

	args := append([]any{fromNs, toNs}, filterArgs...)
	rows, err := queryer.QueryContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("queryByModel: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var name string
		var ms summaryModelStats
		var costMicro int64
		if err := rows.Scan(
			&name,
			&ms.Requests, &ms.Success, &ms.Failure,
			&ms.Tokens.InputTokens, &ms.Tokens.OutputTokens, &ms.Tokens.ReasoningTokens,
			&ms.Tokens.CachedTokens, &ms.Tokens.CacheReadTokens, &ms.Tokens.CacheWriteTokens,
			&ms.Tokens.TotalTokens,
			&costMicro,
			&ms.Latency.TotalMs, &ms.Latency.SampleCount,
		); err != nil {
			return fmt.Errorf("queryByModel scan: %w", err)
		}
		ms.Cost = float64(costMicro) / costMicroPerUSD
		if _, exists := result.ByModel[name]; !exists && summaryDimensionCount(result) >= maxSummarySeriesCount {
			return ErrSummaryQueryTooLarge
		}
		result.ByModel[name] = &ms
	}
	return rows.Err()
}

func (s *Store) queryByCredential(
	ctx context.Context,
	queryer dbQueryer,
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
	rows, err := queryer.QueryContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("queryByCredential: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var credName, providerName string
		var cs summaryCredentialStats
		if err := rows.Scan(&credName, &providerName, &cs.Success, &cs.Failure); err != nil {
			return fmt.Errorf("queryByCredential scan: %w", err)
		}
		cs.Source = credName
		cs.Provider = providerName
		key := credentialMapKey(providerName, credName)
		if _, exists := result.ByCredential[key]; !exists && summaryDimensionCount(result) >= maxSummarySeriesCount {
			return ErrSummaryQueryTooLarge
		}
		result.ByCredential[key] = &cs
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
	queryer dbQueryer,
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
		"SUM(b.input_tokens), SUM(b.output_tokens), SUM(b.reasoning_tokens), SUM(b.cached_tokens), " +
		"SUM(b.cache_read_tokens), SUM(b.cache_write_tokens), SUM(b.total_tokens), " +
		"SUM(b.cost_micro), SUM(b.latency_ms_total), SUM(b.latency_samples) " +
		"FROM " + table + " b LEFT JOIN dim_api_key k ON k.id = b.api_key_id " +
		"WHERE b.bucket_ts_ns >= ? AND b.bucket_ts_ns <= ?" + filterSQL +
		" GROUP BY api_key_name"

	args := append([]any{fromNs, toNs}, filterArgs...)
	rows, err := queryer.QueryContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("queryByAPIKey: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var name string
		var aks summaryAPIKeyStats
		var costMicro int64
		if err := rows.Scan(
			&name,
			&aks.Requests, &aks.Success, &aks.Failure,
			&aks.Tokens.InputTokens, &aks.Tokens.OutputTokens, &aks.Tokens.ReasoningTokens,
			&aks.Tokens.CachedTokens, &aks.Tokens.CacheReadTokens, &aks.Tokens.CacheWriteTokens,
			&aks.Tokens.TotalTokens,
			&costMicro,
			&aks.Latency.TotalMs, &aks.Latency.SampleCount,
		); err != nil {
			return fmt.Errorf("queryByAPIKey scan: %w", err)
		}
		aks.Cost = float64(costMicro) / costMicroPerUSD
		if _, exists := result.ByAPIKey[name]; !exists && summaryDimensionCount(result) >= maxSummarySeriesCount {
			return ErrSummaryQueryTooLarge
		}
		result.ByAPIKey[name] = &aks
	}
	return rows.Err()
}

func (s *Store) queryByAuthIndex(
	ctx context.Context,
	queryer dbQueryer,
	fromNs, toNs int64,
	modelF resolvedFilter, credF resolvedCredentialFilter, apiKeyF resolvedFilter,
	result *QueryResult,
) error {
	filterSQL, filterArgs := composeFilterSQL(modelF, credF, apiKeyF, "e.")
	q := "SELECT a.name, COUNT(*), " +
		"SUM(CASE WHEN e.failed = 0 THEN 1 ELSE 0 END), SUM(e.failed), " +
		"SUM(e.input_tokens), SUM(e.output_tokens), SUM(e.reasoning_tokens), SUM(e.cached_tokens), " +
		"SUM(e.cache_read_tokens), SUM(e.cache_write_tokens), SUM(e.total_tokens), SUM(e.cost_micro) " +
		"FROM events e JOIN dim_auth_index a ON a.id = e.auth_index_id " +
		"WHERE e.ts_ns >= ? AND e.ts_ns <= ?" + filterSQL +
		" GROUP BY a.id, a.name"

	args := append([]any{fromNs, toNs}, filterArgs...)
	rows, err := queryer.QueryContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("queryByAuthIndex: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var name string
		var stats summaryAPIKeyStats
		var costMicro int64
		if err = rows.Scan(
			&name,
			&stats.Requests, &stats.Success, &stats.Failure,
			&stats.Tokens.InputTokens, &stats.Tokens.OutputTokens, &stats.Tokens.ReasoningTokens,
			&stats.Tokens.CachedTokens, &stats.Tokens.CacheReadTokens, &stats.Tokens.CacheWriteTokens,
			&stats.Tokens.TotalTokens, &costMicro,
		); err != nil {
			return fmt.Errorf("queryByAuthIndex scan: %w", err)
		}
		stats.Cost = float64(costMicro) / costMicroPerUSD
		if _, exists := result.ByAuthIndex[name]; !exists && summaryDimensionCount(result) >= maxSummarySeriesCount {
			return ErrSummaryQueryTooLarge
		}
		result.ByAuthIndex[name] = &stats
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
	queryer dbQueryer,
	table string,
	fromNs, toNs int64,
	modelF resolvedFilter, credF resolvedCredentialFilter, apiKeyF resolvedFilter,
	timeExpr string,
	result *QueryResult,
) error {
	filterSQL, filterArgs := composeFilterSQL(modelF, credF, apiKeyF, "")

	q := "SELECT " + timeExpr + " AS ts, " +
		"SUM(requests), SUM(success), SUM(failure), " +
		"SUM(input_tokens), SUM(output_tokens), SUM(reasoning_tokens), SUM(cached_tokens), " +
		"SUM(cache_read_tokens), SUM(cache_write_tokens), SUM(total_tokens), " +
		"SUM(cost_micro) " +
		"FROM " + table + " WHERE bucket_ts_ns >= ? AND bucket_ts_ns <= ?" + filterSQL +
		" GROUP BY ts ORDER BY ts"

	args := append([]any{fromNs, toNs}, filterArgs...)
	rows, err := queryer.QueryContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("queryTimeSeries: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var ts int64
		var tp timePoint
		var costMicro int64
		if err := rows.Scan(
			&ts,
			&tp.Requests, &tp.Success, &tp.Failure,
			&tp.Tokens.InputTokens, &tp.Tokens.OutputTokens, &tp.Tokens.ReasoningTokens,
			&tp.Tokens.CachedTokens, &tp.Tokens.CacheReadTokens, &tp.Tokens.CacheWriteTokens,
			&tp.Tokens.TotalTokens,
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
	queryer dbQueryer,
	table string,
	fromNs, toNs int64,
	modelF resolvedFilter, credF resolvedCredentialFilter, apiKeyF resolvedFilter,
	timeExpr string,
	seriesLimit int,
	result *QueryResult,
) error {
	filterSQL, filterArgs := composeFilterSQL(modelF, credF, apiKeyF, "b.")
	tsExpr := strings.ReplaceAll(timeExpr, "bucket_ts_ns", "b.bucket_ts_ns")

	q := "SELECT m.name, " + tsExpr + " AS ts, " +
		"SUM(b.requests), SUM(b.success), SUM(b.failure), " +
		"SUM(b.input_tokens), SUM(b.output_tokens), SUM(b.reasoning_tokens), SUM(b.cached_tokens), " +
		"SUM(b.cache_read_tokens), SUM(b.cache_write_tokens), SUM(b.total_tokens), " +
		"SUM(b.cost_micro) " +
		"FROM " + table + " b JOIN dim_model m ON m.id = b.model_id " +
		"WHERE b.bucket_ts_ns >= ? AND b.bucket_ts_ns <= ?" + filterSQL +
		" GROUP BY m.id, m.name, ts ORDER BY m.name, ts"

	args := append([]any{fromNs, toNs}, filterArgs...)
	rows, err := queryer.QueryContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("queryTimeSeriesByModel: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var name string
		var ts int64
		var tp timePoint
		var costMicro int64
		if err := rows.Scan(
			&name, &ts,
			&tp.Requests, &tp.Success, &tp.Failure,
			&tp.Tokens.InputTokens, &tp.Tokens.OutputTokens, &tp.Tokens.ReasoningTokens,
			&tp.Tokens.CachedTokens, &tp.Tokens.CacheReadTokens, &tp.Tokens.CacheWriteTokens,
			&tp.Tokens.TotalTokens,
			&costMicro,
		); err != nil {
			return fmt.Errorf("queryTimeSeriesByModel scan: %w", err)
		}
		tp.Time = time.Unix(0, ts).UTC().Format(time.RFC3339)
		tp.Cost = float64(costMicro) / costMicroPerUSD
		if _, exists := result.TimeSeriesByModel[name]; !exists && summaryTimeSeriesCount(result) >= seriesLimit {
			return ErrSummaryQueryTooLarge
		}
		result.TimeSeriesByModel[name] = append(result.TimeSeriesByModel[name], tp)
	}
	return rows.Err()
}

func (s *Store) queryTimeSeriesByCredential(
	ctx context.Context,
	queryer dbQueryer,
	table string,
	fromNs, toNs int64,
	modelF resolvedFilter, credF resolvedCredentialFilter, apiKeyF resolvedFilter,
	timeExpr string,
	seriesLimit int,
	result *QueryResult,
) error {
	filterSQL, filterArgs := composeFilterSQL(modelF, credF, apiKeyF, "b.")
	tsExpr := strings.ReplaceAll(timeExpr, "bucket_ts_ns", "b.bucket_ts_ns")

	// cost_micro is summed even though the byCredential aggregate (totals card)
	// drops it: the chart switcher can plot the reference amount per credential.
	// Without SUM(cost_micro), every point would incorrectly return zero.
	//
	// provider participates in the GROUP BY so each (provider, credential)
	// tuple gets its own time series — keys must match queryByCredential
	// exactly so the FilterBar dropdown and the chart legend stay aligned.
	q := "SELECT c.name, COALESCE(p.name, '') AS provider_name, " + tsExpr + " AS ts, " +
		"SUM(b.requests), SUM(b.success), SUM(b.failure), " +
		"SUM(b.input_tokens), SUM(b.output_tokens), SUM(b.reasoning_tokens), SUM(b.cached_tokens), " +
		"SUM(b.cache_read_tokens), SUM(b.cache_write_tokens), SUM(b.total_tokens), " +
		"SUM(b.cost_micro) " +
		"FROM " + table + " b JOIN dim_credential c ON c.id = b.credential_id " +
		"LEFT JOIN dim_provider p ON p.id = b.provider_id " +
		"WHERE b.bucket_ts_ns >= ? AND b.bucket_ts_ns <= ? AND b.credential_id IS NOT NULL" + filterSQL +
		" GROUP BY c.id, c.name, p.id, provider_name, ts ORDER BY c.name, provider_name, ts"

	args := append([]any{fromNs, toNs}, filterArgs...)
	rows, err := queryer.QueryContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("queryTimeSeriesByCredential: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var credName, providerName string
		var ts int64
		var tp timePoint
		var costMicro int64
		if err := rows.Scan(
			&credName, &providerName, &ts,
			&tp.Requests, &tp.Success, &tp.Failure,
			&tp.Tokens.InputTokens, &tp.Tokens.OutputTokens, &tp.Tokens.ReasoningTokens,
			&tp.Tokens.CachedTokens, &tp.Tokens.CacheReadTokens, &tp.Tokens.CacheWriteTokens,
			&tp.Tokens.TotalTokens,
			&costMicro,
		); err != nil {
			return fmt.Errorf("queryTimeSeriesByCredential scan: %w", err)
		}
		tp.Time = time.Unix(0, ts).UTC().Format(time.RFC3339)
		tp.Cost = float64(costMicro) / costMicroPerUSD
		key := credentialMapKey(providerName, credName)
		if _, exists := result.TimeSeriesByCredential[key]; !exists && summaryTimeSeriesCount(result) >= seriesLimit {
			return ErrSummaryQueryTooLarge
		}
		result.TimeSeriesByCredential[key] = append(result.TimeSeriesByCredential[key], tp)
	}
	return rows.Err()
}

func (s *Store) queryTimeSeriesByAPIKey(
	ctx context.Context,
	queryer dbQueryer,
	table string,
	fromNs, toNs int64,
	modelF resolvedFilter, credF resolvedCredentialFilter, apiKeyF resolvedFilter,
	timeExpr string,
	seriesLimit int,
	result *QueryResult,
) error {
	filterSQL, filterArgs := composeFilterSQL(modelF, credF, apiKeyF, "b.")
	tsExpr := strings.ReplaceAll(timeExpr, "bucket_ts_ns", "b.bucket_ts_ns")

	// Same conservation logic as queryByAPIKey — LEFT JOIN with COALESCE so
	// the "by_api_key" time series sums equal the headline time_series totals.
	q := "SELECT COALESCE(k.name, '(none)') AS api_key_name, " + tsExpr + " AS ts, " +
		"SUM(b.requests), SUM(b.success), SUM(b.failure), " +
		"SUM(b.input_tokens), SUM(b.output_tokens), SUM(b.reasoning_tokens), SUM(b.cached_tokens), " +
		"SUM(b.cache_read_tokens), SUM(b.cache_write_tokens), SUM(b.total_tokens), " +
		"SUM(b.cost_micro) " +
		"FROM " + table + " b LEFT JOIN dim_api_key k ON k.id = b.api_key_id " +
		"WHERE b.bucket_ts_ns >= ? AND b.bucket_ts_ns <= ?" + filterSQL +
		" GROUP BY api_key_name, ts ORDER BY api_key_name, ts"

	args := append([]any{fromNs, toNs}, filterArgs...)
	rows, err := queryer.QueryContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("queryTimeSeriesByAPIKey: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var name string
		var ts int64
		var tp timePoint
		var costMicro int64
		if err := rows.Scan(
			&name, &ts,
			&tp.Requests, &tp.Success, &tp.Failure,
			&tp.Tokens.InputTokens, &tp.Tokens.OutputTokens, &tp.Tokens.ReasoningTokens,
			&tp.Tokens.CachedTokens, &tp.Tokens.CacheReadTokens, &tp.Tokens.CacheWriteTokens,
			&tp.Tokens.TotalTokens,
			&costMicro,
		); err != nil {
			return fmt.Errorf("queryTimeSeriesByAPIKey scan: %w", err)
		}
		tp.Time = time.Unix(0, ts).UTC().Format(time.RFC3339)
		tp.Cost = float64(costMicro) / costMicroPerUSD
		if _, exists := result.TimeSeriesByAPIKey[name]; !exists && summaryTimeSeriesCount(result) >= seriesLimit {
			return ErrSummaryQueryTooLarge
		}
		result.TimeSeriesByAPIKey[name] = append(result.TimeSeriesByAPIKey[name], tp)
	}
	return rows.Err()
}

func (s *Store) queryTimeSeriesByAuthIndex(
	ctx context.Context,
	queryer dbQueryer,
	fromNs, toNs int64,
	modelF resolvedFilter, credF resolvedCredentialFilter, apiKeyF resolvedFilter,
	stepNs int64,
	seriesLimit int,
	result *QueryResult,
) error {
	filterSQL, filterArgs := composeFilterSQL(modelF, credF, apiKeyF, "e.")
	tsExpr := sqliteFloorBucketExpr("e.ts_ns", stepNs)
	q := "SELECT a.name, " + tsExpr + " AS ts, COUNT(*), " +
		"SUM(CASE WHEN e.failed = 0 THEN 1 ELSE 0 END), SUM(e.failed), " +
		"SUM(e.input_tokens), SUM(e.output_tokens), SUM(e.reasoning_tokens), SUM(e.cached_tokens), " +
		"SUM(e.cache_read_tokens), SUM(e.cache_write_tokens), SUM(e.total_tokens), SUM(e.cost_micro) " +
		"FROM events e JOIN dim_auth_index a ON a.id = e.auth_index_id " +
		"WHERE e.ts_ns >= ? AND e.ts_ns <= ?" + filterSQL +
		" GROUP BY a.id, a.name, ts ORDER BY a.name, ts"

	args := append([]any{fromNs, toNs}, filterArgs...)
	rows, err := queryer.QueryContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("queryTimeSeriesByAuthIndex: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var name string
		var ts int64
		var point timePoint
		var costMicro int64
		if err = rows.Scan(
			&name, &ts,
			&point.Requests, &point.Success, &point.Failure,
			&point.Tokens.InputTokens, &point.Tokens.OutputTokens, &point.Tokens.ReasoningTokens,
			&point.Tokens.CachedTokens, &point.Tokens.CacheReadTokens, &point.Tokens.CacheWriteTokens,
			&point.Tokens.TotalTokens, &costMicro,
		); err != nil {
			return fmt.Errorf("queryTimeSeriesByAuthIndex scan: %w", err)
		}
		point.Time = time.Unix(0, ts).UTC().Format(time.RFC3339)
		point.Cost = float64(costMicro) / costMicroPerUSD
		if _, exists := result.TimeSeriesByAuthIndex[name]; !exists && summaryTimeSeriesCount(result) >= seriesLimit {
			return ErrSummaryQueryTooLarge
		}
		result.TimeSeriesByAuthIndex[name] = append(result.TimeSeriesByAuthIndex[name], point)
	}
	return rows.Err()
}

// queryEventDetails serves /usage/events. Pagination is SQL-native (LIMIT/OFFSET) so
// large windows don't pull every row into memory the way DetailStore.QueryRange
// did — that "read everything then slice" pattern was the OOM accomplice in
// the JSON era and explicitly cannot be reintroduced.
func (s *Store) queryEventDetails(
	ctx context.Context,
	from, to time.Time,
	filters EventFilters,
	page, pageSize int,
	sortField string,
	desc bool,
) ([]FlatDetail, int, error) {
	return s.queryEventsPage(ctx, from, to, filters, page, pageSize, sortField, desc, false)
}

// queryCompactEventDetails 只读取请求列表需要的字段，默认 queryEventDetails 响应继续供导出和旧客户端使用
func (s *Store) queryCompactEventDetails(
	ctx context.Context,
	from, to time.Time,
	filters EventFilters,
	page, pageSize int,
	sortField string,
	desc bool,
) ([]FlatDetail, int, error) {
	return s.queryEventsPage(ctx, from, to, filters, page, pageSize, sortField, desc, true)
}

func (s *Store) queryEventsPage(
	ctx context.Context,
	from, to time.Time,
	filters EventFilters,
	page, pageSize int,
	sortField string,
	desc bool,
	compact bool,
) ([]FlatDetail, int, error) {
	if s == nil || s.db == nil {
		return nil, 0, fmt.Errorf("usage: query events on nil store")
	}
	if !from.Before(to) {
		return nil, 0, nil
	}
	if pageSize <= 0 {
		pageSize = 100
	} else if pageSize > MaxEventsPageSize {
		pageSize = MaxEventsPageSize
	}
	if page < 1 {
		page = 1
	}
	if page-1 > maxEventsOffset/pageSize {
		return nil, 0, ErrEventsPageTooDeep
	}
	offset := (page - 1) * pageSize
	if offset > maxEventsOffset {
		return nil, 0, ErrEventsPageTooDeep
	}
	fromNs, err := unixNanoUTC(from)
	if err != nil {
		return nil, 0, fmt.Errorf("usage: invalid events start: %w", err)
	}
	toNs, err := unixNanoUTC(to)
	if err != nil {
		return nil, 0, fmt.Errorf("usage: invalid events end: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, 0, fmt.Errorf("usage: begin events transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	queryPage := s.queryEvents
	if compact {
		queryPage = s.queryCompactEvents
	}
	events, total, err := queryPage(
		ctx,
		tx,
		fromNs,
		toNs,
		filters,
		pageSize,
		offset,
		sortField,
		desc,
	)
	if err != nil {
		return nil, 0, err
	}
	if err = tx.Commit(); err != nil {
		return nil, 0, fmt.Errorf("usage: commit events transaction: %w", err)
	}
	committed = true
	return events, total, nil
}

type eventsPageQuery struct {
	whereSQL  string
	whereArgs []any
	orderCol  string
	orderDir  string
	total     int
}

func prepareEventsPageQuery(
	ctx context.Context,
	queryer dbQueryer,
	fromNs, toNs int64,
	filters EventFilters,
	sortField string,
	desc bool,
) (eventsPageQuery, error) {
	modelF, err := resolveNamesToIDs(ctx, queryer, "dim_model", filters.Model)
	if err != nil {
		return eventsPageQuery{}, err
	}
	credF, err := resolveCredentialFilter(ctx, queryer, filters.Source)
	if err != nil {
		return eventsPageQuery{}, err
	}
	apiKeyF, err := resolveFilterNamesToIDs(ctx, queryer, "dim_api_key", filters.APIKey, filters.APIKeys)
	if err != nil {
		return eventsPageQuery{}, err
	}
	if (modelF.active && !modelF.matchedAny) ||
		(credF.active && !credF.matchedAny) ||
		(apiKeyF.active && !apiKeyF.matchedAny) {
		return eventsPageQuery{}, nil
	}

	filterSQL, filterArgs := composeFilterSQL(modelF, credF, apiKeyF, "")
	switch filters.Status {
	case "success":
		filterSQL += " AND failed = 0"
	case "failure":
		filterSQL += " AND failed = 1"
	}

	searchSQL := ""
	var searchArgs []any
	if query := strings.TrimSpace(filters.Search); query != "" {
		searchSQL = " AND (" +
			"EXISTS (SELECT 1 FROM dim_model m WHERE m.id = events.model_id AND m.name LIKE ?) " +
			"OR EXISTS (SELECT 1 FROM dim_credential c WHERE c.id = events.credential_id AND c.name LIKE ?) " +
			"OR EXISTS (SELECT 1 FROM dim_source s WHERE s.id = events.source_id AND s.name LIKE ?) " +
			"OR EXISTS (SELECT 1 FROM dim_auth_index a WHERE a.id = events.auth_index_id AND a.name LIKE ?) " +
			"OR EXISTS (SELECT 1 FROM dim_api_key k WHERE k.id = events.api_key_id AND k.name LIKE ?)" +
			")"
		pattern := "%" + query + "%"
		searchArgs = []any{pattern, pattern, pattern, pattern, pattern}
	}

	result := eventsPageQuery{
		whereSQL:  "WHERE events.ts_ns >= ? AND events.ts_ns <= ?" + filterSQL + searchSQL,
		whereArgs: append(append([]any{fromNs, toNs}, filterArgs...), searchArgs...),
		orderCol:  "events.ts_ns",
		orderDir:  "ASC",
	}
	switch sortField {
	case "model":
		result.orderCol = "(SELECT name FROM dim_model WHERE id = events.model_id)"
	case "tokens":
		result.orderCol = "events.total_tokens"
	}
	if desc {
		result.orderDir = "DESC"
	}

	startedAt := time.Now()
	if err = queryer.QueryRowContext(
		ctx,
		"SELECT COUNT(*) FROM events "+result.whereSQL,
		result.whereArgs...,
	).Scan(&result.total); err != nil {
		return eventsPageQuery{}, fmt.Errorf("count events: %w", err)
	}
	log.Debugf("usage: events count query took %v", time.Since(startedAt))
	return result, nil
}

func (s *Store) queryCompactEvents(
	ctx context.Context,
	queryer dbQueryer,
	fromNs, toNs int64,
	filters EventFilters,
	pageSize, offset int,
	sortField string,
	desc bool,
) ([]FlatDetail, int, error) {
	pageQuery, err := prepareEventsPageQuery(
		ctx,
		queryer,
		fromNs,
		toNs,
		filters,
		sortField,
		desc,
	)
	if err != nil {
		return nil, 0, err
	}
	if pageQuery.total == 0 {
		return nil, 0, nil
	}

	metadataValue := func(path string) string {
		return "CASE WHEN json_valid(events.metadata) THEN json_extract(events.metadata, '" + path + "') END"
	}
	metadataState := "CASE WHEN events.metadata IS NULL OR events.metadata = '' THEN 0 " +
		"WHEN json_valid(events.metadata) THEN 1 ELSE -1 END"
	dataQ := "SELECT events.ts_ns, " +
		"(SELECT name FROM dim_model      WHERE id = events.model_id), " +
		"(SELECT name FROM dim_source     WHERE id = events.source_id), " +
		"(SELECT name FROM dim_auth_index WHERE id = events.auth_index_id), " +
		"(SELECT name FROM dim_api_key    WHERE id = events.api_key_id), " +
		"(SELECT name FROM dim_provider   WHERE id = events.provider_id), " +
		"events.input_tokens, events.output_tokens, events.reasoning_tokens, events.cached_tokens, " +
		"events.cache_read_tokens, events.cache_write_tokens, events.total_tokens, " +
		"events.cost_micro, events.cost_status, events.cost_reason, events.pricing_receipt, events.failed, " +
		metadataState + ", " +
		metadataValue("$.latency_ms") + ", " +
		metadataValue("$.request_id") + ", " +
		metadataValue("$.time_to_first_byte_ms") + ", " +
		metadataValue("$.total_duration_ms") + ", " +
		metadataValue("$.completed") + ", " +
		metadataValue("$.thinking") + ", " +
		"request_log.state, request_log.home_ip " +
		"FROM events" + requestLogArtifactJoinSQL + " " + pageQuery.whereSQL +
		" ORDER BY " + pageQuery.orderCol + " " + pageQuery.orderDir +
		" LIMIT ? OFFSET ?"
	dataArgs := append([]any{}, pageQuery.whereArgs...)
	dataArgs = append(dataArgs, pageSize, offset)

	startedAt := time.Now()
	rows, err := queryer.QueryContext(ctx, dataQ, dataArgs...)
	if err != nil {
		return nil, 0, fmt.Errorf("query compact events: %w", err)
	}
	log.Debugf("usage: compact events page query took %v", time.Since(startedAt))

	startedAt = time.Now()
	out := make([]FlatDetail, 0, pageSize)
	for rows.Next() {
		var detail FlatDetail
		var tsNs int64
		var modelName, sourceName, authIndexName, apiKeyName, providerName sql.NullString
		var metadataState int64
		var latencyMs, timeToFirstByteMs, totalDurationMs, completed sql.NullInt64
		var requestID, thinking, pricingReceipt, artifactState, artifactHomeIP sql.NullString
		var failed int64
		if err = rows.Scan(
			&tsNs,
			&modelName,
			&sourceName,
			&authIndexName,
			&apiKeyName,
			&providerName,
			&detail.Tokens.InputTokens,
			&detail.Tokens.OutputTokens,
			&detail.Tokens.ReasoningTokens,
			&detail.Tokens.CachedTokens,
			&detail.Tokens.CacheReadTokens,
			&detail.Tokens.CacheWriteTokens,
			&detail.Tokens.TotalTokens,
			&detail.costMicro,
			&detail.CostStatus,
			&detail.CostReason,
			&pricingReceipt,
			&failed,
			&metadataState,
			&latencyMs,
			&requestID,
			&timeToFirstByteMs,
			&totalDurationMs,
			&completed,
			&thinking,
			&artifactState,
			&artifactHomeIP,
		); err != nil {
			_ = rows.Close()
			return nil, 0, fmt.Errorf("scan compact event row: %w", err)
		}
		if metadataState < 0 {
			_ = rows.Close()
			return nil, 0, fmt.Errorf("decode compact event metadata at ts_ns=%d: invalid JSON", tsNs)
		}

		detail.Timestamp = time.Unix(0, tsNs).UTC()
		detail.Model = modelName.String
		detail.Source = sourceName.String
		detail.AuthIndex = authIndexName.String
		detail.APIKey = apiKeyName.String
		detail.Provider = providerName.String
		detail.PricingReceipt, err = decodePricingReceipt(pricingReceipt)
		if err != nil {
			_ = rows.Close()
			return nil, 0, fmt.Errorf("decode compact event pricing receipt at ts_ns=%d: %w", tsNs, err)
		}
		detail.Cost = float64(detail.costMicro) / costMicroPerUSD
		detail.costTrusted = true
		detail.Failed = failed != 0
		detail.MetadataRecorded = metadataState == 1
		if latencyMs.Valid {
			detail.LatencyMs = latencyMs.Int64
		}
		detail.RequestID = strings.TrimSpace(requestID.String)
		if timeToFirstByteMs.Valid {
			detail.TimeToFirstByteMs = timeToFirstByteMs.Int64
		}
		if totalDurationMs.Valid {
			detail.TotalDurationMs = totalDurationMs.Int64
		}
		detail.Completed = completed.Valid && completed.Int64 != 0
		if thinking.Valid && thinking.String != "" {
			var value Thinking
			if err = json.Unmarshal([]byte(thinking.String), &value); err != nil {
				_ = rows.Close()
				return nil, 0, fmt.Errorf("decode compact event thinking at ts_ns=%d: %w", tsNs, err)
			}
			detail.Thinking = cloneThinking(&value)
		}
		if artifactState.Valid {
			detail.RequestLogState = logging.RequestLogState(artifactState.String)
			detail.RequestLogHomeIP = artifactHomeIP.String
		} else if detail.RequestID != "" {
			detail.RequestLogState = logging.RequestLogLegacyUnknown
		}
		out = append(out, detail)
	}
	if err = rows.Err(); err != nil {
		_ = rows.Close()
		return nil, 0, fmt.Errorf("iterate compact events: %w", err)
	}
	if err = rows.Close(); err != nil {
		return nil, 0, fmt.Errorf("close compact event rows: %w", err)
	}
	log.Debugf("usage: compact events metadata projection scan took %v", time.Since(startedAt))
	return out, pageQuery.total, nil
}

func (s *Store) queryEvents(
	ctx context.Context,
	queryer dbQueryer,
	fromNs, toNs int64,
	filters EventFilters,
	pageSize, offset int,
	sortField string,
	desc bool,
) ([]FlatDetail, int, error) {
	pageQuery, err := prepareEventsPageQuery(
		ctx,
		queryer,
		fromNs,
		toNs,
		filters,
		sortField,
		desc,
	)
	if err != nil {
		return nil, 0, err
	}
	if pageQuery.total == 0 {
		return nil, 0, nil
	}
	dataQ := "SELECT events.ts_ns, " +
		"(SELECT name FROM dim_model      WHERE id = events.model_id), " +
		"(SELECT name FROM dim_source     WHERE id = events.source_id), " +
		"(SELECT name FROM dim_auth_index WHERE id = events.auth_index_id), " +
		"(SELECT name FROM dim_api_key    WHERE id = events.api_key_id), " +
		"(SELECT name FROM dim_provider   WHERE id = events.provider_id), " +
		"events.input_tokens, events.output_tokens, events.reasoning_tokens, events.cached_tokens, " +
		"events.cache_read_tokens, events.cache_write_tokens, events.total_tokens, " +
		"events.cost_micro, events.cost_status, events.cost_reason, events.pricing_receipt, events.failed, events.metadata" + requestLogArtifactColumns +
		"FROM events" + requestLogArtifactJoinSQL + " " + pageQuery.whereSQL +
		" ORDER BY " + pageQuery.orderCol + " " + pageQuery.orderDir +
		" LIMIT ? OFFSET ?"

	dataArgs := append([]any{}, pageQuery.whereArgs...)
	dataArgs = append(dataArgs, pageSize, offset)

	rows, err := queryer.QueryContext(ctx, dataQ, dataArgs...)
	if err != nil {
		return nil, 0, fmt.Errorf("query events: %w", err)
	}

	out := make([]FlatDetail, 0, pageSize)
	var metadataBytes int64
	for rows.Next() {
		var d FlatDetail
		var tsNs int64
		var modelName, srcName, authIdxName, apiKeyName, providerName sql.NullString
		var metadata, pricingReceipt sql.NullString
		var artifactState, artifactLocation, artifactFileName sql.NullString
		var artifactHomeIP, artifactReason sql.NullString
		var artifactRevision, artifactUpdatedAtNs sql.NullInt64
		var failedInt int64
		if err := rows.Scan(
			&tsNs,
			&modelName, &srcName, &authIdxName, &apiKeyName, &providerName,
			&d.Tokens.InputTokens, &d.Tokens.OutputTokens, &d.Tokens.ReasoningTokens, &d.Tokens.CachedTokens,
			&d.Tokens.CacheReadTokens, &d.Tokens.CacheWriteTokens, &d.Tokens.TotalTokens,
			&d.costMicro, &d.CostStatus, &d.CostReason,
			&pricingReceipt,
			&failedInt,
			&metadata,
			&artifactState,
			&artifactLocation,
			&artifactFileName,
			&artifactHomeIP,
			&artifactReason,
			&artifactRevision,
			&artifactUpdatedAtNs,
		); err != nil {
			_ = rows.Close()
			return nil, 0, fmt.Errorf("scan event row: %w", err)
		}
		d.Timestamp = time.Unix(0, tsNs).UTC()
		d.Model = modelName.String
		d.Source = srcName.String
		d.AuthIndex = authIdxName.String
		d.APIKey = apiKeyName.String
		d.Provider = providerName.String
		d.PricingReceipt, err = decodePricingReceipt(pricingReceipt)
		if err != nil {
			_ = rows.Close()
			return nil, 0, fmt.Errorf("decode event pricing receipt at ts_ns=%d: %w", tsNs, err)
		}
		d.Cost = float64(d.costMicro) / costMicroPerUSD
		d.costTrusted = true
		d.Failed = failedInt != 0
		if pricingReceipt.Valid {
			metadataBytes += int64(len(pricingReceipt.String))
		}
		if metadata.Valid {
			metadataBytes += int64(len(metadata.String))
			if metadataBytes > maxEventsMetadataBytes {
				_ = rows.Close()
				return nil, 0, ErrEventsPageTooLarge
			}
		}
		if err := decodeEventMetadata(&d, metadata); err != nil {
			_ = rows.Close()
			return nil, 0, err
		}
		artifact, artifactFound, errArtifact := scanRequestLogArtifact(
			artifactState,
			artifactLocation,
			artifactFileName,
			artifactHomeIP,
			artifactReason,
			artifactRevision,
			artifactUpdatedAtNs,
		)
		if errArtifact != nil {
			_ = rows.Close()
			return nil, 0, errArtifact
		}
		applyRequestLogArtifactToDetail(&d, artifact, artifactFound)
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, 0, fmt.Errorf("iterate events: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, 0, fmt.Errorf("close event rows: %w", err)
	}
	return out, pageQuery.total, nil
}

// queryBackupEventSet returns one transactionally consistent, unpaginated event
// set for an encrypted backup. Zero from/to values leave that bound open.
func (s *Store) queryBackupEventSet(
	ctx context.Context,
	from, to time.Time,
	filters EventFilters,
) ([]FlatDetail, error) {
	return s.queryBackupEvents(ctx, from, to, filters, maxPortableBackupPlaintextBytes)
}

func (s *Store) queryBackupEvents(
	ctx context.Context,
	from, to time.Time,
	filters EventFilters,
	maxPlaintextBytes int64,
) ([]FlatDetail, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("usage: query backup events on nil store")
	}
	if maxPlaintextBytes <= 0 || maxPlaintextBytes > maxPortableBackupPlaintextBytes {
		maxPlaintextBytes = maxPortableBackupPlaintextBytes
	}
	if !from.IsZero() {
		if _, err := unixNanoUTC(from); err != nil {
			return nil, fmt.Errorf("usage: invalid backup start: %w", err)
		}
	}
	if !to.IsZero() {
		if _, err := unixNanoUTC(to); err != nil {
			return nil, fmt.Errorf("usage: invalid backup end: %w", err)
		}
	}

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("usage: begin backup transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	modelF, err := resolveNamesToIDs(ctx, tx, "dim_model", filters.Model)
	if err != nil {
		return nil, fmt.Errorf("usage: resolve backup model filter: %w", err)
	}
	credF, err := resolveCredentialFilter(ctx, tx, filters.Source)
	if err != nil {
		return nil, fmt.Errorf("usage: resolve backup credential filter: %w", err)
	}
	apiKeyF, err := resolveFilterNamesToIDs(ctx, tx, "dim_api_key", filters.APIKey, filters.APIKeys)
	if err != nil {
		return nil, fmt.Errorf("usage: resolve backup API key filter: %w", err)
	}
	if (modelF.active && !modelF.matchedAny) ||
		(credF.active && !credF.matchedAny) ||
		(apiKeyF.active && !apiKeyF.matchedAny) {
		if err = tx.Commit(); err != nil {
			return nil, fmt.Errorf("usage: commit empty backup transaction: %w", err)
		}
		committed = true
		return []FlatDetail{}, nil
	}

	whereSQL, args := buildFilterClause(from, to, "ts_ns", "", modelF, apiKeyF, credF)
	query := "SELECT events.fingerprint, events.ts_ns, " +
		"(SELECT name FROM dim_model      WHERE id = events.model_id), " +
		"(SELECT name FROM dim_source     WHERE id = events.source_id), " +
		"(SELECT name FROM dim_auth_index WHERE id = events.auth_index_id), " +
		"(SELECT name FROM dim_api_key    WHERE id = events.api_key_id), " +
		"(SELECT name FROM dim_provider   WHERE id = events.provider_id), " +
		"events.input_tokens, events.output_tokens, events.reasoning_tokens, events.cached_tokens, " +
		"events.cache_read_tokens, events.cache_write_tokens, events.total_tokens, " +
		"events.cost_micro, events.cost_status, events.cost_reason, events.pricing_receipt, events.failed, events.metadata" + requestLogArtifactColumns +
		"FROM events" + requestLogArtifactJoinSQL + whereSQL + " ORDER BY events.ts_ns, events.id"
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("usage: query backup events: %w", err)
	}

	out := make([]FlatDetail, 0)
	estimatedPlaintextBytes := int64(512)
	seenAPIKeys := make(map[string]struct{})
	for rows.Next() {
		var detail FlatDetail
		var fingerprint []byte
		var tsNs int64
		var modelName, sourceName, authIndexName, apiKeyName, providerName sql.NullString
		var metadata, pricingReceipt sql.NullString
		var artifactState, artifactLocation, artifactFileName sql.NullString
		var artifactHomeIP, artifactReason sql.NullString
		var artifactRevision, artifactUpdatedAtNs sql.NullInt64
		var failed int64
		if err = rows.Scan(
			&fingerprint,
			&tsNs,
			&modelName,
			&sourceName,
			&authIndexName,
			&apiKeyName,
			&providerName,
			&detail.Tokens.InputTokens,
			&detail.Tokens.OutputTokens,
			&detail.Tokens.ReasoningTokens,
			&detail.Tokens.CachedTokens,
			&detail.Tokens.CacheReadTokens,
			&detail.Tokens.CacheWriteTokens,
			&detail.Tokens.TotalTokens,
			&detail.costMicro,
			&detail.CostStatus,
			&detail.CostReason,
			&pricingReceipt,
			&failed,
			&metadata,
			&artifactState,
			&artifactLocation,
			&artifactFileName,
			&artifactHomeIP,
			&artifactReason,
			&artifactRevision,
			&artifactUpdatedAtNs,
		); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("usage: scan backup event: %w", err)
		}
		if len(fingerprint) != eventFingerprintBytes {
			_ = rows.Close()
			return nil, fmt.Errorf("usage: backup event has invalid fingerprint length %d", len(fingerprint))
		}
		copy(detail.fingerprint[:], fingerprint)
		detail.fingerprintTrusted = true
		detail.Timestamp = time.Unix(0, tsNs).UTC()
		detail.Model = modelName.String
		detail.Source = sourceName.String
		detail.AuthIndex = authIndexName.String
		detail.APIKey = apiKeyName.String
		detail.Provider = providerName.String
		detail.PricingReceipt, err = decodePricingReceipt(pricingReceipt)
		if err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("usage: decode backup event pricing receipt at ts_ns=%d: %w", tsNs, err)
		}
		detail.Cost = float64(detail.costMicro) / costMicroPerUSD
		detail.costTrusted = true
		detail.Failed = failed != 0
		if err = decodeEventMetadata(&detail, metadata); err != nil {
			_ = rows.Close()
			return nil, err
		}
		artifact, artifactFound, errArtifact := scanRequestLogArtifact(
			artifactState,
			artifactLocation,
			artifactFileName,
			artifactHomeIP,
			artifactReason,
			artifactRevision,
			artifactUpdatedAtNs,
		)
		if errArtifact != nil {
			_ = rows.Close()
			return nil, errArtifact
		}
		applyRequestLogArtifactToDetail(&detail, artifact, artifactFound)
		_, seenAPIKey := seenAPIKeys[detail.APIKey]
		includeAPIKey := detail.APIKey != "" && !seenAPIKey
		estimatedDetailBytes, errEstimate := estimateUsageBackupDetailBytes(detail, includeAPIKey)
		if errEstimate != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("usage: estimate backup event size: %w", errEstimate)
		}
		if estimatedPlaintextBytes > maxPlaintextBytes-estimatedDetailBytes {
			_ = rows.Close()
			return nil, ErrPortableBackupTooLarge
		}
		estimatedPlaintextBytes += estimatedDetailBytes
		if includeAPIKey {
			seenAPIKeys[detail.APIKey] = struct{}{}
		}
		out = append(out, detail)
	}
	if err = rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("usage: iterate backup events: %w", err)
	}
	if err = rows.Close(); err != nil {
		return nil, fmt.Errorf("usage: close backup event rows: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return nil, fmt.Errorf("usage: commit backup transaction: %w", err)
	}
	committed = true
	return out, nil
}

// startRollup launches the day-bucket rollup goroutine and returns a stop
// function. The first catch-up pass still runs synchronously before this
// function returns, but it now resumes from meta.day_bucket_rollup_watermark_ns
// instead of re-scanning every historical day on every startup. The hourly
// ticker keeps re-rolling yesterday/today in the background and advances the
// watermark as new closed days appear.
func (s *Store) startRollup(ctx context.Context) func() {
	// Initial pass must complete before we accept queries — otherwise a
	// startup-time day-bucket query reads an empty day_bucket and the user sees a
	// blank summary until the goroutine catches up.
	if err := s.rollupPendingDays(ctx); err != nil {
		log.Errorf("usage: initial day rollup failed: %v", err)
	}
	if err := s.rollupRecentDays(ctx); err != nil {
		log.Errorf("usage: initial recent day rollup failed: %v", err)
	}
	if err := s.rollupDirtyDays(ctx); err != nil {
		log.Errorf("usage: initial dirty day rollup failed: %v", err)
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
				if err := s.rollupDirtyDays(ctx); err != nil {
					log.Errorf("usage: rollup dirty days: %v", err)
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
		if err := s.rollupDay(ctx, d.UnixNano()); err != nil {
			return fmt.Errorf("rollup day %s: %w", d.Format("2006-01-02"), err)
		}
	}
	return nil
}

func (s *Store) rollupDirtyDays(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, "SELECT day_ts_ns FROM dirty_day ORDER BY day_ts_ns")
	if err != nil {
		return fmt.Errorf("query dirty days: %w", err)
	}
	var days []int64
	for rows.Next() {
		var dayNs int64
		if err := rows.Scan(&dayNs); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan dirty day: %w", err)
		}
		days = append(days, dayNs)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("iterate dirty days: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close dirty days: %w", err)
	}
	for _, dayNs := range days {
		if err := s.rollupDay(ctx, dayNs); err != nil {
			return fmt.Errorf("rollup dirty day %s: %w", time.Unix(0, dayNs).UTC().Format("2006-01-02"), err)
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
	latestClosedDayNs := time.Now().UTC().Truncate(24 * time.Hour).Add(-24 * time.Hour).UnixNano()
	if newestDayNs > latestClosedDayNs {
		newestDayNs = latestClosedDayNs
	}
	watermarkNs, hasWatermark, err := s.getDayRollupWatermark(ctx)
	if err != nil {
		return err
	}
	startDayNs, hasStartDay, err := s.nextPendingRollupDay(ctx, watermarkNs, hasWatermark)
	if err != nil {
		return err
	}
	if !hasStartDay || startDayNs > newestDayNs {
		return nil
	}
	if err := s.rollupDayRange(ctx, startDayNs, newestDayNs); err != nil {
		return err
	}
	return s.setDayRollupWatermark(ctx, newestDayNs)
}

func (s *Store) rollupImportedDays(ctx context.Context, earliestDayNs, latestDayNs int64) error {
	if earliestDayNs > latestDayNs {
		return nil
	}
	return s.rollupDayRange(ctx, earliestDayNs, latestDayNs)
}

func (s *Store) nextPendingRollupDay(
	ctx context.Context,
	watermarkNs int64,
	hasWatermark bool,
) (int64, bool, error) {
	if !hasWatermark {
		startDayNs, hasData, err := s.earliestHourBucketDay(ctx)
		if err != nil {
			return 0, false, err
		}
		return startDayNs, hasData, nil
	}
	if watermarkNs > math.MaxInt64-nsPerDay {
		return 0, false, errors.New("usage: day rollup watermark overflows int64")
	}
	return watermarkNs + nsPerDay, true, nil
}

func (s *Store) rollupDayRange(ctx context.Context, startDayNs, endDayNs int64) error {
	if startDayNs > endDayNs {
		return nil
	}
	for dayNs := startDayNs; ; {
		if err := s.rollupDay(ctx, dayNs); err != nil {
			return fmt.Errorf("rollup day %s: %w", time.Unix(0, dayNs).UTC().Format("2006-01-02"), err)
		}
		if dayNs >= endDayNs {
			return nil
		}
		if dayNs > math.MaxInt64-nsPerDay {
			return errors.New("usage: day rollup range overflows int64")
		}
		dayNs += nsPerDay
	}
}

func (s *Store) earliestHourBucketDay(ctx context.Context) (int64, bool, error) {
	var dayNs sql.NullInt64
	query := "SELECT MIN(" + sqliteFloorBucketExpr("bucket_ts_ns", nsPerDay) + ") FROM hour_bucket"
	if err := s.db.QueryRowContext(ctx, query).Scan(&dayNs); err != nil {
		return 0, false, fmt.Errorf("query earliest day: %w", err)
	}
	if !dayNs.Valid {
		return 0, false, nil
	}
	return dayNs.Int64, true, nil
}

func (s *Store) latestHourBucketDay(ctx context.Context) (int64, bool, error) {
	var dayNs sql.NullInt64
	query := "SELECT MAX(" + sqliteFloorBucketExpr("bucket_ts_ns", nsPerDay) + ") FROM hour_bucket"
	if err := s.db.QueryRowContext(ctx, query).Scan(&dayNs); err != nil {
		return 0, false, fmt.Errorf("query latest day: %w", err)
	}
	if !dayNs.Valid {
		return 0, false, nil
	}
	return dayNs.Int64, true, nil
}

func (s *Store) getDayRollupWatermark(ctx context.Context) (int64, bool, error) {
	var raw string
	err := s.db.QueryRowContext(
		ctx,
		"SELECT value FROM meta WHERE key = ?",
		dayRollupWatermarkKey,
	).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("read day rollup watermark: %w", err)
	}
	parsed, parseErr := time.Parse(time.RFC3339Nano, raw)
	if parseErr == nil {
		parsed = parsed.UTC()
		watermarkNs := parsed.UnixNano()
		if !time.Unix(0, watermarkNs).UTC().Equal(parsed) {
			return 0, false, fmt.Errorf("parse day rollup watermark %q: outside UnixNano range", raw)
		}
		return watermarkNs, true, nil
	}
	var watermarkNs int64
	if _, scanErr := fmt.Sscan(raw, &watermarkNs); scanErr != nil {
		return 0, false, fmt.Errorf("parse day rollup watermark %q: %w", raw, scanErr)
	}
	return watermarkNs, true, nil
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

// rollupDay recomputes day_bucket rows for the day starting at dayStartNs from
// the corresponding hour_bucket rows. Idempotent: re-running with the same day
// produces identical state, so missed runs after a crash are self-healing.
//
// The UPSERT uses "SET col = excluded.col" rather than "+= excluded.col"
// because excluded already holds the recomputed total — adding would double
// count on the second pass.
func (s *Store) rollupDay(ctx context.Context, dayStartNs int64) error {
	if s == nil || s.db == nil {
		return errors.New("usage: rollupDay on nil store")
	}
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
	if err := s.rollupDayTx(ctx, tx, dayStartNs); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit rollup tx: %w", err)
	}
	committed = true
	return nil
}

func (s *Store) rollupDayTx(ctx context.Context, tx *sql.Tx, dayStartNs int64) error {
	if s == nil || tx == nil {
		return errors.New("usage: rollupDayTx on nil store or transaction")
	}
	if dayStartNs > math.MaxInt64-nsPerDay {
		return errors.New("usage: day rollup end overflows int64")
	}
	dayEndNs := dayStartNs + nsPerDay

	const aggSQL = `
SELECT model_id, credential_id, api_key_id, provider_id,
       SUM(requests), SUM(success), SUM(failure),
       SUM(input_tokens), SUM(output_tokens), SUM(reasoning_tokens), SUM(cached_tokens),
       SUM(cache_read_tokens), SUM(cache_write_tokens), SUM(total_tokens),
       SUM(cost_micro),
       SUM(complete_requests), SUM(partial_requests), SUM(unpriced_requests),
       SUM(complete_tokens), SUM(partial_tokens), SUM(unpriced_tokens),
       SUM(latency_ms_total), SUM(latency_samples)
  FROM hour_bucket
 WHERE bucket_ts_ns >= ? AND bucket_ts_ns < ?
 GROUP BY model_id, credential_id, api_key_id, provider_id
`
	rows, err := tx.QueryContext(ctx, aggSQL, dayStartNs, dayEndNs)
	if err != nil {
		return fmt.Errorf("aggregate hour_bucket: %w", err)
	}

	type aggRow struct {
		modelID                                                                   int64
		credID, apiKeyID, providerID                                              sql.NullInt64
		requests, success, failure                                                int64
		input, output, reasoning, cached, cacheRead, cacheWrite, total, costMicro int64
		completeRequests, partialRequests, unpricedRequests                       int64
		completeTokens, partialTokens, unpricedTokens                             int64
		latency, samples                                                          int64
	}
	var aggs []aggRow
	for rows.Next() {
		var a aggRow
		if err := rows.Scan(
			&a.modelID, &a.credID, &a.apiKeyID, &a.providerID,
			&a.requests, &a.success, &a.failure,
			&a.input, &a.output, &a.reasoning, &a.cached, &a.cacheRead, &a.cacheWrite, &a.total,
			&a.costMicro,
			&a.completeRequests, &a.partialRequests, &a.unpricedRequests,
			&a.completeTokens, &a.partialTokens, &a.unpricedTokens,
			&a.latency, &a.samples,
		); err != nil {
			if errClose := rows.Close(); errClose != nil {
				log.Warnf("usage: close aggregate rows after scan error: %v", errClose)
			}
			return fmt.Errorf("scan aggregate: %w", err)
		}
		aggs = append(aggs, a)
	}
	if err := rows.Err(); err != nil {
		if errClose := rows.Close(); errClose != nil {
			log.Warnf("usage: close aggregate rows after iteration error: %v", errClose)
		}
		return fmt.Errorf("iterate aggregate rows: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close aggregate rows: %w", err)
	}

	if _, err := tx.ExecContext(ctx, "DELETE FROM day_bucket WHERE bucket_ts_ns = ?", dayStartNs); err != nil {
		return fmt.Errorf("clear day_bucket: %w", err)
	}

	const upsertSQL = `
INSERT INTO day_bucket (
    bucket_ts_ns, model_id, credential_id, api_key_id, provider_id,
    requests, success, failure,
    input_tokens, output_tokens, reasoning_tokens, cached_tokens,
    cache_read_tokens, cache_write_tokens, total_tokens,
    cost_micro,
    complete_requests, partial_requests, unpriced_requests,
    complete_tokens, partial_tokens, unpriced_tokens,
    latency_ms_total, latency_samples
) VALUES (
    ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
    ?, ?, ?, ?, ?, ?, ?, ?
)
ON CONFLICT(bucket_ts_ns, model_id, ifnull(credential_id, 0), ifnull(api_key_id, 0), ifnull(provider_id, 0))
DO UPDATE SET
    requests         = excluded.requests,
    success          = excluded.success,
    failure          = excluded.failure,
    input_tokens     = excluded.input_tokens,
    output_tokens    = excluded.output_tokens,
    reasoning_tokens = excluded.reasoning_tokens,
    cached_tokens     = excluded.cached_tokens,
    cache_read_tokens = excluded.cache_read_tokens,
    cache_write_tokens = excluded.cache_write_tokens,
    total_tokens      = excluded.total_tokens,
    cost_micro        = excluded.cost_micro,
    complete_requests = excluded.complete_requests,
    partial_requests  = excluded.partial_requests,
    unpriced_requests = excluded.unpriced_requests,
    complete_tokens   = excluded.complete_tokens,
    partial_tokens    = excluded.partial_tokens,
    unpriced_tokens   = excluded.unpriced_tokens,
    latency_ms_total  = excluded.latency_ms_total,
    latency_samples  = excluded.latency_samples
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
			a.input, a.output, a.reasoning, a.cached, a.cacheRead, a.cacheWrite, a.total,
			a.costMicro,
			a.completeRequests, a.partialRequests, a.unpricedRequests,
			a.completeTokens, a.partialTokens, a.unpricedTokens,
			a.latency, a.samples,
		); err != nil {
			return fmt.Errorf("upsert day_bucket: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM dirty_day WHERE day_ts_ns = ?", dayStartNs); err != nil {
		return fmt.Errorf("clear dirty day: %w", err)
	}
	return nil
}
