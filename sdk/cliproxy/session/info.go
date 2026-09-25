// Package session derives stable conversation identities and extracts parent lineage.
package session

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

// SessionInfo contains the flat session identity and its immediate parent.
type SessionInfo struct {
	SessionID       string         `json:"session_id"`
	ParentSessionID string         `json:"parent_session_id,omitempty"`
	AgentName       string         `json:"agent_name,omitempty"`
	ClientType      string         `json:"client_type,omitempty"`
	CallerScope     string         `json:"caller_scope,omitempty"`
	AuthID          string         `json:"auth_id,omitempty"`
	Provider        string         `json:"provider,omitempty"`
	Model           string         `json:"model,omitempty"`
	IsFork          bool           `json:"is_fork,omitempty"`
	IsSubagent      bool           `json:"is_subagent,omitempty"`
	Metadata        map[string]any `json:"metadata,omitempty"`
}

var parentSessionPaths = []string{
	"parent_session_id", "parentSessionId", "parentSessionID",
	"parent_thread_id", "parentThreadId", "parentThreadID",
	"forked_from_thread_id", "forked_from_id",
	"parent_conversation_id", "parentConversationId", "parentConversationID",
	"parent_id", "parentId", "parentID",
	"parent_task_id", "parentTaskId", "parentTaskID",
	"parent_action_id", "parentActionId", "parentActionID",
	"parent_session", "parentSession", "parent_subagent_id", "parentSubagentId",
	"forkSource.sessionId", "fork_source.session_id", "previousSessionId", "previous_session_id",
	"metadata.parent_session_id", "metadata.parentSessionId", "metadata.parentSessionID",
	"metadata.parent_thread_id", "metadata.parentThreadId",
	"metadata.forked_from_thread_id", "metadata.forked_from_id",
	"metadata.parent_id", "metadata.parentId", "metadata.parentID",
	"metadata.parent_task_id", "metadata.parentTaskId", "metadata.parentTaskID",
	"metadata.parent_action_id", "metadata.parentActionId",
	"metadata.parent_subagent_id", "metadata.parentSubagentId",
	"metadata.parent_session", "metadata.parentSession",
	"metadata.parent_agent_id", "metadata.parentAgentId",
	"metadata.forkSource.sessionId", "metadata.previousSessionId",
	"extra_body.parent_session_id", "extra_body.parentSessionId", "extra_body.parentSessionID",
	"extra_body.parent_thread_id", "extra_body.parentThreadId",
	"extra_body.forked_from_thread_id", "extra_body.forked_from_id",
	"extra_body.parent_id", "extra_body.parentId", "extra_body.parentID",
	"extra_body.parent_task_id", "extra_body.parentTaskId",
	"extra_body.parent_action_id", "extra_body.parentActionId",
	"extra_body.parent_subagent_id", "extra_body.parentSubagentId",
	"extra_body.parent_session", "extra_body.parentSession",
}

var bodyForkPaths = []string{
	"forked_from_thread_id", "forked_from_id",
	"forkSource.sessionId", "fork_source.session_id",
	"previousSessionId", "previous_session_id",
	"metadata.forked_from_thread_id", "metadata.forked_from_id",
	"metadata.forkSource.sessionId", "metadata.previousSessionId",
	"extra_body.forked_from_thread_id", "extra_body.forked_from_id",
	"extra_body.forkSource.sessionId", "extra_body.previousSessionId",
}

