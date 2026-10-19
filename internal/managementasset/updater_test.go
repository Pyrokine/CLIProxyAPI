package managementasset

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
)

func TestAutoUpdateSkipReason(t *testing.T) {
	disabled := true
	tests := []struct {
		name       string
		cfg        *config.Config
		wantReason string
		wantSkip   bool
	}{
		{
			name:       "nil config",
			cfg:        nil,
			wantReason: "config not yet available",
			wantSkip:   true,
		},
		{
			name: "cluster mode",
			cfg: &config.Config{
				Home: config.HomeConfig{Enabled: true},
			},
			wantReason: "cluster mode enabled",
			wantSkip:   true,
		},
		{
			name: "control panel disabled",
			cfg: &config.Config{
				RemoteManagement: config.RemoteManagement{DisableControlPanel: true},
			},
			wantReason: "control panel disabled",
			wantSkip:   true,
		},
		{
			name: "auto check disabled",
			cfg: &config.Config{
				RemoteManagement: config.RemoteManagement{AutoCheckUpdate: false},
			},
			wantReason: "auto-check-update is false",
			wantSkip:   true,
		},
		{
			name: "auto update disabled",
			cfg: &config.Config{
				RemoteManagement: config.RemoteManagement{
					AutoCheckUpdate:        true,
					DisableAutoUpdatePanel: &disabled,
				},
			},
			wantReason: "auto-update-panel is false",
			wantSkip:   true,
		},
		{
			name: "enabled",
			cfg: &config.Config{
				RemoteManagement: config.RemoteManagement{AutoCheckUpdate: true},
			},
			wantReason: "",
			wantSkip:   false,
		},
	}

	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				gotReason, gotSkip := autoUpdateSkipReason(tt.cfg)
				if gotReason != tt.wantReason || gotSkip != tt.wantSkip {
					t.Fatalf(
						"autoUpdateSkipReason() = (%q, %t), want (%q, %t)",
						gotReason,
						gotSkip,
						tt.wantReason,
						tt.wantSkip,
					)
				}
			},
		)
	}
}

func TestDownloadAssetRejectsOversizedBody(t *testing.T) {
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(strings.Repeat("x", maxManagementAssetBytes+1)))
			},
		),
	)
	defer server.Close()

	_, _, err := downloadAsset(context.Background(), server.Client(), server.URL)
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("downloadAsset() error = %v, want oversized error", err)
	}
}

func TestFetchReleaseAssetUsesGitHubTokenOnlyForGitHubAPI(t *testing.T) {
	tests := []struct {
		name        string
		url         string
		gitHubToken string
		wantAuth    string
	}{
		{
			name:        "GitHub API",
			url:         "https://api.github.com/repos/example/repository/releases/latest",
			gitHubToken: "github-token",
			wantAuth:    "Bearer github-token",
		},
		{
			name: "fallback variables are ignored",
			url:  "https://api.github.com/repos/example/repository/releases/latest",
		},
		{
			name:        "custom host",
			url:         "https://releases.example/releases/latest",
			gitHubToken: "github-token",
		},
		{
			name:        "GitHub API alternate port",
			url:         "https://api.github.com:8443/repos/example/repository/releases/latest",
			gitHubToken: "github-token",
		},
		{
			name:        "GitHub API explicit HTTPS port",
			url:         "https://api.github.com:443/repos/example/repository/releases/latest",
			gitHubToken: "github-token",
			wantAuth:    "Bearer github-token",
		},
		{
			name:        "GitHub API userinfo uses only URL credentials",
			url:         "https://user@api.github.com/repos/example/repository/releases/latest",
			gitHubToken: "github-token",
			wantAuth:    "Basic dXNlcjo=",
		},
		{
			name:        "GitHub API username and password use only URL credentials",
			url:         "https://user:pass@api.github.com/repos/example/repository/releases/latest",
			gitHubToken: "github-token",
			wantAuth:    "Basic dXNlcjpwYXNz",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("GITHUB_TOKEN", tt.gitHubToken)
			t.Setenv("GITSTORE_GIT_URL", "https://github.com/example/auth-store.git")
			t.Setenv("GITSTORE_GIT_TOKEN", "git-store-token")
			t.Setenv("GH_TOKEN", "gh-token")
			client := &http.Client{
				Transport: roundTripFunc(
					func(request *http.Request) (*http.Response, error) {
						if got := request.Header.Get("Authorization"); got != tt.wantAuth {
							t.Fatalf("Authorization = %q, want %q", got, tt.wantAuth)
						}
						return &http.Response{
							StatusCode: http.StatusOK,
							Header:     make(http.Header),
							Body: io.NopCloser(
								strings.NewReader(
									`{"assets":[{"name":"management.html","browser_download_url":"https://downloads.example/management.html","digest":"sha256:abc"}]}`,
								),
							),
							Request: request,
						}, nil
					},
				),
			}
			if _, _, errFetch := fetchReleaseAsset(context.Background(), client, tt.url); errFetch != nil {
				t.Fatalf("fetchReleaseAsset() error = %v", errFetch)
			}
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}
