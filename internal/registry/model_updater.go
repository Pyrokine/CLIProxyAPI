package registry

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	modelsFetchTimeout           = 30 * time.Second
	defaultModelsRefreshInterval = 3 * time.Hour
)

var configuredRefreshInterval = defaultModelsRefreshInterval

var modelsURLs = []string{
	"https://raw.githubusercontent.com/router-for-me/models/refs/heads/main/models.json",
	"https://models.router-for.me/models.json",
}

//go:embed models/models.json
var embeddedModelsJSON []byte

// staticModelsJSON mirrors the top-level structure of models.json.
type staticModelsJSON struct {
	Claude      []*ModelInfo `json:"claude"`
	Gemini      []*ModelInfo `json:"gemini"`
	Vertex      []*ModelInfo `json:"vertex"`
	GeminiCLI   []*ModelInfo `json:"gemini-cli"`
	AIStudio    []*ModelInfo `json:"aistudio"`
	CodexFree   []*ModelInfo `json:"codex-free"`
	CodexTeam   []*ModelInfo `json:"codex-team"`
	CodexPlus   []*ModelInfo `json:"codex-plus"`
	CodexPro    []*ModelInfo `json:"codex-pro"`
	Qwen        []*ModelInfo `json:"qwen"`
	IFlow       []*ModelInfo `json:"iflow"`
	Kimi        []*ModelInfo `json:"kimi"`
	Antigravity []*ModelInfo `json:"antigravity"`
}

type modelStore struct {
	mu   sync.RWMutex
	data *staticModelsJSON
}

var modelsCatalogStore = &modelStore{}

// CatalogMeta describes the current state of the model catalog so the management UI
// can show where the data came from and when the next refresh is due.
type CatalogMeta struct {
	LastRefreshAt time.Time `json:"last_refresh_at"`
	NextRefreshAt time.Time `json:"next_refresh_at"`
	IntervalHours int       `json:"interval_hours"`
	Source        string    `json:"source"`
	LastError     string    `json:"last_error,omitempty"`
	Sources       []string  `json:"sources"`
}

type catalogMetaStore struct {
	mu   sync.RWMutex
	meta CatalogMeta
}

var modelsCatalogMetaStore = &catalogMetaStore{
	meta: CatalogMeta{Source: "embed"},
}

var updaterOnce sync.Once

// ModelRefreshCallback is invoked when startup or periodic model refresh detects changes.
// changedProviders contains the provider names whose model definitions changed.
type ModelRefreshCallback func(changedProviders []string)

var (
	refreshCallbackMu     sync.Mutex
	refreshCallback       ModelRefreshCallback
	pendingRefreshChanges []string
)

// SetModelRefreshCallback registers a callback that is invoked when startup or
// periodic model refresh detects changes. Only one callback is supported;
// subsequent calls replace the previous callback.
// noinspection GoUnusedExportedFunction — public API for external integrations.
func SetModelRefreshCallback(cb ModelRefreshCallback) {
	refreshCallbackMu.Lock()
	refreshCallback = cb
	var pending []string
	if cb != nil && len(pendingRefreshChanges) > 0 {
		pending = append([]string(nil), pendingRefreshChanges...)
		pendingRefreshChanges = nil
	}
	refreshCallbackMu.Unlock()

	if cb != nil && len(pending) > 0 {
		cb(pending)
	}
}

func init() {
	// Load embedded data as fallback on startup.
	if err := loadModelsFromBytes(embeddedModelsJSON, "embed"); err != nil {
		panic(fmt.Sprintf("registry: failed to parse embedded models.json: %v", err))
	}
	modelsCatalogMetaStore.mu.Lock()
	modelsCatalogMetaStore.meta.Sources = append([]string(nil), modelsURLs...)
	modelsCatalogMetaStore.meta.IntervalHours = int(defaultModelsRefreshInterval / time.Hour)
	modelsCatalogMetaStore.mu.Unlock()
}