// ExtractSessionInfo extracts explicit protocol and harness session identities.
func ExtractSessionInfo(headers http.Header, payload []byte, metadata map[string]any) (SessionInfo, bool) {
	info := SessionInfo{}
	if scope, ok := metadata[cliproxyexecutor.CallerScopeMetadataKey].(string); ok {
		info.CallerScope = strings.TrimSpace(scope)
	}

	root, requestRoot, hasNestedRequest := sessionRoots(payload)
	parentCandidate := firstSessionPath(root, requestRoot, hasNestedRequest, parentSessionPaths...)
	if parentCandidate == "" {
		parentCandidate = ClaudeMetadataParentSessionID(payload)
	}

	if sessionID := sessionHeaderValue(headers, "X-Claude-Code-Session-Id"); sessionID != "" {
		return buildClaudeInfo(info, sessionID, parentCandidate, headers, root, requestRoot, hasNestedRequest, payload)
	}
	if sessionID, parentID, agentID := ClaudeMetadataIdentities(payload); sessionID != "" {
		if parentID == "" {
			parentID = parentCandidate
		}
		return buildClaudeMetadataInfo(info, sessionID, parentID, agentID, headers, root, requestRoot, hasNestedRequest)
	}

	if codexInfo, ok := extractCodexInfo(info, headers, root, requestRoot, hasNestedRequest, parentCandidate); ok {
		return finalizeSessionInfo(codexInfo)
	}

	headerSessions := []struct {
		header        string
		clientType    string
		prefix        string
		defaultAgent  string
		parentHeaders []string
	}{
		{"X-Http-Session-Id", "agy", "agy:", "main", []string{"X-Parent-Session-ID", "X-Parent-Session-Id", "X-Parent-ID", "X-Parent-Id"}},
		{"X-Session-ID", "generic", "header:", "main", []string{"X-Parent-Session-ID", "X-Parent-Session-Id", "X-Parent-ID", "X-Parent-Id"}},
		{"X-Session-Affinity", "opencode", "affinity:", "main", []string{"X-Parent-Session-Affinity", "X-Parent-Session-ID", "X-Parent-ID", "X-Parent-Id"}},
		{"X-Slot-Session-Id", "pi", "slot:", "slot", []string{"X-Parent-Slot-Session-Id", "X-Parent-Session-ID", "X-Parent-Session-Id", "X-Parent-ID", "X-Parent-Id"}},
		{"X-Task-ID", "task", "task:", "main", []string{"X-Parent-Task-ID", "X-Parent-Task-Id", "X-Parent-Session-ID", "X-Parent-Session-Id", "X-Parent-ID", "X-Parent-Id"}},
		{"X-Task-Id", "task", "task:", "main", []string{"X-Parent-Task-ID", "X-Parent-Task-Id", "X-Parent-Session-ID", "X-Parent-Session-Id", "X-Parent-ID", "X-Parent-Id"}},
		{"X-Conversation-Id", "conv", "conv:", "main", []string{"X-Parent-Conversation-Id", "X-Parent-ID"}},
		{"X-Thread-Id", "openai-thread", "thread:", "main", []string{"X-Parent-Thread-Id", "X-Parent-ID"}},
		{"X-Client-Request-Id", "generic", "clientreq:", "main", []string{"X-Parent-Session-ID", "X-Parent-ID", "X-Parent-Id"}},
	}
	for _, candidate := range headerSessions {
		sessionID := sessionHeaderValue(headers, candidate.header)
		if sessionID == "" {
			continue
		}
		parentID := firstHeaderValue(headers, candidate.parentHeaders...)
		if parentID == "" {
			parentID = parentCandidate
		}
		return finalizeSessionInfo(buildPrefixedInfo(info, candidate.clientType, candidate.prefix, sessionID, parentID, candidate.defaultAgent))
	}

	if !root.Exists() {
		return extractMetadataSession(info, metadata)
	}

	if cacheID := firstSessionPath(root, requestRoot, hasNestedRequest, "cachedContent", "cached_content"); cacheID != "" {
		return finalizeSessionInfo(buildPrefixedInfo(info, "gemini", "geminicache:", cacheID, parentCandidate, "main"))
	}
	if threadID := firstSessionPath(root, requestRoot, hasNestedRequest, "thread_id", "threadId", "metadata.thread_id"); threadID != "" {
		bodyInfo := buildPrefixedInfo(info, "openai-thread", "thread:", threadID, parentCandidate, "main")
		markBodyLineage(&bodyInfo, root, requestRoot, hasNestedRequest)
		return finalizeSessionInfo(bodyInfo)
	}

	agentID := firstSessionPath(root, requestRoot, hasNestedRequest, "metadata.agent_id", "metadata.subagent_id")
	if agentID == "" {
		agentID = firstHeaderValue(headers, "X-Claude-Code-Agent-Id", "x-agent-id")
	}
	if sessionID := firstSessionPath(
		root, requestRoot, hasNestedRequest,
		"session_id", "sessionId", "sessionID", "child_session_id", "childSessionId",
		"metadata.session_id", "metadata.sessionId", "metadata.sessionID", "metadata.child_session_id",
		"extra_body.session_id", "extra_body.sessionId", "extra_body.sessionID",
	); sessionID != "" {
		info.ClientType = "generic"
		if agentID != "" && agentID != "main" {
			info.SessionID = "session:" + sessionID + ":agent:" + agentID
			info.ParentSessionID = "session:" + sessionID
			if parentCandidate != "" && parentCandidate != sessionID {
				info.ParentSessionID = "session:" + parentCandidate
			}
			info.AgentName = agentID
			info.IsSubagent = true
		} else {
			info = buildPrefixedInfo(info, "generic", "session:", sessionID, parentCandidate, "main")
			markBodyLineage(&info, root, requestRoot, hasNestedRequest)
		}
		return finalizeSessionInfo(info)
	}

	if taskID := firstSessionPath(
		root, requestRoot, hasNestedRequest,
		"task_id", "taskId", "taskID", "action_id", "actionId", "actionID",
		"metadata.task_id", "metadata.taskId", "metadata.taskID",
		"metadata.action_id", "metadata.actionId", "metadata.actionID",
		"extra_body.task_id", "extra_body.taskId", "extra_body.taskID",
	); taskID != "" {
		bodyInfo := buildPrefixedInfo(info, "task", "task:", taskID, parentCandidate, "main")
		markBodyLineage(&bodyInfo, root, requestRoot, hasNestedRequest)
		return finalizeSessionInfo(bodyInfo)
	}

	if promptCacheKey := firstSessionPath(root, requestRoot, hasNestedRequest, "prompt_cache_key", "promptCacheKey"); promptCacheKey != "" {
		return finalizeSessionInfo(buildPrefixedInfo(info, "generic", "pck:", promptCacheKey, parentCandidate, "main"))
	}
	conversation := root.Get("conversation")
	if !conversation.Exists() && hasNestedRequest {
		conversation = requestRoot.Get("conversation")
	}
	conversationID := normalizedSessionCandidate(conversation.Get("id").String())
	if conversationID == "" && conversation.Type == gjson.String {
		conversationID = normalizedSessionCandidate(conversation.String())
	}
	if conversationID != "" {
		return finalizeSessionInfo(buildPrefixedInfo(info, "conv", "conv:", conversationID, parentCandidate, "main"))
	}
	if userID := firstSessionPath(root, requestRoot, hasNestedRequest, "metadata.user_id"); userID != "" {
		info.ClientType = "generic"
		info.SessionID = "user:" + userID
		info.AgentName = "main"
		return finalizeSessionInfo(info)
	}
	if conversationID = firstSessionPath(
		root, requestRoot, hasNestedRequest,
		"conversation_id", "conversationId", "chat_id", "chatId", "metadata.conversation_id", "extra_body.conversation_id",
	); conversationID != "" {
		return finalizeSessionInfo(buildPrefixedInfo(info, "conv", "conv:", conversationID, parentCandidate, "main"))
	}

	return extractMetadataSession(info, metadata)
}

