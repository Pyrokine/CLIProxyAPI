package management

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/pluginstore"
	"github.com/gin-gonic/gin"
)

type pluginReleaseDoerFunc func(*http.Request) (*http.Response, error)

func (do pluginReleaseDoerFunc) Do(request *http.Request) (*http.Response, error) {
	return do(request)
}

func pluginReleaseResponse(tag string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(`{"tag_name":"` + tag + `"}`)),
	}
}

func TestPluginReleaseCacheRetainsSuccessAndUsesCompletionTime(t *testing.T) {
	started := make(chan struct{})
	releaseRequest := make(chan struct{})
	var calls atomic.Int32
	client := pluginstore.Client{HTTPClient: pluginReleaseDoerFunc(func(*http.Request) (*http.Response, error) {
		call := calls.Add(1)
		if call == 1 {
			close(started)
			<-releaseRequest
			return pluginReleaseResponse("v1.0.0"), nil
		}
		return nil, errors.New("offline")
	})}
	h := &Handler{pluginReleaseCache: make(map[string]pluginReleaseCacheEntry)}
	plugin := pluginstore.Plugin{Repository: "https://github.com/test/plugin"}
	begin := time.Now()
	result := make(chan string, 1)
	go func() {
		result <- h.latestPluginVersion(context.Background(), client, plugin)
	}()
	<-started
	time.Sleep(50 * time.Millisecond)
	close(releaseRequest)
	if got := <-result; got != "1.0.0" {
		t.Fatalf("version = %q, want 1.0.0", got)
	}
	key := pluginReleaseKey(client, plugin)
	h.pluginReleaseCacheMu.Lock()
	entry := h.pluginReleaseCache[key]
	h.pluginReleaseCacheMu.Unlock()
	if !entry.nextCheckAt.After(begin.Add(pluginReleaseCacheTTL + 25*time.Millisecond)) {
		t.Fatalf("success TTL started before lookup completed: next check = %s", entry.nextCheckAt)
	}

	h.pluginReleaseCacheMu.Lock()
	entry.nextCheckAt = time.Now().Add(-time.Second)
	h.pluginReleaseCache[key] = entry
	h.pluginReleaseCacheMu.Unlock()
	failureStarted := time.Now()
	if got := h.latestPluginVersion(context.Background(), client, plugin); got != "1.0.0" {
		t.Fatalf("failed refresh discarded last success: %q", got)
	}
	h.pluginReleaseCacheMu.Lock()
	entry = h.pluginReleaseCache[key]
	h.pluginReleaseCacheMu.Unlock()
	if entry.version != "1.0.0" || entry.nextCheckAt.Before(failureStarted.Add(pluginReleaseFailureCacheTTL-time.Second)) {
		t.Fatalf("failure cache entry = %#v", entry)
	}
	if got := h.latestPluginVersion(context.Background(), client, plugin); got != "1.0.0" || calls.Load() != 2 {
		t.Fatalf("failure TTL was not reused: version=%q calls=%d", got, calls.Load())
	}
}

type pluginReleaseWaitContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (ctx *pluginReleaseWaitContext) Done() <-chan struct{} {
	ctx.once.Do(func() { close(ctx.waiting) })
	return ctx.Context.Done()
}

func TestPluginReleaseCacheCoalescesConcurrentLookups(t *testing.T) {
	started := make(chan struct{})
	releaseRequest := make(chan struct{})
	var calls atomic.Int32
	client := pluginstore.Client{HTTPClient: pluginReleaseDoerFunc(func(*http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-releaseRequest
		return pluginReleaseResponse("v1.0.0"), nil
	})}
	h := &Handler{}
	plugin := pluginstore.Plugin{Repository: "https://github.com/test/plugin"}
	results := make(chan string, 21)
	go func() { results <- h.latestPluginVersion(context.Background(), client, plugin) }()
	<-started
	for range 20 {
		waitContext := &pluginReleaseWaitContext{Context: context.Background(), waiting: make(chan struct{})}
		go func() { results <- h.latestPluginVersion(waitContext, client, plugin) }()
		<-waitContext.waiting
	}
	close(releaseRequest)
	for range 21 {
		if got := <-results; got != "1.0.0" {
			t.Fatalf("version = %q", got)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1", calls.Load())
	}
}

func TestPluginReleaseCacheCanceledLeaderHandsOff(t *testing.T) {
	started := make(chan struct{})
	var calls atomic.Int32
	client := pluginstore.Client{HTTPClient: pluginReleaseDoerFunc(func(request *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			close(started)
			<-request.Context().Done()
			return nil, request.Context().Err()
		}
		return pluginReleaseResponse("v1.0.0"), nil
	})}
	h := &Handler{}
	plugin := pluginstore.Plugin{Repository: "https://github.com/test/plugin"}
	ctx, cancel := context.WithCancel(context.Background())
	leaderResult := make(chan string, 1)
	go func() { leaderResult <- h.latestPluginVersion(ctx, client, plugin) }()
	<-started
	waitContext := &pluginReleaseWaitContext{Context: context.Background(), waiting: make(chan struct{})}
	followerResult := make(chan string, 1)
	go func() { followerResult <- h.latestPluginVersion(waitContext, client, plugin) }()
	<-waitContext.waiting
	cancel()
	if got := <-leaderResult; got != "" {
		t.Fatalf("canceled leader version = %q", got)
	}
	if got := <-followerResult; got != "1.0.0" || calls.Load() != 2 {
		t.Fatalf("handoff version=%q calls=%d", got, calls.Load())
	}
}

