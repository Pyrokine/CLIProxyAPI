// Package vertex provides token storage for Google Vertex AI Gemini via service account credentials.
// It serializes service account JSON into an auth file that is consumed by the runtime executor.
package vertex

import (
	"fmt"
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/auth"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
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

	// Prefix optionally namespaces models for this credential.
	Prefix string `json:"prefix,omitempty"`

	// Metadata holds arbitrary key-value pairs injected before persistence.
	Metadata map[string]any `json:"-"`
}

func (s *CredentialStorage) CloneForPersistence() coreauth.TokenStorage {
	if s == nil {
		return nil
	}
	copyStorage := *s
	return &copyStorage
}

// SetMetadata injects metadata before the credential is persisted.
func (s *CredentialStorage) SetMetadata(metadata map[string]any) {
	s.Metadata = metadata
}

// SaveTokenToFile writes the credential payload to the given file path in JSON format.
// It ensures the parent directory exists and logs the operation for transparency.
func (s *CredentialStorage) SaveTokenToFile(authFilePath string) error {
	if s == nil {
		return fmt.Errorf("vertex credential: storage is nil")
	}
	if s.ServiceAccount == nil {
		return fmt.Errorf("vertex credential: service account content is empty")
	}
	// Ensure we tag the file with the provider type.
	s.Type = "vertex"
	if errSave := auth.SaveTokenJSON(authFilePath, s, s.Metadata); errSave != nil {
		return fmt.Errorf("vertex credential: save token file: %w", errSave)
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