// StartModelsUpdater starts a background updater that fetches models
// immediately on startup and then refreshes the model catalog periodically.
// refreshHours sets the interval in hours; 0 or negative disables periodic refresh.
// Safe to call multiple times; only one updater will run.
func StartModelsUpdater(ctx context.Context, refreshHours int) {
	updaterOnce.Do(
		func() {
			if refreshHours > 0 {
				configuredRefreshInterval = time.Duration(refreshHours) * time.Hour
			}
			go runModelsUpdater(ctx)
		},
	)
}

func runModelsUpdater(ctx context.Context) {
	tryStartupRefresh(ctx)
	periodicRefresh(ctx)
}

func periodicRefresh(ctx context.Context) {
	ticker := time.NewTicker(configuredRefreshInterval)
	defer ticker.Stop()
	log.Infof("periodic model refresh started (interval=%s)", configuredRefreshInterval)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			tryPeriodicRefresh(ctx)
		}
	}
}

// tryPeriodicRefresh fetches models from remote, compares with the current
// catalog, and notifies the registered callback if any provider changed.
func tryPeriodicRefresh(ctx context.Context) {
	tryRefreshModels(ctx, "periodic model refresh")
}

// tryStartupRefresh fetches models from remote in the background during
// process startup. It uses the same change detection as periodic refresh so
// existing auth registrations can be updated after the callback is registered.
func tryStartupRefresh(ctx context.Context) {
	tryRefreshModels(ctx, "startup model refresh")
}

func tryRefreshModels(ctx context.Context, label string) {
	oldData := getModels()

	parsed, url := fetchModelsFromRemote(ctx)
	if parsed == nil {
		log.Warnf("%s: fetch failed from all URLs, keeping current data", label)
		updateCatalogMetaAfterRefresh("", "fetch failed from all URLs")
		return
	}

	// Detect changes before updating store.
	changed := detectChangedProviders(oldData, parsed)

	// Update store with new data regardless.
	modelsCatalogStore.mu.Lock()
	modelsCatalogStore.data = parsed
	modelsCatalogStore.mu.Unlock()

	updateCatalogMetaAfterRefresh(url, "")

	if len(changed) == 0 {
		log.Infof("%s completed from %s, no changes detected", label, url)
		return
	}

	log.Infof("%s completed from %s, changes detected for providers: %v", label, url, changed)
	notifyModelRefresh(changed)
}

func updateCatalogMetaAfterRefresh(source, errMsg string) {
	now := time.Now()
	interval := configuredRefreshInterval
	modelsCatalogMetaStore.mu.Lock()
	modelsCatalogMetaStore.meta.LastRefreshAt = now
	if interval > 0 {
		modelsCatalogMetaStore.meta.NextRefreshAt = now.Add(interval)
		modelsCatalogMetaStore.meta.IntervalHours = int(interval / time.Hour)
	} else {
		modelsCatalogMetaStore.meta.NextRefreshAt = time.Time{}
		modelsCatalogMetaStore.meta.IntervalHours = 0
	}
	if source != "" {
		modelsCatalogMetaStore.meta.Source = source
	}
	modelsCatalogMetaStore.meta.LastError = errMsg
	modelsCatalogMetaStore.meta.Sources = append(modelsCatalogMetaStore.meta.Sources[:0], modelsURLs...)
	modelsCatalogMetaStore.mu.Unlock()
}

// GetCatalogMeta returns a snapshot of the current model catalog refresh state.
func GetCatalogMeta() CatalogMeta {
	modelsCatalogMetaStore.mu.RLock()
	defer modelsCatalogMetaStore.mu.RUnlock()
	snapshot := modelsCatalogMetaStore.meta
	snapshot.Sources = append([]string(nil), modelsCatalogMetaStore.meta.Sources...)
	return snapshot
}

