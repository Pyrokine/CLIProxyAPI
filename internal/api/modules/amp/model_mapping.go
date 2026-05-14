// Package amp provides model mapping functionality for routing Amp CLI requests
// to alternative models when the requested model is not available locally.
package amp

import (
	"maps"
	"regexp"
	"strings"
	"sync"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	log "github.com/sirupsen/logrus"
)

// modelMapper provides model name mapping/aliasing for Amp CLI requests.
// When an Amp request comes in for a model that isn't available locally,
// this mapper can redirect it to an alternative model that IS available.
type modelMapper interface {
	// MapModel returns the target model name if a mapping exists and the target
	// model has available providers. Returns empty string if no mapping applies.
	MapModel(requestedModel string) string

	// updateMappings refreshes the mapping configuration (for hot-reload).
	updateMappings(mappings []config.AmpModelMapping)
}

// defaultModelMapper implements ModelMapper with thread-safe mapping storage.
type defaultModelMapper struct {
	mu       sync.RWMutex
	mappings map[string]string // exact: from -> to (normalized lowercase keys)
	regexps  []regexMapping    // regex rules evaluated in order
}

// newModelMapper creates a new model mapper with the given initial mappings.
func newModelMapper(mappings []config.AmpModelMapping) *defaultModelMapper {
	m := &defaultModelMapper{
		mappings: make(map[string]string),
		regexps:  nil,
	}
	m.updateMappings(mappings)
	return m
}

// MapModel checks if a mapping exists for the requested model and if the
// target model has available local providers. Returns the mapped model name
// or empty string if no valid mapping exists.
//
// If the requested model contains a thinking suffix (e.g., "g25p(8192)"),
// the suffix is preserved in the returned model name (e.g., "gemini-2.5-pro(8192)").
// However, if the mapping target already contains a suffix, the config suffix
// takes priority over the user's suffix.
func (m *defaultModelMapper) MapModel(requestedModel string) string {
	if requestedModel == "" {
		return ""
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	// Extract thinking suffix from requested model using ParseSuffix
	requestResult := thinking.ParseSuffix(requestedModel)
	baseModel := requestResult.ModelName

	// Normalize the base model for lookup (case-insensitive)
	normalizedBase := strings.ToLower(strings.TrimSpace(baseModel))

	// Check for direct mapping using base model name
	targetModel, exists := m.mappings[normalizedBase]
	if !exists {
		// Try regex mappings in order using base model only
		// (suffix is handled separately via ParseSuffix)
		for _, rm := range m.regexps {
			if rm.re.MatchString(baseModel) {
				targetModel = rm.to
				exists = true
				break
			}
		}
		if !exists {
			return ""
		}
	}

	// Check if target model already has a thinking suffix (config priority)
	targetResult := thinking.ParseSuffix(targetModel)

	// Verify target model has available providers (use base model for lookup)
	providers := util.GetProviderName(targetResult.ModelName)
	if len(providers) == 0 {
		log.Debugf("amp model mapping: target model %s has no available providers, skipping mapping", targetModel)
		return ""
	}

	// Note: Detailed routing log is handled by logAmpRouting in fallback_handlers.go
	return thinking.PreserveRequestModelDecorations(targetModel, requestedModel)
}

// updateMappings refreshes the mapping configuration from config.
// This is called during initialization and on config hot-reload.
func (m *defaultModelMapper) updateMappings(mappings []config.AmpModelMapping) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Clear and rebuild mappings
	m.mappings = make(map[string]string, len(mappings))
	m.regexps = make([]regexMapping, 0, len(mappings))

	for _, mapping := range mappings {
		from := strings.TrimSpace(mapping.From)
		to := strings.TrimSpace(mapping.To)

		if from == "" || to == "" {
			log.Warnf("amp model mapping: skipping invalid mapping (from=%q, to=%q)", from, to)
			continue
		}

		if mapping.Regex {
			// Compile case-insensitive regex; wrap with (?i) to match behavior of exact lookups
			pattern := "(?i)" + from
			re, err := regexp.Compile(pattern)
			if err != nil {
				log.Warnf("amp model mapping: invalid regex %q: %v", from, err)
				continue
			}
			m.regexps = append(m.regexps, regexMapping{re: re, to: to})
			log.Debugf("amp model regex mapping registered: /%s/ -> %s", from, to)
		} else {
			// Store with normalized lowercase key for case-insensitive lookup
			normalizedFrom := strings.ToLower(from)
			m.mappings[normalizedFrom] = to
			log.Debugf("amp model mapping registered: %s -> %s", from, to)
		}
	}

	if len(m.mappings) > 0 {
		log.Infof("amp model mapping: loaded %d mapping(s)", len(m.mappings))
	}
	if n := len(m.regexps); n > 0 {
		log.Infof("amp model mapping: loaded %d regex mapping(s)", n)
	}
}

// getMappings returns a copy of current mappings (for debugging/status).
func (m *defaultModelMapper) getMappings() map[string]string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make(map[string]string, len(m.mappings))
	maps.Copy(result, m.mappings)
	return result
}

type regexMapping struct {
	re *regexp.Regexp
	to string
}
