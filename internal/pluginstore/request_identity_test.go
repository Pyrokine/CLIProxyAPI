package pluginstore

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestLatestReleaseCacheKeyNormalizesRepository(t *testing.T) {
	t.Parallel()
	client := Client{}
	key, errKey := client.LatestReleaseCacheKey(Plugin{Repository: "https://github.com/Owner/Repo/"})
	if errKey != nil {
		t.Fatal(errKey)
	}
	other, errOther := client.LatestReleaseCacheKey(Plugin{Repository: "https://github.com/owner/repo"})
	if errOther != nil || key != other {
		t.Fatalf("equivalent repository keys differ: %q / %q (%v)", key, other, errOther)
	}
}

func TestLatestReleaseCacheKeyTracksCredentialsAndNetworkScope(t *testing.T) {
	plugin := Plugin{Repository: "https://github.com/owner/repo"}
	client := Client{
		Auth: []AuthConfig{{
			Match:    "https://api.github.com/",
			Type:     AuthTypeGitHubToken,
			TokenEnv: "PLUGIN_STORE_CACHE_TEST_TOKEN",
		}},
	}
	t.Setenv("PLUGIN_STORE_CACHE_TEST_TOKEN", "secret-token-one")
	first, errFirst := client.LatestReleaseCacheKey(plugin)
	if errFirst != nil {
		t.Fatal(errFirst)
	}
	t.Setenv("PLUGIN_STORE_CACHE_TEST_TOKEN", "secret-token-two")
	second, errSecond := client.LatestReleaseCacheKey(plugin)
	if errSecond != nil || first == second {
		t.Fatalf("credential rotation did not change key: %v", errSecond)
	}
	client.NetworkScope = "proxy-b"
	third, errThird := client.LatestReleaseCacheKey(plugin)
	if errThird != nil || second == third {
		t.Fatalf("network scope did not change key: %v", errThird)
	}
	if strings.Contains(first+second+third, "secret-token") {
		t.Fatal("cache keys retain raw credentials")
	}
}

type pluginIdentityDoerFunc func(*http.Request) (*http.Response, error)

func (do pluginIdentityDoerFunc) Do(request *http.Request) (*http.Response, error) {
	return do(request)
}

func TestPrepareLatestReleaseSnapshotsEnvironmentCredentials(t *testing.T) {
	plugin := Plugin{Repository: "https://github.com/owner/repo"}
	client := Client{
		Auth: []AuthConfig{{
			Match:    "https://api.github.com/",
			Type:     AuthTypeGitHubToken,
			TokenEnv: "PLUGIN_STORE_SNAPSHOT_TEST_TOKEN",
		}},
		HTTPClient: pluginIdentityDoerFunc(func(request *http.Request) (*http.Response, error) {
			if got := request.Header.Get("Authorization"); got != "Bearer original-token" {
				t.Errorf("Authorization = %q, want credential snapshot", got)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{"tag_name":"v1.0.0"}`)),
			}, nil
		}),
	}
	t.Setenv("PLUGIN_STORE_SNAPSHOT_TEST_TOKEN", "original-token")
	prepared, originalKey, errPrepare := client.PrepareLatestRelease(plugin)
	if errPrepare != nil {
		t.Fatal(errPrepare)
	}
	t.Setenv("PLUGIN_STORE_SNAPSHOT_TEST_TOKEN", "rotated-token")
	rotatedKey, errKey := client.LatestReleaseCacheKey(plugin)
	if errKey != nil || originalKey == rotatedKey {
		t.Fatalf("credential rotation did not change unprepared identity: %v", errKey)
	}
	if _, errRelease := prepared.FetchLatestRelease(context.Background(), plugin); errRelease != nil {
		t.Fatal(errRelease)
	}
}
