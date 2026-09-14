package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	internalconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/constant"
	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

const apiKeyReservationTransitionTimeout = 5 * time.Second

type compiledAPIKeyPolicy struct {
	profile string
	ttl     time.Duration
	limits  []coreusage.APIKeyPolicyLimit
}

type apiKeyReservationLifecycle struct {
	store coreusage.APIKeyReservationStore
	id    string
}

func (h *BaseAPIHandler) prepareAPIKeyExecution(
	ctx context.Context,
	handlerType string,
	requestedModel string,
	allowImageModel bool,
	executionID string,
) (
	context.Context,
	[]string,
	string,
	*apiKeyReservationLifecycle,
	*ErrorMessage,
) {
	rawAPIKey := apiKeyFromContext(ctx)
	identity := coreusage.APIKeyIdentityFromContext(ctx)
	if identity == "" {
		identity = coreusage.HashAPIKeyIdentity(rawAPIKey)
	}
	ctx = coreusage.WithAPIKeyIdentity(ctx, identity)

	cfg := h.ConfigSnapshot()
	mappedModel := requestedModel
	mapped := false
	var rule internalconfig.APIKeyRule
	hasRule := false
	if cfg != nil && rawAPIKey != "" {
		rule, hasRule = cfg.APIKeyRule(rawAPIKey)
	}
	var compiled compiledAPIKeyPolicy
	if hasRule {
		var err error
		compiled, err = compileAPIKeyPolicy(rule)
		if err != nil {
			return ctx, nil, "", nil, apiKeyPolicyError(handlerType, http.StatusServiceUnavailable, time.Time{})
		}
		mappedModel, mapped, err = cfg.ResolveAPIKeyModel(rawAPIKey, requestedModel)
		if err != nil {
			return ctx, nil, "", nil, apiKeyPolicyError(handlerType, http.StatusServiceUnavailable, time.Time{})
		}
	}

	providers, canonicalModel, errMsg := h.getRequestDetailsWithOptions(mappedModel, allowImageModel)
	if errMsg != nil {
		if mapped {
			return ctx, nil, "", nil, apiKeyPolicyError(handlerType, http.StatusServiceUnavailable, time.Time{})
		}
		return ctx, nil, "", nil, errMsg
	}
	if h.modelCatalogModelDisabled(mappedModel) || h.modelCatalogModelDisabled(canonicalModel) {
		return ctx, nil, "", nil, &ErrorMessage{
			StatusCode: http.StatusForbidden,
			Error:      errors.New("requested model is disabled"),
		}
	}
	if !hasRule {
		return ctx, providers, canonicalModel, nil, nil
	}
	if h.apiKeyBlocksModel(rawAPIKey, requestedModel) || h.apiKeyBlocksModel(rawAPIKey, mappedModel) ||
		h.apiKeyBlocksModel(rawAPIKey, canonicalModel) {
		return ctx, nil, "", nil, apiKeyPolicyError(handlerType, http.StatusForbidden, time.Time{})
	}
	if !cfg.APIKeyAllowsCanonicalModel(rawAPIKey, canonicalModel) {
		return ctx, nil, "", nil, apiKeyPolicyError(handlerType, http.StatusForbidden, time.Time{})
	}
	providers = cfg.FilterAPIKeyProviders(rawAPIKey, providers)
	if len(providers) == 0 {
		return ctx, nil, "", nil, apiKeyPolicyError(handlerType, http.StatusForbidden, time.Time{})
	}
	if len(compiled.limits) == 0 {
		return ctx, providers, canonicalModel, nil, nil
	}
	store := h.apiKeyPolicyStoreSnapshot()
	if store == nil {
		return ctx, nil, "", nil, apiKeyPolicyError(handlerType, http.StatusServiceUnavailable, time.Time{})
	}
	logicalRequestID := strings.TrimSpace(executionID)
	if logicalRequestID == "" {
		return ctx, nil, "", nil, apiKeyPolicyError(handlerType, http.StatusServiceUnavailable, time.Time{})
	}
	reservation, err := store.ReserveAPIKey(
		ctx, coreusage.APIKeyReservationRequest{
			KeyIdentity:      identity,
			LogicalRequestID: logicalRequestID,
			PolicyProfile:    compiled.profile,
			RequestedModel:   requestedModel,
			CanonicalModel:   canonicalModel,
			Providers:        providers,
			TTL:              compiled.ttl,
			Limits:           compiled.limits,
			Now:              time.Now().UTC(),
		},
	)
	if err != nil {
		if limitError, ok := errors.AsType[*coreusage.APIKeyLimitExceededError](err); ok {
			return ctx, nil, "", nil, apiKeyPolicyError(handlerType, http.StatusTooManyRequests, limitError.RetryAt)
		}
		return ctx, nil, "", nil, apiKeyPolicyError(handlerType, http.StatusServiceUnavailable, time.Time{})
	}
	ctx = coreusage.WithAPIKeyReservation(ctx, reservation.ID)
	return ctx, providers, canonicalModel, &apiKeyReservationLifecycle{store: store, id: reservation.ID}, nil
}

