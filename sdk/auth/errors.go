package auth

import "fmt"

// GCPProject contains details about a Google Cloud project available to the user.
type GCPProject struct {
	ProjectNumber  string           `json:"projectNumber"`
	ProjectID      string           `json:"projectId"`
	LifecycleState string           `json:"lifecycleState"`
	Name           string           `json:"name"`
	Labels         GCPProjectLabels `json:"labels"`
}

// GCPProjectLabels contains metadata labels associated with a Google Cloud project.
type GCPProjectLabels struct {
	GenerativeLanguage string `json:"generative-language"`
}

// ProjectSelectionError indicates that the user must choose a specific project ID.
type ProjectSelectionError struct {
	Email    string
	Projects []GCPProject
}

func (e *ProjectSelectionError) Error() string {
	if e == nil {
		return "cliproxy auth: project selection required"
	}
	return fmt.Sprintf("cliproxy auth: project selection required for %s", e.Email)
}

// ProjectsDisplay returns the projects list for caller presentation.
func (e *ProjectSelectionError) ProjectsDisplay() []GCPProject {
	if e == nil {
		return nil
	}
	return e.Projects
}

// EmailRequiredError indicates that the calling context must provide an email or alias.
type EmailRequiredError struct {
	Prompt string
}

func (e *EmailRequiredError) Error() string {
	if e == nil || e.Prompt == "" {
		return "cliproxy auth: email is required"
	}
	return e.Prompt
}
