package auth

import (
	"context"
	"net/http"
	"strings"
	"sync"

	internalconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executionregistry"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

// HomeDispatchBundle binds one Home client lifetime to its execution registry.
type HomeDispatchBundle struct {
	client     homeAuthDispatcher
	registry   *executionregistry.Registry
	generation uint64
}

// PublishHomeDispatch publishes one immutable Home dispatch lifetime.
func (m *Manager) PublishHomeDispatch(
	client homeAuthDispatcher,
	registry *executionregistry.Registry,
	generation uint64,
) *HomeDispatchBundle {
	if m == nil || client == nil || registry == nil {
		return nil
	}
	bundle := &HomeDispatchBundle{client: client, registry: registry, generation: generation}
	m.homeDispatchBundle.Store(bundle)
	return bundle
}

// ClearHomeDispatchBundle removes bundle only while it remains active.
func (m *Manager) ClearHomeDispatchBundle(bundle *HomeDispatchBundle) bool {
	return m != nil && bundle != nil && m.homeDispatchBundle.CompareAndSwap(bundle, nil)
}

// HomeDispatchBundle returns the active Home dispatch lifetime.
func (m *Manager) HomeDispatchBundle() *HomeDispatchBundle {
	if m == nil {
		return nil
	}
	return m.homeDispatchBundle.Load()
}

func (m *Manager) endHomeSelectionBeforeRedispatch(
	ctx context.Context,
	selection *HomeDispatchSelection,
	reason string,
) error {
	if selection == nil {
		return nil
	}
	ticket := selection.EndWithRelease(reason)
	if ticket == nil {
		return nil
	}

	bound := internalconfig.CredentialConcurrencyConfig{}.WithDefaults().CPACancelBound
	if m != nil {
		if cfg, ok := m.runtimeConfig.Load().(*internalconfig.Config); ok && cfg != nil {
			bound = cfg.CredentialConcurrency.WithDefaults().CPACancelBound
		}
	}
	waitCtx := ctx
	if waitCtx == nil {
		waitCtx = context.Background()
	}
	waitCtx, cancelWait := context.WithTimeout(waitCtx, bound)
	defer cancelWait()
	if errWait := ticket.Wait(waitCtx); errWait != nil {
		return &Error{
			Code:       "home_unavailable",
			Message:    "Home did not acknowledge credential release: " + errWait.Error(),
			Retryable:  true,
			HTTPStatus: http.StatusServiceUnavailable,
		}
	}
	return nil
}

func (m *Manager) takeHomeSessionSelectionsLocked(sessionID string) []*HomeDispatchSelection {
	if m == nil {
		return nil
	}
	selections := m.homeSessionSelections[sessionID]
	delete(m.homeSessionSelections, sessionID)
	result := make([]*HomeDispatchSelection, 0, len(selections))
	for _, selection := range selections {
		result = append(result, selection)
	}
	return result
}

func (m *Manager) takeAllHomeSessionSelectionsLocked() []*HomeDispatchSelection {
	if m == nil {
		return nil
	}
	result := make([]*HomeDispatchSelection, 0)
	for sessionID, selections := range m.homeSessionSelections {
		delete(m.homeSessionSelections, sessionID)
		for _, selection := range selections {
			result = append(result, selection)
		}
	}
	return result
}

type homeSessionSelectionKey struct {
	credentialID string
	routeModel   string
}

func (m *Manager) lockHomeSession(sessionID string) func() {
	if m == nil || sessionID == "" {
		return func() {}
	}
	value, _ := m.homeSessionLocks.LoadOrStore(sessionID, &sync.Mutex{})
	mutex, ok := value.(*sync.Mutex)
	if !ok || mutex == nil {
		return func() {}
	}
	mutex.Lock()
	return mutex.Unlock
}

func (m *Manager) lockHomeWebsocketSession(ctx context.Context, opts cliproxyexecutor.Options) func() {
	if m == nil || !cliproxyexecutor.DownstreamWebsocket(ctx) {
		return nil
	}
	sessionID := homeExecutionSessionIDFromMetadata(opts.Metadata)
	if sessionID == "" {
		return nil
	}
	return m.lockHomeSession(sessionID)
}

func (m *Manager) retainedHomeSessionSelection(
	ctx context.Context,
	opts cliproxyexecutor.Options,
	model string,
) (*HomeDispatchSelection, bool, error) {
	if m == nil || !cliproxyexecutor.DownstreamWebsocket(ctx) {
		return nil, false, nil
	}
	sessionID := homeExecutionSessionIDFromMetadata(opts.Metadata)
	credentialID := pinnedAuthIDFromMetadata(opts.Metadata)
	if sessionID == "" {
		return nil, false, nil
	}
	routeModel, validRouteModel := validCanonicalHomeConcurrencyModelKey(model)
	fallbackAttempt := homeAuthCountFromMetadata(opts.Metadata) > 1
	constraints := homeDispatchConstraints(ctx, opts)
	excluded := make(map[string]struct{}, len(constraints.ExcludedAuthIDs))
	for _, id := range constraints.ExcludedAuthIDs {
		excluded[id] = struct{}{}
	}
	var retained *HomeDispatchSelection
	var ended []*HomeDispatchSelection
	m.mu.Lock()
	selections := m.homeSessionSelections[sessionID]
	for key, selection := range selections {
		if selection == nil {
			delete(selections, key)
			continue
		}
		matchesCredential := credentialID == "" || key.credentialID == credentialID
		matchesRoute := validRouteModel && key.routeModel == routeModel
		_, excludedCredential := excluded[key.credentialID]
		if !excludedCredential && !fallbackAttempt && matchesCredential && matchesRoute && selection.Active() && retained == nil {
			retained = selection
			continue
		}
		delete(selections, key)
		ended = append(ended, selection)
	}
	if len(selections) == 0 {
		delete(m.homeSessionSelections, sessionID)
	}
	m.mu.Unlock()
	for _, selection := range ended {
		if errEnd := m.endHomeSelectionBeforeRedispatch(ctx, selection, "target_changed"); errEnd != nil {
			return nil, false, errEnd
		}
	}
	return retained, retained != nil, nil
}

func (m *Manager) retainHomeWebsocketSelection(
	ctx context.Context,
	opts cliproxyexecutor.Options,
	model string,
	selection *HomeDispatchSelection,
) bool {
	if m == nil || selection == nil || !selection.Retained() || !cliproxyexecutor.DownstreamWebsocket(ctx) {
		return false
	}
	auth := selection.CloneAuth()
	if auth == nil {
		return false
	}
	sessionID := homeExecutionSessionIDFromMetadata(opts.Metadata)
	credentialID := strings.TrimSpace(auth.ID)
	routeModel, validRouteModel := validCanonicalHomeConcurrencyModelKey(model)
	if sessionID == "" || credentialID == "" || !validRouteModel {
		return false
	}
	key := homeSessionSelectionKey{credentialID: credentialID, routeModel: routeModel}
	m.mu.Lock()
	if m.homeSessionSelections == nil {
		m.homeSessionSelections = make(map[string]map[homeSessionSelectionKey]*HomeDispatchSelection)
	}
	selections := m.homeSessionSelections[sessionID]
	if selections == nil {
		selections = make(map[homeSessionSelectionKey]*HomeDispatchSelection)
		m.homeSessionSelections[sessionID] = selections
	}
	previous := selections[key]
	selections[key] = selection
	m.mu.Unlock()
	m.rememberHomeSelectionRuntimeAuth(sessionID, selection)
	if previous != nil && previous != selection {
		previous.End("target_replaced")
	}
	return true
}

func (m *Manager) bindHomeSelectionRuntimeAuth(
	ctx context.Context,
	opts cliproxyexecutor.Options,
	selection *HomeDispatchSelection,
) error {
	if m == nil || selection == nil || !cliproxyexecutor.DownstreamWebsocket(ctx) {
		return nil
	}
	auth := selection.CloneAuth()
	if auth == nil || !authWebsocketsEnabled(auth) {
		return nil
	}
	sessionID := homeExecutionSessionIDFromMetadata(opts.Metadata)
	authID := strings.TrimSpace(auth.ID)
	if sessionID == "" || authID == "" || !selection.runtimeAuthBound.CompareAndSwap(false, true) {
		return nil
	}
	m.rememberHomeSelectionRuntimeAuth(sessionID, selection)
	if errBind := selection.Bind(
		func() error {
			m.forgetHomeRuntimeAuth(sessionID, authID, selection)
			return nil
		},
	); errBind != nil {
		selection.runtimeAuthBound.Store(false)
		m.forgetHomeRuntimeAuth(sessionID, authID, selection)
		return errBind
	}
	return nil
}

func (m *Manager) rememberHomeSelectionRuntimeAuth(sessionID string, selection *HomeDispatchSelection) {
	if m == nil || selection == nil {
		return
	}
	auth := selection.CloneAuth()
	if auth == nil {
		return
	}
	sessionID = strings.TrimSpace(sessionID)
	authID := strings.TrimSpace(auth.ID)
	if sessionID == "" || authID == "" {
		return
	}
	m.mu.Lock()
	if m.homeRuntimeAuths == nil {
		m.homeRuntimeAuths = make(map[string]map[string]*Auth)
	}
	if m.homeRuntimeAuthOwners == nil {
		m.homeRuntimeAuthOwners = make(map[string]map[string]*HomeDispatchSelection)
	}
	if m.homeRuntimeAuths[sessionID] == nil {
		m.homeRuntimeAuths[sessionID] = make(map[string]*Auth)
	}
	if m.homeRuntimeAuthOwners[sessionID] == nil {
		m.homeRuntimeAuthOwners[sessionID] = make(map[string]*HomeDispatchSelection)
	}
	m.homeRuntimeAuths[sessionID][authID] = auth
	m.homeRuntimeAuthOwners[sessionID][authID] = selection
	m.mu.Unlock()
}

func (m *Manager) forgetHomeRuntimeAuth(sessionID, authID string, owner *HomeDispatchSelection) {
	sessionID = strings.TrimSpace(sessionID)
	authID = strings.TrimSpace(authID)
	if m == nil || sessionID == "" || authID == "" {
		return
	}
	m.mu.Lock()
	owners := m.homeRuntimeAuthOwners[sessionID]
	if owner != nil && owners[authID] != owner {
		m.mu.Unlock()
		return
	}
	sessionAuths := m.homeRuntimeAuths[sessionID]
	delete(sessionAuths, authID)
	delete(owners, authID)
	if len(sessionAuths) == 0 {
		delete(m.homeRuntimeAuths, sessionID)
	}
	if len(owners) == 0 {
		delete(m.homeRuntimeAuthOwners, sessionID)
	}
	m.mu.Unlock()
}
