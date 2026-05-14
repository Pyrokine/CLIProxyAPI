// Last compiled: 2026-03-17
// Author: pyro

package management

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	sdkconfig "github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	"github.com/gin-gonic/gin"
)

const (
	releasesUserAgent    = "CLIProxyAPI"
	releasesCacheTTL     = 15 * time.Minute
	releasesDefaultPage  = 1
	releasesDefaultLimit = 30
	releasesMaxLimit     = 100
)

// gitHubRelease represents a single release from the GitHub API.
type gitHubRelease struct {
	TagName     string    `json:"tag_name"`
	Name        string    `json:"name"`
	Body        string    `json:"body"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	PublishedAt time.Time `json:"published_at"`
	HTMLURL     string    `json:"html_url"`
	Assets      []struct {
		Name               string `json:"name"`
		Size               int64  `json:"size"`
		BrowserDownloadURL string `json:"browser_download_url"`
	} `json:"assets"`
}

type releasesCacheEntry struct {
	data      []gitHubRelease
	fetchedAt time.Time
}

type releasesCache struct {
	mu      sync.RWMutex
	entries map[string]releasesCacheEntry
}

var cachedReleases = releasesCache{entries: make(map[string]releasesCacheEntry)}

// GetReleases proxies the GitHub Releases API with a 15-minute per-repo cache.
// Query params: page (default 1), per_page (default 30, max 100), target
// ("cpa" | "panel", default "cpa").
func (h *Handler) GetReleases(c *gin.Context) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", strconv.Itoa(releasesDefaultPage)))
	perPage, _ := strconv.Atoi(c.DefaultQuery("per_page", strconv.Itoa(releasesDefaultLimit)))
	if page < 1 {
		page = 1
	}
	if perPage < 1 {
		perPage = releasesDefaultLimit
	}
	if perPage > releasesMaxLimit {
		perPage = releasesMaxLimit
	}

	target := strings.ToLower(strings.TrimSpace(c.DefaultQuery("target", "cpa")))
	var repo string
	switch target {
	case "panel":
		repo = h.panelRepository()
	default:
		repo = h.cpaRepository()
	}
	releases, err := h.fetchAllReleases(c, repo)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "fetch_failed", "message": err.Error()})
		return
	}

	total := len(releases)
	start := min((page-1)*perPage, total)
	end := min(start+perPage, total)

	c.JSON(
		http.StatusOK, gin.H{
			"releases": releases[start:end],
			"total":    total,
			"page":     page,
			"per_page": perPage,
			"target":   target,
		},
	)
}

func (h *Handler) panelRepository() string {
	repo := "Pyrokine/Cli-Proxy-API-Management-Center"
	if h != nil && h.cfg != nil {
		custom := strings.TrimSpace(h.cfg.RemoteManagement.PanelGitHubRepository)
		if custom != "" {
			custom = strings.TrimPrefix(custom, "https://github.com/")
			// noinspection HttpUrlsUsage
			custom = strings.TrimPrefix(custom, "http://github.com/")
			custom = strings.TrimPrefix(custom, "https://api.github.com/repos/")
			custom = strings.TrimSuffix(custom, "/")
			// Drop "/releases/..." suffix if present in the configured URL.
			if idx := strings.Index(custom, "/releases"); idx > 0 {
				custom = custom[:idx]
			}
			if strings.Count(custom, "/") == 1 {
				repo = custom
			}
		}
	}
	return repo
}

func (h *Handler) cpaRepository() string {
	repo := "Pyrokine/CLIProxyAPI"
	if h != nil && h.cfg != nil {
		custom := strings.TrimSpace(h.cfg.RemoteManagement.CPAGitHubRepository)
		if custom != "" {
			custom = strings.TrimPrefix(custom, "https://github.com/")
			// noinspection HttpUrlsUsage
			custom = strings.TrimPrefix(custom, "http://github.com/")
			custom = strings.TrimSuffix(custom, "/")
			if strings.Count(custom, "/") == 1 {
				repo = custom
			}
		}
	}
	return repo
}

func (h *Handler) fetchAllReleases(c *gin.Context, repo string) ([]gitHubRelease, error) {
	cachedReleases.mu.RLock()
	if entry, ok := cachedReleases.entries[repo]; ok &&
		time.Since(entry.fetchedAt) < releasesCacheTTL && entry.data != nil {
		data := entry.data
		cachedReleases.mu.RUnlock()
		return data, nil
	}
	cachedReleases.mu.RUnlock()

	url := fmt.Sprintf("https://api.github.com/repos/%s/releases?per_page=100", repo)
	client := &http.Client{Timeout: 15 * time.Second}

	proxyURL := ""
	if h != nil && h.cfg != nil {
		proxyURL = strings.TrimSpace(h.cfg.ProxyURL)
	}
	if proxyURL != "" {
		sdkCfg := &sdkconfig.SDKConfig{ProxyURL: proxyURL}
		util.SetProxy(sdkCfg, client)
	}

	body, err := githubGet(c.Request.Context(), client, url)
	if err != nil {
		return nil, err
	}

	var releases []gitHubRelease
	if err = json.Unmarshal(body, &releases); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}

	cachedReleases.mu.Lock()
	cachedReleases.entries[repo] = releasesCacheEntry{data: releases, fetchedAt: time.Now()}
	cachedReleases.mu.Unlock()

	return releases, nil
}

// githubGet performs a GitHub API GET request with standard headers (User-Agent, Accept, Authorization).
func githubGet(ctx context.Context, client *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", releasesUserAgent)
	if token := lookupGitHubToken(); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return io.ReadAll(io.LimitReader(resp.Body, 10<<20))
}

func lookupGitHubToken() string {
	for _, key := range []string{"GITSTORE_GIT_TOKEN", "GITHUB_TOKEN", "GH_TOKEN"} {
		if v, ok := os.LookupEnv(key); ok {
			if t := strings.TrimSpace(v); t != "" {
				return t
			}
		}
	}
	return ""
}
