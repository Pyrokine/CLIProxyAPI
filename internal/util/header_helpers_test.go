package util

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestDynamicCustomHeadersPreserveRequestBoundary(t *testing.T) {
	ctx := WithSessionID(context.Background(), "canonical-session")
	req := httptest.NewRequest(http.MethodPost, "https://example.test/v1/messages", nil).WithContext(ctx)
	incoming := http.Header{"x-client": {"original"}, "Authorization": {"Bearer synthetic"}, "Cookie": {"synthetic"}}
	attrs := map[string]string{
		"header:X-Copy": "$X-Client", "header:X-Session": "$CPA-SESSION-ID",
		"header:X-Template": "prefix:$cpa-session-id/$CPA-SESSION-ID",
		"header:X-Missing":  "$Missing", "header:Authorization": "$X-Client",
		"header:X-Credential": "$Authorization", "header:X-Cookie": "$Cookie",
	}
	before := incoming.Clone()
	ApplyCustomHeadersFromAttrs(req, attrs, incoming)
	if req.Header.Get("X-Copy") != "original" || req.Header.Get("X-Session") != "canonical-session" || req.Header.Get("X-Template") != "prefix:canonical-session/canonical-session" {
		t.Fatalf("dynamic expansion = %v", req.Header)
	}
	for _, key := range []string{"X-Missing", "Authorization", "X-Credential", "X-Cookie"} {
		if req.Header.Get(key) != "" {
			t.Fatalf("unexpected dynamic header %s", key)
		}
	}
	if !reflect.DeepEqual(incoming, before) || attrs["header:X-Copy"] != "$X-Client" {
		t.Fatal("request inputs were mutated")
	}
}

func TestDynamicSessionExplicitClearOverridesGinParent(t *testing.T) {
	ginContext, _ := gin.CreateTestContext(httptest.NewRecorder())
	ginContext.Request = httptest.NewRequest(http.MethodGet, "/", nil).WithContext(WithSessionID(context.Background(), "parent-session"))
	ginContext.Request.Header.Set("X-Client", "from-gin")
	parent := WithGinContext(context.Background(), ginContext)
	for _, cleared := range []bool{false, true} {
		ctx := parent
		if cleared {
			ctx = WithSessionID(ctx, "")
			if !HasExplicitSessionID(ctx) {
				t.Fatal("clear presence was lost")
			}
		}
		req := httptest.NewRequest(http.MethodGet, "https://example.test", nil).WithContext(ctx)
		ApplyCustomHeadersFromAttrs(req, map[string]string{"header:X-Session": "$CPA-SESSION-ID", "header:X-Copy": "$X-Client"})
		want := "parent-session"
		if cleared {
			want = ""
		}
		if req.Header.Get("X-Session") != want || req.Header.Get("X-Copy") != "from-gin" {
			t.Fatalf("cleared=%t headers=%v", cleared, req.Header)
		}
	}
}

func TestDynamicHeadersDoNotReuseOutgoingHeaders(t *testing.T) {
	req := &http.Request{Header: http.Header{
		"X-Source":  {"outgoing-only"},
		"X-Copy":    {"stale-copy"},
		"X-Session": {"stale-session"},
	}}
	ApplyCustomHeadersFromAttrs(req, map[string]string{"header:X-Copy": "$X-Source", "header:X-Session": "$CPA-SESSION-ID"}, nil)
	if req.Header.Get("X-Copy") != "" || req.Header.Get("X-Session") != "" {
		t.Fatalf("missing caller values retained stale dynamic headers: %v", req.Header)
	}
	blank := &http.Request{Header: http.Header{"X-Copy": {"stale-copy"}}}
	ApplyCustomHeadersFromAttrs(
		blank,
		map[string]string{"header:X-Copy": "$X-Blank"},
		http.Header{"X-Blank": {"   "}},
	)
	if blank.Header.Get("X-Copy") != "" {
		t.Fatalf("blank caller value retained stale dynamic header: %v", blank.Header)
	}
	ApplyCustomHeadersFromAttrs(&http.Request{}, map[string]string{"header:X-Test": "literal"})
	ApplyCustomHeadersFromAttrs(nil, nil)
}

func TestApplyCredentialHeaderReplacesAndClearsStaleCredentials(t *testing.T) {
	req := &http.Request{Header: http.Header{
		"Authorization":  {"Bearer stale"},
		"X-Api-Key":      {"stale-claude"},
		"X-Goog-Api-Key": {"stale-gemini"},
	}}
	ApplyCredentialHeader(req, "Authorization", " Bearer current ")
	if req.Header.Get("Authorization") != "Bearer current" ||
		req.Header.Get("X-Api-Key") != "" || req.Header.Get("X-Goog-Api-Key") != "" {
		t.Fatalf("credential replacement = %v", req.Header)
	}

	ApplyCredentialHeader(req, "X-Api-Key", "")
	for _, name := range credentialHeaderNames {
		if req.Header.Get(name) != "" {
			t.Fatalf("empty credential retained %s: %v", name, req.Header)
		}
	}
	ApplyCredentialHeader(req, "X-Not-Credential", "value")
	if req.Header.Get("X-Not-Credential") != "" {
		t.Fatalf("unexpected credential target was applied: %v", req.Header)
	}
	ApplyCredentialHeader(nil, "Authorization", "Bearer ignored")
}

func TestDynamicCustomHeaderCannotClearRestrictedProviderCredential(t *testing.T) {
	req := &http.Request{Header: http.Header{"Authorization": {"Bearer provider"}}}
	ApplyCustomHeadersFromAttrs(
		req,
		map[string]string{"header:Authorization": "$Missing"},
		http.Header{},
	)
	if got := req.Header.Get("Authorization"); got != "Bearer provider" {
		t.Fatalf("restricted provider credential = %q, want preserved", got)
	}
}

func TestApplyCustomHeadersFromAttrsIgnoresRestrictedHeaders(t *testing.T) {
	req, errRequest := http.NewRequest(http.MethodGet, "https://example.com/v1/messages", nil)
	if errRequest != nil {
		t.Fatalf("http.NewRequest returned error: %v", errRequest)
	}
	req.Header.Set("Authorization", "Bearer built-in")

	ApplyCustomHeadersFromAttrs(
		req, map[string]string{
			"header:Authorization":  "Bearer custom",
			"header:Cookie":         "session=custom",
			"header:Host":           "api.example.test",
			"header:X-Custom-Value": "custom",
		},
	)

	if req.Host != "example.com" {
		t.Fatalf("req.Host = %q, want example.com", req.Host)
	}
	if got := req.Header.Get("Host"); got != "" {
		t.Fatalf("Header Host = %q, want empty", got)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer built-in" {
		t.Fatalf("Authorization = %q, want built-in value", got)
	}
	if got := req.Header.Get("Cookie"); got != "" {
		t.Fatalf("Cookie = %q, want empty", got)
	}
	if got := req.Header.Get("X-Custom-Value"); got != "custom" {
		t.Fatalf("X-Custom-Value = %q, want custom", got)
	}
}
