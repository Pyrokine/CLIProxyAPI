package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/credentialweight"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	cliproxysession "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/session"
)

// RoundRobinSelector provides a simple provider scoped round-robin selection strategy.
type RoundRobinSelector struct {
	mu      sync.Mutex
	cursors map[string]string
	maxKeys int
}

// WeightedRoundRobinSelector provides smooth weighted round-robin selection.
type WeightedRoundRobinSelector struct {
	mu      sync.Mutex
	states  map[string]*smoothWeightedState
	maxKeys int
}

// QuotaAwareSelector enables quota-aware scheduling in Manager.
// Direct selector use preserves weighted round-robin semantics because it has no quota owner.
type QuotaAwareSelector struct {
	fallback WeightedRoundRobinSelector
}

type smoothWeightedState struct {
	current    map[string]int64
	weights    map[string]int64
	lastPicked string
}

// FillFirstSelector selects the first available credential (deterministic ordering).
// This "burns" one account before moving to the next, which can help stagger
// rolling-window subscription caps (e.g. chat message limits).
type FillFirstSelector struct{}

// StoppableSelector releases resources held by a selector.
type StoppableSelector interface {
	Stop()
}

// SessionAffinityConfig configures session-sticky routing.
type SessionAffinityConfig struct {
	Fallback Selector
	TTL      time.Duration
}

// SessionAffinitySelector keeps requests from the same session on the same auth when possible.
type SessionAffinitySelector struct {
	fallback Selector
	cache    *SessionCache
}

func (s *SessionAffinitySelector) FallbackSelector() Selector {
	if s == nil {
		return nil
	}
	return s.fallback
}

func sessionAffinityKeys(opts cliproxyexecutor.Options, provider string) (string, string) {
	primary, fallback := extractSessionIDs(opts.Headers, opts.OriginalRequest, opts.Metadata)
	if primary == "" {
		return "", ""
	}
	primaryKey := provider + "::" + primary
	if fallback == "" || fallback == primary {
		return primaryKey, ""
	}
	return primaryKey, provider + "::" + fallback
}

func (s *SessionAffinitySelector) PickSticky(provider string, opts cliproxyexecutor.Options, auths []*Auth) *Auth {
	return s.pickStickyForModel(provider, "", opts, auths)
}

func (s *SessionAffinitySelector) pickStickyForModel(
	provider,
	model string,
	opts cliproxyexecutor.Options,
	auths []*Auth,
) *Auth {
	auth, _ := s.pickStickyForModelWithRule(provider, model, opts, auths)
	return auth
}

func (s *SessionAffinitySelector) pickStickyForModelWithRule(
	provider,
	model string,
	opts cliproxyexecutor.Options,
	auths []*Auth,
) (*Auth, string) {
	cacheKey, fallbackKey := sessionAffinityKeys(opts, provider)
	if cacheKey == "" || s.cache == nil {
		return nil, ""
	}
	availabilityCandidates := auths
	if selectorUsesPositiveWeights(s.fallback) {
		availabilityCandidates = positiveWeightAuths(auths, opts, model)
	}
	available, errAvailable := getAvailableAuths(availabilityCandidates, provider, model, opts, time.Now())
	if errAvailable != nil {
		return nil, ""
	}
	bind := func(authID string) {
		if fallbackKey != "" {
			s.cache.SetAliases(authID, cacheKey, fallbackKey)
			return
		}
		s.cache.Set(cacheKey, authID)
	}
	if authID, ok := s.cache.GetAndRefresh(cacheKey); ok {
		for i := range available {
			if available[i] != nil && available[i].ID == authID {
				bind(authID)
				publishAuthRouteTraceEvent(
					opts, available[i], cliproxyexecutor.RouteTraceEvent{
						Provider: provider,
						Model:    model,
						Action:   "candidate.selected",
						Reason:   "session_affinity",
					},
				)
				return available[i], "session_affinity"
			}
		}
	}
	if fallbackKey != "" {
		if authID, ok := s.cache.Get(fallbackKey); ok {
			for i := range available {
				if available[i] != nil && available[i].ID == authID {
					bind(authID)
					publishAuthRouteTraceEvent(
						opts, available[i], cliproxyexecutor.RouteTraceEvent{
							Provider: provider,
							Model:    model,
							Action:   "candidate.selected",
							Reason:   "session_affinity_fallback",
						},
					)
					return available[i], "session_affinity_fallback"
				}
			}
		}
	}
	return nil, ""
}