func TestPluginReleaseCacheSharesTwoSlotsAndDoesNotCacheCanceledQueue(t *testing.T) {
	started := make(chan struct{}, 3)
	releaseRequests := make(chan struct{})
	var calls atomic.Int32
	client := pluginstore.Client{HTTPClient: pluginReleaseDoerFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		started <- struct{}{}
		<-releaseRequests
		return pluginReleaseResponse("v1.0.0"), nil
	})}
	h := &Handler{}
	results := make(chan string, pluginReleaseLookupConcurrency)
	for index := range pluginReleaseLookupConcurrency {
		go func() {
			plugin := pluginstore.Plugin{Repository: fmt.Sprintf("https://github.com/test/plugin-%d", index)}
			results <- h.latestPluginVersion(context.Background(), client, plugin)
		}()
	}
	for range pluginReleaseLookupConcurrency {
		<-started
	}
	ctx, cancel := context.WithCancel(context.Background())
	waitContext := &pluginReleaseWaitContext{Context: ctx, waiting: make(chan struct{})}
	queuedPlugin := pluginstore.Plugin{Repository: "https://github.com/test/queued"}
	queuedResult := make(chan string, 1)
	go func() { queuedResult <- h.latestPluginVersion(waitContext, client, queuedPlugin) }()
	<-waitContext.waiting
	cancel()
	if got := <-queuedResult; got != "" || calls.Load() != pluginReleaseLookupConcurrency {
		t.Fatalf("queued version=%q calls=%d", got, calls.Load())
	}
	close(releaseRequests)
	for range pluginReleaseLookupConcurrency {
		<-results
	}
	if got := h.latestPluginVersion(context.Background(), client, queuedPlugin); got != "1.0.0" ||
		calls.Load() != pluginReleaseLookupConcurrency+1 {
		t.Fatalf("canceled lookup was cached: version=%q calls=%d", got, calls.Load())
	}
}

func TestPluginReleaseCacheSeparatesCredentialsAndNetworkScope(t *testing.T) {
	var calls atomic.Int32
	client := pluginstore.Client{HTTPClient: pluginReleaseDoerFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return pluginReleaseResponse("v1.0.0"), nil
	})}
	h := &Handler{}
	plugin := pluginstore.Plugin{Repository: "https://github.com/test/plugin"}
	if got := h.latestPluginVersion(context.Background(), client, plugin); got != "1.0.0" {
		t.Fatalf("unauthenticated version = %q", got)
	}
	client.Auth = []pluginstore.AuthConfig{{
		Match: "https://api.github.com/", Type: pluginstore.AuthTypeGitHubToken,
		TokenEnv: "PLUGIN_RELEASE_CACHE_TOKEN",
	}}
	for _, token := range []string{"token-a", "token-b", "token-a"} {
		t.Setenv("PLUGIN_RELEASE_CACHE_TOKEN", token)
		if got := h.latestPluginVersion(context.Background(), client, plugin); got != "1.0.0" {
			t.Fatalf("authenticated version = %q", got)
		}
	}
	if calls.Load() != 3 {
		t.Fatalf("credential identities made %d calls, want 3", calls.Load())
	}
	client.NetworkScope = "other-proxy"
	h.latestPluginVersion(context.Background(), client, plugin)
	if calls.Load() != 4 {
		t.Fatalf("network scopes made %d calls, want 4", calls.Load())
	}
	for key := range h.pluginReleaseCache {
		if strings.Contains(key, "token-") {
			t.Fatalf("cache key contains credential: %q", key)
		}
	}
}

