package executor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"sync"

	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

const claudeOAuthToolAliasStateLimit = 1024
const claudeOAuthToolAliasMessageIDLimit = 256

const claudeThreadNotFoundMessage = "No thread state was found for the requested previous_message_id. Replay the full conversation with thread create to start a new Thread."

type claudeOAuthToolAliasStore struct {
	mu      sync.Mutex
	entries map[string]map[string]string
	order   []string
}

func (s *claudeOAuthToolAliasStore) load(scope, messageID string) (map[string]string, bool) {
	key := claudeOAuthToolAliasKey(scope, messageID)
	if key == "" {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	aliases, ok := s.entries[key]
	if !ok {
		return nil, false
	}
	return cloneClaudeOAuthToolAliases(aliases), true
}

func (s *claudeOAuthToolAliasStore) save(scope, messageID string, aliases map[string]string) {
	key := claudeOAuthToolAliasKey(scope, messageID)
	if key == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entries == nil {
		s.entries = make(map[string]map[string]string)
	}
	if _, exists := s.entries[key]; !exists {
		s.order = append(s.order, key)
	}
	s.entries[key] = cloneClaudeOAuthToolAliases(aliases)
	for len(s.order) > claudeOAuthToolAliasStateLimit {
		oldest := s.order[0]
		s.order = s.order[1:]
		delete(s.entries, oldest)
	}
}

func claudeOAuthToolAliasKey(scope, messageID string) string {
	messageID = strings.TrimSpace(messageID)
	if scope == "" || messageID == "" || len(messageID) > claudeOAuthToolAliasMessageIDLimit {
		return ""
	}
	return scope + ":" + messageID
}

func cloneClaudeOAuthToolAliases(source map[string]string) map[string]string {
	clone := make(map[string]string, len(source))
	for alias, original := range source {
		clone[alias] = original
	}
	return clone
}

func claudeOAuthToolAliasScope(ctx context.Context, auth *cliproxyauth.Auth) string {
	caller := strings.TrimSpace(apiKeyFromContext(ctx))
	if caller == "" {
		if auth == nil || strings.TrimSpace(auth.ID) == "" {
			return ""
		}
		caller = "auth:" + auth.ID
	}
	sum := sha256.Sum256([]byte(caller))
	return hex.EncodeToString(sum[:])
}

func claudeThreadContinuationNeedsAliasState(payload []byte) bool {
	if gjson.GetBytes(payload, "thread.type").String() != "continue" ||
		gjson.GetBytes(payload, "thread.previous_message_id").String() == "" {
		return false
	}
	tools := gjson.GetBytes(payload, "tools")
	return !tools.IsArray() || len(tools.Array()) == 0
}

func (e *ClaudeExecutor) prepareClaudeOAuthToolNamesForRequest(
	payload []byte, options claudeMCPAliasOptions, scope string,
) ([]byte, map[string]string, error) {
	if claudeThreadContinuationNeedsAliasState(payload) {
		previousMessageID := gjson.GetBytes(payload, "thread.previous_message_id").String()
		aliases, ok := e.oauthToolAliases.load(scope, previousMessageID)
		if !ok {
			return nil, nil, claudeThreadNotFoundError{statusErr{code: http.StatusNotFound, msg: claudeThreadNotFoundMessage}}
		}
		return payload, aliases, nil
	}
	remapped, aliases := prepareClaudeOAuthToolNamesForUpstream(payload, options)
	return remapped, aliases, nil
}

func (e *ClaudeExecutor) rememberClaudeOAuthToolAliases(payload []byte, aliases map[string]string, messageID, scope string) {
	if gjson.GetBytes(payload, "thread.type").String() == "" {
		return
	}
	e.oauthToolAliases.save(scope, messageID, aliases)
}

type claudeThreadNotFoundError struct {
	statusErr
}

func (claudeThreadNotFoundError) IsRequestScoped() bool { return true }

func (claudeThreadNotFoundError) ResponseBody() []byte {
	return []byte(`{"type":"error","error":{"type":"not_found_error","message":"` + claudeThreadNotFoundMessage + `"}}`)
}

var _ cliproxyexecutor.RequestScopedError = claudeThreadNotFoundError{}

type claudeOAuthToolAliasStreamObserver struct {
	executor  *ClaudeExecutor
	request   []byte
	aliases   map[string]string
	scope     string
	messageID string
}

func (o *claudeOAuthToolAliasStreamObserver) ObserveClaudeSSELine(line []byte) {
	payload := jsonPayload(line)
	if gjson.GetBytes(payload, "type").String() == "message_start" {
		o.messageID = gjson.GetBytes(payload, "message.id").String()
	}
}

func (o *claudeOAuthToolAliasStreamObserver) FinishClaudeStream(outcome claudeStreamOutcome) {
	if outcome == claudeStreamOutcomeComplete {
		o.executor.rememberClaudeOAuthToolAliases(o.request, o.aliases, o.messageID, o.scope)
	}
}
