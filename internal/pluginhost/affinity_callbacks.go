package pluginhost

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
)

type affinityLookupManager interface {
	LookupSessionAffinity(provider, model, sessionID string) (*coreauth.Auth, string)
}

// SetAffinityManager installs the routing manager used for read-only affinity observation.
func (h *Host) SetAffinityManager(manager affinityLookupManager) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.affinityManager = manager
	h.mu.Unlock()
}

func (h *Host) currentAffinityManager() affinityLookupManager {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	manager := h.affinityManager
	h.mu.Unlock()
	return manager
}

func (h *Host) callHostAffinityLookup(_ context.Context, request []byte) ([]byte, error) {
	var req pluginapi.HostAffinityLookupRequest
	if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode host affinity lookup request: %w", errUnmarshal)
	}
	req.Provider = strings.TrimSpace(req.Provider)
	req.Model = strings.TrimSpace(req.Model)
	req.SessionID = strings.TrimSpace(req.SessionID)
	if req.Provider == "" {
		return nil, fmt.Errorf("provider is required")
	}
	if req.Model == "" {
		return nil, fmt.Errorf("model is required")
	}
	if req.SessionID == "" {
		return nil, fmt.Errorf("session_id is required")
	}

	observedAt := time.Now().UTC()
	manager := h.currentAffinityManager()
	if manager == nil {
		return marshalRPCResult(pluginapi.HostAffinityLookupResponse{
			Status:     pluginapi.HostAffinityStatusUnsupported,
			ObservedAt: observedAt,
		})
	}

	auth, status := manager.LookupSessionAffinity(req.Provider, req.Model, req.SessionID)
	switch status {
	case pluginapi.HostAffinityStatusBound:
		if auth == nil {
			return nil, fmt.Errorf("affinity lookup returned bound without auth")
		}
		authIndex := strings.TrimSpace(auth.Index)
		if authIndex == "" {
			return nil, fmt.Errorf("affinity lookup returned bound without auth index")
		}
		return marshalRPCResult(pluginapi.HostAffinityLookupResponse{
			Status:      status,
			AuthIndex:   authIndex,
			ObservedAt:  observedAt,
			Disabled:    auth.Disabled || auth.Status == coreauth.StatusDisabled,
			Unavailable: auth.Unavailable || auth.Status == coreauth.StatusError,
		})
	case pluginapi.HostAffinityStatusUnbound,
		pluginapi.HostAffinityStatusAmbiguous,
		pluginapi.HostAffinityStatusUnsupported:
		return marshalRPCResult(pluginapi.HostAffinityLookupResponse{
			Status:     status,
			ObservedAt: observedAt,
		})
	default:
		return nil, fmt.Errorf("unsupported affinity lookup status %q", status)
	}
}
