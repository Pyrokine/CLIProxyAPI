package handlers

import "context"

// DirectLoopbackIdentity identifies the authenticated keyless local execution mode.
const DirectLoopbackIdentity = "local:direct-loopback"

// VideoTask records the immutable creator and upstream routing of a video task.
type VideoTask struct {
	ID            string `json:"id"`
	Provider      string `json:"provider"`
	OwnerIdentity string `json:"owner_identity"`
	AuthID        string `json:"auth_id"`
	Model         string `json:"model"`
}

// VideoTaskStore keeps ownership independently of usage recording and auth caches.
type VideoTaskStore interface {
	CheckVideoTaskStorage(context.Context) error
	SaveVideoTask(context.Context, VideoTask) error
	LoadVideoTask(context.Context, string, string) (VideoTask, bool, error)
}

func (h *BaseAPIHandler) SetVideoTaskStore(store VideoTaskStore) {
	if h == nil {
		return
	}
	h.cfgMu.Lock()
	h.videoTaskStore = store
	h.cfgMu.Unlock()
}

func (h *BaseAPIHandler) VideoTaskStoreSnapshot() VideoTaskStore {
	if h == nil {
		return nil
	}
	h.cfgMu.RLock()
	defer h.cfgMu.RUnlock()
	return h.videoTaskStore
}
