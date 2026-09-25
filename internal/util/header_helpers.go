package util

import (
	"net/http"
	"regexp"
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/proxyutil"
	"github.com/gin-gonic/gin"
)

var cpaSessionHeaderVariable = regexp.MustCompile(`(?i)\$CPA-SESSION-ID`)

var credentialHeaderNames = [...]string{"Authorization", "X-Api-Key", "X-Goog-Api-Key"}

// ApplyCredentialHeader replaces the credential header set for one selected auth.
// An empty value removes stale credentials without installing a replacement.
func ApplyCredentialHeader(r *http.Request, name, value string) {
	if r == nil {
		return
	}
	if r.Header == nil {
		r.Header = make(http.Header)
	}
	ApplyCredentialHeaders(r.Header, name, value)
}

// ApplyCredentialHeaders replaces the selected credential in a reusable header map.
func ApplyCredentialHeaders(headers http.Header, name, value string) {
	name = strings.TrimSpace(name)
	if headers == nil || !isCredentialHeader(name) {
		return
	}
	for _, credentialHeader := range credentialHeaderNames {
		headers.Del(credentialHeader)
	}
	if value = strings.TrimSpace(value); value != "" {
		headers.Set(name, value)
	}
}

func isCredentialHeader(name string) bool {
	for _, credentialHeader := range credentialHeaderNames {
		if strings.EqualFold(strings.TrimSpace(name), credentialHeader) {
			return true
		}
	}
	return false
}

// ApplyCustomHeadersFromAttrs 解析自定义请求头，保留受限头保护和请求身份边界
func ApplyCustomHeadersFromAttrs(r *http.Request, attrs map[string]string, clientHeaders ...http.Header) {
	if r == nil {
		return
	}
	ctx := r.Context()
	var incoming http.Header
	if len(clientHeaders) > 0 {
		incoming = clientHeaders[0]
	}
	ginContext, _ := GinContextValue(ctx).(*gin.Context)
	if ginContext == nil {
		ginContext, _ = ctx.(*gin.Context)
	}
	if ginContext != nil && ginContext.Request != nil {
		if len(clientHeaders) == 0 {
			incoming = ginContext.Request.Header
		}
		if !HasExplicitSessionID(ctx) {
			ctx = ginContext.Request.Context()
		}
	}
	headers := extractCustomHeaders(attrs)
	for name, value := range headers {
		if cpaSessionHeaderVariable.MatchString(value) {
			if !proxyutil.IsRestrictedCustomHeader(name) {
				r.Header.Del(name)
			}
			sessionID := SessionIDFromContext(ctx)
			if sessionID == "" {
				delete(headers, name)
				continue
			}
			headers[name] = cpaSessionHeaderVariable.ReplaceAllStringFunc(value, func(string) string { return sessionID })
		} else if variable, dynamic := strings.CutPrefix(value, "$"); dynamic {
			variable = strings.TrimSpace(variable)
			delete(headers, name)
			if proxyutil.IsRestrictedCustomHeader(name) {
				continue
			}
			r.Header.Del(name)
			if variable == "" || proxyutil.IsRestrictedCustomHeader(variable) {
				continue
			}
			for key, values := range incoming {
				if strings.EqualFold(key, variable) && len(values) > 0 && strings.TrimSpace(values[0]) != "" {
					headers[name] = values[0]
					break
				}
			}
		}
	}
	applyCustomHeaders(r, headers)
}

func extractCustomHeaders(attrs map[string]string) map[string]string {
	if len(attrs) == 0 {
		return nil
	}
	headers := make(map[string]string)
	for k, v := range attrs {
		if !strings.HasPrefix(k, "header:") {
			continue
		}
		name := strings.TrimSpace(strings.TrimPrefix(k, "header:"))
		if name == "" {
			continue
		}
		val := strings.TrimSpace(v)
		if val == "" {
			continue
		}
		headers[name] = val
	}
	if len(headers) == 0 {
		return nil
	}
	return headers
}

func applyCustomHeaders(r *http.Request, headers map[string]string) {
	if r == nil || len(headers) == 0 {
		return
	}
	if r.Header == nil {
		r.Header = make(http.Header)
	}
	for k, v := range headers {
		if k == "" || v == "" {
			continue
		}
		if proxyutil.IsRestrictedCustomHeader(k) {
			continue
		}
		r.Header.Set(k, v)
	}
}