func sessionRoots(payload []byte) (root, requestRoot gjson.Result, hasNestedRequest bool) {
	if len(payload) == 0 {
		return gjson.Result{}, gjson.Result{}, false
	}
	root = util.ParseGJSONBytesNoCopy(payload)
	requestRoot = root
	request := root.Get("request")
	hasNestedRequest = request.Exists() && !root.Get("contents").Exists()
	if hasNestedRequest {
		requestRoot = request
	}
	return root, requestRoot, hasNestedRequest
}

func buildClaudeInfo(
	info SessionInfo,
	sessionID, parentCandidate string,
	headers http.Header,
	root, requestRoot gjson.Result,
	hasNestedRequest bool,
	payload []byte,
) (SessionInfo, bool) {
	agentID := firstHeaderValue(headers, "X-Claude-Code-Agent-Id")
	if agentID == "" {
		agentID = firstSessionPath(root, requestRoot, hasNestedRequest, "metadata.agent_id", "metadata.subagent_id")
	}
	if agentID == "" {
		_, _, agentID = ClaudeMetadataIdentities(payload)
	}
	return buildClaudeMetadataInfo(info, sessionID, parentCandidate, agentID, headers, root, requestRoot, hasNestedRequest)
}

func buildClaudeMetadataInfo(
	info SessionInfo,
	sessionID, parentSessionID, agentID string,
	headers http.Header,
	root, requestRoot gjson.Result,
	hasNestedRequest bool,
) (SessionInfo, bool) {
	info.ClientType = "claude"
	if agentID == "" {
		agentID = firstHeaderValue(headers, "X-Claude-Code-Agent-Id")
	}
	if agentID == "" {
		agentID = firstSessionPath(root, requestRoot, hasNestedRequest, "metadata.agent_id", "metadata.subagent_id")
	}
	parentAgentID := firstHeaderValue(headers, "X-Claude-Code-Parent-Agent-Id")
	if parentAgentID == "" {
		parentAgentID = firstSessionPath(root, requestRoot, hasNestedRequest, "metadata.parent_agent_id", "metadata.parentAgentId")
	}
	if agentID != "" && agentID != "main" {
		info.SessionID = "claude:" + sessionID + ":agent:" + agentID
		info.ParentSessionID = "claude:" + sessionID
		if parentAgentID != "" && parentAgentID != "main" && parentAgentID != agentID {
			info.ParentSessionID = "claude:" + sessionID + ":agent:" + parentAgentID
		} else if parentSessionID != "" && parentSessionID != sessionID {
			info.ParentSessionID = "claude:" + parentSessionID
		}
		info.AgentName = agentID
		info.IsSubagent = true
	} else {
		info.SessionID = "claude:" + sessionID
		info.AgentName = "main"
		if parentSessionID != "" && parentSessionID != sessionID {
			info.ParentSessionID = "claude:" + parentSessionID
			info.AgentName = "subagent"
			info.IsSubagent = true
		}
	}
	return finalizeSessionInfo(info)
}

