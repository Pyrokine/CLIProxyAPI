package management

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/managementasset"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
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
	force := c.Query("force") == "1"
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
	cfg := h.configSnapshot()
	proxyURL, panelRepo := "", ""
	if cfg != nil {
		proxyURL = strings.TrimSpace(cfg.ProxyURL)
		panelRepo = cfg.RemoteManagement.PanelGitHubRepository
	}
	var releases []gitHubRelease
	if target == "panel" {
		source, err := managementasset.ResolveReleasesURL(panelRepo)
		if err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": "fetch_failed", "message": err.Error()})
			return
		}
		releases, err = h.fetchReleaseCollection(c, source, source.String(), force, proxyURL)
		if err != nil {
			c.JSON(
				http.StatusBadGateway,
				gin.H{"error": "fetch_failed", "message": "Failed to fetch management panel releases"},
			)
			return
		}
	} else {
		var err error
		releases, err = h.fetchAllReleases(c, cpaRepositoryFromConfig(cfg), force, proxyURL)
		if err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": "fetch_failed", "message": err.Error()})
			return
		}
	}

	total := len(releases)
	start := total
	if page-1 <= total/perPage {
		start = min((page-1)*perPage, total)
	}
	end := start + min(perPage, total-start)

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

func cpaRepositoryFromConfig(cfg *config.Config) string {
	repo := "Pyrokine/CLIProxyAPI"
	if cfg != nil {
		custom := strings.TrimSpace(cfg.RemoteManagement.CPAGitHubRepository)
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

func (h *Handler) fetchAllReleases(c *gin.Context, repo string, force bool, proxyURL string) ([]gitHubRelease, error) {
	source, err := url.Parse(fmt.Sprintf("https://api.github.com/repos/%s/releases", repo))
	if err != nil {
		return nil, fmt.Errorf("invalid releases URL: %w", err)
	}
	return h.fetchReleaseCollection(c, source, repo, force, proxyURL)
}

func (h *Handler) fetchReleaseCollection(
	c *gin.Context, source *url.URL, cacheKey string, force bool, proxyURL string,
) ([]gitHubRelease, error) {
	cachedReleases.mu.RLock()
	if !force {
		if entry, ok := cachedReleases.entries[cacheKey]; ok &&
			time.Since(entry.fetchedAt) < releasesCacheTTL && entry.data != nil {
			data := entry.data
			cachedReleases.mu.RUnlock()
			return data, nil
		}
	}
	cachedReleases.mu.RUnlock()

	first := *source
	query := first.Query()
	query.Set("per_page", strconv.Itoa(releasesMaxLimit))
	first.RawQuery = query.Encode()
	requestURL := first.String()
	client := managementasset.NewReleaseHTTPClient(proxyURL, source)
	seen := make(map[string]struct{})
	releases := make([]gitHubRelease, 0)
	for requestURL != "" {
		if _, duplicate := seen[requestURL]; duplicate {
			return nil, fmt.Errorf("repeated releases continuation")
		}
		seen[requestURL] = struct{}{}
		var headers http.Header
		body, err := githubGet(c.Request.Context(), client, requestURL, &headers)
		if err != nil {
			return nil, err
		}
		var page []gitHubRelease
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, fmt.Errorf("decode: %w", err)
		}
		releases = append(releases, page...)
		requestURL, err = releasesNextURL(headers, requestURL, source)
		if err != nil {
			return nil, err
		}
	}

	cachedReleases.mu.Lock()
	cachedReleases.entries[cacheKey] = releasesCacheEntry{data: releases, fetchedAt: time.Now()}
	cachedReleases.mu.Unlock()

	return releases, nil
}

func releasesNextURL(headers http.Header, currentURL string, source *url.URL) (string, error) {
	current, err := url.Parse(currentURL)
	if err != nil {
		return "", fmt.Errorf("invalid releases continuation")
	}
	next := ""
	for _, line := range headers.Values("Link") {
		for link := range strings.SplitSeq(line, ",") {
			link = strings.TrimSpace(link)
			end := strings.IndexByte(link, '>')
			if !strings.HasPrefix(link, "<") || end < 1 {
				return "", fmt.Errorf("invalid releases continuation")
			}
			_, parameters, err := mime.ParseMediaType("link" + link[end+1:])
			if err != nil {
				return "", fmt.Errorf("invalid releases continuation")
			}
			isNext := false
			for relation := range strings.FieldsSeq(parameters["rel"]) {
				isNext = isNext || strings.EqualFold(relation, "next")
			}
			if !isNext {
				continue
			}
			if next != "" {
				return "", fmt.Errorf("multiple releases continuations")
			}
			reference, err := url.Parse(link[1:end])
			if err != nil {
				return "", fmt.Errorf("invalid releases continuation")
			}
			candidate := current.ResolveReference(reference)
			if !managementasset.AllowsReleaseOrigin(
				source, candidate,
			) || candidate.EscapedPath() != source.EscapedPath() {
				return "", fmt.Errorf("invalid releases continuation")
			}
			query, err := url.ParseQuery(candidate.RawQuery)
			if err != nil {
				return "", fmt.Errorf("invalid releases continuation")
			}
			candidate.User = source.User
			candidate.RawQuery = query.Encode()
			next = candidate.String()
		}
	}
	return next, nil
}

// githubGet performs a GET request to the GitHub API with standard headers (User-Agent, Accept, Authorization).
func githubGet(ctx context.Context, client *http.Client, url string, responseHeaders ...*http.Header) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", releasesUserAgent)
	if isGitHubAPIRequestURL(url) {
		if token := util.GitHubToken(); token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if len(responseHeaders) > 0 && responseHeaders[0] != nil {
		*responseHeaders[0] = resp.Header.Clone()
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return io.ReadAll(io.LimitReader(resp.Body, 256<<20))
}

func isGitHubAPIRequestURL(rawURL string) bool {
	return managementasset.IsGitHubAPIRequestURL(rawURL)
}
