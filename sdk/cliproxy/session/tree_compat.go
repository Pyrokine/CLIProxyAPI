package session

import (
	"maps"
	"sync"
	"time"
)

// SessionTreeInfo 与平面会话身份使用相同字段
// Deprecated: 层级会话管理由 Home 提供
//
//goland:noinspection GoDeprecation,GoNameStartsWithPackageName
type SessionTreeInfo = SessionInfo

// SessionTreeNode 保存会话及其直接父会话的记录
// Deprecated: 层级会话管理由 Home 提供
//
//goland:noinspection GoDeprecation,GoNameStartsWithPackageName
type SessionTreeNode struct {
	SessionID       string         `json:"session_id"`
	ParentSessionID string         `json:"parent_session_id,omitempty"`
	TreePath        string         `json:"tree_path"`
	TreeDepth       int            `json:"tree_depth"`
	AgentName       string         `json:"agent_name,omitempty"`
	ClientType      string         `json:"client_type,omitempty"`
	CallerScope     string         `json:"caller_scope,omitempty"`
	LastAuthID      string         `json:"last_auth_id,omitempty"`
	LastProvider    string         `json:"last_provider,omitempty"`
	LastModel       string         `json:"last_model,omitempty"`
	NodeKind        string         `json:"node_kind,omitempty"`
	IsFork          bool           `json:"is_fork,omitempty"`
	IsCompaction    bool           `json:"is_compaction,omitempty"`
	CreatedAt       time.Time      `json:"created_at"`
	UpdatedAt       time.Time      `json:"updated_at"`
	Metadata        map[string]any `json:"metadata,omitempty"`
}

// Clone 复制节点与顶层元数据，元数据内的值保持原类型
// noinspection GoDeprecation
func (n *SessionTreeNode) Clone() *SessionTreeNode {
	if n == nil {
		return nil
	}
	snapshot := *n
	snapshot.Metadata = maps.Clone(n.Metadata)
	return &snapshot
}

// SessionTreeStore 由调用方记录、查询并清理独立的会话记录
// Deprecated: 层级会话管理由 Home 提供
//
//goland:noinspection GoDeprecation,GoNameStartsWithPackageName
type SessionTreeStore interface {
	RecordNode(info SessionTreeInfo) *SessionTreeNode
	GetNode(sessionID string) (*SessionTreeNode, bool)
	GetTree(rootSessionID string) []*SessionTreeNode
	GetSubtree(sessionID string) []*SessionTreeNode
	Ancestors(sessionID string) []string
	UpdateAffinity(sessionID, authID, provider, model string) bool
	Len() int
	Clear()
}

// InMemorySessionTreeStore 保存独立于路由绑定的会话记录，生命周期由调用方 Clear 管理
// Deprecated: 层级会话管理由 Home 提供
//
// noinspection GoDeprecation
type InMemorySessionTreeStore struct {
	mu    sync.RWMutex
	nodes map[string]*SessionTreeNode
}

// NewInMemorySessionTreeStore 创建由调用方管理的会话记录
// Deprecated: 层级会话管理由 Home 提供
//
// noinspection GoDeprecation
func NewInMemorySessionTreeStore(_ int, _ time.Duration) *InMemorySessionTreeStore {
	return &InMemorySessionTreeStore{nodes: make(map[string]*SessionTreeNode)}
}

// noinspection GoDeprecation
func (s *InMemorySessionTreeStore) RecordNode(info SessionTreeInfo) *SessionTreeNode {
	if s == nil || info.SessionID == "" {
		return nil
	}
	now := time.Now()
	node := &SessionTreeNode{
		SessionID: info.SessionID, ParentSessionID: info.ParentSessionID,
		TreePath: info.SessionID, AgentName: info.AgentName, ClientType: info.ClientType,
		CallerScope: info.CallerScope, LastAuthID: info.AuthID, LastProvider: info.Provider,
		LastModel: info.Model, NodeKind: info.NodeKind, IsFork: info.IsFork,
		IsCompaction: info.IsCompaction, CreatedAt: now, UpdatedAt: now,
		Metadata: maps.Clone(info.Metadata),
	}
	if info.ParentSessionID != "" {
		node.TreePath = info.ParentSessionID + "/" + info.SessionID
		node.TreeDepth = 1
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.nodes == nil {
		s.nodes = make(map[string]*SessionTreeNode)
	}
	s.nodes[info.SessionID] = node
	return node.Clone()
}

// noinspection GoDeprecation
func (s *InMemorySessionTreeStore) GetNode(sessionID string) (*SessionTreeNode, bool) {
	if s == nil {
		return nil, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	node, exists := s.nodes[sessionID]
	return node.Clone(), exists
}

// noinspection GoDeprecation
func (s *InMemorySessionTreeStore) GetTree(rootSessionID string) []*SessionTreeNode {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var nodes []*SessionTreeNode
	for _, node := range s.nodes {
		if node.SessionID == rootSessionID || node.ParentSessionID == rootSessionID {
			nodes = append(nodes, node.Clone())
		}
	}
	return nodes
}

// noinspection GoDeprecation
func (s *InMemorySessionTreeStore) GetSubtree(sessionID string) []*SessionTreeNode {
	return s.GetTree(sessionID)
}

// noinspection GoDeprecation
func (s *InMemorySessionTreeStore) Ancestors(sessionID string) []string {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if node := s.nodes[sessionID]; node != nil && node.ParentSessionID != "" {
		return []string{node.ParentSessionID}
	}
	return nil
}

// noinspection GoDeprecation
func (s *InMemorySessionTreeStore) UpdateAffinity(sessionID, authID, provider, model string) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	node := s.nodes[sessionID]
	if node == nil {
		return false
	}
	if authID != "" {
		node.LastAuthID = authID
	}
	if provider != "" {
		node.LastProvider = provider
	}
	if model != "" {
		node.LastModel = model
	}
	node.UpdatedAt = time.Now()
	return true
}

// noinspection GoDeprecation
func (s *InMemorySessionTreeStore) Len() int {
	if s == nil {
		return 0
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.nodes)
}

// noinspection GoDeprecation
func (s *InMemorySessionTreeStore) Clear() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nodes = nil
}
