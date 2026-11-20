package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/home"
	log "github.com/sirupsen/logrus"
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
	return a.publishAntigravityCreditsHint(
		func() bool {
			return storeAntigravityCreditsHint(a.ID, hint, a)
		},
	)
}

type homeSelectionContextKey struct{}

// PublishAntigravityCreditsHintWithContext 将当前凭证的观察值写入请求所属的缓存
func (a *Auth) PublishAntigravityCreditsHintWithContext(ctx context.Context, hint AntigravityCreditsHint) bool {
	if a == nil || strings.TrimSpace(a.ID) == "" {
		return false
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Err() != nil {
		return false
	}
	client, homeMode, err := home.KVClientFromContext(ctx)
	if err != nil {
		log.Errorf("antigravity credits: Home publication failed prefix=cpa:antigravity:*: %v", err)
		return false
	}
	if !homeMode {
		return a.PublishAntigravityCreditsHint(hint)
	}
	cas, ok := client.(interface {
		KVCompareAndSwap(context.Context, string, []byte, bool, []byte, time.Duration) (bool, error)
	})
	if !ok {
		log.Errorf(
			"antigravity credits: Home publication failed prefix=cpa:antigravity:*: %v",
			home.ErrCompareAndSwapUnsupported,
		)
		return false
	}
	if selection, ok := ctx.Value(homeSelectionContextKey{}).(*HomeDispatchSelection); ok {
		selection.authMu.RLock()
		defer selection.authMu.RUnlock()
		if !selection.Active() || selection.Auth == nil || !runtimeAuthCredentialsEqual(selection.Auth, a) {
			return false
		}
	}
	if hint.UpdatedAt.IsZero() {
		hint.UpdatedAt = time.Now()
	}
	raw, err := json.Marshal(hint)
	if err != nil {
		log.Errorf("antigravity credits: Home publication failed prefix=cpa:antigravity:*: %v", err)
		return false
	}
	return a.publishAntigravityCreditsHint(
		func() bool {
			keys := []string{"cpa:antigravity:credits-hint:" + strings.TrimSpace(a.ID)}
			// 没有余额的 paidTier 仅发布不可用提示，避免零值被旧读取方判为可用余额
			if hint.Available || hint.CreditAmount != 0 || hint.MinCreditAmount != 0 {
				keys = append(keys, "cpa:antigravity:credits-balance:"+strings.TrimSpace(a.ID))
			}
			for _, key := range keys {
				previous, found, errGet := client.KVGet(ctx, key)
				if errGet != nil {
					log.Errorf("antigravity credits: Home publication read failed prefix=cpa:antigravity:*: %v", errGet)
					return false
				}
				if found {
					var prior AntigravityCreditsHint
					if errDecode := json.Unmarshal(previous, &prior); errDecode != nil {
						log.Errorf(
							"antigravity credits: Home publication decode failed prefix=cpa:antigravity:*: %v",
							errDecode,
						)
						return false
					}
					if prior.UpdatedAt.After(hint.UpdatedAt) {
						return false
					}
				}
				written, errSet := cas.KVCompareAndSwap(ctx, key, previous, found, raw, 30*time.Minute)
				if errSet != nil || !written {
					log.Warnf(
						"antigravity credits: Home publication failed prefix=cpa:antigravity:* written=%t: %v", written,
						errSet,
					)
					return false
				}
			}
			return true
		},
	)
}

func (a *Auth) publishAntigravityCreditsHint(publish func() bool) bool {
	if a == nil {
		return false
	}
	if a.creditsHintManager != nil {
		return a.creditsHintManager.publishAntigravityCreditsHint(a, publish)
	}
	if a.Generation != 0 {
		return false
	}
	return publish()
}

func (m *Manager) publishAntigravityCreditsHint(expected *Auth, publish func() bool) bool {
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
	return publish()
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
		entry.auth.creditsHintRemovalEpoch == auth.creditsHintRemovalEpoch && runtimeAuthCredentialsEqual(
		entry.auth, auth,
	) {
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
//
//goland:noinspection GoUnnecessarilyExportedIdentifiers
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

// GetAntigravityCreditsHintRequired reads Home-owned observations when Home is active.
func GetAntigravityCreditsHintRequired(ctx context.Context, authID string) (AntigravityCreditsHint, bool, error) {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return AntigravityCreditsHint{}, false, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	client, homeMode, err := home.KVClientFromContext(ctx)
	if err != nil {
		return AntigravityCreditsHint{}, false, err
	}
	if !homeMode {
		hint, found := GetAntigravityCreditsHint(authID)
		return hint, found, nil
	}
	raw, found, err := client.KVGet(ctx, "cpa:antigravity:credits-hint:"+authID)
	if err != nil || !found {
		return AntigravityCreditsHint{}, false, err
	}
	var hint AntigravityCreditsHint
	if err := json.Unmarshal(raw, &hint); err != nil {
		return AntigravityCreditsHint{}, false, fmt.Errorf("decode Home credits observation: %w", err)
	}
	return hint, true, nil
}
