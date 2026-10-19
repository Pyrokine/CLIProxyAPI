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
	"github.com/tidwall/gjson"
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
	Fallback         Selector
	TTL              time.Duration
	SubagentAffinity *bool
}

// SessionAffinitySelector keeps explicit and rolling-prefix sessions on one auth.
type SessionAffinitySelector struct {
	fallback         Selector
	cache            *SessionCache
	matcher          *cliproxysession.MerklePrefixMatcher
	subagentAffinity bool
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
	subagentAffinity := true
	if cfg.SubagentAffinity != nil {
		subagentAffinity = *cfg.SubagentAffinity
	}
	return &SessionAffinitySelector{
		fallback:         cfg.Fallback,
		cache:            NewSessionCache(cfg.TTL),
		matcher:          cliproxysession.NewMerklePrefixMatcher(cfg.TTL),
		subagentAffinity: subagentAffinity,
	}
}

func (s *SessionAffinitySelector) FallbackSelector() Selector {
	if s == nil {
		return nil
	}
	return s.fallback
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
	if s == nil {
		return nil, ""
	}
	setSessionAffinityNamespaceMetadata(opts.Metadata, provider, model)
	availabilityCandidates := auths
	if selectorUsesPositiveWeights(s.fallback) {
		availabilityCandidates = positiveWeightAuths(auths, opts, model)
	}
	available := availableAffinityAuths(availabilityCandidates, provider, model, opts)
	if len(available) == 0 {
		return nil, ""
	}

	primaryID, parentID := extractExplicitSessionIDs(opts.Headers, opts.OriginalRequest, opts.Metadata)
	if primaryID == "" {
		if auth, match := s.pickLCPAffinity(provider, model, opts, available); match {
			return auth, "session_affinity_lcp"
		}
		if _, prepared := lcpFingerprintsFromMetadata(opts.Metadata); prepared {
			return nil, ""
		}
		primaryID, parentID = extractSessionIDs(opts.Headers, opts.OriginalRequest, opts.Metadata)
	}
	if primaryID == "" || s.cache == nil {
		return nil, ""
	}
	primaryID = cliproxysession.BoundSessionIdentity(primaryID)
	parentID = cliproxysession.BoundSessionIdentity(parentID)
	setCanonicalSessionMetadata(opts.Metadata, primaryID, parentID)

	isFork := metadataBool(opts.Metadata, cliproxyexecutor.IsForkMetadataKey)
	isSubagent := !isFork && isSubagentSession(primaryID, parentID)
	cacheKey, parentKey, aliasParent := affinityCacheKeys(provider, model, primaryID, parentID, isFork, isSubagent)
	bind := func(authID string) {
		if aliasParent && parentKey != "" {
			s.cache.SetAliases(authID, cacheKey, parentKey)
			return
		}
		s.cache.Set(cacheKey, authID)
	}
	if authID, ok := s.cache.GetAndRefresh(cacheKey); ok {
		if auth := authByID(available, authID); auth != nil {
			bind(authID)
			publishSessionAffinitySelection(opts, auth, provider, model, "session_affinity")
			return auth, "session_affinity"
		}
	}
	if parentKey != "" && (isFork || (isSubagent && s.subagentAffinity) || aliasParent) {
		if authID, ok := s.cache.Get(parentKey); ok {
			if auth := authByID(available, authID); auth != nil {
				bind(authID)
				rule := "session_affinity_fallback"
				if isFork {
					rule = "session_affinity_fork"
				} else if isSubagent {
					rule = "session_affinity_parent"
				}
				publishSessionAffinitySelection(opts, auth, provider, model, rule)
				return auth, rule
			}
		}
	}
	return nil, ""
}

