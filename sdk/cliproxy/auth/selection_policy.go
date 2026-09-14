package auth

import (
	"context"
	"net/http"
	"strings"

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
	requiredKind        string
	credentialPolicy    string
	disallowFreeAuth    bool
	requireContinuation bool
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
	eligibility := authSelectionEligibility{
		disallowFreeAuth:    disallowFreeAuthFromMetadata(opts.Metadata),
		requireContinuation: requireResponsesContinuationFromMetadata(opts.Metadata),
	}
	if ctx != nil {
		eligibility.requiredKind, _ = ctx.Value(requiredAuthKindContextKey{}).(string)
		eligibility.credentialPolicy, _ = ctx.Value(credentialPolicyContextKey{}).(string)
	}
	return eligibility
}

func (e authSelectionEligibility) unrestricted() bool {
	return e.requiredKind == "" && e.credentialPolicy == "" && !e.disallowFreeAuth && !e.requireContinuation
}

func (e authSelectionEligibility) allows(auth *Auth) bool {
	if auth == nil {
		return false
	}
	if e.requiredKind != "" && auth.AuthKind() != e.requiredKind {
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
	if aliasResult := m.resolveExecutionAliasResultForRequested(
		auth, resolved,
	); strings.TrimSpace(aliasResult.UpstreamModel) != "" {
		return strings.TrimSpace(aliasResult.UpstreamModel)
	}
	return resolved
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
