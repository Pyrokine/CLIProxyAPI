package management

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
)

type releaseRoundTripper func(*http.Request) (*http.Response, error)

func (transport releaseRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

func TestPanelReleasesAndInstallationUseSameSource(t *testing.T) {
	const tag, content = "v1.2.3", "verified fixture panel"
	for _, tc := range []struct{ name, configured, source, asset, auth string }{
		{
			"GitHub shorthand", "synthetic/panel", "https://api.github.com/repos/synthetic/panel/releases",
			"https://github.com/synthetic/panel/releases/download/" + tag + "/management.html", "Bearer fixture-token",
		},
		{
			"private configured release API", "https://127.0.0.1:8443/api/v3/repos/synthetic/panel/releases/latest",
			"https://127.0.0.1:8443/api/v3/repos/synthetic/panel/releases",
			"https://127.0.0.1:8443/releases/assets/management.html", "",
		},
	} {
		t.Run(
			tc.name, func(t *testing.T) {
				dir := t.TempDir()
				t.Setenv("MANAGEMENT_STATIC_PATH", dir)
				t.Setenv("GITHUB_TOKEN", "fixture-token")
				var requests []string
				previous := http.DefaultTransport
				http.DefaultTransport = releaseRoundTripper(
					func(req *http.Request) (*http.Response, error) {
						requestURL := req.URL.String()
						requests = append(requests, requestURL)
						var body, auth string
						switch requestURL {
						case tc.source + "?per_page=100":
							body, auth = `[{"tag_name":"`+tag+`"}]`, tc.auth
						case tc.source + "/tags/" + tag:
							body = fmt.Sprintf(
								`{"tag_name":%q,"assets":[{"name":"management.html","browser_download_url":%q,"digest":"sha256:%x"}]}`,
								tag, tc.asset, sha256.Sum256([]byte(content)),
							)
							auth = tc.auth
						case tc.asset:
							body = content
						default:
							return nil, fmt.Errorf("unexpected fixture destination: %s", requestURL)
						}
						if req.Header.Get("Authorization") != auth {
							return nil, fmt.Errorf("wrong authorization at fixture destination: %s", requestURL)
						}
						return &http.Response{
							StatusCode: http.StatusOK, Header: make(http.Header),
							Body: io.NopCloser(strings.NewReader(body)), Request: req,
						}, nil
					},
				)
				t.Cleanup(func() { http.DefaultTransport = previous })
				cfg := &config.Config{RemoteManagement: config.RemoteManagement{PanelGitHubRepository: tc.configured}}
				h := &Handler{cfg: cfg, configFilePath: filepath.Join(dir, "config.yaml")}
				listed := runConfigListRequest(h.GetReleases, http.MethodGet, "/releases?target=panel&force=1", "")
				if listed.Code != http.StatusOK || !strings.Contains(listed.Body.String(), tag) {
					t.Fatalf("release list = %d: %s; requests = %v", listed.Code, listed.Body, requests)
				}
				installed := runConfigListRequest(
					h.PostPanelUpdate, http.MethodPost, "/panel-update", `{"version":"`+tag+`"}`,
				)
				if installed.Code != http.StatusOK || len(requests) != 3 {
					t.Fatalf("installation = %d: %s; requests = %v", installed.Code, installed.Body, requests)
				}
				if data, err := os.ReadFile(
					filepath.Join(
						dir, "management.html",
					),
				); err != nil || string(data) != content {
					t.Fatalf("installed panel = %q, %v", data, err)
				}
			},
		)
	}
}

func TestGitHubGetUsesOnlyGitHubTokenForGitHubAPI(t *testing.T) {
	//goland:noinspection SpellCheckingInspection
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
		t.Run(
			tt.name, func(t *testing.T) {
				t.Setenv("GITHUB_TOKEN", tt.gitHubToken)
				t.Setenv("GITSTORE_GIT_URL", "https://github.com/example/auth-store.git")
				t.Setenv("GITSTORE_GIT_TOKEN", tt.gitStoreToken)
				t.Setenv("GH_TOKEN", tt.ghToken)
				client := &http.Client{
					Transport: releaseRoundTripper(
						func(request *http.Request) (*http.Response, error) {
							if got := request.Header.Get("Authorization"); got != tt.wantAuthorize {
								t.Fatalf("Authorization = %q, want %q", got, tt.wantAuthorize)
							}
							return &http.Response{
								StatusCode: http.StatusOK,
								Header:     make(http.Header),
								Body:       io.NopCloser(strings.NewReader(`[]`)),
								Request:    request,
							}, nil
						},
					),
				}
				if _, errGet := githubGet(context.Background(), client, tt.requestURL); errGet != nil {
					t.Fatalf("githubGet() error = %v", errGet)
				}
			},
		)
	}
}