func (s *SessionAffinitySelector) pickLCPAffinity(
	provider,
	model string,
	opts cliproxyexecutor.Options,
	available []*Auth,
) (*Auth, bool) {
	if s == nil || s.matcher == nil {
		return nil, false
	}
	namespace := lcpAffinityNamespace(provider, model, opts.Metadata)
	if namespace == "" {
		return nil, false
	}
	turns := cliproxysession.ExtractCanonicalTurns(opts.SourceFormat, opts.OriginalRequest)
	fingerprints, minimum, tails, envDigest := s.matcher.PrepareExt(turns)
	if len(fingerprints) == 0 || minimum <= 0 || minimum > len(fingerprints) {
		return nil, false
	}
	setLCPMetadata(opts.Metadata, fingerprints, minimum, tails, envDigest)
	match, ok := s.matcher.MatchFingerprintsWithContext(namespace, fingerprints, tails, envDigest, minimum)
	if !ok {
		return nil, false
	}
	auth := authByID(available, match.AuthID)
	if auth == nil || !s.matcher.TouchFingerprintsWithContext(namespace, fingerprints, tails, envDigest, minimum, match.AuthID) {
		return nil, false
	}
	if bound, boundOK := s.matcher.MatchFingerprintsWithContext(namespace, fingerprints, tails, envDigest, minimum); boundOK {
		match = bound
	}
	setLCPMatchMetadata(opts.Metadata, match.SessionID, match.ParentSessionID, match.IsFork, match.IsCompaction, match.AccessNumber)
	publishSessionAffinitySelection(opts, auth, provider, model, "session_affinity_lcp")
	return auth, true
}

func (s *SessionAffinitySelector) Remember(provider string, opts cliproxyexecutor.Options, auth *Auth) {
	if s == nil || auth == nil {
		return
	}
	model := sessionMetadataString(opts.Metadata, cliproxyexecutor.SessionAffinityModelMetadataKey)
	setSessionAffinityNamespaceMetadata(opts.Metadata, provider, model)
	primaryID, parentID := extractExplicitSessionIDs(opts.Headers, opts.OriginalRequest, opts.Metadata)
	if primaryID == "" {
		if s.rememberLCP(provider, model, opts, auth.ID) {
			return
		}
		primaryID, parentID = extractSessionIDs(opts.Headers, opts.OriginalRequest, opts.Metadata)
	}
	if primaryID == "" || s.cache == nil {
		return
	}
	primaryID = cliproxysession.BoundSessionIdentity(primaryID)
	parentID = cliproxysession.BoundSessionIdentity(parentID)
	setCanonicalSessionMetadata(opts.Metadata, primaryID, parentID)
	isFork := metadataBool(opts.Metadata, cliproxyexecutor.IsForkMetadataKey)
	isSubagent := !isFork && isSubagentSession(primaryID, parentID)
	cacheKey, parentKey, aliasParent := affinityCacheKeys(provider, model, primaryID, parentID, isFork, isSubagent)
	if aliasParent && parentKey != "" {
		s.cache.SetAliases(auth.ID, cacheKey, parentKey)
		return
	}
	s.cache.Set(cacheKey, auth.ID)
}

func (s *SessionAffinitySelector) rememberLCP(
	provider,
	model string,
	opts cliproxyexecutor.Options,
	authID string,
) bool {
	if s.matcher == nil {
		return false
	}
	fingerprints, prepared := lcpFingerprintsFromMetadata(opts.Metadata)
	if !prepared {
		return false
	}
	minimum := metadataInt(opts.Metadata, cliproxyexecutor.LCPMinPrefixLengthMetadataKey)
	namespace := lcpAffinityNamespace(provider, model, opts.Metadata)
	if namespace == "" || minimum <= 0 || minimum > len(fingerprints) {
		return false
	}
	tails, _ := opts.Metadata[cliproxyexecutor.LCPTailFingerprintsMetadataKey].([]string)
	envDigest := sessionMetadataString(opts.Metadata, cliproxyexecutor.LCPEnvironmentDigestMetadataKey)
	result := s.matcher.BindFingerprintsWithContext(namespace, fingerprints, tails, envDigest, minimum, authID)
	if result.SessionID == "" {
		return false
	}
	setLCPMatchMetadata(opts.Metadata, result.SessionID, result.ParentSessionID, result.IsFork, result.IsCompaction, result.AccessNumber)
	return true
}

