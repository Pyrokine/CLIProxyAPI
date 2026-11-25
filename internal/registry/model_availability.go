package registry

import (
	"sort"
	"strings"
	"time"
)

const modelQuotaCooldownDuration = 5 * time.Minute

// ModelAvailability describes the configured and currently usable runtime clients for one model.
type ModelAvailability struct {
	ModelID              string                    `json:"model_id"`
	Info                 *ModelInfo                `json:"model_info,omitempty"`
	ConfiguredClients    int                       `json:"configured_clients"`
	AvailableClients     int                       `json:"available_clients"`
	QuotaCooldownClients int                       `json:"quota_cooldown_clients"`
	SuspendedClients     int                       `json:"suspended_clients"`
	ConfiguredProviders  []string                  `json:"configured_providers,omitempty"`
	AvailableProviders   []string                  `json:"available_providers,omitempty"`
	PreferredProvider    string                    `json:"preferred_provider,omitempty"`
	Clients              []ModelAvailabilityClient `json:"-"`
}

type ModelClientState uint8

const (
	ModelClientSuspended ModelClientState = iota
	ModelClientQuotaCooldown
	ModelClientAvailable
)

// ModelAvailabilityClient identifies a credential's registration before public IDs are grouped in the catalog.
type ModelAvailabilityClient struct {
	ID          string
	Provider    string
	CatalogName string
	State       ModelClientState
}

type modelAvailabilityState struct {
	availability       ModelAvailability
	registration       *modelRegistration
	configuredProvider map[string]int
	availableProvider  map[string]int
}

// GetModelAvailabilitySnapshot returns one entry per runtime-registered model.
func (r *ModelRegistry) GetModelAvailabilitySnapshot() []ModelAvailability {
	if r == nil {
		return nil
	}
	r.mutex.RLock()
	defer r.mutex.RUnlock()

	states := make(map[string]*modelAvailabilityState, len(r.models))
	for modelID, registration := range r.models {
		if registration == nil {
			continue
		}
		key := modelAvailabilityKey(modelID)
		if key == "" {
			continue
		}
		states[key] = &modelAvailabilityState{
			availability:       ModelAvailability{ModelID: modelID},
			registration:       registration,
			configuredProvider: make(map[string]int),
			availableProvider:  make(map[string]int),
		}
	}

	now := time.Now()
	for clientID, modelIDs := range r.clientModels {
		provider := strings.ToLower(strings.TrimSpace(r.clientProviders[clientID]))
		seen := make(map[string]struct{}, len(modelIDs))
		for _, modelID := range modelIDs {
			key := modelAvailabilityKey(modelID)
			state := states[key]
			registration := r.models[modelID]
			if state == nil || registration == nil {
				continue
			}
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}

			state.availability.ConfiguredClients++
			if provider != "" {
				state.configuredProvider[provider]++
			}

			client := ModelAvailabilityClient{ID: clientID, Provider: provider}
			if info := r.clientModelInfos[clientID][modelID]; info != nil {
				client.CatalogName = info.CatalogName
			}
			reason := strings.TrimSpace(registration.SuspendedClients[clientID])
			quotaTime := registration.QuotaExceededClients[clientID]
			switch {
			case reason != "" && !strings.EqualFold(reason, "quota"):
				client.State = ModelClientSuspended
				state.availability.SuspendedClients++
			case strings.EqualFold(
				reason, "quota",
			) || quotaTime != nil && now.Sub(*quotaTime) < modelQuotaCooldownDuration:
				client.State = ModelClientQuotaCooldown
				state.availability.QuotaCooldownClients++
			default:
				client.State = ModelClientAvailable
				state.availability.AvailableClients++
				if provider != "" {
					state.availableProvider[provider]++
				}
			}
			state.availability.Clients = append(state.availability.Clients, client)
		}
	}

	result := make([]ModelAvailability, 0, len(states))
	for _, state := range states {
		availability := state.availability
		availability.ConfiguredProviders = sortedProvidersByCount(state.configuredProvider)
		availability.AvailableProviders = sortedProvidersByCount(state.availableProvider)
		if len(availability.AvailableProviders) > 0 {
			availability.PreferredProvider = availability.AvailableProviders[0]
		} else if len(availability.ConfiguredProviders) > 0 {
			availability.PreferredProvider = availability.ConfiguredProviders[0]
		}
		if info := state.registration.InfoByProvider[availability.PreferredProvider]; info != nil {
			availability.Info = cloneModelInfo(info)
		} else {
			availability.Info = cloneModelInfo(state.registration.Info)
		}
		result = append(result, availability)
	}
	sort.Slice(
		result, func(i, j int) bool {
			return strings.ToLower(result[i].ModelID) < strings.ToLower(result[j].ModelID)
		},
	)
	return result
}

func modelAvailabilityKey(modelID string) string {
	modelID = strings.TrimSpace(modelID)
	if len(modelID) >= len("models/") && strings.EqualFold(modelID[:len("models/")], "models/") {
		modelID = modelID[len("models/"):]
	}
	return strings.ToLower(strings.TrimSpace(modelID))
}

func sortedProvidersByCount(counts map[string]int) []string {
	providers := make([]string, 0, len(counts))
	for provider, count := range counts {
		if provider != "" && count > 0 {
			providers = append(providers, provider)
		}
	}
	sort.Slice(
		providers, func(i, j int) bool {
			if counts[providers[i]] == counts[providers[j]] {
				return providers[i] < providers[j]
			}
			return counts[providers[i]] > counts[providers[j]]
		},
	)
	return providers
}
