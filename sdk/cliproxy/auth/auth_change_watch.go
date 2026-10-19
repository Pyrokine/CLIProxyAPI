package auth

// WatchAuthChanges 观察单个凭证的注册、准备、刷新、替换、删除和存储重载
// 通知会合并，调用方须读取 Manager 的最新状态，并在任务结束时取消订阅
// 普通请求结果记账不发送通知
func (m *Manager) WatchAuthChanges(authID string) (<-chan struct{}, func()) {
	changes := make(chan struct{}, 1)
	if m == nil || authID == "" {
		close(changes)
		return changes, func() {}
	}
	m.mu.Lock()
	if m.authChangeWatchers == nil {
		m.authChangeWatchers = make(map[string]map[chan struct{}]struct{})
	}
	if m.authChangeWatchers[authID] == nil {
		m.authChangeWatchers[authID] = make(map[chan struct{}]struct{})
	}
	m.authChangeWatchers[authID][changes] = struct{}{}
	m.mu.Unlock()
	return changes, func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		watchers := m.authChangeWatchers[authID]
		if _, exists := watchers[changes]; !exists {
			return
		}
		delete(watchers, changes)
		if len(watchers) == 0 {
			delete(m.authChangeWatchers, authID)
		}
		close(changes)
	}
}

// 调用方持有 m.mu，锁内只发送非阻塞通知
func (m *Manager) notifyAuthChangeLocked(authID string) {
	if m.auths[authID] == nil {
		antigravityCreditsHintMu.Lock()
		antigravityCreditsHintByAuth.Delete(authID)
		antigravityCreditsHintMu.Unlock()
	}
	for changes := range m.authChangeWatchers[authID] {
		select {
		case changes <- struct{}{}:
		default:
		}
	}
}
