package access

import (
	"context"
	"net/http"
	"reflect"
	"sync"
)

// Manager coordinates authentication providers.
type Manager struct {
	mu         sync.RWMutex
	providers  []Provider
	generation uint64
}

// NewManager constructs an empty manager.
func NewManager() *Manager {
	return &Manager{}
}

// SetProviders replaces the active provider list.
func (m *Manager) SetProviders(providers []Provider) {
	if m == nil {
		return
	}
	cloned := make([]Provider, len(providers))
	copy(cloned, providers)
	m.mu.Lock()
	if !sameProviderInstances(m.providers, cloned) {
		m.generation++
	}
	m.providers = cloned
	m.mu.Unlock()
}

func sameProviderInstances(current, next []Provider) bool {
	if len(current) != len(next) {
		return false
	}
	for i, provider := range current {
		other := next[i]
		if reflect.TypeOf(provider) != reflect.TypeOf(other) {
			return false
		}
		value := reflect.ValueOf(provider)
		if value.IsValid() && value.Kind() == reflect.Pointer {
			if value.Pointer() != reflect.ValueOf(other).Pointer() {
				return false
			}
		} else if !reflect.DeepEqual(provider, other) {
			return false
		}
	}
	return true
}

// Generation changes whenever the active provider instances change.
func (m *Manager) Generation() uint64 {
	if m == nil {
		return 0
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.generation
}

// Providers returns a snapshot of the active providers.
func (m *Manager) Providers() []Provider {
	providers, _ := m.providerSnapshot()
	return providers
}

func (m *Manager) providerSnapshot() ([]Provider, uint64) {
	if m == nil {
		return nil, 0
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	snapshot := make([]Provider, len(m.providers))
	copy(snapshot, m.providers)
	return snapshot, m.generation
}

// Authenticate evaluates providers until one succeeds.
func (m *Manager) Authenticate(ctx context.Context, r *http.Request) (*Result, *AuthError) {
	result, _, err := m.AuthenticateWithGeneration(ctx, r)
	return result, err
}

// AuthenticateWithGeneration returns the provider generation that authenticated the request.
func (m *Manager) AuthenticateWithGeneration(ctx context.Context, r *http.Request) (*Result, uint64, *AuthError) {
	providers, generation := m.providerSnapshot()
	if len(providers) == 0 {
		return nil, generation, nil
	}

	var (
		missing bool
		invalid bool
	)

	for _, provider := range providers {
		if provider == nil {
			continue
		}
		res, authErr := provider.Authenticate(ctx, r)
		if authErr == nil {
			return res, generation, nil
		}
		if IsAuthErrorCode(authErr, AuthErrorCodeNotHandled) {
			continue
		}
		if IsAuthErrorCode(authErr, AuthErrorCodeNoCredentials) {
			missing = true
			continue
		}
		if IsAuthErrorCode(authErr, AuthErrorCodeInvalidCredential) {
			invalid = true
			continue
		}
		return nil, generation, authErr
	}

	if invalid {
		return nil, generation, NewInvalidCredentialError()
	}
	if missing {
		return nil, generation, NewNoCredentialsError()
	}
	return nil, generation, NewNoCredentialsError()
}