func extractCodexInfo(
	info SessionInfo,
	headers http.Header,
	root, requestRoot gjson.Result,
	hasNestedRequest bool,
	parentCandidate string,
) (SessionInfo, bool) {
	sessionID := firstHeaderValue(headers, "Session-Id", "Session_id")
	threadID := firstHeaderValue(headers, "Thread-Id", "Thread_id")
	turnMetadataRaw := firstHeaderValue(headers, "X-Codex-Turn-Metadata")
	turnMetadata := gjson.Parse(turnMetadataRaw)
	if sessionID == "" && turnMetadata.Exists() {
		sessionID = normalizedSessionCandidate(turnMetadata.Get("session_id").String())
	}
	if threadID == "" && turnMetadata.Exists() {
		threadID = normalizedSessionCandidate(turnMetadata.Get("thread_id").String())
	}
	if threadID == "" && sessionID != "" {
		threadID = firstSessionPath(root, requestRoot, hasNestedRequest, "thread_id", "threadId", "metadata.thread_id")
	}
	if sessionID == "" && threadID == "" {
		return SessionInfo{}, false
	}

	info.ClientType = "codex"
	parentThread := firstHeaderValue(headers, "x-codex-parent-thread-id", "X-Codex-Parent-Thread-Id")
	if parentThread == "" && turnMetadata.Exists() {
		parentThread = normalizedSessionCandidate(turnMetadata.Get("parent_thread_id").String())
	}
	forkedFrom := ""
	if turnMetadata.Exists() {
		forkedFrom = firstResultCandidate(turnMetadata, "forked_from_thread_id", "forked_from_id")
	}
	if forkedFrom == "" {
		forkedFrom = firstSessionPath(
			root, requestRoot, hasNestedRequest,
			"forked_from_thread_id", "forked_from_id",
			"metadata.forked_from_thread_id", "metadata.forked_from_id",
			"extra_body.forked_from_thread_id", "extra_body.forked_from_id",
		)
	}
	if forkedFrom != "" {
		forkSessionID := threadID
		if forkSessionID == "" {
			forkSessionID = sessionID
		}
		if forkSessionID == forkedFrom && sessionID != "" && sessionID != forkedFrom {
			forkSessionID = sessionID
		}
		info.SessionID = "codex:" + forkSessionID
		info.ParentSessionID = "codex:" + forkedFrom
		info.AgentName = "main"
		info.IsFork = true
		return info, true
	}

	agentName := ""
	if turnMetadata.Exists() {
		agentName = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(turnMetadata.Get("agent_name").String(), "/root/"), "/"))
		agentName = normalizedSessionCandidate(agentName)
		if agentName == "root" || agentName == "main" {
			agentName = ""
		}
	}
	subagentHeader := firstHeaderValue(headers, "X-Openai-Subagent")
	subagentSignal := subagentHeader != "" && !strings.EqualFold(subagentHeader, "false") && subagentHeader != "0"
	if turnMetadata.Get("subagent_kind").String() == "thread_spawn" {
		subagentSignal = true
	}
	if subagentSignal || (threadID != "" && sessionID != "" && threadID != sessionID) ||
		(parentThread != "" && parentThread != threadID && parentThread != sessionID) {
		childSessionID := threadID
		if childSessionID == "" {
			childSessionID = sessionID
		}
		parentID := parentThread
		if parentID == "" {
			parentID = sessionID
		}
		if agentName != "" && sessionID != "" {
			info.SessionID = "codex:" + sessionID + ":agent:" + agentName
			info.AgentName = agentName
		} else {
			info.SessionID = "codex:" + childSessionID
			info.AgentName = "subagent"
			if agentName != "" {
				info.AgentName = agentName
			}
		}
		if parentID != "" && parentID != childSessionID {
			info.ParentSessionID = "codex:" + parentID
		} else if parentCandidate != "" && parentCandidate != childSessionID {
			info.ParentSessionID = "codex:" + parentCandidate
		}
		info.IsSubagent = true
		return info, true
	}

	primary := sessionID
	if primary == "" {
		primary = threadID
	}
	info = buildPrefixedInfo(info, "codex", "codex:", primary, firstNonSelf(parentThread, parentCandidate, primary), "main")
	if info.ParentSessionID != "" {
		info.IsSubagent = true
	}
	return info, true
}

