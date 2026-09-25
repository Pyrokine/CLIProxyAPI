package auth

import (
	"context"
	"strings"

	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

// ResultPolicy inspects and mutates an execution result before runtime state mutation,
// cooldown persistence, scheduler/registry publication, and result hooks.
// Implementations must be safe for concurrent use by multiple goroutines.
// MarkResult retains the selected auth/model identity and request generation tokens;
// clearing AuthID drops the result.
type ResultPolicy interface {
	ApplyResultPolicy(ctx context.Context, result Result) Result
}

// ResultPolicyFunc enables using a function as a ResultPolicy.
type ResultPolicyFunc func(ctx context.Context, result Result) Result

// ApplyResultPolicy calls f(ctx, result).
func (f ResultPolicyFunc) ApplyResultPolicy(ctx context.Context, result Result) Result {
	return f(ctx, result)
}

type resultPolicyHolder struct {
	policy ResultPolicy
}

type resultGenerationSnapshot struct {
	valid                     bool
	quotaFamily               string
	credentialStateGeneration uint64
	exactModelStateGeneration uint64
	quotaFamilyGeneration     uint64
}

func captureResultGenerationSnapshot(auth *Auth, model string) resultGenerationSnapshot {
	if auth == nil || strings.TrimSpace(auth.ID) == "" {
		return resultGenerationSnapshot{}
	}
	model = canonicalModelKey(model)
	family, familyGeneration := observedQuotaFamilyGeneration(auth, model)
	return resultGenerationSnapshot{
		valid:                     true,
		quotaFamily:               family,
		credentialStateGeneration: normalizedStateGeneration(auth.StateGeneration),
		exactModelStateGeneration: stateGenerationForDisposition(
			auth,
			model,
			cliproxyexecutor.ExecutionDisposition{
				EffectScope: cliproxyexecutor.StateEffectScopeExactModel,
				EffectKey:   model,
			},
		),
		quotaFamilyGeneration: familyGeneration,
	}
}

func observedQuotaFamilyGeneration(auth *Auth, model string) (string, uint64) {
	state, stateModel := findModelState(auth, model)
	family := quotaFamilyKeyForState(auth, stateModel, state)
	if state == nil {
		return family, 1
	}
	return family, normalizedStateGeneration(state.Quota.Generation)
}

// SetResultPolicy sets the execution result policy used by MarkResult.
func (m *Manager) SetResultPolicy(policy ResultPolicy) {
	if m == nil {
		return
	}
	if policy == nil {
		m.resultPolicy.Store(nil)
		return
	}
	m.resultPolicy.Store(&resultPolicyHolder{policy: policy})
}

// ResultPolicy returns the current execution result policy.
func (m *Manager) ResultPolicy() ResultPolicy {
	if m == nil {
		return nil
	}
	holder := m.resultPolicy.Load()
	if holder == nil {
		return nil
	}
	return holder.policy
}

func applyResultPolicy(ctx context.Context, policy ResultPolicy, result Result) (Result, bool) {
	original := result
	originalDisposition := normalizeResultDisposition(original)
	result = policy.ApplyResultPolicy(ctx, result)
	if result.AuthID == "" {
		return Result{}, false
	}

	result.AuthID = original.AuthID
	result.AuthGeneration = original.AuthGeneration
	result.RuntimeOnly = original.RuntimeOnly
	result.Provider = original.Provider
	result.Model = original.Model
	result.ModelQuotaEpoch = original.ModelQuotaEpoch
	result.generationSnapshot = original.generationSnapshot
	result.StateGeneration = original.StateGeneration

	result.Disposition = normalizeResultDisposition(result).Normalize(originalDisposition.DispatchPhase)
	if !resultPolicyReplaySafe(originalDisposition) || !resultPolicyReplaySafe(result.Disposition) {
		result.Disposition.RetryDirective = cliproxyexecutor.RetryDirectiveNoRetry
	}

	if original.generationSnapshot.valid {
		if disposition, generation, ok := original.generationSnapshot.apply(result, result.Disposition); ok {
			result.Disposition = disposition
			result.StateGeneration = generation
		}
	} else if resultPolicyStateTargetChanged(original, originalDisposition, result.Disposition) {
		result.Disposition.EffectScope = originalDisposition.EffectScope
		result.Disposition.EffectKey = originalDisposition.EffectKey
	}
	return result, true
}

func resultPolicyReplaySafe(disposition cliproxyexecutor.ExecutionDisposition) bool {
	return disposition.DispatchPhase != cliproxyexecutor.DispatchPhaseDownstreamCommitted &&
		disposition.Outcome == cliproxyexecutor.ExecutionOutcomeRejectedBeforeProcessing
}

func (snapshot resultGenerationSnapshot) apply(
	result Result,
	disposition cliproxyexecutor.ExecutionDisposition,
) (cliproxyexecutor.ExecutionDisposition, uint64, bool) {
	switch disposition.EffectScope {
	case cliproxyexecutor.StateEffectScopeRequest:
		disposition.EffectKey = ""
		return disposition, 0, true
	case cliproxyexecutor.StateEffectScopeExactModel:
		disposition.EffectKey = canonicalModelKey(result.Model)
		return disposition, snapshot.exactModelStateGeneration, true
	case cliproxyexecutor.StateEffectScopeQuotaFamily:
		disposition.EffectKey = snapshot.quotaFamily
		return disposition, snapshot.quotaFamilyGeneration, true
	case cliproxyexecutor.StateEffectScopeCredential:
		disposition.EffectKey = result.AuthID
		return disposition, snapshot.credentialStateGeneration, true
	case cliproxyexecutor.StateEffectScopeGroup:
		disposition.EffectKey = ""
		return disposition, 0, true
	case cliproxyexecutor.StateEffectScopeProvider:
		disposition.EffectKey = strings.ToLower(strings.TrimSpace(result.Provider))
		return disposition, 0, true
	default:
		return disposition, 0, false
	}
}

func resultPolicyStateTargetChanged(
	result Result,
	original,
	proposed cliproxyexecutor.ExecutionDisposition,
) bool {
	if !resultPolicyScopeMutatesAvailability(proposed.EffectScope) {
		return false
	}
	if original.EffectScope != proposed.EffectScope {
		return true
	}
	return resultPolicyEffectTarget(result, original) != resultPolicyEffectTarget(result, proposed)
}

func resultPolicyScopeMutatesAvailability(scope cliproxyexecutor.StateEffectScope) bool {
	switch scope {
	case cliproxyexecutor.StateEffectScopeExactModel,
		cliproxyexecutor.StateEffectScopeQuotaFamily,
		cliproxyexecutor.StateEffectScopeCredential:
		return true
	default:
		return false
	}
}

func resultPolicyEffectTarget(
	result Result,
	disposition cliproxyexecutor.ExecutionDisposition,
) string {
	if target := strings.TrimSpace(disposition.EffectKey); target != "" {
		return target
	}
	switch disposition.EffectScope {
	case cliproxyexecutor.StateEffectScopeExactModel,
		cliproxyexecutor.StateEffectScopeQuotaFamily:
		return canonicalModelKey(result.Model)
	case cliproxyexecutor.StateEffectScopeCredential:
		return result.AuthID
	default:
		return ""
	}
}
