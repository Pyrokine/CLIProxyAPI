package util

import (
	"os"
	"strings"
)

// GitHubToken returns the explicit token used for GitHub API requests.
func GitHubToken() string {
	return strings.TrimSpace(os.Getenv("GITHUB_TOKEN"))
}
