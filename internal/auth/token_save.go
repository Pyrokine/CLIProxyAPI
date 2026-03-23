package auth

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/misc"
)

// SaveTokenJSON persists a token storage struct to disk in JSON format.
// It creates the necessary directory structure, merges metadata into the
// top-level JSON object, and writes the result atomically.
func SaveTokenJSON(authFilePath string, storage any, metadata map[string]any) error {
	misc.LogSavingCredentials(authFilePath)

	if err := os.MkdirAll(filepath.Dir(authFilePath), 0o700); err != nil {
		return fmt.Errorf("failed to create directory: %v", err)
	}

	f, err := os.OpenFile(authFilePath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("failed to create token file: %w", err)
	}
	defer func() { _ = f.Close() }()

	data, errMerge := misc.MergeMetadata(storage, metadata)
	if errMerge != nil {
		return fmt.Errorf("failed to merge metadata: %w", errMerge)
	}

	if err = json.NewEncoder(f).Encode(data); err != nil {
		return fmt.Errorf("failed to write token to file: %w", err)
	}
	return nil
}
