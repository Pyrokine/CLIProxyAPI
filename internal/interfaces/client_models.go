// Package interfaces defines the core interfaces and shared structures for the CLI Proxy API server.
// These interfaces provide a common contract for different components of the application,
// such as AI service clients, API handlers, and data models.
package interfaces

import (
	"time"
)

// GCPProject represents the response structure for a Google Cloud project list request.
// This structure is used when fetching available projects for a Google Cloud account.
type GCPProject struct {
	// Projects is a list of Google Cloud projects accessible by the user.
	Projects []GCPProjectProjects `json:"projects"`
}

// gCPProjectLabels defines the labels associated with a GCP project.
// These labels can contain metadata about the project's purpose or configuration.
type gCPProjectLabels struct {
	// GenerativeLanguage indicates if the project has generative language APIs enabled.
	GenerativeLanguage string `json:"generative-language"`
}

// GCPProjectProjects contains details about a single Google Cloud project.
// This includes identifying information, metadata, and configuration details.
type GCPProjectProjects struct {
	// ProjectNumber is the unique numeric identifier for the project.
	ProjectNumber string `json:"projectNumber"`

	// ProjectID is the unique string identifier for the project.
	ProjectID string `json:"projectId"`

	// LifecycleState indicates the current state of the project (e.g., "ACTIVE").
	LifecycleState string `json:"lifecycleState"`

	// Name is the human-readable name of the project.
	Name string `json:"name"`

	// Labels contains metadata labels associated with the project.
	Labels gCPProjectLabels `json:"labels"`

	// CreateTime is the timestamp when the project was created.
	CreateTime time.Time `json:"createTime"`
}