func (s *SessionAffinitySelector) Remember(provider string, opts cliproxyexecutor.Options, auth *Auth) {
	if s == nil || s.cache == nil || auth == nil {
		return
	}
	cacheKey, fallbackKey := sessionAffinityKeys(opts, provider)
	if cacheKey == "" {
		return
	}
	if fallbackKey != "" {
		s.cache.SetAliases(auth.ID, cacheKey, fallbackKey)
		return
	}
	s.cache.Set(cacheKey, auth.ID)
}

func (s *SessionAffinitySelector) InvalidateAuth(authID string) {
	if s == nil || s.cache == nil {
		return
	}
	s.cache.InvalidateAuth(authID)
}

func NewSessionAffinitySelector(fallback Selector) *SessionAffinitySelector {
	return NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: fallback, TTL: time.Hour})
}

func NewSessionAffinitySelectorWithConfig(cfg SessionAffinityConfig) *SessionAffinitySelector {
	if cfg.Fallback == nil {
		cfg.Fallback = &RoundRobinSelector{}
	}
	if cfg.TTL <= 0 {
		cfg.TTL = time.Hour
	}
	return &SessionAffinitySelector{fallback: cfg.Fallback, cache: NewSessionCache(cfg.TTL)}
}

func (s *SessionAffinitySelector) Stop() {
	if s == nil || s.cache == nil {
		return
	}
	s.cache.Stop()
}

type blockReason int

const (
	blockReasonNone blockReason = iota
	blockReasonCooldown
	blockReasonDisabled
	blockReasonOther
)

type modelCooldownError struct {
	model    string
	resetIn  time.Duration
	provider string
}

func newModelCooldownError(model, provider string, resetIn time.Duration) *modelCooldownError {
	if resetIn < 0 {
		resetIn = 0
	}
	return &modelCooldownError{
		model:    model,
		provider: provider,
		resetIn:  resetIn,
	}
}

func (e *modelCooldownError) Error() string {
	modelName := e.model
	if modelName == "" {
		modelName = "requested model"
	}
	message := fmt.Sprintf("All credentials for model %s are cooling down", modelName)
	if e.provider != "" {
		message = fmt.Sprintf("%s via provider %s", message, e.provider)
	}
	resetSeconds := max(int(math.Ceil(e.resetIn.Seconds())), 0)
	displayDuration := e.resetIn
	if displayDuration > 0 && displayDuration < time.Second {
		displayDuration = time.Second
	} else {
		displayDuration = displayDuration.Round(time.Second)
	}
	errorBody := map[string]any{
		"code":          "model_cooldown",
		"message":       message,
		"model":         e.model,
		"reset_time":    displayDuration.String(),
		"reset_seconds": resetSeconds,
	}
	if e.provider != "" {
		errorBody["provider"] = e.provider
	}
	payload := map[string]any{"error": errorBody}
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Sprintf(`{"error":{"code":"model_cooldown","message":"%s"}}`, message)
	}
	return string(data)
}

func (e *modelCooldownError) StatusCode() int {
	return http.StatusTooManyRequests
}

func (e *modelCooldownError) Headers() http.Header {
	headers := make(http.Header)
	headers.Set("Content-Type", "application/json")
	resetSeconds := max(int(math.Ceil(e.resetIn.Seconds())), 0)
	headers.Set("Retry-After", strconv.Itoa(resetSeconds))
	return headers
}

