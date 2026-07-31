package auth

import (
	"encoding/json"
	"fmt"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/misc"
)

// SaveTokenJSON persists a token storage struct to disk in JSON format.
// It creates the necessary directory structure, merges metadata into the
// top-level JSON object, and writes the result atomically.
func SaveTokenJSON(authFilePath string, storage any, metadata map[string]any) error {
	misc.LogSavingCredentials(authFilePath)

	data, errMerge := misc.MergeMetadata(storage, metadata)
	if errMerge != nil {
		return fmt.Errorf("failed to merge metadata: %w", errMerge)
	}
	payload, errMarshal := json.Marshal(data)
	if errMarshal != nil {
		return fmt.Errorf("failed to encode token file: %w", errMarshal)
	}
	payload = append(payload, '\n')
	if errWrite := WriteTokenFileAtomic(authFilePath, payload); errWrite != nil {
		return fmt.Errorf("failed to write token to file: %w", errWrite)
	}
	return nil
}