func compileAPIKeyPolicy(rule internalconfig.APIKeyRule) (compiledAPIKeyPolicy, error) {
	compiled := compiledAPIKeyPolicy{
		profile: strings.TrimSpace(rule.PolicyProfile),
		ttl:     24 * time.Hour,
		limits:  make([]coreusage.APIKeyPolicyLimit, 0, len(rule.Limits)),
	}
	if compiled.profile == "" {
		compiled.profile = "default"
	}
	if !validAPIKeyPolicyProfile(compiled.profile) {
		return compiledAPIKeyPolicy{}, errors.New("invalid API key policy profile")
	}
	if rawTTL := strings.TrimSpace(rule.ReservationTTL); rawTTL != "" {
		ttl, err := time.ParseDuration(rawTTL)
		if err != nil || ttl <= 0 {
			return compiledAPIKeyPolicy{}, errors.New("invalid API key reservation TTL")
		}
		compiled.ttl = ttl
	}
	for _, provider := range rule.Providers {
		if strings.TrimSpace(provider) == "" {
			return compiledAPIKeyPolicy{}, errors.New("empty API key provider")
		}
	}
	for _, model := range rule.CanonicalModels {
		if strings.TrimSpace(model) == "" {
			return compiledAPIKeyPolicy{}, errors.New("empty canonical model rule")
		}
	}
	mappingSources := make(map[string]struct{}, len(rule.ModelMappings))
	for source, target := range rule.ModelMappings {
		source = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(source, "models/")))
		if source == "" || strings.TrimSpace(target) == "" {
			return compiledAPIKeyPolicy{}, errors.New("invalid API key model mapping")
		}
		if _, duplicate := mappingSources[source]; duplicate {
			return compiledAPIKeyPolicy{}, errors.New("ambiguous API key model mapping")
		}
		mappingSources[source] = struct{}{}
	}
	seenWindows := make(map[time.Duration]struct{}, len(rule.Limits))
	for _, raw := range rule.Limits {
		window, err := time.ParseDuration(strings.TrimSpace(raw.Window))
		if err != nil || window <= 0 {
			return compiledAPIKeyPolicy{}, errors.New("invalid API key policy window")
		}
		if _, duplicate := seenWindows[window]; duplicate {
			return compiledAPIKeyPolicy{}, errors.New("duplicate API key policy window")
		}
		seenWindows[window] = struct{}{}
		if raw.Requests < 0 || raw.Tokens < 0 || raw.CostMicro < 0 ||
			(raw.Requests == 0 && raw.Tokens == 0 && raw.CostMicro == 0) {
			return compiledAPIKeyPolicy{}, errors.New("invalid API key policy limit")
		}
		compiled.limits = append(
			compiled.limits, coreusage.APIKeyPolicyLimit{
				Window:    window,
				Requests:  raw.Requests,
				Tokens:    raw.Tokens,
				CostMicro: raw.CostMicro,
			},
		)
	}
	sort.Slice(
		compiled.limits, func(i, j int) bool {
			return compiled.limits[i].Window < compiled.limits[j].Window
		},
	)
	return compiled, nil
}

func validAPIKeyPolicyProfile(profile string) bool {
	if len(profile) == 0 || len(profile) > 128 {
		return false
	}
	for index := range len(profile) {
		ch := profile[index]
		if (ch < 'a' || ch > 'z') && (ch < 'A' || ch > 'Z') &&
			(ch < '0' || ch > '9') && ch != '.' && ch != '_' && ch != '-' {
			return false
		}
	}
	return true
}

