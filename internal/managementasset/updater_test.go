package managementasset

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
)

func TestAutoUpdateSkipReason(t *testing.T) {
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
			name: "auto install disabled keeps checking",
			cfg: &config.Config{
				RemoteManagement: config.RemoteManagement{
					AutoCheckUpdate:        true,
					DisableAutoUpdatePanel: true,
				},
			},
			wantReason: "",
			wantSkip:   false,
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

const managementTestAssetURL = "https://github.com/fixture/panel/releases/download/v1/management.html"

func TestDownloadAssetRejectsOversizedBody(t *testing.T) {
	source, err := ResolveReleasesURL("fixture/panel")
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{
		Transport: roundTripFunc(
			func(request *http.Request) (*http.Response, error) {
				return managementTestResponse(
					request, http.StatusOK, strings.Repeat("x", maxManagementAssetBytes+1),
				), nil
			},
		),
	}
	_, _, err = downloadAsset(t.Context(), client, source, managementTestAssetURL)
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("downloadAsset() error = %v, want oversized error", err)
	}
}

func TestFetchReleaseAssetUsesGitHubTokenOnlyForGitHubAPI(t *testing.T) {
	//goland:noinspection SpellCheckingInspection
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
		t.Run(
			tt.name, func(t *testing.T) {
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
										managementTestRelease("panel", managementTestAssetURL, ""),
									),
								),
								Request: request,
							}, nil
						},
					),
				}
				if _, _, errFetch := fetchReleaseAsset(context.Background(), client, tt.url, ""); errFetch != nil {
					t.Fatalf("fetchReleaseAsset() error = %v", errFetch)
				}
			},
		)
	}
}

func TestResolveReleasesURL(t *testing.T) {
	const public = "https://api.github.com/repos/fixture/panel/releases"
	for _, tc := range []struct{ repo, want string }{
		{"", "https://api.github.com/repos/Pyrokine/Cli-Proxy-API-Management-Center/releases"},
		{" fixture/panel ", public},
		{"https://github.com/fixture/panel.git", public},
		{"http://www.github.com/fixture/panel/releases/latest", public},
		{"https://api.github.com:443/repos/fixture/panel/releases", public},
		{"https://api.github.com/repos/fixture/panel/releases/tags/v1.2.3/", public},
		{
			"https://panel.example:8443/api/v3/repos/fixture/panel/releases/latest?scope=fixture",
			"https://panel.example:8443/api/v3/repos/fixture/panel/releases?scope=fixture",
		},
		{"not-a-repository", ""},
		{"https://api.github.com:8443/repos/fixture/panel/releases", ""},
		{"https://panel.example/other", ""},
		{"http://panel.example/api/v3/repos/fixture/panel/releases", ""},
	} {
		t.Run(
			tc.repo, func(t *testing.T) {
				source, err := ResolveReleasesURL(tc.repo)
				if tc.want == "" {
					if err == nil {
						t.Fatalf("resolved invalid reference to %s", source)
					}
					return
				}
				if err != nil || source.String() != tc.want {
					t.Fatalf("release collection = %v, %v; want %s", source, err, tc.want)
				}
			},
		)
	}
}

func TestReleaseHTTPClientPreservesExplicitPrivateProxy(t *testing.T) {
	source, err := ResolveReleasesURL("fixture/panel")
	if err != nil {
		t.Fatal(err)
	}
	const proxyURL = "http://127.0.0.1:12345"
	client := NewReleaseHTTPClient(proxyURL, source)
	transport, ok := client.Transport.(*http.Transport)
	if !ok || transport.Proxy == nil {
		t.Fatalf("release transport = %T, want configured HTTP proxy", client.Transport)
	}
	proxy, err := transport.Proxy(&http.Request{URL: source})
	if err != nil || proxy.String() != proxyURL {
		t.Fatalf("release proxy = %v, %v; want %s", proxy, err, proxyURL)
	}
}

func TestAssetDownloadChecksEveryDestination(t *testing.T) {
	const cdn = "https://release-assets.githubusercontent.com/github-production-release-asset/123/fixture?sig=example"
	for _, tc := range []struct {
		name, initial, redirect string
		wantRequests            int
		wantErr                 string
	}{
		{"GitHub release and CDN", managementTestAssetURL, cdn, 2, ""},
		{"unsupported first origin", "https://assets.example/management.html", "", 0, "target rejected"},
		{
			"unrelated GitHub path", "https://github.com/fixture/panel/blob/main/management.html", "", 0,
			"target rejected",
		},
		{
			"unsupported redirect", managementTestAssetURL, "https://assets.example/management.html", 1,
			"redirect rejected",
		},
		{
			"HTTP redirect", managementTestAssetURL, "http://release-assets.githubusercontent.com/fixture", 1,
			"redirect rejected",
		},
		{
			"alternate port", managementTestAssetURL, "https://release-assets.githubusercontent.com:8443/fixture", 1,
			"redirect rejected",
		},
	} {
		t.Run(
			tc.name, func(t *testing.T) {
				source, err := ResolveReleasesURL("fixture/panel")
				if err != nil {
					t.Fatal(err)
				}
				calls := 0
				client := &http.Client{
					Transport: roundTripFunc(
						func(req *http.Request) (*http.Response, error) {
							calls++
							if req.Header.Get("Authorization") != "" {
								t.Errorf("asset request unexpectedly authenticated")
							}
							if calls == 1 && tc.redirect != "" {
								resp := managementTestResponse(req, http.StatusFound, "")
								resp.Header.Set("Location", tc.redirect)
								return resp, nil
							}
							return managementTestResponse(req, http.StatusOK, "fixture panel"), nil
						},
					),
				}
				data, _, err := downloadAsset(t.Context(), client, source, tc.initial)
				if tc.wantErr == "" {
					if err != nil || string(data) != "fixture panel" {
						t.Fatalf("download = %q, %v", data, err)
					}
				} else if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v; want %q", err, tc.wantErr)
				}
				if calls != tc.wantRequests {
					t.Fatalf("transport requests = %d, want %d", calls, tc.wantRequests)
				}
			},
		)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}
