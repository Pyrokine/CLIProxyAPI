package auth

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type requiredAuthKindContextKey struct{}
type credentialPolicyContextKey struct{}

type homeCredentialPolicyDispatcher interface {
	RPopAuthWithPolicy(
		ctx context.Context,
		requestedModel, sessionID string,
		headers http.Header,
		count int,
		credentialPolicy string,
	) ([]byte, error)
}

type authSelectionEligibility struct {
	retryRound          int
	defaultRetry        int
	excluded            map[string]struct{}
	requiredKind        string
	credentialPolicy    string
	disallowFreeAuth    bool
	requireContinuation bool
	requireAPIKey       bool
}

func withRequiredAuthKind(ctx context.Context, requiredKind string) context.Context {
	return context.WithValue(ctx, requiredAuthKindContextKey{}, requiredKind)
}

func withCredentialPolicy(ctx context.Context, policy string) context.Context {
	return context.WithValue(ctx, credentialPolicyContextKey{}, policy)
}

func credentialPolicyFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	policy, _ := ctx.Value(credentialPolicyContextKey{}).(string)
	return policy
}

func authSelectionEligibilityForRequest(ctx context.Context, opts cliproxyexecutor.Options) authSelectionEligibility {
	requireAPIKey, _ := opts.Metadata[cliproxyexecutor.RequireAPIKeyAuthMetadataKey].(bool)
	eligibility := authSelectionEligibility{
		requireAPIKey:       requireAPIKey,
		excluded:            excludedAuthIDs(opts),
		disallowFreeAuth:    disallowFreeAuthFromMetadata(opts.Metadata),
		requireContinuation: requireResponsesContinuationFromMetadata(opts.Metadata),
	}
	if ctx != nil {
		eligibility.requiredKind, _ = ctx.Value(requiredAuthKindContextKey{}).(string)
		eligibility.credentialPolicy, _ = ctx.Value(credentialPolicyContextKey{}).(string)
	}
	if state := retryRoundFromContext(ctx); state != nil {
		eligibility.retryRound, eligibility.defaultRetry = state.round, state.defaultRetry
	}
	return eligibility
}

func (e authSelectionEligibility) unrestricted() bool {
	return e.retryRound == 0 && len(e.excluded) == 0 && e.requiredKind == "" && e.credentialPolicy == "" &&
		!e.disallowFreeAuth &&
		!e.requireContinuation && !e.requireAPIKey
}

func (e authSelectionEligibility) allows(auth *Auth) bool {
	if auth == nil {
		return false
	}
	if _, excluded := e.excluded[auth.ID]; excluded {
		return false
	}
	if e.retryRound > effectiveRequestRetryLimit(auth, e.defaultRetry) {
		return false
	}
	if e.requiredKind != "" && auth.AuthKind() != e.requiredKind {
		return false
	}
	if e.requireAPIKey && auth.AuthKind() != AuthKindAPIKey {
		return false
	}
	if e.credentialPolicy != "" && !credentialPolicyAllows(e.credentialPolicy, auth) {
		return false
	}
	if e.disallowFreeAuth && isFreeCodexAuth(auth) {
		return false
	}
	return !e.requireContinuation || authSupportsResponsesContinuation(auth)
}

// ResolveExecutionModel returns the credential-aware upstream model used by normal execution.
func (m *Manager) ResolveExecutionModel(auth *Auth, routeModel string) string {
	routeModel = strings.TrimSpace(routeModel)
	if auth == nil {
		return routeModel
	}
	if auth.Attributes != nil {
		if upstreamModel := strings.TrimSpace(auth.Attributes[homeUpstreamModelAttributeKey]); upstreamModel != "" {
			return upstreamModel
		}
	}
	resolved := rewriteModelForAuth(routeModel, auth)
	if m == nil {
		return resolved
	}
	aliases := m.executionModelAliases(auth, resolved)
	fallback := aliases[0].UpstreamModel
	var earliest time.Time
	foundEligible := false
	now := time.Now()
	for _, alias := range aliases {
		target := strings.TrimSpace(alias.UpstreamModel)
		if thinking.HasContext1MTag(routeModel) && !m.executionModelSupportsContext1M(auth, resolved, target) {
			continue
		}
		blocked, _, retryAt := isAuthBlockedForModel(auth, target, now)
		if !blocked {
			return target
		}
		if !foundEligible || !retryAt.IsZero() && (earliest.IsZero() || retryAt.Before(earliest)) {
			fallback, earliest = target, retryAt
			foundEligible = true
		}
	}
	return fallback
}