func authPriority(auth *Auth) int {
	if auth == nil || auth.Attributes == nil {
		return 0
	}
	raw := strings.TrimSpace(auth.Attributes["priority"])
	if raw == "" {
		return 0
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil {
		return 0
	}
	return parsed
}

func authWeight(auth *Auth) int64 {
	if auth == nil {
		return credentialweight.Default
	}
	if rawWeight, ok := auth.Attributes[AttributeWeight]; ok && strings.TrimSpace(rawWeight) != "" {
		weight, errParse := credentialweight.ParseString(rawWeight)
		if errParse != nil {
			return 0
		}
		return weight
	}
	if rawWeight, ok := auth.Metadata[AttributeWeight]; ok {
		weight, errParse := credentialweight.ParseValue(rawWeight)
		if errParse != nil {
			return 0
		}
		return weight
	}
	return credentialweight.Default
}

func canonicalModelKey(model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return ""
	}
	parsed := thinking.ParseSuffix(model)
	modelName := strings.TrimSpace(parsed.ModelName)
	if modelName == "" {
		return model
	}
	return modelName
}

func authWebsocketsEnabled(auth *Auth) bool {
	if auth == nil {
		return false
	}
	if len(auth.Attributes) > 0 {
		if raw := strings.TrimSpace(auth.Attributes["websockets"]); raw != "" {
			parsed, errParse := strconv.ParseBool(raw)
			if errParse == nil {
				return parsed
			}
		}
	}
	if len(auth.Metadata) == 0 {
		return false
	}
	raw, ok := auth.Metadata["websockets"]
	if !ok || raw == nil {
		return false
	}
	switch v := raw.(type) {
	case bool:
		return v
	case string:
		parsed, errParse := strconv.ParseBool(strings.TrimSpace(v))
		if errParse == nil {
			return parsed
		}
	default:
	}
	return false
}

func preferCodexWebsocketAuths(ctx context.Context, provider string, available []*Auth) []*Auth {
	if len(available) == 0 {
		return available
	}
	if !cliproxyexecutor.DownstreamWebsocket(ctx) {
		return available
	}
	if !strings.EqualFold(strings.TrimSpace(provider), "codex") {
		return available
	}

	wsEnabled := make([]*Auth, 0, len(available))
	for i := range available {
		candidate := available[i]
		if authWebsocketsEnabled(candidate) {
			wsEnabled = append(wsEnabled, candidate)
		}
	}
	if len(wsEnabled) > 0 {
		return wsEnabled
	}
	return available
}

func collectAvailableByPriority(
	auths []*Auth,
	provider,
	model string,
	opts cliproxyexecutor.Options,
	now time.Time,
) (available map[int][]*Auth, cooldownCount int, earliest time.Time) {
	available = make(map[int][]*Auth)
	for i := range auths {
		candidate := auths[i]
		blocked, reason, next := isAuthBlockedForModel(candidate, model, now)
		if !blocked {
			priority := authPriority(candidate)
			available[priority] = append(available[priority], candidate)
			continue
		}
		reasonCode := "blocked"
		retryAfterMS := int64(0)
		// None and other blocked reasons retain the generic blocked route reason.
		// noinspection GoSwitchMissingCasesForIotaConsts
		switch reason {
		case blockReasonCooldown:
			reasonCode = "cooldown"
			cooldownCount++
			if !next.IsZero() && (earliest.IsZero() || next.Before(earliest)) {
				earliest = next
			}
			if next.After(now) {
				retryAfterMS = next.Sub(now).Milliseconds()
			}
		case blockReasonDisabled:
			reasonCode = "disabled"
		}
		publishAuthRouteTraceEvent(
			opts, candidate, cliproxyexecutor.RouteTraceEvent{
				Provider:     provider,
				Model:        model,
				Action:       "candidate.excluded",
				Reason:       reasonCode,
				RetryAfterMS: retryAfterMS,
			},
		)
	}
	return available, cooldownCount, earliest
}

