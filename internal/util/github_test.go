package util

import "testing"

func TestGitHubTokenUsesOnlyGitHubToken(t *testing.T) {
	tests := []struct {
		name          string
		gitHubToken   string
		gitStoreToken string
		lowerToken    string
		ghToken       string
		want          string
	}{
		{
			name:          "explicit token",
			gitHubToken:   " github-token ",
			gitStoreToken: "git-store-token",
			lowerToken:    "lower-token",
			ghToken:       "gh-token",
			want:          "github-token",
		},
		{
			name:          "other token variables are ignored",
			gitStoreToken: "git-store-token",
			lowerToken:    "lower-token",
			ghToken:       "gh-token",
			want:          "",
		},
		{
			name:        "blank explicit token",
			gitHubToken: "  ",
			want:        "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("GITHUB_TOKEN", tt.gitHubToken)
			t.Setenv("GITSTORE_GIT_URL", "https://github.com/example/auth-store.git")
			t.Setenv("GITSTORE_GIT_TOKEN", tt.gitStoreToken)
			t.Setenv("github_token", tt.lowerToken)
			t.Setenv("GH_TOKEN", tt.ghToken)

			if got := GitHubToken(); got != tt.want {
				t.Fatalf("GitHubToken() = %q, want %q", got, tt.want)
			}
		})
	}
}