func (m *Manager) executionModelAliases(auth *Auth, requestedModel string) []OAuthModelAliasResult {
	if !m.HomeEnabled() && auth != nil {
		if kind, _ := auth.AccountInfo(); strings.EqualFold(kind, "api_key") {
			if pool := resolveModelAliasPoolFromConfigModels(
				requestedModel, m.apiKeyModelAliasEntries(auth),
			); len(pool) > 0 {
				return pool
			}
		}
	}
	result := m.resolveExecutionAliasResultForRequested(auth, requestedModel)
	if auth != nil && strings.TrimSpace(auth.Attributes[homeUpstreamModelAttributeKey]) != "" {
		result.UpstreamModel = strings.TrimSpace(auth.Attributes[homeUpstreamModelAttributeKey])
	}
	if result.UpstreamModel == "" {
		result.UpstreamModel = requestedModel
	}
	return []OAuthModelAliasResult{result}
}

func (m *Manager) rotateExecutionModelAliases(
	auth *Auth,
	requestedModel string,
	aliases []OAuthModelAliasResult,
) []OAuthModelAliasResult {
	if len(aliases) < 2 {
		return aliases
	}
	key := canonicalModelKey(requestedModel)
	m.mu.Lock()
	if current := m.auths[auth.ID]; current == nil || current.credentialGeneration != auth.credentialGeneration {
		m.mu.Unlock()
		return aliases
	}
	if m.modelPoolOffsets == nil {
		m.modelPoolOffsets = make(map[string]map[string]int)
	}
	offsets := m.modelPoolOffsets[auth.ID]
	if offsets == nil {
		offsets = make(map[string]int)
		m.modelPoolOffsets[auth.ID] = offsets
	}
	offset := offsets[key] % len(aliases)
	offsets[key] = (offset + 1) % len(aliases)
	m.mu.Unlock()
	return append(append(make([]OAuthModelAliasResult, 0, len(aliases)), aliases[offset:]...), aliases[:offset]...)
}

func (m *Manager) selectionModelForAuth(auth *Auth, routeModel string) string {
	// 别名改变实际模型时使用目标状态，显式前缀键沿用原作用域
	target := m.ResolveExecutionModel(auth, routeModel)
	if canonicalModelKey(target) == canonicalModelKey(rewriteModelForAuth(routeModel, auth)) {
		return routeModel
	}
	return target
}

func stateModelForExecution(auth *Auth, routeModel, upstreamModel string) string {
	if strings.TrimSpace(upstreamModel) == "" ||
		canonicalModelKey(upstreamModel) == canonicalModelKey(rewriteModelForAuth(routeModel, auth)) {
		return routeModel
	}
	return upstreamModel
}

func (m *Manager) authSupportsRouteModel(auth *Auth, routeModel string) bool {
	routeKey := canonicalModelKey(routeModel)
	if routeKey == "" {
		return true
	}
	reg := registry.GetGlobalRegistry()
	if reg.ClientSupportsModel(auth.ID, routeKey) {
		return true
	}
	target := canonicalModelKey(m.ResolveExecutionModel(auth, routeModel))
	return target != "" && target != routeKey && reg.ClientSupportsModel(auth.ID, target)
}

func (m *Manager) availableAuthsForRouteModel(auths []*Auth, provider, routeModel string) ([]*Auth, error) {
	available := auths[:0]
	var unavailable []*Auth
	now := time.Now()
	for _, candidate := range auths {
		target := m.selectionModelForAuth(candidate, routeModel)
		if target != routeModel {
			if blocked, _, _ := isAuthBlockedForModel(candidate, target, now); blocked {
				unavailable = append(unavailable, candidate)
				continue
			}
		}
		available = append(available, candidate)
	}
	if len(available) == 0 {
		return nil, m.unavailableSelectionError(unavailable, provider, routeModel)
	}
	return available, nil
}

// SelectAuth selects one local credential through the configured scheduling strategy.
func (m *Manager) SelectAuth(
	ctx context.Context,
	provider, model string,
	opts cliproxyexecutor.Options,
) (*Auth, error) {
	if m != nil && m.HomeEnabled() {
		return nil, &Error{
			Code: "home_unavailable", Message: "local auth selection is unavailable while Home is enabled",
			HTTPStatus: http.StatusServiceUnavailable,
		}
	}
	selected, _, errPick := m.PickNext(ctx, provider, model, opts, nil)
	if errPick != nil {
		return nil, errPick
	}
	if selected == nil {
		return nil, &Error{Code: "auth_not_found", Message: "selector returned no auth"}
	}
	if m != nil && m.HomeEnabled() {
		return nil, &Error{
			Code: "home_unavailable", Message: "local auth selection is unavailable while Home is enabled",
			HTTPStatus: http.StatusServiceUnavailable,
		}
	}
	return selected, nil
}