func getAvailableAuths(
	auths []*Auth,
	provider,
	model string,
	opts cliproxyexecutor.Options,
	now time.Time,
) ([]*Auth, error) {
	if len(auths) == 0 {
		return nil, &Error{Code: "auth_not_found", Message: "no auth candidates"}
	}

	availableByPriority, cooldownCount, earliest := collectAvailableByPriority(auths, provider, model, opts, now)
	if len(availableByPriority) == 0 {
		if cooldownCount == len(auths) && !earliest.IsZero() {
			providerForError := provider
			if providerForError == "mixed" {
				providerForError = ""
			}
			resetIn := max(earliest.Sub(now), 0)
			return nil, newModelCooldownError(model, providerForError, resetIn)
		}
		return nil, &Error{Code: "auth_unavailable", Message: "no auth available"}
	}

	bestPriority := 0
	found := false
	for priority := range availableByPriority {
		if !found || priority > bestPriority {
			bestPriority = priority
			found = true
		}
	}

	available := availableByPriority[bestPriority]
	if len(available) > 1 {
		sort.Slice(available, func(i, j int) bool { return available[i].ID < available[j].ID })
	}
	return available, nil
}

// Pick selects the next available auth for the provider in a round-robin manner.
// For gemini-cli virtual auths (identified by the gemini_virtual_parent attribute),
// a two-level round-robin is used: first cycling across credential groups (parent
// accounts), then cycling within each group's project auths.
func (s *RoundRobinSelector) Pick(
	ctx context.Context,
	provider, model string,
	opts cliproxyexecutor.Options,
	auths []*Auth,
) (*Auth, error) {
	now := time.Now()
	available, err := getAvailableAuths(auths, provider, model, opts, now)
	if err != nil {
		return nil, err
	}
	available = preferCodexWebsocketAuths(ctx, provider, available)
	key := provider + ":" + canonicalModelKey(model)
	s.mu.Lock()
	if s.cursors == nil {
		s.cursors = make(map[string]string)
	}
	limit := s.maxKeys
	if limit <= 0 {
		limit = 4096
	}

	// Check if any available auth has gemini_virtual_parent attribute,
	// indicating gemini-cli virtual auths that should use credential-level polling.
	groups, parentOrder := groupByVirtualParent(available)
	if len(parentOrder) > 1 {
		// Two-level round-robin: first select a credential group, then pick within it.
		groupKey := key + "::group"
		s.ensureCursorKey(groupKey, limit)
		groupIndex := 0
		if lastParent, exists := s.cursors[groupKey]; exists {
			groupIndex = nextSortedStringIndex(parentOrder, lastParent)
		} else {
			// Seed with a random initial offset so the starting credential is randomized.
			groupIndex = rand.IntN(len(parentOrder))
		}

		selectedParent := parentOrder[groupIndex]
		s.cursors[groupKey] = selectedParent
		group := groups[selectedParent]

		// Second level: round-robin within the selected credential group.
		innerKey := key + "::cred:" + selectedParent
		s.ensureCursorKey(innerKey, limit)
		innerIndex := nextSortedAuthIndex(group, s.cursors[innerKey])
		selected := group[innerIndex]
		s.cursors[innerKey] = selected.ID
		s.mu.Unlock()
		publishAuthRouteTraceEvent(
			opts, selected, cliproxyexecutor.RouteTraceEvent{
				Provider: provider,
				Model:    model,
				Action:   "candidate.selected",
				Reason:   "round_robin_group",
			},
		)
		return selected, nil
	}

	// Flat round-robin for non-grouped auths (original behavior).
	s.ensureCursorKey(key, limit)
	index := nextSortedAuthIndex(available, s.cursors[key])
	selected := available[index]
	s.cursors[key] = selected.ID
	s.mu.Unlock()
	publishAuthRouteTraceEvent(
		opts, selected, cliproxyexecutor.RouteTraceEvent{
			Provider: provider,
			Model:    model,
			Action:   "candidate.selected",
			Reason:   "round_robin",
		},
	)
	return selected, nil
}

func nextSortedStringIndex(values []string, lastPicked string) int {
	if len(values) == 0 || lastPicked == "" {
		return 0
	}
	index := sort.SearchStrings(values, lastPicked)
	if index < len(values) && values[index] == lastPicked {
		return (index + 1) % len(values)
	}
	if index < len(values) {
		return index
	}
	return 0
}

