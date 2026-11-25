package managementasset

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func setManagementTestTransport(t *testing.T, transport http.RoundTripper) {
	t.Helper()
	t.Setenv("GITHUB_TOKEN", "")
	previous := http.DefaultTransport
	http.DefaultTransport = transport
	t.Cleanup(func() { http.DefaultTransport = previous })
}

func managementTestResponse(request *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: request,
	}
}

func managementTestRelease(data, assetURL, version string) string {
	if version == "" {
		version = "v1"
	}
	return fmt.Sprintf(
		`{"tag_name":%q,"assets":[{"name":"management.html","browser_download_url":%q,"digest":"sha256:%x"}]}`,
		version, assetURL, sha256.Sum256([]byte(data)),
	)
}

func TestUpdateManagementHTMLReportsFailureWhileEnsureKeepsAvailableAsset(t *testing.T) {
	for _, tc := range []struct{ name, expected string }{
		{"release", "unexpected release status 503"},
		{"download", "unexpected download status 503"},
		{"missing digest", "has no digest"},
		{"invalid digest", "invalid management asset SHA-256 digest"},
		{"wrong digest algorithm", "digest is not SHA-256"},
		{"wrong digest", "digest mismatch"},
		{"empty download", "download is empty"},
		{"asset target", "download target rejected"},
		{"asset redirect", "asset redirect rejected"},
		{"metadata redirect", "release redirect rejected"},
	} {
		t.Run(
			tc.name, func(t *testing.T) {
				dir := t.TempDir()
				path := filepath.Join(dir, managementAssetName)
				if err := os.WriteFile(path, []byte("previous panel"), 0o600); err != nil {
					t.Fatal(err)
				}
				setManagementTestTransport(
					t, roundTripFunc(
						func(request *http.Request) (*http.Response, error) {
							switch request.URL.Hostname() {
							case "api.github.com":
								body := managementTestRelease("new panel", managementTestAssetURL, "")
								switch tc.name {
								case "release":
									return managementTestResponse(
										request, http.StatusServiceUnavailable, "fixture release failure",
									), nil
								case "metadata redirect":
									resp := managementTestResponse(request, http.StatusFound, "")
									resp.Header.Set("Location", "https://releases.example/fixture")
									return resp, nil
								case "missing digest":
									body = `{"assets":[{"name":"management.html"}]}`
								case "invalid digest":
									body = `{"assets":[{"name":"management.html","digest":"sha256:short"}]}`
								case "wrong digest algorithm":
									body = strings.Replace(body, "sha256:", "sha512:", 1)
								case "wrong digest":
									body = managementTestRelease("different panel", managementTestAssetURL, "")
								case "asset target":
									body = managementTestRelease(
										"new panel", "https://assets.example/management.html", "",
									)
								}
								return managementTestResponse(request, http.StatusOK, body), nil
							case "github.com":
								switch tc.name {
								case "download":
									return managementTestResponse(
										request, http.StatusServiceUnavailable, "fixture download failure",
									), nil
								case "asset redirect":
									resp := managementTestResponse(request, http.StatusFound, "")
									resp.Header.Set("Location", "https://assets.example/management.html")
									return resp, nil
								case "empty download":
									return managementTestResponse(request, http.StatusOK, ""), nil
								}
								return managementTestResponse(request, http.StatusOK, "new panel"), nil
							default:
								return nil, fmt.Errorf("unexpected fixture host %q", request.URL.Hostname())
							}
						},
					),
				)
				if err := updateManagementHTML(
					t.Context(), dir, "", "", "",
				); err == nil || !strings.Contains(err.Error(), tc.expected) {
					t.Fatalf("update error=%v, want %q", err, tc.expected)
				}
				if !EnsureManagementHTML(t.Context(), dir, "", "", "") {
					t.Fatal("existing valid panel became unavailable after update failure")
				}
				if data, err := os.ReadFile(path); err != nil || string(data) != "previous panel" {
					t.Fatalf("failed update changed existing panel: data=%q err=%v", data, err)
				}
			},
		)
	}
}