// TriggerCatalogRefresh asks the updater to re-fetch the catalog immediately.
// It runs synchronously and returns the new meta snapshot along with any
// error recorded by the refresh attempt.
func TriggerCatalogRefresh(ctx context.Context) (CatalogMeta, error) {
	tryRefreshModels(ctx, "manual model refresh")
	meta := GetCatalogMeta()
	if meta.LastError != "" {
		return meta, fmt.Errorf("%s", meta.LastError)
	}
	return meta, nil
}

// fetchModelsFromRemote tries all remote URLs and returns the parsed model catalog
// along with the URL it was fetched from. Returns (nil, "") if all fetches fail.
func fetchModelsFromRemote(ctx context.Context) (*staticModelsJSON, string) {
	client := &http.Client{Timeout: modelsFetchTimeout}
	for _, url := range modelsURLs {
		reqCtx, cancel := context.WithTimeout(ctx, modelsFetchTimeout)
		req, err := http.NewRequestWithContext(reqCtx, "GET", url, nil)
		if err != nil {
			cancel()
			log.Debugf("models fetch request creation failed for %s: %v", url, err)
			continue
		}

		resp, err := client.Do(req)
		if err != nil {
			cancel()
			log.Debugf("models fetch failed from %s: %v", url, err)
			continue
		}

		if resp.StatusCode != 200 {
			_ = resp.Body.Close()
			cancel()
			log.Debugf("models fetch returned %d from %s", resp.StatusCode, url)
			continue
		}

		data, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
		_ = resp.Body.Close()
		cancel()

		if err != nil {
			log.Debugf("models fetch read error from %s: %v", url, err)
			continue
		}

		var parsed staticModelsJSON
		if err := json.Unmarshal(data, &parsed); err != nil {
			log.Warnf("models parse failed from %s: %v", url, err)
			continue
		}
		if err := validateModelsCatalog(&parsed); err != nil {
			log.Warnf("models validate failed from %s: %v", url, err)
			continue
		}

		return &parsed, url
	}
	return nil, ""
}

// detectChangedProviders compares two model catalogs and returns provider names
// whose model definitions differ. Codex tiers (free/team/plus/pro) are grouped
// under a single "codex" provider.
func detectChangedProviders(oldData, newData *staticModelsJSON) []string {
	if oldData == nil || newData == nil {
		return nil
	}

	type section struct {
		provider string
		oldList  []*ModelInfo
		newList  []*ModelInfo
	}

	sections := []section{
		{"claude", oldData.Claude, newData.Claude},
		{"gemini", oldData.Gemini, newData.Gemini},
		{"vertex", oldData.Vertex, newData.Vertex},
		{"gemini-cli", oldData.GeminiCLI, newData.GeminiCLI},
		{"aistudio", oldData.AIStudio, newData.AIStudio},
		{"codex", oldData.CodexFree, newData.CodexFree},
		{"codex", oldData.CodexTeam, newData.CodexTeam},
		{"codex", oldData.CodexPlus, newData.CodexPlus},
		{"codex", oldData.CodexPro, newData.CodexPro},
		{"qwen", oldData.Qwen, newData.Qwen},
		{"iflow", oldData.IFlow, newData.IFlow},
		{"kimi", oldData.Kimi, newData.Kimi},
		{"antigravity", oldData.Antigravity, newData.Antigravity},
	}

	seen := make(map[string]bool, len(sections))
	var changed []string
	for _, s := range sections {
		if seen[s.provider] {
			continue
		}
		if modelSectionChanged(s.oldList, s.newList) {
			changed = append(changed, s.provider)
			seen[s.provider] = true
		}
	}
	return changed
}

// modelSectionChanged reports whether two model slices differ.
func modelSectionChanged(a, b []*ModelInfo) bool {
	if len(a) != len(b) {
		return true
	}
	if len(a) == 0 {
		return false
	}
	aj, err1 := json.Marshal(a)
	bj, err2 := json.Marshal(b)
	if err1 != nil || err2 != nil {
		return true
	}
	return string(aj) != string(bj)
}

