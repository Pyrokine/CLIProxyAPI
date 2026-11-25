package managementasset

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadManagementHTMLStateUsesDiskAndTargetWithoutPublishing(t *testing.T) {
	for _, scenario := range []struct {
		name          string
		previous      string
		version       string
		missing       bool
		blockedTarget bool
		canceled      bool
		releaseError  bool
		missingDigest bool
		matches       bool
		wantError     string
	}{
		{name: "latest already installed", previous: "panel", matches: true},
		{name: "old installed panel", previous: "old panel"},
		{name: "explicit installed version", previous: "panel", version: "v2", matches: true},
		{name: "missing asset directory", missing: true},
		{name: "release unavailable", previous: "panel", releaseError: true, wantError: "fetch management release"},
		{name: "release has no digest", previous: "panel", missingDigest: true, wantError: "has no digest"},
		{name: "local asset unreadable", blockedTarget: true, wantError: "read management asset"},
		{name: "canceled read", previous: "panel", canceled: true, wantError: "context canceled"},
	} {
		t.Run(
			scenario.name, func(t *testing.T) {
				dir := filepath.Join(t.TempDir(), "static")
				path := filepath.Join(dir, managementAssetName)
				if !scenario.missing {
					if err := os.Mkdir(dir, 0o700); err != nil {
						t.Fatal(err)
					}
					if scenario.blockedTarget {
						if err := os.Mkdir(path, 0o700); err != nil {
							t.Fatal(err)
						}
					} else if err := os.WriteFile(path, []byte(scenario.previous), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				requests := 0
				setManagementTestTransport(
					t, roundTripFunc(
						func(request *http.Request) (*http.Response, error) {
							requests++
							if request.URL.Hostname() != "api.github.com" {
								t.Errorf("readback downloaded an asset: %s", request.URL.Hostname())
								return nil, fmt.Errorf("unexpected fixture host %q", request.URL.Hostname())
							}
							wantSuffix := "/releases/latest"
							if scenario.version != "" {
								wantSuffix = "/releases/tags/" + scenario.version
							}
							if !strings.HasSuffix(request.URL.Path, wantSuffix) {
								t.Errorf("target path = %q, want suffix %q", request.URL.Path, wantSuffix)
							}
							if scenario.releaseError {
								return managementTestResponse(
									request, http.StatusServiceUnavailable, "fixture release error",
								), nil
							}
							body := managementTestRelease("panel", managementTestAssetURL, scenario.version)
							if scenario.missingDigest {
								body = `{"assets":[{"name":"management.html","browser_download_url":"https://` +
									`assets.example/management.html"}]}`
							}
							return managementTestResponse(request, http.StatusOK, body), nil
						},
					),
				)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				if scenario.canceled {
					cancel()
				}
				state, err := ReadManagementHTMLState(ctx, dir, "", "", scenario.version)
				if scenario.wantError != "" {
					if err == nil || !strings.Contains(
						err.Error(), scenario.wantError,
					) || state.Installed || state.MatchesTarget {
						t.Fatalf("state=%+v, error=%v, want %q", state, err, scenario.wantError)
					}
				} else {
					wantDigest := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(scenario.previous)))
					if scenario.missing {
						wantDigest = ""
					}
					wantTarget := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte("panel")))
					if err !=
						nil ||
						state.Installed ==
							scenario.missing ||
						state.MatchesTarget !=
							scenario.matches ||
						state.Digest !=
							wantDigest ||
						state.TargetDigest !=
							wantTarget {
						t.Fatalf(
							"state=%+v, error=%v; want installed=%t, matches=%t, digest=%q, target=%q", state, err,
							!scenario.missing, scenario.matches, wantDigest, wantTarget,
						)
					}
				}
				wantRequests := 1
				if scenario.canceled {
					wantRequests = 0
				}
				if requests != wantRequests {
					t.Fatalf("requests=%d, want %d", requests, wantRequests)
				}
				if scenario.missing {
					if _, err := os.Stat(dir); !os.IsNotExist(err) {
						t.Fatalf("readback created the missing directory: %v", err)
					}
				} else if !scenario.blockedTarget {
					if data, err := os.ReadFile(path); err != nil || string(data) != scenario.previous {
						t.Fatalf("readback changed the installed file: data=%q, error=%v", data, err)
					}
				}
			},
		)
	}
}
