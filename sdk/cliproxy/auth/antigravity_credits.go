package auth

import (
	"context"
	"strings"
	"sync"
	"time"
)

type antigravityUseCreditsContextKey struct{}

// WithAntigravityCredits returns a child context that signals the executor to
// inject enabledCreditTypes into the request payload.
func WithAntigravityCredits(ctx context.Context) context.Context {
	return context.WithValue(ctx, antigravityUseCreditsContextKey{}, true)
}

// AntigravityCreditsRequested reports whether the context carries the credits flag.
func AntigravityCreditsRequested(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v, _ := ctx.Value(antigravityUseCreditsContextKey{}).(bool)
	return v
}

// AntigravityCreditsHint stores the latest known AI credits state for one auth.
type AntigravityCreditsHint struct {
	Known           bool
	Available       bool
	CreditAmount    float64
	MinCreditAmount float64
	PaidTierID      string
	UpdatedAt       time.Time
}

type antigravityCreditsHintEntry struct {
	hint AntigravityCreditsHint
	auth *Auth
}

var antigravityCreditsHintByAuth sync.Map
var antigravityCreditsHintMu sync.Mutex

// SetAntigravityCreditsHint updates the latest known AI credits state for an auth.
func SetAntigravityCreditsHint(authID string, hint AntigravityCreditsHint) {
	storeAntigravityCreditsHint(authID, hint, nil)
}

func storeAntigravityCreditsHint(authID string, hint AntigravityCreditsHint, expected *Auth) bool {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return false
	}
	if hint.UpdatedAt.IsZero() {
		hint.UpdatedAt = time.Now()
	}
	antigravityCreditsHintMu.Lock()
	defer antigravityCreditsHintMu.Unlock()
	if expected != nil {
		if previous, ok := GetAntigravityCreditsHint(authID); ok && previous.UpdatedAt.After(hint.UpdatedAt) {
			return false
		}
	}
	antigravityCreditsHintByAuth.Store(authID, antigravityCreditsHintEntry{hint: hint, auth: expected.Clone()})
	return true
}

// PublishAntigravityCreditsHint 仅发布仍属于当前凭证的积分观察值
func (a *Auth) PublishAntigravityCreditsHint(hint AntigravityCreditsHint) bool {
	if a == nil {
		return false
	}
	if a.creditsHintManager != nil {
		return a.creditsHintManager.publishAntigravityCreditsHint(a, hint)
	}
	if a.Generation != 0 {
		return false
	}
	return storeAntigravityCreditsHint(a.ID, hint, a)
}

func (m *Manager) publishAntigravityCreditsHint(expected *Auth, hint AntigravityCreditsHint) bool {
	m.authIntentMu.Lock()
	defer m.authIntentMu.Unlock()
	m.mu.RLock()
	defer m.mu.RUnlock()
	current := m.auths[expected.ID]
	if current == nil || current.Disabled || current.Status == StatusDisabled ||
		expected.creditsHintGeneration != current.creditsHintGeneration ||
		expected.creditsHintRemovalEpoch != m.authRemovalEpoch(expected.ID) {
		return false
	}
	if current.Generation != expected.Generation && !runtimeAuthCredentialsEqual(current, expected) {
		return false
	}
	return storeAntigravityCreditsHint(expected.ID, hint, expected)
}

func invalidateAntigravityCreditsHint(auth, previous *Auth) {
	antigravityCreditsHintMu.Lock()
	defer antigravityCreditsHintMu.Unlock()
	value, ok := antigravityCreditsHintByAuth.Load(auth.ID)
	if !ok {
		return
	}
	entry, ok := value.(antigravityCreditsHintEntry)
	if ok && entry.auth == nil && (previous == nil || runtimeAuthCredentialsEqual(previous, auth)) {
		return
	}
	if ok && entry.auth != nil && entry.auth.creditsHintManager == auth.creditsHintManager &&
		entry.auth.creditsHintGeneration == auth.creditsHintGeneration &&
		entry.auth.creditsHintRemovalEpoch == auth.creditsHintRemovalEpoch && runtimeAuthCredentialsEqual(entry.auth, auth) {
		return
	}
	antigravityCreditsHintByAuth.Delete(auth.ID)
}

// GetAntigravityCreditsHint returns the latest known AI credits state for an auth.
func GetAntigravityCreditsHint(authID string) (AntigravityCreditsHint, bool) {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return AntigravityCreditsHint{}, false
	}
	value, ok := antigravityCreditsHintByAuth.Load(authID)
	if !ok {
		return AntigravityCreditsHint{}, false
	}
	entry, ok := value.(antigravityCreditsHintEntry)
	if !ok {
		antigravityCreditsHintByAuth.Delete(authID)
		return AntigravityCreditsHint{}, false
	}
	return entry.hint, true
}

// HasKnownAntigravityCreditsHint reports whether credits state has been discovered for an auth.
func HasKnownAntigravityCreditsHint(authID string) bool {
	hint, ok := GetAntigravityCreditsHint(authID)
	return ok && hint.Known
}

func antigravityCreditsAvailableForModel(auth *Auth, model string) bool {
	if auth == nil {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(auth.Provider), "antigravity") {
		return false
	}
	if !strings.Contains(strings.ToLower(strings.TrimSpace(model)), "claude") {
		return false
	}
	if !HasKnownAntigravityCreditsHint(auth.ID) {
		return false
	}
	hint, _ := GetAntigravityCreditsHint(auth.ID)
	return hint.Available
}
