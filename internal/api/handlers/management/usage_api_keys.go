package management

import (
	"sort"
	"strconv"
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

type usageAPIKeyPresentation struct {
	display string
	alias   string
}

type usageAPIKeyPresenter struct {
	byIdentity         map[string]usageAPIKeyPresentation
	byRedactedIdentity map[string]usageAPIKeyPresentation
	rawKeys            []string
}

func newUsageAPIKeyPresenter(cfg *config.Config) usageAPIKeyPresenter {
	if cfg == nil {
		return usageAPIKeyPresenter{}
	}
	rawSet := make(map[string]struct{}, len(cfg.APIKeys)+len(cfg.APIKeyAliases)+len(cfg.APIKeyRules))
	for _, raw := range cfg.APIKeys {
		if raw = strings.TrimSpace(raw); raw != "" {
			rawSet[raw] = struct{}{}
		}
	}
	for raw := range cfg.APIKeyAliases {
		if raw = strings.TrimSpace(raw); raw != "" {
			rawSet[raw] = struct{}{}
		}
	}
	for raw := range cfg.APIKeyRules {
		if raw = strings.TrimSpace(raw); raw != "" {
			rawSet[raw] = struct{}{}
		}
	}
	if len(rawSet) == 0 {
		return usageAPIKeyPresenter{}
	}
	rawKeys := make([]string, 0, len(rawSet))
	for raw := range rawSet {
		rawKeys = append(rawKeys, raw)
	}
	sort.Strings(rawKeys)
	byIdentity := make(map[string]usageAPIKeyPresentation, len(rawKeys))
	identities := make([]string, 0, len(rawKeys))
	for _, raw := range rawKeys {
		identity := coreusage.HashAPIKeyIdentity(raw)
		identities = append(identities, identity)
		byIdentity[identity] = usageAPIKeyPresentation{
			display: util.HideAPIKey(raw),
			alias:   cfg.APIKeyAliases[raw],
		}
	}
	sort.Strings(identities)
	maskedTotals := make(map[string]int, len(identities))
	for _, identity := range identities {
		maskedTotals[util.HideAPIKey(identity)]++
	}
	maskedCounts := make(map[string]int, len(maskedTotals))
	byRedactedIdentity := make(map[string]usageAPIKeyPresentation, len(identities))
	for _, identity := range identities {
		masked := util.HideAPIKey(identity)
		key := masked
		if maskedTotals[masked] > 1 {
			maskedCounts[masked]++
			key += "#" + strconv.Itoa(maskedCounts[masked])
		}
		byRedactedIdentity[key] = byIdentity[identity]
		if maskedTotals[masked] == 1 {
			byRedactedIdentity[masked] = byIdentity[identity]
		}
	}
	return usageAPIKeyPresenter{
		byIdentity:         byIdentity,
		byRedactedIdentity: byRedactedIdentity,
		rawKeys:            rawKeys,
	}
}

func newUsageAPIKeyPresenterFromAliases(aliases map[string]string) usageAPIKeyPresenter {
	if len(aliases) == 0 {
		return usageAPIKeyPresenter{}
	}
	return newUsageAPIKeyPresenter(&config.Config{APIKeyAliases: aliases})
}

func (p usageAPIKeyPresenter) present(value string) usageAPIKeyPresentation {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || trimmed == "(none)" {
		return usageAPIKeyPresentation{display: value}
	}
	if presentation, ok := p.byIdentity[trimmed]; ok {
		return presentation
	}
	if presentation, ok := p.byIdentity[coreusage.EnsureAPIKeyIdentity(trimmed)]; ok {
		return presentation
	}
	return usageAPIKeyPresentation{display: util.HideAPIKey(trimmed)}
}

func (p usageAPIKeyPresenter) presentRedacted(value string) usageAPIKeyPresentation {
	if presentation, ok := p.byRedactedIdentity[strings.TrimSpace(value)]; ok {
		return presentation
	}
	return usageAPIKeyPresentation{display: value}
}

func (p usageAPIKeyPresenter) expandFilters(raw string, repeated []string) (string, []string) {
	inputs := make([]string, 0, len(repeated)+1)
	for _, value := range repeated {
		if value = strings.TrimSpace(value); value != "" {
			inputs = append(inputs, value)
		}
	}
	if len(inputs) == 0 {
		if raw = strings.TrimSpace(raw); raw != "" {
			inputs = append(inputs, raw)
		}
	}
	if len(inputs) == 0 {
		return "", nil
	}

	expanded := make([]string, 0, len(inputs))
	seen := make(map[string]struct{})
	add := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		if _, duplicate := seen[value]; duplicate {
			return
		}
		seen[value] = struct{}{}
		expanded = append(expanded, value)
	}
	for _, input := range inputs {
		matched := false
		if input == "(none)" {
			add(input)
			matched = true
		}
		for _, candidate := range p.rawKeys {
			if candidate == input {
				add(coreusage.HashAPIKeyIdentity(candidate))
				matched = true
			}
		}
		masked, ordinal, hasOrdinal := splitMaskedAPIKeyOrdinal(input)
		matches := matchingMaskedAPIKeyCandidates(masked, p.rawKeys)
		if hasOrdinal {
			if ordinal <= len(matches) {
				add(coreusage.HashAPIKeyIdentity(matches[ordinal-1]))
				matched = true
			}
		} else {
			for _, candidate := range matches {
				add(coreusage.HashAPIKeyIdentity(candidate))
				matched = true
			}
		}
		if !matched {
			add(input)
		}
	}
	return "", expanded
}