func nextSortedAuthIndex(auths []*Auth, lastPicked string) int {
	if len(auths) == 0 || lastPicked == "" {
		return 0
	}
	index := sort.Search(
		len(auths), func(index int) bool {
			return auths[index] != nil && auths[index].ID >= lastPicked
		},
	)
	if index < len(auths) && auths[index] != nil && auths[index].ID == lastPicked {
		return (index + 1) % len(auths)
	}
	if index < len(auths) {
		return index
	}
	return 0
}

// ensureCursorKey ensures the cursor map has capacity for the given key.
// Must be called with s.mu held.
func (s *RoundRobinSelector) ensureCursorKey(key string, limit int) {
	if _, ok := s.cursors[key]; !ok && len(s.cursors) >= limit {
		s.cursors = make(map[string]string)
	}
}

func selectorUsesPositiveWeights(selector Selector) bool {
	switch selector.(type) {
	case *WeightedRoundRobinSelector, *QuotaAwareSelector:
		return true
	default:
		return false
	}
}

func positiveWeightAuths(
	auths []*Auth,
	opts cliproxyexecutor.Options,
	model string,
) []*Auth {
	weightedCandidates := make([]*Auth, 0, len(auths))
	for _, auth := range auths {
		if authWeight(auth) > 0 {
			weightedCandidates = append(weightedCandidates, auth)
			continue
		}
		traceCandidateExcluded(opts, auth, model, "weight_non_positive")
	}
	return weightedCandidates
}

// Pick selects the next available auth using smooth weighted round-robin.
func (s *WeightedRoundRobinSelector) Pick(
	ctx context.Context,
	provider, model string,
	opts cliproxyexecutor.Options,
	auths []*Auth,
) (*Auth, error) {
	available, errAvailable := getAvailableAuths(
		positiveWeightAuths(auths, opts, model), provider, model, opts, time.Now(),
	)
	if errAvailable != nil {
		return nil, errAvailable
	}
	available = preferCodexWebsocketAuths(ctx, provider, available)
	key := provider + ":" + canonicalModelKey(model)

	s.mu.Lock()
	if s.states == nil {
		s.states = make(map[string]*smoothWeightedState)
	}
	limit := s.maxKeys
	if limit <= 0 {
		limit = 4096
	}
	if _, ok := s.states[key]; !ok && len(s.states) >= limit {
		s.states = make(map[string]*smoothWeightedState)
	}
	state := s.states[key]
	if state == nil {
		state = &smoothWeightedState{}
		s.states[key] = state
	}
	state.prepare(authWeightVector(available))
	picked := pickSmoothWeightedAuth(available, state)
	if picked == nil {
		s.mu.Unlock()
		return nil, &Error{Code: "auth_unavailable", Message: "no auth available with positive weight"}
	}
	s.mu.Unlock()
	publishAuthRouteTraceEvent(
		opts, picked, cliproxyexecutor.RouteTraceEvent{
			Provider: provider,
			Model:    model,
			Action:   "candidate.selected",
			Reason:   "weighted_round_robin",
		},
	)
	return picked, nil
}

func (s *QuotaAwareSelector) Pick(
	ctx context.Context,
	provider, model string,
	opts cliproxyexecutor.Options,
	auths []*Auth,
) (*Auth, error) {
	if s == nil {
		return (&WeightedRoundRobinSelector{}).Pick(ctx, provider, model, opts, auths)
	}
	return s.fallback.Pick(ctx, provider, model, opts, auths)
}

func (s *smoothWeightedState) prepare(weights map[string]int64) {
	if s.current == nil || !weightVectorsEqual(s.weights, weights) {
		s.current = make(map[string]int64)
	}
	s.weights = weights
}

func weightVectorsEqual(left, right map[string]int64) bool {
	if len(left) != len(right) {
		return false
	}
	for authID, weight := range left {
		if right[authID] != weight {
			return false
		}
	}
	return true
}

func authWeightVector(auths []*Auth) map[string]int64 {
	weights := make(map[string]int64, len(auths))
	for _, auth := range auths {
		if auth == nil {
			continue
		}
		if weight := authWeight(auth); weight > 0 {
			weights[auth.ID] = weight
		}
	}
	return weights
}

