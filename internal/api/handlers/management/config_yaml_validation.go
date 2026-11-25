package management

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginstore"
	"gopkg.in/yaml.v3"
)

type configYAMLFieldError struct {
	configValidationError
}

func (e *configYAMLFieldError) Error() string {
	return e.Field + ": " + e.Message
}

func configYAMLValidationDetail(err error) configValidationError {
	if fieldError, ok := errors.AsType[*configYAMLFieldError](err); ok {
		return fieldError.configValidationError
	}
	return configValidationError{Field: inferConfigValidationField(err.Error()), Message: err.Error()}
}

func (h *Handler) previewConfigYAML(body []byte) ([]string, error) {
	containsPlaceholder, err := configYAMLContainsPlaceholder(body)
	if err != nil {
		return nil, err
	}
	previous, err := os.ReadFile(h.configFilePath)
	if err != nil && !os.IsNotExist(err) {
		return nil, &configValidationInternalError{err: err}
	}
	if containsPlaceholder {
		if os.IsNotExist(err) {
			return nil, errors.New("redacted values require the current config file")
		}
		key, errKey := h.configYAMLRedactionKey()
		if errKey != nil {
			return nil, &configValidationInternalError{err: errKey}
		}
		body, err = restoreConfigYAMLPlaceholdersWithKey(body, previous, key)
		if err != nil {
			return nil, err
		}
	}
	_, warnings, err := h.validateConfigBody(body, previous)
	return warnings, err
}

// These checks use the submitted YAML before the loader normalizes or discards rules.
func validateConfigYAMLRules(next *config.Config, previous []byte) ([]string, error) {
	var original config.Config
	if len(previous) > 0 && yaml.Unmarshal(previous, &original) != nil {
		// An unreadable old document cannot identify an unchanged rule.
		original = config.Config{}
	}
	var warnings []string
	for _, section := range []struct {
		name string
		next []config.PayloadRule
		old  []config.PayloadRule
	}{
		{"default-raw", next.Payload.DefaultRaw, original.Payload.DefaultRaw},
		{"override-raw", next.Payload.OverrideRaw, original.Payload.OverrideRaw},
	} {
		if err := validateChangedConfigRows(
			&warnings, section.next, section.old, func(i int, rule config.PayloadRule) configValidationError {
				if path, invalid := config.InvalidRawPayloadParam(rule); invalid {
					return configValidationError{
						Field:   fmt.Sprintf("payload.%s[%d].params.%s", section.name, i, path),
						Message: "invalid Raw JSON; this rule is inactive",
					}
				}
				return configValidationError{}
			},
		); err != nil {
			return warnings, err
		}
	}
	for _, channel := range slices.Sorted(maps.Keys(next.OAuthRequestScopedErrors)) {
		// The normal config validator already warns about unchanged invalid OAuth regex patterns.
		if err := validateChangedConfigRows(
			nil, next.OAuthRequestScopedErrors[channel], original.OAuthRequestScopedErrors[channel],
			func(i int, rule config.RequestScopedErrorRule) configValidationError {
				for index, pattern := range rule.MatchRegexr {
					if _, err := regexp.Compile(strings.TrimSpace(pattern)); err != nil {
						return configValidationError{
							Field: fmt.Sprintf(
								"oauth-request-scoped-errors.%s[%d].match-regexr[%d]", channel, i, index,
							),
							Message: "invalid Go regular expression",
						}
					}
				}
				return configValidationError{}
			},
		); err != nil {
			return warnings, err
		}
	}
	if err := validateChangedConfigRows(
		&warnings, next.Plugins.StoreAuth, original.Plugins.StoreAuth,
		func(i int, rule pluginstore.AuthConfig) configValidationError {
			if strings.TrimSpace(rule.Match) == "" {
				return configValidationError{
					Field:   fmt.Sprintf("plugins.store-auth[%d].match", i),
					Message: "repository match is required; this rule is inactive",
				}
			}
			return configValidationError{}
		},
	); err != nil {
		return warnings, err
	}
	if err := validateChangedConfigRows(
		&warnings, next.CodexKey, original.CodexKey, func(i int, key config.CodexKey) configValidationError {
			if strings.TrimSpace(key.BaseURL) == "" {
				return configValidationError{
					Field:   fmt.Sprintf("codex-api-key[%d].base-url", i),
					Message: "base-url is required; this credential is inactive",
				}
			}
			return configValidationError{}
		},
	); err != nil {
		return warnings, err
	}
	return warnings, nil
}

func validateChangedConfigRows[T any](
	warnings *[]string,
	next, original []T,
	validate func(int, T) configValidationError,
) error {
	matched := make([]bool, len(original))
	for index, row := range next {
		problem := validate(index, row)
		if problem.Field == "" {
			continue
		}
		same := -1
		for i, before := range original {
			if !matched[i] && reflect.DeepEqual(row, before) {
				same = i
				break
			}
		}
		issue := &configYAMLFieldError{problem}
		if same < 0 {
			return issue
		}
		matched[same] = true
		if warnings != nil {
			*warnings = append(*warnings, issue.Error())
		}
	}
	return nil
}
