package modelcatalog

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
)

type fieldChange struct {
	Current any `json:"current,omitempty"`
	Default any `json:"default,omitempty"`
}

type defaultUpdateChange struct {
	ID      string                 `json:"id"`
	Name    string                 `json:"name"`
	Type    string                 `json:"type"`
	Current *Model                 `json:"current,omitempty"`
	Default *Model                 `json:"default,omitempty"`
	Fields  map[string]fieldChange `json:"fields,omitempty"`
}

type DefaultUpdatePreview struct {
	CurrentDefaultsVersion string                `json:"current_defaults_version"`
	LatestDefaultsVersion  string                `json:"latest_defaults_version"`
	Changes                []defaultUpdateChange `json:"changes"`
}

type ApplyDecision struct {
	Name   string `json:"name"`
	Action string `json:"action"`
}

type ApplyResult struct {
	Status     string `json:"status"`
	BackupPath string `json:"backup_path,omitempty"`
	Applied    int    `json:"applied"`
}

func PreviewDefaultUpdate(configPath string) (DefaultUpdatePreview, error) {
	current, err := LoadOrInit(configPath, nil)
	if err != nil {
		return DefaultUpdatePreview{}, err
	}
	defaults := DefaultFile()
	changes := diffDefaults(current, defaults)
	return DefaultUpdatePreview{
		CurrentDefaultsVersion: current.DefaultsVersion,
		LatestDefaultsVersion:  defaults.DefaultsVersion,
		Changes:                changes,
	}, nil
}

func applyDefaultUpdate(configPath string, decisions []ApplyDecision) (ApplyResult, error) {
	applied := 0
	_, backupPath, err := update(
		configPath,
		nil,
		UpdateOptions{Backup: true},
		func(doc *File) error {
			var errApply error
			applied, errApply = ApplyDefaultDecisions(doc, decisions)
			return errApply
		},
	)
	if err != nil {
		return ApplyResult{}, err
	}
	return ApplyResult{Status: "ok", BackupPath: backupPath, Applied: applied}, nil
}

func ApplyDefaultDecisions(current *File, decisions []ApplyDecision) (int, error) {
	if current == nil {
		return 0, fmt.Errorf("model catalog is nil")
	}
	defaults := DefaultFile()
	applied := 0
	for _, decision := range decisions {
		name := normalizeModelName(decision.Name)
		if name == "" {
			continue
		}
		action := strings.ToLower(strings.TrimSpace(decision.Action))
		defaultModel, hasDefaultModel := defaults.Find(name)
		defaultPrice, hasDefaultPrice := defaults.Price(name)
		switch action {
		case "use_default", "restore", "adopt":
			if !hasDefaultModel && !hasDefaultPrice {
				changed := current.RemoveModel(name, defaults)
				changed = current.DeletePrice(name) || changed
				if changed {
					applied++
				}
				continue
			}
			if hasDefaultModel {
				if existing, ok := current.Find(name); ok {
					enabled := existing.Enabled
					*existing = *defaultModel
					existing.Enabled = enabled
				} else {
					current.Models = append(current.Models, *defaultModel)
				}
				current.RemovedDefaultModels = removeString(current.RemovedDefaultModels, name)
			}
			if hasDefaultPrice {
				current.SetPrice(name, defaultPrice)
			} else {
				current.DeletePrice(name)
			}
			applied++
		case "remove_default":
			changed := current.RemoveModel(name, defaults)
			if hasDefaultPrice {
				current.SetPrice(name, defaultPrice)
			} else {
				changed = current.DeletePrice(name) || changed
			}
			if changed {
				applied++
			}
		case "keep_current", "skip", "":
			continue
		default:
			return 0, fmt.Errorf("unknown default update action %q", decision.Action)
		}
	}
	current.DefaultsVersion = defaults.DefaultsVersion
	current.Sanitize()
	return applied, nil
}

