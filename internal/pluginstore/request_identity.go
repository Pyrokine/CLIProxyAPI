package pluginstore

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

type preparedPluginStoreAuth struct {
	requestURL    string
	kind          string
	headers       http.Header
	authenticated bool
}

// LatestReleaseCacheKey identifies a repository under the effective credentials
// and network scope without retaining credential material in the key.
func (c Client) LatestReleaseCacheKey(plugin Plugin) (string, error) {
	_, key, errPrepare := c.PrepareLatestRelease(plugin)
	return key, errPrepare
}

// PrepareLatestRelease binds the cache identity and release request to the same
// credential snapshot even when environment values change while a lookup waits.
func (c Client) PrepareLatestRelease(plugin Plugin) (Client, string, error) {
	owner, repo, errRepository := gitHubRepositoryParts(plugin.Repository)
	if errRepository != nil {
		return Client{}, "", errRepository
	}
	requestURL := fmt.Sprintf(
		"https://api.github.com/repos/%s/%s/releases/latest",
		url.PathEscape(owner),
		url.PathEscape(repo),
	)
	headers, authenticated, errHeaders := c.authHeaders(requestURL, RequestKindMetadata)
	if errHeaders != nil {
		return Client{}, "", errHeaders
	}
	c.preparedAuth = &preparedPluginStoreAuth{
		requestURL:    requestURL,
		kind:          RequestKindMetadata,
		headers:       headers,
		authenticated: authenticated,
	}
	key := strings.ToLower(requestURL) + "/" + requestIdentity(c.NetworkScope, headers, authenticated)
	return c, key, nil
}

func (c Client) authHeaders(requestURL, kind string) (http.Header, bool, error) {
	if errURL := validatePluginStoreRequestURL(c.Auth, requestURL, kind); errURL != nil {
		return nil, false, errURL
	}
	if prepared := c.preparedAuth; prepared != nil && prepared.requestURL == requestURL && prepared.kind == kind {
		return prepared.headers.Clone(), prepared.authenticated, nil
	}
	headers := make(http.Header)
	authenticated, errAuth := applyPluginStoreAuthForClient(headers, c.Auth, requestURL, kind)
	return headers, authenticated, errAuth
}

func requestIdentity(networkScope string, headers http.Header, authenticated bool) string {
	hash := sha256.New()
	_, _ = fmt.Fprintf(hash, "%s\x00%t\x00", strings.TrimSpace(networkScope), authenticated)
	if authenticated {
		// Header.Write sorts header names, so equivalent auth rules share an identity.
		_ = headers.Write(hash)
	}
	return fmt.Sprintf("%x", hash.Sum(nil))
}