// RecordResult applies expected-auth and generation guards to one completed attempt.
func (s *SessionAffinitySelector) RecordResult(
	provider,
	model string,
	opts cliproxyexecutor.Options,
	authID string,
	success bool,
) {
	if s == nil || strings.TrimSpace(authID) == "" {
		return
	}
	if metadataProvider := sessionMetadataString(opts.Metadata, cliproxyexecutor.SessionAffinityProviderMetadataKey); metadataProvider != "" {
		provider = metadataProvider
	}
	if metadataModel := sessionMetadataString(opts.Metadata, cliproxyexecutor.SessionAffinityModelMetadataKey); metadataModel != "" {
		model = metadataModel
	}
	primaryID, parentID := extractExplicitSessionIDs(opts.Headers, opts.OriginalRequest, opts.Metadata)
	if primaryID == "" && s.matcher != nil {
		fingerprints, prepared := lcpFingerprintsFromMetadata(opts.Metadata)
		minimum := metadataInt(opts.Metadata, cliproxyexecutor.LCPMinPrefixLengthMetadataKey)
		namespace := lcpAffinityNamespace(provider, model, opts.Metadata)
		if prepared && namespace != "" && minimum > 0 && minimum <= len(fingerprints) {
			if success {
				if opts.ExecutionLifecycle != nil {
					s.matcher.CompleteSessionSelection(
						namespace, fingerprints,
						sessionMetadataString(opts.Metadata, cliproxyexecutor.LCPAffinitySessionIDMetadataKey),
						authID, metadataUint64(opts.Metadata, cliproxyexecutor.LCPAccessGenerationMetadataKey),
					)
				} else {
					tails, _ := opts.Metadata[cliproxyexecutor.LCPTailFingerprintsMetadataKey].([]string)
					envDigest := sessionMetadataString(opts.Metadata, cliproxyexecutor.LCPEnvironmentDigestMetadataKey)
					s.matcher.TouchFingerprintsWithContext(namespace, fingerprints, tails, envDigest, minimum, authID)
				}
			} else {
				s.matcher.RemoveFingerprintsBefore(
					namespace, fingerprints, authID,
					metadataUint64(opts.Metadata, cliproxyexecutor.LCPAccessGenerationMetadataKey),
				)
			}
			return
		}
	}
	if primaryID == "" {
		primaryID, parentID = extractSessionIDs(opts.Headers, opts.OriginalRequest, opts.Metadata)
	}
	if primaryID == "" || s.cache == nil {
		return
	}
	primaryID = cliproxysession.BoundSessionIdentity(primaryID)
	parentID = cliproxysession.BoundSessionIdentity(parentID)
	isFork := metadataBool(opts.Metadata, cliproxyexecutor.IsForkMetadataKey)
	isSubagent := !isFork && isSubagentSession(primaryID, parentID)
	cacheKey, parentKey, aliasParent := affinityCacheKeys(provider, model, primaryID, parentID, isFork, isSubagent)
	if success {
		s.cache.Touch(cacheKey, authID)
		if aliasParent && parentKey != "" {
			s.cache.Touch(parentKey, authID)
		}
		return
	}
	s.cache.CompareAndDelete(cacheKey, authID)
	if aliasParent && parentKey != "" {
		s.cache.CompareAndDelete(parentKey, authID)
	}
}

// OnResult refreshes or releases affinity only for credential-attributed outcomes.
func (s *SessionAffinitySelector) OnResult(result Result) {
	if s == nil || strings.TrimSpace(result.AuthID) == "" {
		return
	}
	if result.Error != nil && shouldSkipCredentialCooldown(result.Error) {
		return
	}
	s.RecordResult(result.Provider, result.Model, result.Options, result.AuthID, result.Success)
}

func (s *SessionAffinitySelector) InvalidateAuth(authID string) {
	if s == nil {
		return
	}
	if s.cache != nil {
		s.cache.InvalidateAuth(authID)
	}
	if s.matcher != nil {
		s.matcher.InvalidateAuth(authID)
	}
}

