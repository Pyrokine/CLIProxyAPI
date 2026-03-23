// Package vertex provides token storage for Google Vertex AI Gemini via service account credentials.
// It serializes service account JSON into an auth file that is consumed by the runtime executor.
package vertex

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/misc"
	log "github.com/sirupsen/logrus"
)

// CredentialStorage stores the service account JSON for Vertex AI access.
// The content is persisted verbatim under the "service_account" key, together with
// helper fields for project, location and email to improve logging and discovery.
type CredentialStorage struct {
	// ServiceAccount holds the parsed service account JSON content.
	ServiceAccount map[string]any `json:"service_account"`

	// ProjectID is derived from the service account JSON (project_id).
	ProjectID string `json:"project_id"`

	// Email is the client_email from the service account JSON.
	Email string `json:"email"`

	// Location optionally sets a default region (e.g., us-central1) for Vertex endpoints.
	Location string `json:"location,omitempty"`

	// Type is the provider identifier stored alongside credentials. Always "vertex".
	Type string `json:"type"`
}

// SaveTokenToFile writes the credential payload to the given file path in JSON format.
// It ensures the parent directory exists and logs the operation for transparency.
func (s *CredentialStorage) SaveTokenToFile(authFilePath string) error {
	misc.LogSavingCredentials(authFilePath)
	if s == nil {
		return fmt.Errorf("vertex credential: storage is nil")
	}
	if s.ServiceAccount == nil {
		return fmt.Errorf("vertex credential: service account content is empty")
	}
	// Ensure we tag the file with the provider type.
	s.Type = "vertex"

	if err := os.MkdirAll(filepath.Dir(authFilePath), 0o700); err != nil {
		return fmt.Errorf("vertex credential: create directory failed: %w", err)
	}
	f, err := os.Create(authFilePath)
	if err != nil {
		return fmt.Errorf("vertex credential: create file failed: %w", err)
	}
	defer func() {
		if errClose := f.Close(); errClose != nil {
			log.Errorf("vertex credential: failed to close file: %v", errClose)
		}
	}()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err = enc.Encode(s); err != nil {
		return fmt.Errorf("vertex credential: encode failed: %w", err)
	}
	return nil
}

// Label builds a human-readable label for a Vertex AI credential.
func Label(projectID, email string) string {
	p := strings.TrimSpace(projectID)
	e := strings.TrimSpace(email)
	if p != "" && e != "" {
		return fmt.Sprintf("%s (%s)", p, e)
	}
	if p != "" {
		return p
	}
	if e != "" {
		return e
	}
	return "vertex"
}

// SanitizeFilePart replaces path separators and whitespace in s for safe use in file names.
func SanitizeFilePart(s string) string {
	out := strings.TrimSpace(s)
	replacers := []string{"/", "_", "\\", "_", ":", "_", " ", "-"}
	for i := 0; i < len(replacers); i += 2 {
		out = strings.ReplaceAll(out, replacers[i], replacers[i+1])
	}
	if out == "" {
		return "vertex"
	}
	return out
}
