package config

import (
	"encoding/json"
	"fmt"
	"strings"
)

// DisableImageGenerationMode controls image generation endpoint and tool filtering behavior.
type DisableImageGenerationMode int

const (
	// DisableImageGenerationOff leaves image generation fully enabled.
	DisableImageGenerationOff DisableImageGenerationMode = iota
	// DisableImageGenerationAll disables all image generation endpoints (returns 404).
	DisableImageGenerationAll
	// DisableImageGenerationChat disables image generation when the request originates from chat.
	DisableImageGenerationChat
	// DisableImageGenerationPassthrough keeps client-supplied image generation tools untouched.
	DisableImageGenerationPassthrough
)

func parseDisableImageGenerationMode(s string) (DisableImageGenerationMode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "off", "false", "0":
		return DisableImageGenerationOff, nil
	case "all", "true", "1":
		return DisableImageGenerationAll, nil
	case "chat":
		return DisableImageGenerationChat, nil
	case "passthrough":
		return DisableImageGenerationPassthrough, nil
	default:
		return DisableImageGenerationOff, fmt.Errorf(
			"unknown disable-image-generation value %q (expected off/all/chat/passthrough)", s,
		)
	}
}

func (m DisableImageGenerationMode) String() string {
	switch m {
	case DisableImageGenerationAll:
		return "all"
	case DisableImageGenerationChat:
		return "chat"
	case DisableImageGenerationPassthrough:
		return "passthrough"
	default:
		return "off"
	}
}

// MarshalYAML serialises to the string representation.
func (m DisableImageGenerationMode) MarshalYAML() (any, error) {
	return m.String(), nil
}

// UnmarshalYAML accepts string ("off"/"all"/"chat"/"passthrough") and bool/int YAML values.
func (m *DisableImageGenerationMode) UnmarshalYAML(unmarshal func(any) error) error {
	var s string
	if err := unmarshal(&s); err != nil {
		// Try boolean
		var b bool
		if errBool := unmarshal(&b); errBool == nil {
			if b {
				*m = DisableImageGenerationAll
			} else {
				*m = DisableImageGenerationOff
			}
			return nil
		}
		return err
	}
	parsed, err := parseDisableImageGenerationMode(s)
	if err != nil {
		return err
	}
	*m = parsed
	return nil
}

// MarshalJSON serialises to the string representation.
func (m DisableImageGenerationMode) MarshalJSON() ([]byte, error) {
	return json.Marshal(m.String())
}

// UnmarshalJSON accepts string or boolean JSON values.
func (m *DisableImageGenerationMode) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		parsed, parseErr := parseDisableImageGenerationMode(s)
		if parseErr != nil {
			return parseErr
		}
		*m = parsed
		return nil
	}
	var b bool
	if err := json.Unmarshal(data, &b); err == nil {
		if b {
			*m = DisableImageGenerationAll
		} else {
			*m = DisableImageGenerationOff
		}
		return nil
	}
	return fmt.Errorf("cannot unmarshal %s into DisableImageGenerationMode", string(data))
}
