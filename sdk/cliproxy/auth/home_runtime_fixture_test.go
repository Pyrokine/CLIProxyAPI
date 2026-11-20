package auth

import (
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executionregistry"
)

// rememberHomeRuntimeAuth supplies an owned scope for the original runtime-cache inputs.
func (m *Manager) rememberHomeRuntimeAuth(sessionID string, auth *Auth) {
	sessionID = strings.TrimSpace(sessionID)
	if m == nil || auth == nil || sessionID == "" || strings.TrimSpace(auth.ID) == "" {
		return
	}
	m.mu.RLock()
	_, removed := m.removedAuths[auth.ID]
	m.mu.RUnlock()
	if removed {
		return
	}
	registry := executionregistry.New()
	pending, err := registry.BeginDispatch()
	if err != nil {
		panic(err)
	}
	scope, err := registry.Install(
		pending, executionregistry.ScopeSpec{CredentialID: auth.ID, Model: "synthetic-runtime-model"},
	)
	if err != nil {
		panic(err)
	}
	provider, _ := m.Executor(homeRuntimeExecutorKey(auth))
	selection, err := newHomeDispatchSelection(auth, provider, auth.Provider, scope)
	if err != nil {
		panic(err)
	}
	auth.homeRuntimeOwner = selection
	selection.runtimeAuthBound.Store(true)
	if err := selection.Bind(
		func() error {
			m.forgetHomeRuntimeAuth(sessionID, auth.ID, selection)
			return nil
		},
	); err != nil {
		panic(err)
	}
	m.rememberHomeSelectionRuntimeAuth(sessionID, selection)
}
