package misc

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func overrideAntigravityVersionURLsForTest(t *testing.T, manifestURL string, latestURL string, gcsURL string) func() {
	t.Helper()

	oldManifestURL := antigravityCLIUpdaterBaseURL
	oldLatestURL := antigravityCLILatestURL
	oldGCSURL := antigravityCLIGCSListURL
	antigravityCLIUpdaterBaseURL = manifestURL
	antigravityCLILatestURL = latestURL
	antigravityCLIGCSListURL = gcsURL

	return func() {
		antigravityCLIUpdaterBaseURL = oldManifestURL
		antigravityCLILatestURL = oldLatestURL
		antigravityCLIGCSListURL = oldGCSURL
	}
}

func overrideAntigravityVersionCacheForTest(t *testing.T, version string, expiry time.Time) func() {
	t.Helper()

	antigravityVersionMu.Lock()
	oldVersion := cachedAntigravityVersion
	oldExpiry := antigravityVersionExpiry
	cachedAntigravityVersion = version
	antigravityVersionExpiry = expiry
	antigravityVersionMu.Unlock()

	return func() {
		antigravityVersionMu.Lock()
		cachedAntigravityVersion = oldVersion
		antigravityVersionExpiry = oldExpiry
		antigravityVersionMu.Unlock()
	}
}

func TestAntigravityLatestVersionUsesCurrentCLIFallback(t *testing.T) {
	restore := overrideAntigravityVersionCacheForTest(t, "", time.Time{})
	defer restore()

	if version := antigravityLatestVersion(); version != "1.0.13" {
		t.Fatalf("AntigravityLatestVersion() = %q, want %q", version, "1.0.13")
	}
}

func TestAntigravityUserAgentUsesAgyCLIFamily(t *testing.T) {
	restore := overrideAntigravityVersionCacheForTest(t, "1.2.3", time.Now().Add(time.Hour))
	defer restore()

	want := "antigravity/cli/1.2.3 (aidev_client; os_type=darwin; arch=arm64)"
	if got := AntigravityUserAgent(); got != want {
		t.Fatalf("AntigravityUserAgent() = %q, want %q", got, want)
	}
	if got := AntigravityLoadCodeAssistUserAgent(""); got != want {
		t.Fatalf("AntigravityLoadCodeAssistUserAgent() = %q, want %q", got, want)
	}
	if got := AntigravityOnboardUserUserAgent(""); got != want+" google-api-nodejs-client/10.3.0" {
		t.Fatalf("AntigravityOnboardUserUserAgent() = %q", got)
	}
}