// SelectAuthByKind selects one local credential of the required kind.
func (m *Manager) SelectAuthByKind(
	ctx context.Context,
	provider, model, requiredKind string,
	opts cliproxyexecutor.Options,
) (*Auth, error) {
	requiredKind = normalizeAuthKind(requiredKind)
	if requiredKind == "" {
		return nil, &Error{
			Code: "invalid_auth_kind", Message: "required auth kind is invalid", HTTPStatus: http.StatusBadRequest,
		}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return m.SelectAuth(withRequiredAuthKind(ctx, requiredKind), provider, model, opts)
}

// SelectAuthWithCredentialPolicy selects one local credential allowed by a fixed policy.
func (m *Manager) SelectAuthWithCredentialPolicy(
	ctx context.Context,
	provider, model, policy string,
	opts cliproxyexecutor.Options,
) (*Auth, error) {
	policy = normalizeCredentialPolicy(policy)
	if policy == "" {
		return nil, &Error{
			Code: "invalid_credential_policy", Message: "credential policy is invalid",
			HTTPStatus: http.StatusBadRequest,
		}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return m.SelectAuth(withCredentialPolicy(ctx, policy), provider, model, opts)
}

// SelectHomeAuthByKind selects a Home dispatch while retaining its execution scope.
func (m *Manager) SelectHomeAuthByKind(
	ctx context.Context,
	provider, model, requiredKind string,
	opts cliproxyexecutor.Options,
) (*HomeDispatchSelection, error) {
	requiredKind = normalizeAuthKind(requiredKind)
	if requiredKind == "" {
		return nil, &Error{
			Code: "invalid_auth_kind", Message: "required auth kind is invalid", HTTPStatus: http.StatusBadRequest,
		}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return m.selectHomeAuth(
		ctx, provider, model, withRequiredAuthKind(ctx, requiredKind), opts, func(auth *Auth) bool {
			return auth != nil && auth.AuthKind() == requiredKind
		},
	)
}

// SelectHomeAuthWithCredentialPolicy selects a policy-constrained Home dispatch while retaining its execution scope.
func (m *Manager) SelectHomeAuthWithCredentialPolicy(
	ctx context.Context,
	provider, model, policy string,
	opts cliproxyexecutor.Options,
) (*HomeDispatchSelection, error) {
	policy = normalizeCredentialPolicy(policy)
	if policy == "" {
		return nil, &Error{
			Code: "invalid_credential_policy", Message: "credential policy is invalid",
			HTTPStatus: http.StatusBadRequest,
		}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	selectionCtx := withCredentialPolicy(ctx, policy)
	return m.selectHomeAuth(
		ctx, provider, model, selectionCtx, opts, func(auth *Auth) bool {
			return credentialPolicyAllows(policy, auth)
		},
	)
}

func (m *Manager) selectHomeAuthForExecution(
	ctx context.Context, model string, opts cliproxyexecutor.Options,
) (*HomeDispatchSelection, error) {
	eligibility := authSelectionEligibilityForRequest(ctx, opts)
	if eligibility.requireAPIKey {
		return m.selectHomeAuth(ctx, "", model, ctx, opts, eligibility.allows)
	}
	return m.pickHomeDispatchSelection(ctx, model, opts)
}

func (m *Manager) selectHomeAuth(
	ctx context.Context,
	provider, model string,
	selectionCtx context.Context,
	opts cliproxyexecutor.Options,
	allowed func(*Auth) bool,
) (*HomeDispatchSelection, error) {
	if m == nil || !m.HomeEnabled() {
		return nil, &Error{
			Code: "home_unavailable", Message: "home control center unavailable",
			HTTPStatus: http.StatusServiceUnavailable,
		}
	}
	homeAuthCount := homeAuthCountFromMetadata(opts.Metadata)
	tried := make(map[string]struct{})
	for {
		selectionOpts := withHomeAuthCount(opts, homeAuthCount)
		selection, errSelection := m.pickHomeDispatchSelection(selectionCtx, model, selectionOpts)
		if errSelection != nil {
			return nil, errSelection
		}
		auth := selection.CloneAuth()
		providerMatches := strings.TrimSpace(provider) == "" || strings.EqualFold(
			strings.TrimSpace(selection.Provider), strings.TrimSpace(provider),
		)
		if providerMatches && allowed(auth) {
			return selection, nil
		}

		authID := ""
		if auth != nil {
			authID = strings.TrimSpace(auth.ID)
		}
		reason := "credential_policy_mismatch"
		if !providerMatches {
			reason = "provider_mismatch"
		}
		if errEnd := m.endHomeSelectionBeforeRedispatch(ctx, selection, reason); errEnd != nil {
			return nil, errEnd
		}
		if authID == "" {
			return nil, &Error{Code: "auth_not_found", Message: "selected auth has no ID"}
		}
		if _, alreadyTried := tried[authID]; alreadyTried {
			return nil, &Error{Code: "auth_not_found", Message: "selector repeatedly returned an ineligible auth"}
		}
		tried[authID] = struct{}{}
		homeAuthCount++
	}
}
