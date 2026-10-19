package management

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type releaseRoundTripper func(*http.Request) (*http.Response, error)

func (transport releaseRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

func TestGitHubGetUsesOnlyGitHubTokenForGitHubAPI(t *testing.T) {
	tests := []struct {
		name          string
		requestURL    string
		gitHubToken   string
		gitStoreToken string
		ghToken       string
		wantAuthorize string
	}{
		{
			name:          "GitHub API uses explicit token",
			requestURL:    "https://api.github.com/repos/example/project/releases",
			gitHubToken:   " github-token ",
			gitStoreToken: "git-store-token",
			ghToken:       "gh-token",
			wantAuthorize: "Bearer github-token",
		},
		{
			name:          "fallback variables are ignored",
			requestURL:    "https://api.github.com/repos/example/project/releases",
			gitStoreToken: "git-store-token",
			ghToken:       "gh-token",
		},
		{
			name:        "custom host receives no GitHub token",
			requestURL:  "https://releases.example/api",
			gitHubToken: "github-token",
		},
		{
			name:        "lookalike host receives no GitHub token",
			requestURL:  "https://api.github.com.example/releases",
			gitHubToken: "github-token",
		},
		{
			name:        "alternate port receives no GitHub token",
			requestURL:  "https://api.github.com:8443/releases",
			gitHubToken: "github-token",
		},
		{
			name:          "explicit HTTPS port receives GitHub token",
			requestURL:    "https://api.github.com:443/releases",
			gitHubToken:   "github-token",
			wantAuthorize: "Bearer github-token",
		},
		{
			name:          "userinfo uses only URL credentials",
			requestURL:    "https://user@api.github.com/releases",
			gitHubToken:   "github-token",
			wantAuthorize: "Basic dXNlcjo=",
		},
		{
			name:          "username and password use only URL credentials",
			requestURL:    "https://user:pass@api.github.com/releases",
			gitHubToken:   "github-token",
			wantAuthorize: "Basic dXNlcjpwYXNz",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("GITHUB_TOKEN", tt.gitHubToken)
			t.Setenv("GITSTORE_GIT_URL", "https://github.com/example/auth-store.git")
			t.Setenv("GITSTORE_GIT_TOKEN", tt.gitStoreToken)
			t.Setenv("GH_TOKEN", tt.ghToken)
			client := &http.Client{Transport: releaseRoundTripper(func(request *http.Request) (*http.Response, error) {
				if got := request.Header.Get("Authorization"); got != tt.wantAuthorize {
					t.Fatalf("Authorization = %q, want %q", got, tt.wantAuthorize)
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(`[]`)),
					Request:    request,
				}, nil
			})}
			if _, errGet := githubGet(context.Background(), client, tt.requestURL); errGet != nil {
				t.Fatalf("githubGet() error = %v", errGet)
			}
		})
	}
}