func pickSmoothWeightedAuth(auths []*Auth, state *smoothWeightedState) *Auth {
	if state == nil {
		return nil
	}
	active := make(map[string]struct{}, len(auths))
	start := nextSortedAuthIndex(auths, state.lastPicked)
	var picked *Auth
	var pickedCurrent int64
	var totalWeight int64
	for offset := range auths {
		auth := auths[(start+offset)%len(auths)]
		if auth == nil {
			continue
		}
		weight := authWeight(auth)
		if weight <= 0 {
			continue
		}
		active[auth.ID] = struct{}{}
		state.current[auth.ID] = saturatingAddInt64(state.current[auth.ID], weight)
		totalWeight = saturatingAddInt64(totalWeight, weight)
		if picked == nil || state.current[auth.ID] > pickedCurrent {
			picked = auth
			pickedCurrent = state.current[auth.ID]
		}
	}
	for authID := range state.current {
		if _, ok := active[authID]; !ok {
			delete(state.current, authID)
		}
	}
	if picked == nil {
		return nil
	}
	state.current[picked.ID] = saturatingAddInt64(state.current[picked.ID], -totalWeight)
	state.lastPicked = picked.ID
	return picked
}

func saturatingAddInt64(value, delta int64) int64 {
	if delta > 0 && value > math.MaxInt64-delta {
		return math.MaxInt64
	}
	if delta < 0 && value < math.MinInt64-delta {
		return math.MinInt64
	}
	return value + delta
}

// groupByVirtualParent groups auths by their gemini_virtual_parent attribute.
// Returns a map of parentID -> auths and a sorted slice of parent IDs for stable iteration.
// Only auths with a non-empty gemini_virtual_parent are grouped; if any auth lacks
// this attribute, nil/nil is returned so the caller falls back to flat round-robin.
func groupByVirtualParent(auths []*Auth) (map[string][]*Auth, []string) {
	if len(auths) == 0 {
		return nil, nil
	}
	groups := make(map[string][]*Auth)
	for _, a := range auths {
		parent := ""
		if a.Attributes != nil {
			parent = strings.TrimSpace(a.Attributes["gemini_virtual_parent"])
		}
		if parent == "" {
			// Non-virtual auth present; fall back to flat round-robin.
			return nil, nil
		}
		groups[parent] = append(groups[parent], a)
	}
	// Collect parent IDs in sorted order for stable cursor indexing.
	parentOrder := make([]string, 0, len(groups))
	for p := range groups {
		parentOrder = append(parentOrder, p)
	}
	sort.Strings(parentOrder)
	return groups, parentOrder
}

// Pick selects the first available auth for the provider in a deterministic manner.
func (s *FillFirstSelector) Pick(
	ctx context.Context,
	provider, model string,
	opts cliproxyexecutor.Options,
	auths []*Auth,
) (*Auth, error) {
	now := time.Now()
	available, err := getAvailableAuths(auths, provider, model, opts, now)
	if err != nil {
		return nil, err
	}
	available = preferCodexWebsocketAuths(ctx, provider, available)
	selected := available[0]
	publishAuthRouteTraceEvent(
		opts, selected, cliproxyexecutor.RouteTraceEvent{
			Provider: provider,
			Model:    model,
			Action:   "candidate.selected",
			Reason:   "fill_first",
		},
	)
	return selected, nil
}

func headerValueIgnoreCase(headers http.Header, name string) string {
	if len(headers) == 0 {
		return ""
	}
	target := strings.ToLower(strings.TrimSpace(name))
	for key, values := range headers {
		if strings.ToLower(strings.TrimSpace(key)) != target || len(values) == 0 {
			continue
		}
		return strings.TrimSpace(values[0])
	}
	return ""
}

func normalizedSessionCandidate(raw string) string {
	return cliproxysession.NormalizeExplicitID(raw)
}

func sessionHeaderValue(headers http.Header, name string) string {
	return cliproxysession.ExplicitHeaderValue(headers, name)
}