func notifyModelRefresh(changedProviders []string) {
	if len(changedProviders) == 0 {
		return
	}

	refreshCallbackMu.Lock()
	cb := refreshCallback
	if cb == nil {
		pendingRefreshChanges = mergeProviderNames(pendingRefreshChanges, changedProviders)
		refreshCallbackMu.Unlock()
		return
	}
	refreshCallbackMu.Unlock()
	cb(changedProviders)
}

func mergeProviderNames(existing, incoming []string) []string {
	if len(incoming) == 0 {
		return existing
	}
	seen := make(map[string]struct{}, len(existing)+len(incoming))
	merged := make([]string, 0, len(existing)+len(incoming))
	for _, provider := range existing {
		name := strings.ToLower(strings.TrimSpace(provider))
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		merged = append(merged, name)
	}
	for _, provider := range incoming {
		name := strings.ToLower(strings.TrimSpace(provider))
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		merged = append(merged, name)
	}
	return merged
}

func loadModelsFromBytes(data []byte, source string) error {
	var parsed staticModelsJSON
	if err := json.Unmarshal(data, &parsed); err != nil {
		return fmt.Errorf("%s: decode models catalog: %w", source, err)
	}
	if err := validateModelsCatalog(&parsed); err != nil {
		return fmt.Errorf("%s: validate models catalog: %w", source, err)
	}

	modelsCatalogStore.mu.Lock()
	modelsCatalogStore.data = &parsed
	modelsCatalogStore.mu.Unlock()
	return nil
}

func getModels() *staticModelsJSON {
	modelsCatalogStore.mu.RLock()
	defer modelsCatalogStore.mu.RUnlock()
	return modelsCatalogStore.data
}

func validateModelsCatalog(data *staticModelsJSON) error {
	if data == nil {
		return fmt.Errorf("catalog is nil")
	}

	sections := []struct {
		name   string
		models []*ModelInfo
	}{
		{name: "claude", models: data.Claude},
		{name: "gemini", models: data.Gemini},
		{name: "vertex", models: data.Vertex},
		{name: "gemini-cli", models: data.GeminiCLI},
		{name: "aistudio", models: data.AIStudio},
		{name: "codex-free", models: data.CodexFree},
		{name: "codex-team", models: data.CodexTeam},
		{name: "codex-plus", models: data.CodexPlus},
		{name: "codex-pro", models: data.CodexPro},
		{name: "qwen", models: data.Qwen},
		{name: "iflow", models: data.IFlow},
		{name: "kimi", models: data.Kimi},
		{name: "antigravity", models: data.Antigravity},
	}

	// Upstream occasionally ships a catalog with some providers removed (e.g. qwen
	// was dropped, then restored). Rejecting the whole catalog for a missing
	// section forces the server onto the embedded fallback and surfaces
	// "fetch failed from all URLs" every interval — even though the rest of the
	// catalog is valid. Accept the catalog as long as it is not fully empty and
	// every populated section passes structural validation.
	nonEmpty := 0
	for _, section := range sections {
		if len(section.models) == 0 {
			log.Debugf("models catalog: %s section is empty (accepted)", section.name)
			continue
		}
		if err := validateModelSection(section.name, section.models); err != nil {
			return err
		}
		nonEmpty++
	}
	if nonEmpty == 0 {
		return fmt.Errorf("catalog has no populated provider sections")
	}
	return nil
}

func validateModelSection(section string, models []*ModelInfo) error {
	seen := make(map[string]struct{}, len(models))
	for i, model := range models {
		if model == nil {
			return fmt.Errorf("%s[%d] is null", section, i)
		}
		modelID := strings.TrimSpace(model.ID)
		if modelID == "" {
			return fmt.Errorf("%s[%d] has empty id", section, i)
		}
		if _, exists := seen[modelID]; exists {
			return fmt.Errorf("%s contains duplicate model id %q", section, modelID)
		}
		seen[modelID] = struct{}{}
	}
	return nil
}