func buildPrefixedInfo(info SessionInfo, clientType, prefix, sessionID, parentID, defaultAgent string) SessionInfo {
	info.ClientType = clientType
	info.SessionID = prefix + sessionID
	info.AgentName = defaultAgent
	if parentID != "" && parentID != sessionID {
		info.ParentSessionID = prefix + parentID
		info.AgentName = "subagent"
		info.IsSubagent = true
	}
	return info
}

func markBodyLineage(info *SessionInfo, root, requestRoot gjson.Result, hasNestedRequest bool) {
	if info == nil || info.ParentSessionID == "" {
		return
	}
	if firstSessionPath(root, requestRoot, hasNestedRequest, bodyForkPaths...) != "" {
		info.IsFork = true
		info.IsSubagent = false
		info.AgentName = "main"
	}
}

func extractMetadataSession(info SessionInfo, metadata map[string]any) (SessionInfo, bool) {
	if executionID := normalizedMetadataSessionCandidate(metadata, cliproxyexecutor.ExecutionSessionMetadataKey); executionID != "" {
		info.ClientType = "generic"
		info.SessionID = "execution:" + executionID
		info.AgentName = "main"
		return finalizeSessionInfo(info)
	}
	if lcpID := normalizedMetadataSessionCandidate(metadata, cliproxyexecutor.LCPAffinitySessionIDMetadataKey); lcpID != "" {
		info.ClientType = "lcp"
		info.SessionID = lcpID
		info.AgentName = "main"
		if parentID := normalizedMetadataSessionCandidate(metadata, cliproxyexecutor.ParentSessionIDMetadataKey); parentID != "" && parentID != lcpID {
			info.ParentSessionID = parentID
			info.IsFork = true
		}
		return finalizeSessionInfo(info)
	}
	return SessionInfo{}, false
}

