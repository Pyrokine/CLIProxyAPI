package pluginhost

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
	log "github.com/sirupsen/logrus"
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

func (h *Host) callHostRoutingResetCooldown(ctx context.Context, request []byte) ([]byte, error) {
	var req pluginapi.HostRoutingResetCooldownRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return nil, fmt.Errorf("decode host routing reset cooldown request: %w", err)
	}
	authIndex := strings.TrimSpace(req.AuthIndex)
	if authIndex == "" {
		return nil, fmt.Errorf("auth_index is required")
	}
	manager, ok := h.currentAffinityManager().(*coreauth.Manager)
	if !ok || manager == nil {
		return nil, fmt.Errorf("core auth manager unavailable")
	}
	for _, auth := range manager.List() {
		if auth == nil || strings.TrimSpace(auth.Index) != authIndex {
			continue
		}
		updated, models, err := manager.ResetCooldown(coreauth.WithSkipPersist(ctx), auth.ID)
		if err != nil {
			return nil, fmt.Errorf("reset cooldown: %w", err)
		}
		if updated == nil {
			break
		}
		log.WithFields(log.Fields{
			"plugin_id":  hostCallbackPluginIDFromContext(ctx),
			"auth_index": updated.Index,
			"models":     len(models),
		}).Info("pluginhost: plugin reset credential cooldown")
		return marshalRPCResult(pluginapi.HostRoutingResetCooldownResponse{
			AuthIndex: updated.Index,
			Models:    models,
		})
	}
	return nil, fmt.Errorf("auth not found for auth_index %s", authIndex)
}