func diffDefaults(current, defaults File) []defaultUpdateChange {
	changes := make([]defaultUpdateChange, 0)
	priceChangesHandled := make(map[string]struct{})
	removed := make(map[string]struct{}, len(current.RemovedDefaultModels))
	for _, name := range current.RemovedDefaultModels {
		removed[modelKey(name)] = struct{}{}
	}
	for _, defaultModel := range defaults.Models {
		name := defaultModel.Name
		key := modelKey(name)
		if _, wasRemoved := removed[key]; wasRemoved {
			modelCopy := defaultModel
			changes = append(
				changes, defaultUpdateChange{
					ID:      "restore:" + key,
					Name:    name,
					Type:    "restore_removed_default",
					Default: &modelCopy,
				},
			)
			continue
		}
		currentModel, ok := current.Find(name)
		if !ok {
			modelCopy := defaultModel
			changes = append(
				changes, defaultUpdateChange{ID: "new:" + key, Name: name, Type: "new_default", Default: &modelCopy},
			)
			continue
		}
		if currentModel.UserCreated {
			continue
		}
		fields := changedDefaultFields(*currentModel, defaultModel)
		appendPriceFieldChange(fields, current, defaults, name)
		if len(fields) > 0 {
			currentCopy := *currentModel
			defaultCopy := defaultModel
			changes = append(
				changes, defaultUpdateChange{
					ID:      "changed:" + key,
					Name:    name,
					Type:    "changed_default",
					Current: &currentCopy,
					Default: &defaultCopy,
					Fields:  fields,
				},
			)
		}
	}
	for _, currentModel := range current.Models {
		if currentModel.UserCreated {
			continue
		}
		if _, ok := defaults.Find(currentModel.Name); ok {
			continue
		}
		currentCopy := currentModel
		fields := make(map[string]fieldChange)
		appendPriceFieldChange(fields, current, defaults, currentModel.Name)
		priceChangesHandled[modelKey(currentModel.Name)] = struct{}{}
		changes = append(
			changes, defaultUpdateChange{
				ID:      "removed:" + modelKey(currentModel.Name),
				Name:    currentModel.Name,
				Type:    "default_removed_upstream",
				Current: &currentCopy,
				Fields:  fields,
			},
		)
	}

	priceOnlyNames := make([]string, 0, len(defaults.Prices))
	for name := range defaults.Prices {
		if _, ok := defaults.Find(name); !ok {
			priceOnlyNames = append(priceOnlyNames, name)
		}
	}
	sort.Slice(
		priceOnlyNames, func(i, j int) bool {
			return strings.ToLower(priceOnlyNames[i]) < strings.ToLower(priceOnlyNames[j])
		},
	)
	for _, name := range priceOnlyNames {
		if _, handled := priceChangesHandled[modelKey(name)]; handled {
			continue
		}
		fields := make(map[string]fieldChange)
		appendPriceFieldChange(fields, current, defaults, name)
		if len(fields) == 0 {
			continue
		}
		changeType := "changed_default"
		if _, ok := current.Price(name); !ok {
			changeType = "new_default"
		}
		changes = append(
			changes, defaultUpdateChange{
				ID:     "price:" + modelKey(name),
				Name:   name,
				Type:   changeType,
				Fields: fields,
			},
		)
	}
	return changes
}

func changedDefaultFields(current, defaults Model) map[string]fieldChange {
	fields := make(map[string]fieldChange)
	compare := func(name string, currentValue, defaultValue any) {
		if !reflect.DeepEqual(currentValue, defaultValue) {
			fields[name] = fieldChange{Current: currentValue, Default: defaultValue}
		}
	}
	compare("provider", current.Provider, defaults.Provider)
	compare("channel", current.Channel, defaults.Channel)
	compare("group", current.Group, defaults.Group)
	compare("display_name", current.DisplayName, defaults.DisplayName)
	compare("aliases", current.Aliases, defaults.Aliases)
	return fields
}

func appendPriceFieldChange(fields map[string]fieldChange, current, defaults File, name string) {
	currentPrice, hasCurrent := current.Price(name)
	defaultPrice, hasDefault := defaults.Price(name)
	if hasCurrent == hasDefault && (!hasCurrent || reflect.DeepEqual(currentPrice, defaultPrice)) {
		return
	}
	var currentValue, defaultValue any
	if hasCurrent {
		currentValue = currentPrice
	}
	if hasDefault {
		defaultValue = defaultPrice
	}
	fields["price"] = fieldChange{Current: currentValue, Default: defaultValue}
}

func removeString(values []string, name string) []string {
	key := modelKey(name)
	out := values[:0]
	for _, value := range values {
		if modelKey(value) == key {
			continue
		}
		out = append(out, value)
	}
	return uniqueStrings(out)
}