func firstSessionPath(root, requestRoot gjson.Result, hasNestedRequest bool, paths ...string) string {
	for _, path := range paths {
		if value := normalizedSessionCandidate(root.Get(path).String()); value != "" {
			return value
		}
		if hasNestedRequest {
			if value := normalizedSessionCandidate(requestRoot.Get(path).String()); value != "" {
				return value
			}
		}
	}
	return ""
}

func firstResultCandidate(root gjson.Result, paths ...string) string {
	for _, path := range paths {
		if value := normalizedSessionCandidate(root.Get(path).String()); value != "" {
			return value
		}
	}
	return ""
}

func firstHeaderValue(headers http.Header, names ...string) string {
	for _, name := range names {
		if value := sessionHeaderValue(headers, name); value != "" {
			return value
		}
	}
	return ""
}

func firstNonSelf(first, second, self string) string {
	if first != "" && first != self {
		return first
	}
	if second != "" && second != self {
		return second
	}
	return ""
}

func normalizedMetadataSessionCandidate(metadata map[string]any, key string) string {
	if metadata == nil {
		return ""
	}
	switch value := metadata[key].(type) {
	case string:
		return normalizedSessionCandidate(value)
	case []byte:
		return normalizedSessionCandidate(string(value))
	default:
		return ""
	}
}

// BoundSessionIdentity bounds a session key without losing long-value uniqueness.
func BoundSessionIdentity(identity string) string {
	if len(identity) <= 256 {
		return identity
	}
	sum := sha256.Sum256([]byte(identity))
	digest := hex.EncodeToString(sum[:])
	prefixLength := 256 - 1 - len(digest)
	if prefixLength > len(identity) {
		prefixLength = len(identity)
	}
	prefix := identity[:prefixLength]
	for len(prefix) > 0 && !utf8.ValidString(prefix) {
		prefix = prefix[:len(prefix)-1]
	}
	return prefix + "#" + digest
}

func finalizeSessionInfo(info SessionInfo) (SessionInfo, bool) {
	if info.SessionID == "" {
		return SessionInfo{}, false
	}
	info.SessionID = BoundSessionIdentity(info.SessionID)
	info.ParentSessionID = BoundSessionIdentity(info.ParentSessionID)
	if info.ParentSessionID == info.SessionID {
		info.ParentSessionID = ""
		info.IsFork = false
		info.IsSubagent = false
	}
	if info.AgentName == "" {
		info.AgentName = "main"
	}
	if info.ClientType == "" {
		info.ClientType = "generic"
	}
	return info, true
}

func sessionHeaderValue(headers http.Header, name string) string {
	if headers == nil {
		return ""
	}
	if value := normalizedSessionCandidate(headers.Get(name)); value != "" {
		return value
	}
	for key, values := range headers {
		if !strings.EqualFold(strings.TrimSpace(key), strings.TrimSpace(name)) {
			continue
		}
		for _, value := range values {
			if normalized := normalizedSessionCandidate(value); normalized != "" {
				return normalized
			}
		}
	}
	return ""
}

func normalizedSessionCandidate(raw string) string {
	return NormalizeExplicitID(raw)
}
