package modelcatalog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"
)

type FieldChange struct {
	Current any `json:"current,omitempty"`
	Default any `json:"default,omitempty"`
}

type DefaultUpdateChange struct {
	ID      string                 `json:"id"`
	Name    string                 `json:"name"`
	Type    string                 `json:"type"`
	Current *Model                 `json:"current,omitempty"`
	Default *Model                 `json:"default,omitempty"`
	Fields  map[string]FieldChange `json:"fields,omitempty"`
}

type DefaultUpdatePreview struct {
	CurrentDefaultsVersion string                `json:"current_defaults_version"`
	LatestDefaultsVersion  string                `json:"latest_defaults_version"`
	Changes                []DefaultUpdateChange `json:"changes"`
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

func ApplyDefaultUpdate(configPath string, decisions []ApplyDecision) (ApplyResult, error) {
	current, err := LoadOrInit(configPath, nil)
	if err != nil {
		return ApplyResult{}, err
	}
	backupPath, err := Backup(configPath)
	if err != nil {
		return ApplyResult{}, err
	}
	defaults := DefaultFile()
	applied := 0
	for _, decision := range decisions {
		name := normalizeModelName(decision.Name)
		if name == "" {
			continue
		}
		action := strings.ToLower(strings.TrimSpace(decision.Action))
		switch action {
		case "use_default", "restore", "adopt":
			defaultModel, ok := defaults.Find(name)
			if !ok {
				if current.RemoveModel(name, defaults) {
					applied++
				}
				continue
			}
			if existing, ok := current.Find(name); ok {
				enabled := existing.Enabled
				*existing = *defaultModel
				existing.Enabled = enabled
			} else {
				current.Models = append(current.Models, *defaultModel)
			}
			current.RemovedDefaultModels = removeString(current.RemovedDefaultModels, name)
			applied++
		case "remove_default":
			if current.RemoveModel(name, defaults) {
				applied++
			}
		case "keep_current", "skip", "":
			continue
		default:
			return ApplyResult{}, fmt.Errorf("unknown default update action %q", decision.Action)
		}
	}
	current.DefaultsVersion = defaults.DefaultsVersion
	current.Sanitize()
	if err := Save(configPath, current); err != nil {
		return ApplyResult{}, err
	}
	return ApplyResult{Status: "ok", BackupPath: backupPath, Applied: applied}, nil
}

func Backup(configPath string) (string, error) {
	filePath := Path(configPath)
	if filePath == "" {
		return "", fmt.Errorf("config file path not set")
	}
	data, err := os.ReadFile(filePath)
	if err != nil {
		return "", err
	}
	backupPath := fmt.Sprintf("%s.%s.bak", filePath, time.Now().Format("20060102-150405"))
	if err := os.WriteFile(backupPath, data, 0o600); err != nil {
		return "", err
	}
	return backupPath, nil
}

func diffDefaults(current, defaults File) []DefaultUpdateChange {
	changes := make([]DefaultUpdateChange, 0)
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
				changes, DefaultUpdateChange{
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
				changes, DefaultUpdateChange{ID: "new:" + key, Name: name, Type: "new_default", Default: &modelCopy},
			)
			continue
		}
		if currentModel.UserCreated {
			continue
		}
		fields := changedDefaultFields(*currentModel, defaultModel)
		if len(fields) > 0 {
			currentCopy := *currentModel
			defaultCopy := defaultModel
			changes = append(
				changes, DefaultUpdateChange{
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
		changes = append(
			changes, DefaultUpdateChange{
				ID:      "removed:" + modelKey(currentModel.Name),
				Name:    currentModel.Name,
				Type:    "default_removed_upstream",
				Current: &currentCopy,
			},
		)
	}
	return changes
}

func changedDefaultFields(current, defaults Model) map[string]FieldChange {
	fields := make(map[string]FieldChange)
	compare := func(name string, currentValue, defaultValue any) {
		if !reflect.DeepEqual(currentValue, defaultValue) {
			fields[name] = FieldChange{Current: currentValue, Default: defaultValue}
		}
	}
	compare("provider", current.Provider, defaults.Provider)
	compare("channel", current.Channel, defaults.Channel)
	compare("group", current.Group, defaults.Group)
	compare("display_name", current.DisplayName, defaults.DisplayName)
	compare("aliases", current.Aliases, defaults.Aliases)
	compare("price", current.Price, defaults.Price)
	return fields
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

func writeDebugJSON(path string, value any) error {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Clean(path), append(data, '\n'), 0o600)
}