func TestUpdateManagementHTMLLatestBypassesAutomaticThrottle(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, managementAssetName)
	if err := os.WriteFile(path, []byte("panel 1"), 0o600); err != nil {
		t.Fatal(err)
	}
	var current, releaseRequests atomic.Int32
	current.Store(1)
	setManagementTestTransport(
		t, roundTripFunc(
			func(request *http.Request) (*http.Response, error) {
				data := fmt.Sprintf("panel %d", current.Load())
				if request.URL.Hostname() == "api.github.com" {
					releaseRequests.Add(1)
					return managementTestResponse(
						request,
						http.StatusOK,
						managementTestRelease(data, managementTestAssetURL, ""),
					), nil
				}
				if request.URL.Hostname() != "github.com" && request.URL.Hostname() != "release-assets.githubusercontent.com" {
					return nil, fmt.Errorf("unexpected fixture host %q", request.URL.Hostname())
				}
				return managementTestResponse(request, http.StatusOK, data), nil
			},
		),
	)
	if !EnsureManagementHTML(t.Context(), dir, "", "", "") {
		t.Fatal("initial automatic check lost available panel")
	}
	current.Store(2)
	if err := updateManagementHTML(t.Context(), dir, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if releaseRequests.Load() != 2 {
		t.Fatalf("manual latest request was throttled: requests=%d", releaseRequests.Load())
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "panel 2" {
		t.Fatalf("latest update was not published: data=%q err=%v", data, err)
	}
}

func TestUpdateManagementHTMLSeparatesConcurrentTargets(t *testing.T) {
	for _, tc := range []struct{ name, firstRepo, secondRepo, firstVersion, secondVersion string }{
		{"versions", "https://github.com/fixture/panel", "https://github.com/fixture/panel", "v1", "v2"},
		{"repositories", "https://github.com/fixture/first", "https://github.com/fixture/second", "v1", "v1"},
	} {
		t.Run(
			tc.name, func(t *testing.T) {
				dir := t.TempDir()
				started, releaseFirst := make(chan struct{}), make(chan struct{})
				firstDone := make(chan error, 1)
				var requests atomic.Int32
				setManagementTestTransport(
					t, roundTripFunc(
						func(request *http.Request) (*http.Response, error) {
							if request.URL.Hostname() == "api.github.com" {
								index := requests.Add(1)
								if index == 1 {
									close(started)
									select {
									case <-releaseFirst:
									case <-request.Context().Done():
										return nil, request.Context().Err()
									}
								}
								assetURL := fmt.Sprintf(
									"https://release-assets.githubusercontent.com/fixture/%d", index,
								)
								return managementTestResponse(
									request, http.StatusOK,
									managementTestRelease(assetURL, assetURL, path.Base(request.URL.Path)),
								), nil
							}
							if request.URL.Hostname() != "github.com" && request.URL.Hostname() != "release-assets.githubusercontent.com" {
								return nil, fmt.Errorf("unexpected fixture host %q", request.URL.Hostname())
							}
							return managementTestResponse(request, http.StatusOK, request.URL.String()), nil
						},
					),
				)
				go func() { firstDone <- updateManagementHTML(t.Context(), dir, "", tc.firstRepo, tc.firstVersion) }()
				defer func() {
					close(releaseFirst)
					if err := <-firstDone; err != nil {
						t.Error(err)
					}
				}()
				<-started
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				if err := updateManagementHTML(ctx, dir, "", tc.secondRepo, tc.secondVersion); err != nil {
					t.Fatalf("different targets shared blocked work: %v", err)
				}
				if requests.Load() != 2 {
					t.Fatalf("different targets coalesced: requests=%d", requests.Load())
				}
				if data, err := os.ReadFile(
					filepath.Join(
						dir, managementAssetName,
					),
				); err != nil || string(data) != "https://release-assets.githubusercontent.com/fixture/2" {
					t.Fatalf("second target was not independently published: data=%q err=%v", data, err)
				}
			},
		)
	}
}

type observedDoneContext struct {
	context.Context
	observed chan struct{}
	once     sync.Once
}

func (ctx *observedDoneContext) Done() <-chan struct{} {
	ctx.once.Do(func() { close(ctx.observed) })
	return ctx.Context.Done()
}

func TestUpdateManagementHTMLCanceledWaiterKeepsSharedDownloadAlive(t *testing.T) {
	dir := t.TempDir()
	started, release := make(chan struct{}), make(chan struct{})
	firstDone := make(chan error, 1)
	setManagementTestTransport(
		t, roundTripFunc(
			func(request *http.Request) (*http.Response, error) {
				if request.URL.Hostname() == "api.github.com" {
					close(started)
					select {
					case <-release:
					case <-request.Context().Done():
						return nil, request.Context().Err()
					}
					return managementTestResponse(
						request,
						http.StatusOK,
						managementTestRelease("panel", managementTestAssetURL, ""),
					), nil
				}
				if request.URL.Hostname() != "github.com" && request.URL.Hostname() != "release-assets.githubusercontent.com" {
					return nil, fmt.Errorf("unexpected fixture host %q", request.URL.Hostname())
				}
				return managementTestResponse(request, http.StatusOK, "panel"), nil
			},
		),
	)
	go func() { firstDone <- updateManagementHTML(t.Context(), dir, "", "", "") }()
	defer func() {
		close(release)
		if err := <-firstDone; err != nil {
			t.Error(err)
		}
	}()
	<-started
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	waiter := &observedDoneContext{Context: ctx, observed: make(chan struct{})}
	waiterDone := make(chan error, 1)
	go func() { waiterDone <- updateManagementHTML(waiter, dir, "", "", "") }()
	<-waiter.observed
	cancel()
	select {
	case err := <-waiterDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled waiter error=%v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled waiter remained blocked on shared download")
	}
}
