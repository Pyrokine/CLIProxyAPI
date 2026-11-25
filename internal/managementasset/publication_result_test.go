package managementasset

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestUpdateManagementHTMLResultConfirmsActualPublication(t *testing.T) {
	for _, scenario := range []struct {
		name           string
		previous       string
		version        string
		wrongTag       bool
		downloadStatus int
		blockedTarget  bool
		wantPublished  bool
		wantDownloads  int32
		wantError      string
	}{
		{
			name: "changed latest", previous: "old panel", downloadStatus: http.StatusOK, wantPublished: true,
			wantDownloads: 1,
		},
		{name: "latest already current", previous: "panel", downloadStatus: http.StatusOK},
		{
			name: "explicit version same digest", previous: "panel", version: "v2", downloadStatus: http.StatusOK,
			wantPublished: true, wantDownloads: 1,
		},
		{
			name: "metadata tag does not match", previous: "old panel", version: "v2", wrongTag: true,
			wantError: "tag does not match",
		},
		{
			name: "failed download preserves old panel", previous: "old panel",
			downloadStatus: http.StatusServiceUnavailable, wantDownloads: 1, wantError: "download management asset",
		},
		{
			name: "failed publication", downloadStatus: http.StatusOK, blockedTarget: true, wantDownloads: 1,
			wantError: "publish management asset",
		},
	} {
		t.Run(
			scenario.name, func(t *testing.T) {
				dir := t.TempDir()
				path := filepath.Join(dir, managementAssetName)
				if scenario.blockedTarget {
					if err := os.Mkdir(path, 0o700); err != nil {
						t.Fatal(err)
					}
				} else if err := os.WriteFile(path, []byte(scenario.previous), 0o600); err != nil {
					t.Fatal(err)
				}
				var downloads atomic.Int32
				setManagementTestTransport(
					t, roundTripFunc(
						func(request *http.Request) (*http.Response, error) {
							switch request.URL.Hostname() {
							case "api.github.com":
								version := scenario.version
								if scenario.wrongTag {
									version = "v1"
								}
								return managementTestResponse(
									request, http.StatusOK,
									managementTestRelease("panel", managementTestAssetURL, version),
								), nil
							case "github.com":
								downloads.Add(1)
								return managementTestResponse(request, scenario.downloadStatus, "panel"), nil
							default:
								return nil, fmt.Errorf("unexpected fixture host %q", request.URL.Hostname())
							}
						},
					),
				)
				published, err := UpdateManagementHTMLWithResult(t.Context(), dir, "", "", scenario.version)
				if published != scenario.wantPublished {
					t.Fatalf("published = %t, want %t; error = %v", published, scenario.wantPublished, err)
				}
				if scenario.wantError == "" && err != nil ||
					scenario.wantError != "" && (err == nil || !strings.Contains(
						err.Error(), scenario.wantError,
					)) {
					t.Fatalf("error = %v, want %q", err, scenario.wantError)
				}
				if downloads.Load() != scenario.wantDownloads {
					t.Fatalf("downloads = %d, want %d", downloads.Load(), scenario.wantDownloads)
				}
				if scenario.blockedTarget {
					if info, err := os.Stat(path); err != nil || !info.IsDir() {
						t.Fatalf("failed publication changed the blocked target: %v, %v", info, err)
					}
					return
				}
				want := "panel"
				if err != nil {
					want = scenario.previous
				}
				if data, err := os.ReadFile(path); err != nil || string(data) != want {
					t.Fatalf("published file = %q, error = %v, want %q", data, err, want)
				}
			},
		)
	}
}

func TestUpdateManagementHTMLResultRejectsCanceledRequest(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var requests atomic.Int32
	setManagementTestTransport(
		t, roundTripFunc(
			func(*http.Request) (*http.Response, error) {
				requests.Add(1)
				return nil, errors.New("canceled update reached transport")
			},
		),
	)
	published, err := UpdateManagementHTMLWithResult(ctx, t.TempDir(), "", "", "")
	if published || !errors.Is(err, context.Canceled) || requests.Load() != 0 {
		t.Fatalf("canceled update: published=%t, error=%v, requests=%d", published, err, requests.Load())
	}
}