func TestListPluginStoreRefreshesInstalledMatchingSourceOnly(t *testing.T) {
	gin.SetMode(gin.TestMode)
	pluginsDir := t.TempDir()
	archDir := filepath.Join(pluginsDir, runtime.GOOS, runtime.GOARCH)
	if errMkdir := os.MkdirAll(archDir, 0o755); errMkdir != nil {
		t.Fatal(errMkdir)
	}
	pluginPath := filepath.Join(archDir, "sample-provider-v1.0.0"+testPluginExtension())
	if errWrite := os.WriteFile(pluginPath, []byte("plugin"), 0o755); errWrite != nil {
		t.Fatal(errWrite)
	}
	customURL := "https://registry.example/registry.json"
	defaultRegistry := pluginStoreReleaseRegistryJSON("https://github.com/owner/default-plugin", "1.0.0")
	customRegistry := pluginStoreReleaseRegistryJSON("https://github.com/owner/custom-plugin", "1.0.0")
	var defaultCalls atomic.Int32
	var customCalls atomic.Int32
	h := &Handler{
		cfg: &config.Config{Plugins: config.PluginsConfig{
			Dir:          pluginsDir,
			StoreSources: []string{customURL},
			Configs: map[string]config.PluginInstanceConfig{
				"sample-provider": pluginConfigFromYAML(
					t,
					"enabled: true\nstore:\n  id: sample-provider\n  source-id: "+
						pluginstore.DefaultSourceID+"\n  source-url: "+pluginstore.DefaultRegistryURL+"\n",
				),
			},
		}},
		pluginStoreHTTPClient: pluginReleaseDoerFunc(func(request *http.Request) (*http.Response, error) {
			var body string
			switch request.URL.String() {
			case pluginstore.DefaultRegistryURL:
				body = defaultRegistry
			case customURL:
				body = customRegistry
			case "https://api.github.com/repos/owner/default-plugin/releases/latest":
				defaultCalls.Add(1)
				body = `{"tag_name":"v2.0.0"}`
			case "https://api.github.com/repos/owner/custom-plugin/releases/latest":
				customCalls.Add(1)
				body = `{"tag_name":"v3.0.0"}`
			default:
				t.Fatalf("unexpected URL %s", request.URL)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		}),
	}
	response := listPluginStoreReleaseTest(t, h)
	if len(response.Plugins) != 2 || response.Plugins[0].Version != "2.0.0" ||
		response.Plugins[1].Version != "1.0.0" {
		t.Fatalf("plugins = %#v", response.Plugins)
	}
	if defaultCalls.Load() != 1 || customCalls.Load() != 0 {
		t.Fatalf("release calls = default:%d custom:%d", defaultCalls.Load(), customCalls.Load())
	}
}

func TestListPluginStoreUsesCachedVersionWithoutRefreshingUninstalled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	registry := pluginStoreReleaseRegistryJSON("https://github.com/owner/plugin", "1.0.0")
	var releaseCalls atomic.Int32
	h := &Handler{
		cfg: &config.Config{Plugins: config.PluginsConfig{Dir: t.TempDir()}},
		pluginStoreHTTPClient: pluginReleaseDoerFunc(func(request *http.Request) (*http.Response, error) {
			if request.URL.String() != pluginstore.DefaultRegistryURL {
				releaseCalls.Add(1)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(registry)),
			}, nil
		}),
		pluginReleaseCache: make(map[string]pluginReleaseCacheEntry),
	}
	plugin := pluginstore.Plugin{Repository: "https://github.com/owner/plugin"}
	key := pluginReleaseKey(h.newPluginStoreClient("", "", nil), plugin)
	h.pluginReleaseCache[key] = pluginReleaseCacheEntry{version: "2.0.0"}
	response := listPluginStoreReleaseTest(t, h)
	if len(response.Plugins) != 1 || response.Plugins[0].Version != "2.0.0" || releaseCalls.Load() != 0 {
		t.Fatalf("plugins=%#v release calls=%d", response.Plugins, releaseCalls.Load())
	}
}

func TestPluginStoreRateLimitResponseAndTagTraversal(t *testing.T) {
	gin.SetMode(gin.TestMode)
	retryAt := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	if !writePluginStoreRateLimit(
		ctx,
		&pluginstore.RateLimitError{StatusCode: http.StatusTooManyRequests, RetryAt: retryAt},
	) {
		t.Fatal("rate limit was not handled")
	}
	if recorder.Code != http.StatusTooManyRequests || recorder.Header().Get("Retry-After") == "" ||
		!strings.Contains(recorder.Body.String(), `"retry_at"`) {
		t.Fatalf("status=%d headers=%v body=%s", recorder.Code, recorder.Header(), recorder.Body.String())
	}

	calls := 0
	client := pluginstore.Client{HTTPClient: pluginReleaseDoerFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, &pluginstore.RateLimitError{
			StatusCode: http.StatusTooManyRequests,
			RetryAt:    retryAt,
		}
	})}
	_, errPrepare := preparePluginStoreGitHubRelease(
		context.Background(),
		client,
		pluginstore.Plugin{
			ID: "sample-provider", Name: "Sample", Description: "test", Author: "owner",
			Repository: "https://github.com/owner/repo",
		},
		"1.0.0",
		pluginstore.InstallOptions{PluginsDir: t.TempDir(), GOOS: "linux", GOARCH: "amd64"},
	)
	var rateLimit *pluginstore.RateLimitError
	if !errors.As(errPrepare, &rateLimit) || calls != 1 {
		t.Fatalf("error=%v calls=%d", errPrepare, calls)
	}
}