// LookupAffinity observes a session binding without selecting, binding, or refreshing its TTL.
func (s *SessionAffinitySelector) LookupAffinity(
	provider,
	model,
	sessionID string,
	authFilters ...func(string) bool,
) (string, string) {
	if s == nil || s.cache == nil {
		return "", "unsupported"
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	model = strings.TrimSpace(model)
	sessionID = strings.TrimSpace(sessionID)
	if provider == "" || model == "" || sessionID == "" {
		return "", "unbound"
	}
	var authFilter func(string) bool
	if len(authFilters) > 0 {
		authFilter = authFilters[0]
	}
	modelKey := canonicalModelKey(model)
	if modelKey == "" {
		modelKey = model
	}
	providers := []string{provider}
	if provider != "mixed" {
		providers = append(providers, "mixed")
	}
	prefixes := cliproxysession.CandidateSessionPrefixes
	hasKnownPrefix := false
	for _, prefix := range prefixes {
		if strings.HasPrefix(sessionID, prefix) {
			hasKnownPrefix = true
			break
		}
	}
	candidates := []string{sessionID}
	if !hasKnownPrefix {
		candidates = make([]string, 0, len(prefixes)+1)
		candidates = append(candidates, sessionID)
		for _, prefix := range prefixes {
			candidates = append(candidates, prefix+sessionID)
		}
	}
	foundAuths := make(map[string]struct{})
	for _, candidateProvider := range providers {
		for _, candidate := range candidates {
			bounded := cliproxysession.BoundSessionIdentity(candidate)
			key := candidateProvider + "::" + bounded + "::" + modelKey
			if authID, ok := s.cache.Get(key); ok && authID != "" &&
				(authFilter == nil || authFilter(authID)) {
				foundAuths[authID] = struct{}{}
			}
		}
	}
	if s.matcher != nil {
		for _, candidate := range candidates {
			authIDs, namespace, ok := s.matcher.LookupSession(candidate)
			if !ok || len(authIDs) == 0 {
				continue
			}
			namespaceProvider, namespaceModel, _, parsed := parseLCPNamespace(namespace)
			if parsed && ((provider != "mixed" && namespaceProvider != "mixed" &&
				canonicalLCPProvider(namespaceProvider) != canonicalLCPProvider(provider)) ||
				(modelKey != "" && namespaceModel != modelKey)) {
				continue
			}
			for _, authID := range authIDs {
				if authFilter == nil || authFilter(authID) {
					foundAuths[authID] = struct{}{}
				}
			}
		}
	}
	switch len(foundAuths) {
	case 0:
		return "", "unbound"
	case 1:
		for authID := range foundAuths {
			return authID, "bound"
		}
	default:
		return "", "ambiguous"
	}
	return "", "unbound"
}

func (s *SessionAffinitySelector) Stop() {
	if s == nil {
		return
	}
	if s.cache != nil {
		s.cache.Stop()
	}
	if s.matcher != nil {
		s.matcher.Clear()
	}
}

func availableAffinityAuths(auths []*Auth, provider, model string, opts cliproxyexecutor.Options) []*Auth {
	available, _ := getAvailableAuths(auths, provider, model, opts, time.Now())
	return available
}

func affinityCacheKeys(provider, model, primaryID, parentID string, isFork, isSubagent bool) (string, string, bool) {
	modelKey := canonicalModelKey(model)
	primaryKey := provider + "::" + primaryID + "::" + modelKey
	if parentID == "" || parentID == primaryID {
		return primaryKey, "", false
	}
	parentKey := provider + "::" + parentID + "::" + modelKey
	return primaryKey, parentKey, !isFork && !isSubagent
}

func authByID(auths []*Auth, authID string) *Auth {
	for _, auth := range auths {
		if auth != nil && auth.ID == authID {
			return auth
		}
	}
	return nil
}

func publishSessionAffinitySelection(
	opts cliproxyexecutor.Options,
	auth *Auth,
	provider,
	model,
	reason string,
) {
	publishAuthRouteTraceEvent(opts, auth, cliproxyexecutor.RouteTraceEvent{
		Provider: provider,
		Model:    model,
		Action:   "candidate.selected",
		Reason:   reason,
	})
}

func extractExplicitSessionIDs(headers http.Header, payload []byte, metadata map[string]any) (string, string) {
	info, ok := cliproxysession.ExtractSessionInfo(headers, payload, metadata)
	if !ok || info.ClientType == "lcp" {
		return "", ""
	}
	if metadata != nil {
		if info.IsFork {
			metadata[cliproxyexecutor.IsForkMetadataKey] = true
		} else {
			delete(metadata, cliproxyexecutor.IsForkMetadataKey)
		}
		if info.ParentSessionID != "" {
			metadata[cliproxyexecutor.ParentSessionIDMetadataKey] = info.ParentSessionID
		} else {
			delete(metadata, cliproxyexecutor.ParentSessionIDMetadataKey)
		}
	}
	parentID := info.ParentSessionID
	if parentID == "" && strings.HasPrefix(info.SessionID, "pck:") {
		parentID = extractConversationAlias(payload)
	}
	return info.SessionID, parentID
}

func extractConversationAlias(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	root := gjson.GetBytes(payload, "conversation")
	if !root.Exists() {
		request := gjson.GetBytes(payload, "request")
		if request.Exists() && !gjson.GetBytes(payload, "contents").Exists() {
			root = request.Get("conversation")
		}
	}
	if sessionID := normalizedSessionCandidate(root.Get("id").String()); sessionID != "" {
		return "conv:" + sessionID
	}
	if root.Type == gjson.String {
		if sessionID := normalizedSessionCandidate(root.String()); sessionID != "" {
			return "conv:" + sessionID
		}
	}
	return ""
}

func isSubagentSession(primaryID, parentID string) bool {
	if strings.Contains(primaryID, ":agent:") {
		return true
	}
	return isHierarchyParent(primaryID, parentID)
}

func canonicalLCPProvider(provider string) string {
	provider = strings.ToLower(strings.TrimSpace(provider))
	switch provider {
	case "google", "gemini", "vertex", "aistudio":
		return "google"
	case "codex", "openai":
		return "openai"
	case "claude", "anthropic":
		return "claude"
	default:
		return provider
	}
}

func lcpAffinityNamespace(provider, model string, metadata map[string]any) string {
	provider = canonicalLCPProvider(provider)
	callerScope := sessionMetadataString(metadata, cliproxyexecutor.CallerScopeMetadataKey)
	if provider == "" || callerScope == "" {
		return ""
	}
	return strings.Join([]string{"lcp:v1", provider, canonicalModelKey(model), callerScope}, "::")
}

func parseLCPNamespace(namespace string) (provider, model, callerScope string, ok bool) {
	if !strings.HasPrefix(namespace, "lcp:v1::") {
		return "", "", "", false
	}
	rest := strings.TrimPrefix(namespace, "lcp:v1::")
	provider, rest, found := strings.Cut(rest, "::")
	if !found {
		return "", "", "", false
	}
	lastSeparator := strings.LastIndex(rest, "::")
	if lastSeparator < 0 {
		return provider, rest, "", true
	}
	return provider, rest[:lastSeparator], rest[lastSeparator+2:], true
}

func lcpFingerprintsFromMetadata(metadata map[string]any) ([]string, bool) {
	if metadata == nil {
		return nil, false
	}
	switch value := metadata[cliproxyexecutor.LCPFingerprintMetadataKey].(type) {
	case []string:
		return value, len(value) > 0
	case []any:
		fingerprints := make([]string, 0, len(value))
		for _, raw := range value {
			if fingerprint, ok := raw.(string); ok && fingerprint != "" {
				fingerprints = append(fingerprints, fingerprint)
			}
		}
		return fingerprints, len(fingerprints) > 0
	default:
		return nil, false
	}
}

func setSessionAffinityNamespaceMetadata(metadata map[string]any, provider, model string) {
	if metadata == nil {
		return
	}
	metadata[cliproxyexecutor.SessionAffinityProviderMetadataKey] = provider
	metadata[cliproxyexecutor.SessionAffinityModelMetadataKey] = model
}

func setCanonicalSessionMetadata(metadata map[string]any, sessionID, parentID string) {
	if metadata == nil || sessionID == "" {
		return
	}
	metadata[cliproxyexecutor.CanonicalSessionIDMetadataKey] = sessionID
	if parentID != "" && parentID != sessionID {
		metadata[cliproxyexecutor.ParentSessionIDMetadataKey] = parentID
	} else {
		delete(metadata, cliproxyexecutor.ParentSessionIDMetadataKey)
	}
}

func setLCPMetadata(metadata map[string]any, fingerprints []string, minimum int, tails []string, envDigest string) {
	if metadata == nil {
		return
	}
	metadata[cliproxyexecutor.LCPFingerprintMetadataKey] = fingerprints
	metadata[cliproxyexecutor.LCPMinPrefixLengthMetadataKey] = minimum
	metadata[cliproxyexecutor.LCPTailFingerprintsMetadataKey] = tails
	metadata[cliproxyexecutor.LCPEnvironmentDigestMetadataKey] = envDigest
}

func setLCPMatchMetadata(metadata map[string]any, sessionID, parentID string, isFork, isCompaction bool, accessNumber uint64) {
	if metadata == nil {
		return
	}
	metadata[cliproxyexecutor.LCPAffinitySessionIDMetadataKey] = sessionID
	setCanonicalSessionMetadata(metadata, sessionID, parentID)
	metadata[cliproxyexecutor.LCPAccessGenerationMetadataKey] = accessNumber
	switch {
	case isFork:
		metadata[cliproxyexecutor.IsForkMetadataKey] = true
		delete(metadata, cliproxyexecutor.IsCompactionMetadataKey)
		metadata[cliproxyexecutor.NodeKindMetadataKey] = "fork"
	case isCompaction:
		delete(metadata, cliproxyexecutor.IsForkMetadataKey)
		metadata[cliproxyexecutor.IsCompactionMetadataKey] = true
		metadata[cliproxyexecutor.NodeKindMetadataKey] = "compaction"
	default:
		delete(metadata, cliproxyexecutor.IsForkMetadataKey)
		delete(metadata, cliproxyexecutor.IsCompactionMetadataKey)
		delete(metadata, cliproxyexecutor.NodeKindMetadataKey)
	}
}

func sessionMetadataString(metadata map[string]any, key string) string {
	if metadata == nil {
		return ""
	}
	value, _ := metadata[key].(string)
	return strings.TrimSpace(value)
}

func metadataBool(metadata map[string]any, key string) bool {
	if metadata == nil {
		return false
	}
	value, _ := metadata[key].(bool)
	return value
}

func metadataInt(metadata map[string]any, key string) int {
	if metadata == nil {
		return 0
	}
	switch value := metadata[key].(type) {
	case int:
		return value
	case int64:
		return int(value)
	case float64:
		return int(value)
	default:
		return 0
	}
}

func metadataUint64(metadata map[string]any, key string) uint64 {
	if metadata == nil {
		return 0
	}
	switch value := metadata[key].(type) {
	case uint64:
		return value
	case uint:
		return uint64(value)
	case int:
		if value > 0 {
			return uint64(value)
		}
	case int64:
		if value > 0 {
			return uint64(value)
		}
	case float64:
		if value > 0 {
			return uint64(value)
		}
	}
	return 0
}

// CanonicalSessionID returns the strongest bounded identity available for reporting.
func CanonicalSessionID(headers http.Header, payload []byte, metadata map[string]any) string {
	if explicitID, _ := extractExplicitSessionIDs(headers, payload, metadata); explicitID != "" {
		return cliproxysession.BoundSessionIdentity(explicitID)
	}
	for _, key := range []string{
		cliproxyexecutor.CanonicalSessionIDMetadataKey,
		cliproxyexecutor.LCPAffinitySessionIDMetadataKey,
	} {
		if value := sessionMetadataString(metadata, key); value != "" {
			return cliproxysession.BoundSessionIdentity(value)
		}
	}
	return cliproxysession.BoundSessionIdentity(ExtractSessionID(headers, payload, metadata))
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
	cause    error
}

// NewModelCooldownError creates an error representing model-level cooldown.
func NewModelCooldownError(model, provider string, resetIn time.Duration) error {
	return newModelCooldownErrorWithCause(model, provider, resetIn, nil)
}

func newModelCooldownErrorWithCause(
	model,
	provider string,
	resetIn time.Duration,
	cause error,
) *modelCooldownError {
	if resetIn < 0 {
		resetIn = 0
	}
	return &modelCooldownError{
		model:    model,
		provider: provider,
		resetIn:  resetIn,
		cause:    cause,
	}
}

func (e *modelCooldownError) IsModelCooldown() bool {
	return e != nil
}

func (e *modelCooldownError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
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
		if next.After(now) {
			if earliest.IsZero() || next.Before(earliest) {
				earliest = next
			}
			retryAfterMS = next.Sub(now).Milliseconds()
		}
		// None and other blocked reasons retain the generic blocked route reason.
		// noinspection GoSwitchMissingCasesForIotaConsts
		switch reason {
		case blockReasonCooldown:
			reasonCode = "cooldown"
			cooldownCount++
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
		lastCandidateErr := latestSelectionCandidateError(auths, model)
		if cooldownCount == len(auths) && !earliest.IsZero() {
			providerForError := provider
			if providerForError == "mixed" {
				providerForError = ""
			}
			resetIn := max(earliest.Sub(now), 0)
			return nil, newModelCooldownErrorWithCause(model, providerForError, resetIn, lastCandidateErr)
		}
		return nil, newAuthUnavailableErrorWithCause(earliest, now, lastCandidateErr)
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

func latestSelectionCandidateError(auths []*Auth, model string) error {
	var (
		latestModelErr    error
		latestModelTime   time.Time
		latestModelAuthID string
		latestAuthErr     error
		latestAuthTime    time.Time
		latestAuthID      string
	)
	for _, auth := range auths {
		if auth == nil {
			continue
		}
		modelCandidate := modelCandidateError(auth, model)
		if modelCandidate.err != nil &&
			(latestModelErr == nil || modelCandidate.observedAt.After(latestModelTime) ||
				modelCandidate.observedAt.Equal(latestModelTime) && auth.ID > latestModelAuthID) {
			latestModelErr = modelCandidate.err
			latestModelTime = modelCandidate.observedAt
			latestModelAuthID = auth.ID
		}
		authCandidate := credentialCandidateError(auth)
		if authCandidate.err != nil &&
			(latestAuthErr == nil || authCandidate.observedAt.After(latestAuthTime) ||
				authCandidate.observedAt.Equal(latestAuthTime) && auth.ID > latestAuthID) {
			latestAuthErr = authCandidate.err
			latestAuthTime = authCandidate.observedAt
			latestAuthID = auth.ID
		}
	}
	if latestModelErr != nil {
		return latestModelErr
	}
	return latestAuthErr
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

// maxSmoothWeightedStateEntries bounds stale credentials retained by one accumulator.
const maxSmoothWeightedStateEntries = 1024

// prepare preserves credits across temporary candidate subsets and resets them
// when a retained credential changes weight.
func (s *smoothWeightedState) prepare(weights map[string]int64) {
	if s.current == nil || weightsConfigChanged(s.weights, weights) {
		s.current = make(map[string]int64, len(weights))
	}
	if s.weights == nil {
		s.weights = make(map[string]int64, len(weights))
	}
	for authID, weight := range weights {
		s.weights[authID] = weight
	}
	s.pruneStale(weights)
}

// pruneStale removes inactive credentials only after retained state crosses the bound.
func (s *smoothWeightedState) pruneStale(weights map[string]int64) {
	if len(s.current) <= maxSmoothWeightedStateEntries && len(s.weights) <= maxSmoothWeightedStateEntries {
		return
	}
	for authID := range s.current {
		if _, ok := weights[authID]; !ok {
			delete(s.current, authID)
		}
	}
	for authID := range s.weights {
		if _, ok := weights[authID]; !ok {
			delete(s.weights, authID)
		}
	}
}

// weightsConfigChanged ignores identities missing from either side because filtering changes membership.
func weightsConfigChanged(left, right map[string]int64) bool {
	if len(left) == 0 {
		return false
	}
	for authID, weight := range right {
		if previous, ok := left[authID]; ok && previous != weight {
			return true
		}
	}
	return false
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
		state.current[auth.ID] = saturatingAddInt64(state.current[auth.ID], weight)
		totalWeight = saturatingAddInt64(totalWeight, weight)
		if picked == nil || state.current[auth.ID] > pickedCurrent {
			picked = auth
			pickedCurrent = state.current[auth.ID]
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
	if opts.Metadata == nil {
		opts.Metadata = make(map[string]any)
	}
	setSessionAffinityNamespaceMetadata(opts.Metadata, provider, model)
	if auth, _ := s.pickStickyForModelWithRule(provider, model, opts, auths); auth != nil {
		return auth, nil
	}
	auth, err := s.fallback.Pick(ctx, provider, model, opts, auths)
	if err != nil || auth == nil {
		return auth, err
	}
	s.Remember(provider, opts, auth)
	return auth, nil
}

func isAuthBlockedForModel(auth *Auth, model string, now time.Time) (bool, blockReason, time.Time) {
	if auth == nil {
		return true, blockReasonOther, time.Time{}
	}
	if auth.Disabled || auth.Status == StatusDisabled {
		return true, blockReasonDisabled, time.Time{}
	}
	if hasTerminalUnauthorizedAuthFailure(auth) || auth.hasRejectedAccessToken() {
		return true, blockReasonOther, time.Time{}
	}
	if expiresAt, ok := auth.AccessTokenExpirationTime(); ok && !expiresAt.IsZero() && !expiresAt.After(now) {
		return true, blockReasonOther, time.Time{}
	}
	credentialScoped := auth.StateEffectScope == cliproxyexecutor.StateEffectScopeCredential
	if !credentialScoped && auth.LastError != nil {
		credentialScoped = auth.LastError.EffectScope == cliproxyexecutor.StateEffectScopeCredential
	}
	if credentialScoped {
		if blocked, reason, next := unavailableRoutingBlock(
			auth.Unavailable,
			auth.Quota.Exceeded,
			auth.NextRetryAfter,
			auth.Quota.NextRecoverAt,
			now,
		); blocked {
			return true, reason, next
		}
	}
	if model != "" {
		if len(auth.ModelStates) > 0 {
			state, _ := findModelState(auth, model)
			if state == nil {
				return false, blockReasonNone, time.Time{}
			}
			if state.Status == StatusDisabled {
				return true, blockReasonDisabled, time.Time{}
			}
			return unavailableRoutingBlock(
				state.Unavailable,
				state.Quota.Exceeded,
				state.NextRetryAfter,
				state.Quota.NextRecoverAt,
				now,
			)
		}
		return unavailableRoutingBlock(
			auth.Unavailable,
			auth.Quota.Exceeded,
			auth.NextRetryAfter,
			auth.Quota.NextRecoverAt,
			now,
		)
	}
	return unavailableRoutingBlock(
		auth.Unavailable,
		auth.Quota.Exceeded,
		auth.NextRetryAfter,
		auth.Quota.NextRecoverAt,
		now,
	)
}

func unavailableRoutingBlock(
	unavailable bool,
	quotaExceeded bool,
	nextRetryAfter,
	nextRecoverAt,
	now time.Time,
) (bool, blockReason, time.Time) {
	next := nextRetryAfter
	if quotaExceeded && nextRecoverAt.After(next) {
		next = nextRecoverAt
	}
	if !unavailable && (!quotaExceeded || !next.After(now)) {
		return false, blockReasonNone, time.Time{}
	}
	if next.IsZero() {
		return true, blockReasonOther, time.Time{}
	}
	if !next.After(now) {
		return false, blockReasonNone, time.Time{}
	}
	if quotaExceeded {
		return true, blockReasonCooldown, next
	}
	return true, blockReasonOther, next
}