func TestFetchAntigravityLatestVersionPrefersCLIUpdaterManifest(t *testing.T) {
	var latestRequests atomic.Int32
	var gcsRequests atomic.Int32

	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/manifests/darwin_arm64.json":
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"version":"1.0.9","url":"https://example.test/agy","sha512":"abc"}`))
				case "/latest":
					latestRequests.Add(1)
					_, _ = w.Write([]byte("1.0.8"))
				case "/gcs":
					gcsRequests.Add(1)
					http.Error(w, "unused", http.StatusInternalServerError)
				default:
					http.NotFound(w, r)
				}
			},
		),
	)
	defer server.Close()

	restore := overrideAntigravityVersionURLsForTest(
		t, server.URL+"/manifests", server.URL+"/latest", server.URL+"/gcs",
	)
	defer restore()

	version, errFetch := fetchAntigravityLatestVersion(context.Background())
	if errFetch != nil {
		t.Fatalf("fetchAntigravityLatestVersion() error = %v", errFetch)
	}
	if version != "1.0.9" {
		t.Fatalf("fetchAntigravityLatestVersion() = %q, want %q", version, "1.0.9")
	}
	if got := latestRequests.Load(); got != 0 {
		t.Fatalf("CLI latest requests = %d, want 0", got)
	}
	if got := gcsRequests.Load(); got != 0 {
		t.Fatalf("CLI GCS requests = %d, want 0", got)
	}
}

func TestFetchAntigravityLatestVersionFallsBackToCLILatest(t *testing.T) {
	var gcsRequests atomic.Int32

	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/manifests/darwin_arm64.json":
					http.Error(w, "temporary outage", http.StatusInternalServerError)
				case "/latest":
					_, _ = w.Write([]byte("1.1.0\n"))
				case "/gcs":
					gcsRequests.Add(1)
					http.Error(w, "unused", http.StatusInternalServerError)
				default:
					http.NotFound(w, r)
				}
			},
		),
	)
	defer server.Close()

	restore := overrideAntigravityVersionURLsForTest(
		t, server.URL+"/manifests", server.URL+"/latest", server.URL+"/gcs",
	)
	defer restore()

	version, errFetch := fetchAntigravityLatestVersion(context.Background())
	if errFetch != nil {
		t.Fatalf("fetchAntigravityLatestVersion() error = %v", errFetch)
	}
	if version != "1.1.0" {
		t.Fatalf("fetchAntigravityLatestVersion() = %q, want %q", version, "1.1.0")
	}
	if got := gcsRequests.Load(); got != 0 {
		t.Fatalf("CLI GCS requests = %d, want 0", got)
	}
}

func TestFetchAntigravityLatestVersionFallsBackToCLIGCSList(t *testing.T) {
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/manifests/darwin_arm64.json":
					http.Error(w, "temporary outage", http.StatusInternalServerError)
				case "/latest":
					http.Error(w, "temporary outage", http.StatusInternalServerError)
				case "/gcs":
					w.Header().Set("Content-Type", "application/xml")
					_, _ = w.Write(
						[]byte(`<?xml version='1.0' encoding='UTF-8'?>
<ListBucketResult xmlns='http://doc.s3.amazonaws.com/2006-03-01'>
  <CommonPrefixes><Prefix>antigravity-cli/1.0.8/</Prefix></CommonPrefixes>
  <CommonPrefixes><Prefix>antigravity-cli/1.0.10-6560309696135168/</Prefix></CommonPrefixes>
  <CommonPrefixes><Prefix>antigravity-cli/latest/</Prefix></CommonPrefixes>
</ListBucketResult>`),
					)
				default:
					http.NotFound(w, r)
				}
			},
		),
	)
	defer server.Close()

	restore := overrideAntigravityVersionURLsForTest(
		t, server.URL+"/manifests", server.URL+"/latest", server.URL+"/gcs",
	)
	defer restore()

	version, errFetch := fetchAntigravityLatestVersion(context.Background())
	if errFetch != nil {
		t.Fatalf("fetchAntigravityLatestVersion() error = %v", errFetch)
	}
	if version != "1.0.10" {
		t.Fatalf("fetchAntigravityLatestVersion() = %q, want %q", version, "1.0.10")
	}
}

func TestLatestAntigravityCLIVersionFromPrefixesSortsByNumericSemver(t *testing.T) {
	prefixes := []string{
		"antigravity-cli/1.0.9/",
		"antigravity-cli/1.0.10/",
		"antigravity-cli/1.0.11-6560309696135168/",
		"antigravity-cli/not-a-version/",
	}

	version, errParse := latestAntigravityCLIVersionFromPrefixes(prefixes)
	if errParse != nil {
		t.Fatalf("latestAntigravityCLIVersionFromPrefixes() error = %v", errParse)
	}
	if version != "1.0.11" {
		t.Fatalf("latestAntigravityCLIVersionFromPrefixes() = %q, want %q", version, "1.0.11")
	}
}

func TestAntigravityCLIVersionFromPrefixIgnoresNonVersionNames(t *testing.T) {
	for _, prefix := range []string{
		"antigravity-cli/latest/",
		"antigravity-cli/test/",
		"antigravity-cli/tools/",
		"antigravity-cli/v1/",
		"antigravity-hub/2.1.0-6066040229199872/",
	} {
		t.Run(
			strings.Trim(prefix, "/"), func(t *testing.T) {
				if got, ok := antigravityCLIVersionFromPrefix(prefix); ok || got != "" {
					t.Fatalf("antigravityCLIVersionFromPrefix(%q) = %q, %v; want empty false", prefix, got, ok)
				}
			},
		)
	}
}