func TestListPluginStorePropagatesRegistryRateLimit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &Handler{
		cfg:                    &config.Config{ProxyURL: t.Name(), Plugins: config.PluginsConfig{Dir: t.TempDir()}},
		pluginStoreRegistryURL: "https://api.github.com/repos/owner/store/contents/registry.json",
		pluginStoreHTTPClient: pluginReleaseDoerFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusTooManyRequests,
				Header:     http.Header{"Retry-After": []string{"3600"}},
				Body:       io.NopCloser(strings.NewReader("limited")),
			}, nil
		}),
	}
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/plugin-store", nil)
	h.ListPluginStore(ctx)
	if recorder.Code != http.StatusTooManyRequests || recorder.Header().Get("Retry-After") == "" {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestManagementPluginClientSharesCooldownByCredentialAndScope(t *testing.T) {
	t.Setenv("PLUGIN_MANAGEMENT_COOLDOWN_TOKEN", t.Name())
	auth := []pluginstore.AuthConfig{{
		Match: "https://api.github.com/", Type: pluginstore.AuthTypeGitHubToken,
		TokenEnv: "PLUGIN_MANAGEMENT_COOLDOWN_TOKEN",
	}}
	scope := "scope-" + t.Name()
	seedCalls := 0
	seed := pluginstore.Client{
		NetworkScope: scope,
		Auth:         auth,
		HTTPClient: pluginReleaseDoerFunc(func(*http.Request) (*http.Response, error) {
			seedCalls++
			return &http.Response{
				StatusCode: http.StatusTooManyRequests,
				Header:     http.Header{"Retry-After": []string{"3600"}},
				Body:       io.NopCloser(strings.NewReader("limited")),
			}, nil
		}),
	}
	plugin := pluginstore.Plugin{Repository: "https://github.com/owner/seed"}
	_, _ = seed.FetchLatestRelease(context.Background(), plugin)
	managementCalls := 0
	h := &Handler{pluginStoreHTTPClient: pluginReleaseDoerFunc(func(*http.Request) (*http.Response, error) {
		managementCalls++
		return pluginReleaseResponse("v1.0.0"), nil
	})}
	client := h.newPluginStoreClient(scope, "", auth)
	_, errRelease := client.FetchLatestRelease(
		context.Background(),
		pluginstore.Plugin{Repository: "https://github.com/owner/management"},
	)
	var rateLimit *pluginstore.RateLimitError
	if !errors.As(errRelease, &rateLimit) || seedCalls != 1 || managementCalls != 0 {
		t.Fatalf("error=%v seed=%d management=%d", errRelease, seedCalls, managementCalls)
	}
}

func listPluginStoreReleaseTest(t *testing.T, h *Handler) pluginStoreListResponse {
	t.Helper()
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/plugin-store", nil)
	h.ListPluginStore(ctx)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response pluginStoreListResponse
	if errDecode := json.Unmarshal(recorder.Body.Bytes(), &response); errDecode != nil {
		t.Fatal(errDecode)
	}
	return response
}

func pluginStoreReleaseRegistryJSON(repository, version string) string {
	return fmt.Sprintf(
		`{"schema_version":2,"plugins":[{"id":"sample-provider","name":"Sample Provider","description":"Sample","author":"owner","version":%q,"repository":%q,"install":{"type":"github-release"}}]}`,
		version,
		repository,
	)
}

func TestPluginStoreRateLimitRetryAfterIsPositive(t *testing.T) {
	retryAt := time.Now().Add(time.Second + 100*time.Millisecond)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	writePluginStoreRateLimit(ctx, &pluginstore.RateLimitError{RetryAt: retryAt})
	seconds, errParse := strconv.ParseInt(recorder.Header().Get("Retry-After"), 10, 64)
	if errParse != nil || seconds < 1 {
		t.Fatalf("Retry-After = %q", recorder.Header().Get("Retry-After"))
	}
}
