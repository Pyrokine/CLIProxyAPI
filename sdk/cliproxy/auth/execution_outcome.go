package auth

import (
	"errors"
	"net/http"
	"sort"
	"strings"

	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func executionDispositionForAttempt(
	err error,
	fallbackPhase cliproxyexecutor.DispatchPhase,
	auth *Auth,
	provider,
	model string,
) cliproxyexecutor.ExecutionDisposition {
	disposition := executionDispositionForError(err, fallbackPhase)
	switch disposition.EffectScope {
	case cliproxyexecutor.StateEffectScopeExactModel:
		if disposition.EffectKey == "" {
			disposition.EffectKey = canonicalModelKey(model)
		}
	case cliproxyexecutor.StateEffectScopeQuotaFamily:
		if disposition.EffectKey == "" {
			disposition.EffectKey = quotaFamilyKey(auth, model)
		}
	case cliproxyexecutor.StateEffectScopeCredential:
		if disposition.EffectKey == "" && auth != nil {
			disposition.EffectKey = auth.ID
		}
	case cliproxyexecutor.StateEffectScopeGroup:
		if disposition.EffectKey == "" && auth != nil && auth.Attributes != nil {
			disposition.EffectKey = strings.TrimSpace(auth.Attributes["group"])
		}
	case cliproxyexecutor.StateEffectScopeProvider:
		if disposition.EffectKey == "" {
			disposition.EffectKey = strings.ToLower(strings.TrimSpace(provider))
		}
	}
	return disposition
}

func executionDispositionForError(
	err error,
	fallbackPhase cliproxyexecutor.DispatchPhase,
) cliproxyexecutor.ExecutionDisposition {
	if err == nil {
		return cliproxyexecutor.ExecutionDisposition{
			DispatchPhase:  fallbackPhase,
			Outcome:        cliproxyexecutor.ExecutionOutcomeSucceeded,
			RetryDirective: cliproxyexecutor.RetryDirectiveNoRetry,
			EffectScope:    cliproxyexecutor.StateEffectScopeExactModel,
		}.Normalize(fallbackPhase)
	}

	var dispositionErr cliproxyexecutor.ExecutionDispositionError
	var disposition cliproxyexecutor.ExecutionDisposition
	normalizationPhase := fallbackPhase
	if errors.As(err, &dispositionErr) && dispositionErr != nil {
		disposition = dispositionErr.ExecutionDisposition()
		if disposition.DispatchPhase != "" {
			normalizationPhase = disposition.DispatchPhase
		}
	} else if status := statusCodeFromError(err); status != 0 {
		disposition = cliproxyexecutor.HTTPStatusDisposition(status, retryAfterFromError(err) != nil)
	} else if fallbackPhase == cliproxyexecutor.DispatchPhaseNotSent {
		disposition = cliproxyexecutor.ExecutionDisposition{
			DispatchPhase:  fallbackPhase,
			Outcome:        cliproxyexecutor.ExecutionOutcomeRejectedBeforeProcessing,
			RetryDirective: cliproxyexecutor.RetryDirectiveNextAuth,
			EffectScope:    cliproxyexecutor.StateEffectScopeCredential,
		}
	} else {
		disposition = cliproxyexecutor.ExecutionDisposition{
			DispatchPhase:  fallbackPhase,
			Outcome:        cliproxyexecutor.ExecutionOutcomeUnknown,
			RetryDirective: cliproxyexecutor.RetryDirectiveNoRetry,
			EffectScope:    cliproxyexecutor.StateEffectScopeRequest,
		}
	}
	disposition = disposition.Normalize(normalizationPhase)

	switch {
	case isRequestScopedError(err):
		disposition.RetryDirective = cliproxyexecutor.RetryDirectiveNoRetry
		disposition.EffectScope = cliproxyexecutor.StateEffectScopeRequest
		if disposition.DispatchPhase == cliproxyexecutor.DispatchPhaseNotSent ||
			statusCodeFromError(err) >= http.StatusBadRequest {
			disposition.Outcome = cliproxyexecutor.ExecutionOutcomeRejectedBeforeProcessing
		} else {
			disposition.Outcome = cliproxyexecutor.ExecutionOutcomeUnknown
		}
	case isConnectionLifecycleError(err):
		disposition.Outcome = cliproxyexecutor.ExecutionOutcomeUnknown
		disposition.RetryDirective = cliproxyexecutor.RetryDirectiveNoRetry
		disposition.EffectScope = cliproxyexecutor.StateEffectScopeRequest
	case isCloudflareChallengeError(err):
		disposition.Outcome = cliproxyexecutor.ExecutionOutcomeRejectedBeforeProcessing
		disposition.RetryDirective = cliproxyexecutor.RetryDirectiveNextProvider
		disposition.EffectScope = cliproxyexecutor.StateEffectScopeProvider
	case isInvalidGrantError(err):
		disposition.Outcome = cliproxyexecutor.ExecutionOutcomeRejectedBeforeProcessing
		disposition.RetryDirective = cliproxyexecutor.RetryDirectiveNextAuth
		disposition.EffectScope = cliproxyexecutor.StateEffectScopeCredential
	case isModelSupportError(err):
		disposition.Outcome = cliproxyexecutor.ExecutionOutcomeRejectedBeforeProcessing
		disposition.RetryDirective = cliproxyexecutor.RetryDirectiveNextAuth
		disposition.EffectScope = cliproxyexecutor.StateEffectScopeExactModel
	}
	return disposition
}

func successDispositionForAttempt(
	phase cliproxyexecutor.DispatchPhase,
	auth *Auth,
	model string,
) cliproxyexecutor.ExecutionDisposition {
	disposition := cliproxyexecutor.ExecutionDisposition{
		DispatchPhase:  phase,
		Outcome:        cliproxyexecutor.ExecutionOutcomeSucceeded,
		RetryDirective: cliproxyexecutor.RetryDirectiveNoRetry,
		EffectScope:    cliproxyexecutor.StateEffectScopeExactModel,
		EffectKey:      canonicalModelKey(model),
	}
	if auth != nil && auth.StateEffectScope == cliproxyexecutor.StateEffectScopeCredential {
		disposition.EffectScope = cliproxyexecutor.StateEffectScopeCredential
		disposition.EffectKey = auth.ID
		return disposition.Normalize(phase)
	}
	if family := activeQuotaFamilyForModel(auth, model); family != "" {
		disposition.EffectScope = cliproxyexecutor.StateEffectScopeQuotaFamily
		disposition.EffectKey = family
	}
	return disposition.Normalize(phase)
}

func executionResult(
	auth *Auth,
	provider,
	model string,
	err error,
	phase cliproxyexecutor.DispatchPhase,
) Result {
	disposition := executionDispositionForAttempt(err, phase, auth, provider, model)
	if err == nil {
		disposition = successDispositionForAttempt(phase, auth, model)
	}
	result := Result{
		Provider:    provider,
		Model:       model,
		Success:     err == nil,
		Disposition: disposition,
	}
	if auth == nil {
		return result
	}
	result.AuthID = auth.ID
	result.AuthGeneration = auth.Generation
	result.RuntimeOnly = auth.Generation == 0
	result.ModelQuotaEpoch = modelQuotaEpoch(auth, model)
	result.StateGeneration = stateGenerationForDisposition(auth, model, disposition)
	if err != nil {
		result.Error = errorForResultWithDisposition(err, disposition, result.StateGeneration)
	}
	return result
}

func errorForResultWithDisposition(
	err error,
	disposition cliproxyexecutor.ExecutionDisposition,
	stateGeneration uint64,
) *Error {
	resultErr := errorForResult(err)
	if resultErr == nil {
		return nil
	}
	resultErr.DispatchPhase = disposition.DispatchPhase
	resultErr.Outcome = disposition.Outcome
	resultErr.RetryDirective = disposition.RetryDirective
	resultErr.EffectScope = disposition.EffectScope
	resultErr.EffectKey = disposition.EffectKey
	resultErr.StateGeneration = stateGeneration
	return resultErr
}

func modelStateForDisposition(
	auth *Auth,
	model string,
	disposition cliproxyexecutor.ExecutionDisposition,
	create bool,
) (*ModelState, string) {
	if auth == nil {
		return nil, ""
	}
	if disposition.EffectScope == cliproxyexecutor.StateEffectScopeQuotaFamily {
		family := strings.TrimSpace(disposition.EffectKey)
		if family == "" {
			family = quotaFamilyKey(auth, model)
		}
		for stateModel, state := range auth.ModelStates {
			if state != nil && quotaFamilyKeyForState(auth, stateModel, state) == family {
				return state, stateModel
			}
		}
	}
	key := strings.TrimSpace(disposition.EffectKey)
	if key == "" {
		key = model
	}
	if state, stateModel := findModelState(auth, key); state != nil {
		return state, stateModel
	}
	if !create {
		return nil, canonicalModelKey(key)
	}
	state := ensureModelState(auth, key)
	_, stateModel := findModelState(auth, key)
	return state, stateModel
}

func stateGenerationForDisposition(
	auth *Auth,
	model string,
	disposition cliproxyexecutor.ExecutionDisposition,
) uint64 {
	if auth == nil {
		return 0
	}
	switch disposition.EffectScope {
	case cliproxyexecutor.StateEffectScopeCredential:
		return normalizedStateGeneration(auth.StateGeneration)
	case cliproxyexecutor.StateEffectScopeExactModel:
		state, _ := modelStateForDisposition(auth, model, disposition, false)
		if state == nil {
			return 1
		}
		return normalizedStateGeneration(state.Generation)
	case cliproxyexecutor.StateEffectScopeQuotaFamily:
		family := strings.TrimSpace(disposition.EffectKey)
		if family == "" {
			family = quotaFamilyKey(auth, model)
		}
		return quotaFamilyGeneration(auth, family)
	default:
		return 0
	}
}

func resultStateGenerationMatches(auth *Auth, result Result) bool {
	if auth == nil || result.StateGeneration == 0 {
		return true
	}
	return stateGenerationForDisposition(auth, result.Model, result.Disposition) == result.StateGeneration
}

func quotaFamilyModelStates(
	auth *Auth,
	model,
	family string,
	create bool,
) map[string]*ModelState {
	states := make(map[string]*ModelState)
	if auth == nil {
		return states
	}
	family = strings.TrimSpace(family)
	if family == "" {
		family = quotaFamilyKey(auth, model)
	}
	for stateModel, state := range auth.ModelStates {
		if state == nil || quotaFamilyKeyForState(auth, stateModel, state) != family {
			continue
		}
		states[stateModel] = state
	}
	if !create {
		return states
	}
	state, stateModel := findModelState(auth, model)
	if state == nil {
		state = ensureModelState(auth, model)
		_, stateModel = findModelState(auth, model)
	}
	if state != nil && stateModel != "" {
		states[stateModel] = state
	}
	return states
}

func resetAuthorityAllows(required, action StateResetAuthority) bool {
	if required == "" {
		return true
	}
	return resetAuthorityRank(action) >= resetAuthorityRank(required)
}

func resetAuthorityRank(authority StateResetAuthority) int {
	switch authority {
	case StateResetAuthoritySuccess:
		return 1
	case StateResetAuthorityReset:
		return 2
	case StateResetAuthorityOperator:
		return 3
	default:
		return 0
	}
}

func normalizedStateGeneration(generation uint64) uint64 {
	if generation == 0 {
		return 1
	}
	return generation
}

func nextStateGeneration(generation uint64) uint64 {
	return normalizedStateGeneration(generation) + 1
}

func quotaFamilyGeneration(auth *Auth, family string) uint64 {
	generation := uint64(1)
	if auth == nil || family == "" {
		return generation
	}
	for model, state := range auth.ModelStates {
		if state == nil || quotaFamilyKeyForState(auth, model, state) != family {
			continue
		}
		if current := normalizedStateGeneration(state.Quota.Generation); current > generation {
			generation = current
		}
	}
	return generation
}

// QuotaFamilyGeneration returns the normalized generation for one quota family.
func QuotaFamilyGeneration(auth *Auth, family string) uint64 {
	return quotaFamilyGeneration(auth, strings.TrimSpace(family))
}

// QuotaFamilyRecoveryCandidate snapshots one family eligible for authoritative refresh recovery.
type QuotaFamilyRecoveryCandidate struct {
	Family             string
	Models             []string
	ExpectedGeneration uint64
}

// QuotaFamilyRecoveryCandidates returns only explicit family quota states and legacy unscoped model 429 states.
func QuotaFamilyRecoveryCandidates(auth *Auth) []QuotaFamilyRecoveryCandidate {
	if auth == nil || len(auth.ModelStates) == 0 {
		return nil
	}
	familyStates := make(map[string]map[string]*ModelState)
	for model, state := range auth.ModelStates {
		model = strings.TrimSpace(model)
		if model == "" || state == nil {
			continue
		}
		family := strings.TrimSpace(quotaFamilyKeyForState(auth, model, state))
		if family == "" {
			continue
		}
		states := familyStates[family]
		if states == nil {
			states = make(map[string]*ModelState)
			familyStates[family] = states
		}
		states[model] = state
	}

	candidates := make([]QuotaFamilyRecoveryCandidate, 0, len(familyStates))
	for family, states := range familyStates {
		models := make([]string, 0, len(states))
		recoverable := len(states) > 0
		for model, state := range states {
			if !quotaFamilyStateAllowsRefreshRecovery(model, state, family) {
				recoverable = false
				break
			}
			models = append(models, model)
		}
		if !recoverable {
			continue
		}
		sort.Strings(models)
		candidates = append(
			candidates,
			QuotaFamilyRecoveryCandidate{
				Family:             family,
				Models:             models,
				ExpectedGeneration: quotaFamilyGeneration(auth, family),
			},
		)
	}
	sort.Slice(
		candidates, func(i, j int) bool {
			return candidates[i].Family < candidates[j].Family
		},
	)
	return candidates
}

func quotaFamilyStateAllowsRefreshRecovery(model string, state *ModelState, family string) bool {
	if state == nil || state.Status == StatusDisabled ||
		!hasQuotaCooldownState(state.Quota, state.LastError) ||
		!resetAuthorityAllows(state.ResetAuthority, StateResetAuthoritySuccess) ||
		!resetAuthorityAllows(state.Quota.ResetAuthority, StateResetAuthoritySuccess) {
		return false
	}
	if state.LastError != nil && state.LastError.HTTPStatus != 0 &&
		state.LastError.HTTPStatus != http.StatusTooManyRequests {
		return false
	}
	family = strings.TrimSpace(family)
	if family == "" {
		return false
	}
	switch state.EffectScope {
	case cliproxyexecutor.StateEffectScopeQuotaFamily:
		effectFamily := strings.TrimSpace(state.EffectKey)
		quotaFamily := strings.TrimSpace(state.Quota.Family)
		return (effectFamily == "" || effectFamily == family) &&
			(quotaFamily == "" || quotaFamily == family)
	case "":
		if state.LastError == nil || state.LastError.HTTPStatus != http.StatusTooManyRequests {
			return false
		}
		quotaFamily := strings.TrimSpace(state.Quota.Family)
		if quotaFamily != "" {
			return quotaFamily == family
		}
		return canonicalModelKey(model) == family
	default:
		return false
	}
}

func quotaFamilyKey(auth *Auth, model string) string {
	modelKey := canonicalModelKey(model)
	if state, _ := findModelState(auth, modelKey); state != nil {
		if family := strings.TrimSpace(state.Quota.Family); family != "" {
			return family
		}
		if state.EffectScope == cliproxyexecutor.StateEffectScopeQuotaFamily {
			if family := strings.TrimSpace(state.EffectKey); family != "" {
				return family
			}
		}
	}
	return modelKey
}

func quotaFamilyKeyForState(auth *Auth, model string, state *ModelState) string {
	if state != nil {
		if family := strings.TrimSpace(state.Quota.Family); family != "" {
			return family
		}
		if state.EffectScope == cliproxyexecutor.StateEffectScopeQuotaFamily {
			if family := strings.TrimSpace(state.EffectKey); family != "" {
				return family
			}
		}
	}
	return quotaFamilyKey(auth, model)
}

func activeQuotaFamilyForModel(auth *Auth, model string) string {
	if auth == nil {
		return ""
	}
	family := quotaFamilyKey(auth, model)
	for stateModel, state := range auth.ModelStates {
		if state == nil || !state.Quota.Exceeded {
			continue
		}
		if quotaFamilyKeyForState(auth, stateModel, state) == family {
			return family
		}
	}
	return ""
}

func retryDirectiveAllowsRefresh(disposition cliproxyexecutor.ExecutionDisposition) bool {
	return disposition.Outcome == cliproxyexecutor.ExecutionOutcomeRejectedBeforeProcessing &&
		disposition.RetryDirective == cliproxyexecutor.RetryDirectiveSameAuthAfterRefresh
}

func retryDirectiveAllowsRedispatch(disposition cliproxyexecutor.ExecutionDisposition) bool {
	if disposition.Outcome != cliproxyexecutor.ExecutionOutcomeRejectedBeforeProcessing {
		return false
	}
	switch disposition.RetryDirective {
	case cliproxyexecutor.RetryDirectiveNextAuth,
		cliproxyexecutor.RetryDirectiveNextProvider,
		cliproxyexecutor.RetryDirectiveRetryAfter:
		return true
	default:
		return false
	}
}

func retryDirectiveAllowsCooldownRetry(disposition cliproxyexecutor.ExecutionDisposition) bool {
	if disposition.Outcome != cliproxyexecutor.ExecutionOutcomeRejectedBeforeProcessing {
		return false
	}
	switch disposition.RetryDirective {
	case cliproxyexecutor.RetryDirectiveNextAuth,
		cliproxyexecutor.RetryDirectiveNextProvider,
		cliproxyexecutor.RetryDirectiveRetryAfter:
		return true
	default:
		return false
	}
}

func normalizeResultDisposition(result Result) cliproxyexecutor.ExecutionDisposition {
	disposition := result.Disposition
	if disposition.DispatchPhase == "" && disposition.Outcome == "" &&
		disposition.RetryDirective == "" && disposition.EffectScope == "" {
		if result.Success {
			disposition = cliproxyexecutor.ExecutionDisposition{
				DispatchPhase:  cliproxyexecutor.DispatchPhaseResponseStarted,
				Outcome:        cliproxyexecutor.ExecutionOutcomeSucceeded,
				RetryDirective: cliproxyexecutor.RetryDirectiveNoRetry,
				EffectScope:    cliproxyexecutor.StateEffectScopeExactModel,
				EffectKey:      canonicalModelKey(result.Model),
			}
		} else {
			disposition = executionDispositionForAttempt(
				result.Error,
				cliproxyexecutor.DispatchPhaseResponseStarted,
				nil,
				result.Provider,
				result.Model,
			)
		}
		if strings.TrimSpace(result.Model) == "" && result.Success {
			disposition.EffectScope = cliproxyexecutor.StateEffectScopeCredential
			disposition.EffectKey = result.AuthID
		}
	}
	fallbackPhase := disposition.DispatchPhase
	if fallbackPhase == "" {
		fallbackPhase = cliproxyexecutor.DispatchPhaseResponseStarted
	}
	return disposition.Normalize(fallbackPhase)
}

func dispositionIsAvailabilityNeutral(disposition cliproxyexecutor.ExecutionDisposition) bool {
	if disposition.Outcome == cliproxyexecutor.ExecutionOutcomeUnknown {
		return true
	}
	switch disposition.EffectScope {
	case cliproxyexecutor.StateEffectScopeRequest,
		cliproxyexecutor.StateEffectScopeGroup,
		cliproxyexecutor.StateEffectScopeProvider:
		return true
	default:
		return false
	}
}

func applyDispositionToRouteTrace(
	event *cliproxyexecutor.RouteTraceEvent,
	disposition cliproxyexecutor.ExecutionDisposition,
	stateGeneration uint64,
) {
	if event == nil {
		return
	}
	event.DispatchPhase = disposition.DispatchPhase
	event.Outcome = disposition.Outcome
	event.RetryDirective = disposition.RetryDirective
	event.EffectScope = disposition.EffectScope
	event.EffectKey = disposition.EffectKey
	event.StateGeneration = stateGeneration
}