func apiKeyPolicyError(handlerType string, status int, retryAt time.Time) *ErrorMessage {
	message := "API key policy does not allow this request"
	openAIType := "invalid_request_error"
	openAICode := "api_key_policy_forbidden"
	anthropicType := "permission_error"
	switch status {
	case http.StatusTooManyRequests:
		message = "API key usage limit exceeded"
		openAIType = "rate_limit_error"
		openAICode = "api_key_rate_limit_exceeded"
		anthropicType = "rate_limit_error"
	case http.StatusServiceUnavailable:
		message = "API key policy service unavailable"
		openAIType = "server_error"
		openAICode = "api_key_policy_unavailable"
		anthropicType = "api_error"
	}
	var body []byte
	if handlerType == constant.Claude {
		body, _ = json.Marshal(
			map[string]any{
				"type": "error",
				"error": map[string]any{
					"type":    anthropicType,
					"message": message,
				},
			},
		)
	} else {
		body, _ = json.Marshal(
			ErrorResponse{
				Error: ErrorDetail{
					Message: message,
					Type:    openAIType,
					Code:    openAICode,
				},
			},
		)
	}
	headers := make(http.Header)
	headers.Set("Content-Type", "application/json")
	if status == http.StatusTooManyRequests {
		delay := time.Until(retryAt)
		seconds := max(int64((delay+time.Second-1)/time.Second), 1)
		headers.Set("Retry-After", strconv.FormatInt(seconds, 10))
	}
	return &ErrorMessage{
		StatusCode:     status,
		Error:          errors.New(message),
		DirectResponse: true,
		Body:           body,
		Headers:        headers,
	}
}

func (l *apiKeyReservationLifecycle) release(ctx context.Context, reason string) {
	if l == nil || l.store == nil || l.id == "" {
		return
	}
	transitionCtx := context.Background()
	if ctx != nil {
		transitionCtx = context.WithoutCancel(ctx)
	}
	transitionCtx, cancel := context.WithTimeout(transitionCtx, apiKeyReservationTransitionTimeout)
	defer cancel()
	if err := l.store.ReleaseAPIKeyReservation(transitionCtx, l.id, reason); err != nil {
		log.WithError(err).Error("API key reservation release failed")
	}
}

func (l *apiKeyReservationLifecycle) wrapAcknowledgement(
	ctx context.Context,
	acknowledge func(error),
) func(error) {
	if l == nil && acknowledge == nil {
		return nil
	}
	return func(deliveryErr error) {
		if deliveryErr != nil {
			l.markUnknown(ctx)
		}
		if acknowledge != nil {
			acknowledge(deliveryErr)
		}
	}
}

func (l *apiKeyReservationLifecycle) settleError(ctx context.Context, executionErr error) {
	if l == nil || l.store == nil || l.id == "" || executionErr == nil {
		return
	}
	disposition := coreexecutor.ExecutionDisposition{
		DispatchPhase:  coreexecutor.DispatchPhaseMaybeSent,
		Outcome:        coreexecutor.ExecutionOutcomeUnknown,
		RetryDirective: coreexecutor.RetryDirectiveNoRetry,
		EffectScope:    coreexecutor.StateEffectScopeRequest,
	}
	if dispositionError, ok := errors.AsType[coreexecutor.ExecutionDispositionError](executionErr); ok {
		disposition = dispositionError.ExecutionDisposition().Normalize("")
	}
	transitionCtx := context.Background()
	if ctx != nil {
		transitionCtx = context.WithoutCancel(ctx)
	}
	transitionCtx, cancel := context.WithTimeout(transitionCtx, apiKeyReservationTransitionTimeout)
	defer cancel()
	var err error
	switch {
	case disposition.DispatchPhase == coreexecutor.DispatchPhaseNotSent ||
		disposition.Outcome == coreexecutor.ExecutionOutcomeRejectedBeforeProcessing:
		err = l.store.ReleaseAPIKeyReservation(transitionCtx, l.id, "rejected before processing")
	case disposition.Outcome == coreexecutor.ExecutionOutcomeFailed:
		err = l.store.CommitAPIKeyReservation(transitionCtx, l.id, "processed request failed")
	case disposition.Outcome == coreexecutor.ExecutionOutcomeSucceeded:
		return
	default:
		err = l.store.MarkAPIKeyReservationUnknown(transitionCtx, l.id, "execution outcome unknown")
	}
	if err != nil {
		log.WithError(err).Error("API key reservation transition failed")
	}
}

func (l *apiKeyReservationLifecycle) markUnknown(ctx context.Context) {
	if l == nil || l.store == nil || l.id == "" {
		return
	}
	transitionCtx := context.Background()
	if ctx != nil {
		transitionCtx = context.WithoutCancel(ctx)
	}
	transitionCtx, cancel := context.WithTimeout(transitionCtx, apiKeyReservationTransitionTimeout)
	defer cancel()
	if err := l.store.MarkAPIKeyReservationUnknown(
		transitionCtx,
		l.id,
		"request canceled after dispatch",
	); err != nil {
		log.WithError(err).Error("API key reservation cancellation transition failed")
	}
}
