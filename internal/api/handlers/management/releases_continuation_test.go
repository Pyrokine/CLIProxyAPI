package management

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/gin-gonic/gin"
)

func TestReleasesContinuationPreservesPaginationAndCompleteCache(t *testing.T) {
	const repo = "synthetic/full-release-continuation"
	t.Setenv("GITHUB_TOKEN", "synthetic-github-token")
	cachedReleases.mu.Lock()
	delete(cachedReleases.entries, repo)
	cachedReleases.mu.Unlock()
	t.Cleanup(
		func() { cachedReleases.mu.Lock(); delete(cachedReleases.entries, repo); cachedReleases.mu.Unlock() },
	)
	previous := http.DefaultTransport
	calls, failSecond := 0, false
	http.DefaultTransport = releaseRoundTripper(
		func(request *http.Request) (*http.Response, error) {
			calls++
			if request.URL.Host != "api.github.com" ||
				request.Header.Get("Authorization") != "Bearer synthetic-github-token" {
				t.Fatalf("unexpected auth destination/header: %s", request.URL.Host)
			}
			start, count := 0, 100
			header := make(http.Header)
			status := 200
			if request.URL.Query().Get("page") == "2" {
				start, count = 100, 23
				if failSecond {
					status = 503
				}
			} else {
				header.Set(
					"Link",
					"<https://api.github.com/repos/"+
						repo+
						("/releases?page=2&per_page=100>; rel=\"next\", "+
							"<https://api.github.com/repos/")+
						repo+
						("/releases?page"+
							"=2&per_page=10"+
							"0>; rel=\"last"+
							"\""),
				)
			}
			items := make([]gitHubRelease, count)
			for i := range items {
				items[i].TagName = fmt.Sprintf("v%d", start+i)
			}
			body, err := json.Marshal(items)
			if err != nil {
				t.Fatal(err)
			}
			return &http.Response{
				StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(string(body))),
				Request: request,
			}, nil
		},
	)
	t.Cleanup(func() { http.DefaultTransport = previous })
	cfg := &config.Config{}
	cfg.RemoteManagement.CPAGitHubRepository = repo
	h := &Handler{cfg: cfg}
	engine := gin.New()
	engine.GET("/releases", h.GetReleases)
	call := func(query string, status int) struct {
		Releases []gitHubRelease `json:"releases"`
		Total    int             `json:"total"`
		Page     int             `json:"page"`
		PerPage  int             `json:"per_page"`
	} {
		t.Helper()
		response := httptest.NewRecorder()
		engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/releases?"+query, nil))
		if response.Code != status {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
		var payload struct {
			Releases []gitHubRelease `json:"releases"`
			Total    int             `json:"total"`
			Page     int             `json:"page"`
			PerPage  int             `json:"per_page"`
		}
		if status == 200 {
			if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
				t.Fatal(err)
			}
		}
		return payload
	}
	page := call("page=11&per_page=10", 200)
	if calls != 2 || page.Total != 123 || page.Page != 11 || page.PerPage != 10 || len(page.Releases) != 10 ||
		page.Releases[0].TagName != "v100" {
		t.Fatalf("full list calls=%d page=%+v", calls, page)
	}
	last := call("page=13&per_page=10", 200)
	if calls != 2 || len(last.Releases) != 3 || last.Releases[0].TagName != "v120" {
		t.Fatalf("cached last page calls=%d data=%+v", calls, last)
	}
	large := call("page="+strconv.Itoa(int(^uint(0)>>1))+"&per_page=100", 200)
	if len(large.Releases) != 0 || calls != 2 {
		t.Fatalf("large page overflowed: %+v calls=%d", large, calls)
	}
	cachedReleases.mu.RLock()
	before := cachedReleases.entries[repo]
	cachedReleases.mu.RUnlock()
	failSecond = true
	call("force=1", 502)
	cachedReleases.mu.RLock()
	after := cachedReleases.entries[repo]
	cachedReleases.mu.RUnlock()
	if len(after.data) != 123 || !after.fetchedAt.Equal(before.fetchedAt) {
		t.Fatalf("partial refresh replaced complete cache: %+v", after)
	}
}

func TestReleasesRejectInvalidAndRepeatedContinuationWithoutCachingPartialResults(t *testing.T) {
	//goland:noinspection HttpUrlsUsage
	for _, tc := range []struct{ name, link string }{
		{"repeat", "<https://api.github.com/repos/synthetic/invalid-continuation/releases?per_page=100>; rel=next"},
		{
			"host",
			"<https://api.github.com.evil.example/repos/synthetic/invalid-continuation/releases?page=2>; rel=next",
		},
		{"scheme", "<http://api.github.com/repos/synthetic/invalid-continuation/releases?page=2>; rel=next"},
		{"userinfo", "<https://user@api.github.com/repos/synthetic/invalid-continuation/releases?page=2>; rel=next"},
		{"port", "<https://api.github.com:444/repos/synthetic/invalid-continuation/releases?page=2>; rel=next"},
		{"repository", "<https://api.github.com/repos/other/repository/releases?page=2>; rel=next"},
		{"fragment", "<?page=2#private>; rel=next"},
		{"invalid-query", "<?page=2&bad=%zz>; rel=next"},
		{"malformed", "not-a-link; rel=next"},
		{"multiple", "<?page=2>; rel=next, <?page=3>; rel=next"},
	} {
		t.Run(
			tc.name, func(t *testing.T) {
				const repo = "synthetic/invalid-continuation"
				cachedReleases.mu.Lock()
				delete(cachedReleases.entries, repo)
				cachedReleases.mu.Unlock()
				previous, calls := http.DefaultTransport, 0
				http.DefaultTransport = releaseRoundTripper(
					func(request *http.Request) (*http.Response, error) {
						calls++
						if calls > 1 {
							t.Fatalf("followed an invalid/repeated continuation to %s", request.URL.Host)
						}
						header := make(http.Header)
						header.Set("Link", tc.link)
						return &http.Response{
							StatusCode: 200, Header: header,
							Body: io.NopCloser(strings.NewReader(`[{"tag_name":"v1"}]`)), Request: request,
						}, nil
					},
				)
				t.Cleanup(func() { http.DefaultTransport = previous })
				cfg := &config.Config{}
				cfg.RemoteManagement.CPAGitHubRepository = repo
				h := &Handler{cfg: cfg}
				response := runConfigListRequest(h.GetReleases, http.MethodGet, "/releases?force=1", "")
				cachedReleases.mu.RLock()
				_, cached := cachedReleases.entries[repo]
				cachedReleases.mu.RUnlock()
				if response.Code != 502 || calls != 1 || cached {
					t.Fatalf(
						"status=%d calls=%d cached=%t body=%s",
						response.Code,
						calls,
						cached,
						response.Body.String(),
					)
				}
			},
		)
	}
}