func (s *SessionAffinitySelector) Pick(
	ctx context.Context,
	provider, model string,
	opts cliproxyexecutor.Options,
	auths []*Auth,
) (*Auth, error) {
	if s == nil {
		return (&RoundRobinSelector{}).Pick(ctx, provider, model, opts, auths)
	}
	cacheKey, fallbackKey := sessionAffinityKeys(opts, provider)
	if cacheKey == "" {
		return s.fallback.Pick(ctx, provider, model, opts, auths)
	}

	now := time.Now()
	availabilityCandidates := auths
	if selectorUsesPositiveWeights(s.fallback) {
		availabilityCandidates = positiveWeightAuths(auths, opts, model)
	}
	available, err := getAvailableAuths(availabilityCandidates, provider, model, opts, now)
	if err != nil {
		return nil, err
	}
	available = preferCodexWebsocketAuths(ctx, provider, available)
	bind := func(authID string) {
		if fallbackKey != "" {
			s.cache.SetAliases(authID, cacheKey, fallbackKey)
			return
		}
		s.cache.Set(cacheKey, authID)
	}
	if authID, ok := s.cache.GetAndRefresh(cacheKey); ok {
		for i := range available {
			if available[i].ID == authID {
				bind(authID)
				publishAuthRouteTraceEvent(
					opts, available[i], cliproxyexecutor.RouteTraceEvent{
						Provider: provider,
						Model:    model,
						Action:   "candidate.selected",
						Reason:   "session_affinity",
					},
				)
				return available[i], nil
			}
		}
	}
	if fallbackKey != "" {
		if authID, ok := s.cache.Get(fallbackKey); ok {
			for i := range available {
				if available[i].ID == authID {
					bind(authID)
					publishAuthRouteTraceEvent(
						opts, available[i], cliproxyexecutor.RouteTraceEvent{
							Provider: provider,
							Model:    model,
							Action:   "candidate.selected",
							Reason:   "session_affinity_fallback",
						},
					)
					return available[i], nil
				}
			}
		}
	}
	auth, err := s.fallback.Pick(ctx, provider, model, opts, auths)
	if err != nil {
		return nil, err
	}
	bind(auth.ID)
	return auth, nil
}

func isAuthBlockedForModel(auth *Auth, model string, now time.Time) (bool, blockReason, time.Time) {
	if auth == nil {
		return true, blockReasonOther, time.Time{}
	}
	if auth.Disabled || auth.Status == StatusDisabled {
		return true, blockReasonDisabled, time.Time{}
	}
	if auth.Unavailable {
		if auth.NextRetryAfter.IsZero() {
			return true, blockReasonOther, time.Time{}
		}
		if auth.NextRetryAfter.After(now) {
			next := auth.NextRetryAfter
			if !auth.Quota.NextRecoverAt.IsZero() && auth.Quota.NextRecoverAt.After(now) {
				next = auth.Quota.NextRecoverAt
			}
			if auth.Quota.Exceeded {
				return true, blockReasonCooldown, next
			}
			return true, blockReasonOther, next
		}
	}
	if model != "" {
		if len(auth.ModelStates) > 0 {
			state, ok := auth.ModelStates[model]
			// noinspection GoDfaConstantCondition — state can be nil even when ok is true (nil map value).
			if !ok || state == nil {
				baseModel := canonicalModelKey(model)
				if baseModel != "" && baseModel != model {
					state, ok = auth.ModelStates[baseModel]
				}
			}
			if ok && state != nil {
				if state.Status == StatusDisabled {
					return true, blockReasonDisabled, time.Time{}
				}
				if state.Unavailable {
					if state.NextRetryAfter.IsZero() {
						return true, blockReasonOther, time.Time{}
					}
					if state.NextRetryAfter.After(now) {
						next := state.NextRetryAfter
						if !state.Quota.NextRecoverAt.IsZero() && state.Quota.NextRecoverAt.After(now) {
							next = state.Quota.NextRecoverAt
						}
						if next.Before(now) {
							next = now
						}
						if state.Quota.Exceeded {
							return true, blockReasonCooldown, next
						}
						return true, blockReasonOther, next
					}
				}
				return false, blockReasonNone, time.Time{}
			}
		}
		return false, blockReasonNone, time.Time{}
	}
	return false, blockReasonNone, time.Time{}
}
