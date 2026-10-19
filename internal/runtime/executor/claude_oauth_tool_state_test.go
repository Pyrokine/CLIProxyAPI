package executor

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

func TestClaudeOAuthToolAliasesRespectCallerScopeAndEviction(t *testing.T) {
	exec := NewClaudeExecutor(nil)
	create := []byte(`{"thread":{"type":"create"}}`)
	continueFromFirst := []byte(`{"thread":{"type":"continue","previous_message_id":"msg-one"}}`)
	auth := &cliproxyauth.Auth{ID: "claude-thread-auth"}
	scopeA := claudeOAuthToolAliasScope(xaiC03ContextWithAPIKey("caller-a"), auth)
	scopeB := claudeOAuthToolAliasScope(xaiC03ContextWithAPIKey("caller-b"), auth)
	fallbackScope := claudeOAuthToolAliasScope(context.Background(), auth)
	if scopeA == "" || scopeB == "" || fallbackScope == "" ||
		scopeA == scopeB || scopeA == fallbackScope || strings.Contains(scopeA, "caller-a") ||
		claudeOAuthToolAliasScope(context.Background(), nil) != "" {
		t.Fatalf("caller scopes are not isolated or hashed")
	}
	aliases := map[string]string{"mcp__test__Read": "Read"}
	exec.rememberClaudeOAuthToolAliases(create, aliases, "msg-one", scopeA)
	aliases["mcp__test__Read"] = "modified"
	_, restored, err := exec.prepareClaudeOAuthToolNamesForRequest(continueFromFirst, claudeMCPAliasOptions{}, scopeA)
	if err != nil || restored["mcp__test__Read"] != "Read" {
		t.Fatalf("same caller continuation = (%v, %v), want Read", restored, err)
	}
	restored["mcp__test__Read"] = "changed"
	_, restored, err = exec.prepareClaudeOAuthToolNamesForRequest(continueFromFirst, claudeMCPAliasOptions{}, scopeA)
	if err != nil || restored["mcp__test__Read"] != "Read" {
		t.Fatalf("returned map changed stored alias: (%v, %v)", restored, err)
	}
	for _, scope := range []string{scopeB, fallbackScope, ""} {
		_, _, err = exec.prepareClaudeOAuthToolNamesForRequest(continueFromFirst, claudeMCPAliasOptions{}, scope)
		var requestErr cliproxyexecutor.RequestScopedError
		var status interface{ StatusCode() int }
		if !errors.As(err, &requestErr) || !requestErr.IsRequestScoped() ||
			!errors.As(err, &status) || status.StatusCode() != http.StatusNotFound {
			t.Fatalf("scope %q missing alias error = %v, want request-scoped 404", scope, err)
		}
		if body := err.(interface{ ResponseBody() []byte }).ResponseBody(); gjson.GetBytes(body, "error.type").String() != "not_found_error" {
			t.Fatalf("missing alias error body = %s", body)
		}
	}

	for i := 0; i < claudeOAuthToolAliasStateLimit; i++ {
		exec.rememberClaudeOAuthToolAliases(create, nil, fmt.Sprintf("msg-%d", i), scopeA)
	}
	_, _, err = exec.prepareClaudeOAuthToolNamesForRequest(continueFromFirst, claudeMCPAliasOptions{}, scopeA)
	var requestErr cliproxyexecutor.RequestScopedError
	if !errors.As(err, &requestErr) || !requestErr.IsRequestScoped() ||
		len(exec.oauthToolAliases.entries) != claudeOAuthToolAliasStateLimit {
		t.Fatalf("evicted state = (%v, %d entries), want bounded request-scoped error", err, len(exec.oauthToolAliases.entries))
	}
}
