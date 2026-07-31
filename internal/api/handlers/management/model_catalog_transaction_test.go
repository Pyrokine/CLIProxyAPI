package management

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/modelcatalog"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/usage"
	"github.com/gin-gonic/gin"
)

func TestModelCatalogMutationRestoresCatalogWhenConfigPersistenceFails(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.Mkdir(configPath, 0o700); err != nil {
		t.Fatalf("Mkdir config path: %v", err)
	}
	originalPrice := modelcatalog.Price{Prompt: 1, Completion: 2, Cache: 0.5}
	if err := modelcatalog.Save(
		configPath,
		modelcatalog.File{Models: []modelcatalog.Model{{Name: "original-model", Enabled: true, Price: &originalPrice}}},
	); err != nil {
		t.Fatalf("Save original catalog: %v", err)
	}
	var runtimeCalls atomic.Int32
	h := &Handler{
		cfg:            &config.Config{},
		configFilePath: configPath,
		modelCatalogRuntimeSync: func(*config.Config) {
			runtimeCalls.Add(1)
		},
	}

	response := performManagementJSONRequest(
		t,
		h.PutModelPrices,
		http.MethodPut,
		"/model-prices",
		gin.H{"prices": gin.H{"new-model": gin.H{"prompt": 3, "completion": 4, "cache": 1}}},
	)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusInternalServerError, response.Body.String())
	}
	if runtimeCalls.Load() != 0 {
		t.Fatalf("runtime sync calls = %d, want 0", runtimeCalls.Load())
	}
	doc, err := modelcatalog.Load(configPath)
	if err != nil {
		t.Fatalf("Load restored catalog: %v", err)
	}
	if _, ok := doc.Find("original-model"); !ok {
		t.Fatal("original-model missing after config persistence failure")
	}
	if _, ok := doc.Find("new-model"); ok {
		t.Fatal("new-model persisted after config persistence failure")
	}
	if len(h.configSnapshot().ModelCatalog.Models) != 0 {
		t.Fatalf("published config changed after failed transaction: %#v", h.configSnapshot().ModelCatalog.Models)
	}
}

func TestModelCatalogMutationRemovesNewCatalogWhenConfigPersistenceFails(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.Mkdir(configPath, 0o700); err != nil {
		t.Fatalf("Mkdir config path: %v", err)
	}
	h := &Handler{cfg: &config.Config{}, configFilePath: configPath}

	response := performManagementJSONRequest(
		t,
		h.PutModelPrices,
		http.MethodPut,
		"/model-prices",
		gin.H{"prices": gin.H{"new-model": gin.H{"prompt": 3, "completion": 4, "cache": 1}}},
	)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusInternalServerError, response.Body.String())
	}
	if _, err := os.Stat(modelcatalog.Path(configPath)); !os.IsNotExist(err) {
		t.Fatalf("models.json still exists after failed first commit: %v", err)
	}
	if h.configGeneration != 0 {
		t.Fatalf("config generation = %d, want 0", h.configGeneration)
	}
}

func TestModelCatalogRuntimePanicReturnsUnavailableAfterCommit(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(
		configPath,
		[]byte("oauth-model-alias:\n  codex:\n    - name: stale-model\n      alias: stale-alias\n"),
		0o600,
	); err != nil {
		t.Fatalf("write config: %v", err)
	}
	var configCommitCalls atomic.Int32
	var modelRuntimeCalls atomic.Int32
	h := &Handler{
		cfg:            &config.Config{},
		configFilePath: configPath,
		configCommitted: func(_ context.Context, _ ConfigCommit) error {
			configCommitCalls.Add(1)
			return nil
		},
		modelCatalogRuntimeSync: func(*config.Config) {
			modelRuntimeCalls.Add(1)
			panic("runtime panic")
		},
	}

	response := performManagementJSONRequest(
		t,
		h.PutModelPrices,
		http.MethodPut,
		"/model-prices",
		gin.H{"prices": gin.H{"committed-model": gin.H{"prompt": 3, "completion": 4, "cache": 1}}},
	)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusServiceUnavailable, response.Body.String())
	}
	if configCommitCalls.Load() != 1 {
		t.Fatalf("config committed calls = %d, want 1", configCommitCalls.Load())
	}
	if modelRuntimeCalls.Load() != 1 {
		t.Fatalf("model runtime calls = %d, want 1", modelRuntimeCalls.Load())
	}
	if h.configGeneration != 1 {
		t.Fatalf("config generation = %d, want 1", h.configGeneration)
	}
	doc, err := modelcatalog.Load(configPath)
	if err != nil {
		t.Fatalf("Load committed catalog: %v", err)
	}
	model, ok := doc.Find("committed-model")
	if !ok || model.Price == nil || model.Price.Prompt != 3 {
		t.Fatalf("committed-model = %#v", model)
	}
	persistedConfig, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if strings.Contains(string(persistedConfig), "oauth-model-alias") || strings.Contains(string(persistedConfig), "model-catalog") {
		t.Fatalf("external model fields remain in config.yaml: %s", persistedConfig)
	}
}

func TestConfigCallbackFailureUsesPublishedConfigForModelRuntime(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("port: 8317\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	var committed ConfigCommit
	var runtimeConfig *config.Config
	h := &Handler{
		cfg:            &config.Config{},
		configFilePath: configPath,
		configCommitted: func(_ context.Context, commit ConfigCommit) error {
			committed = commit
			panic("config callback panic")
		},
		modelCatalogRuntimeSync: func(cfg *config.Config) {
			runtimeConfig = cfg
		},
	}

	response := performManagementJSONRequest(
		t,
		h.PutModelPrices,
		http.MethodPut,
		"/model-prices",
		gin.H{"prices": gin.H{"callback-model": gin.H{"prompt": 3, "completion": 4, "cache": 1}}},
	)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusServiceUnavailable, response.Body.String())
	}
	if committed.Generation != 1 || committed.Config == nil {
		t.Fatalf("commit = %#v", committed)
	}
	if runtimeConfig != committed.Config {
		t.Fatalf("model runtime config = %p, want published config %p", runtimeConfig, committed.Config)
	}
	if h.configSnapshot() != committed.Config {
		t.Fatal("published config differs from callback generation")
	}
}

func TestConcurrentModelCatalogPriceAndAliasMutationsKeepAllUpdates(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("port: 8317\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	h := &Handler{cfg: &config.Config{}, configFilePath: configPath}
	start := make(chan struct{})
	responses := make(chan *httptest.ResponseRecorder, 3)
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		<-start
		responses <- performManagementJSONRequest(
			t,
			h.PatchModelCatalogModel,
			http.MethodPatch,
			"/model-catalog",
			gin.H{"name": "concurrent-model", "display_name": "Concurrent Model"},
		)
	}()
	go func() {
		defer wg.Done()
		<-start
		responses <- performManagementJSONRequest(
			t,
			h.PutOAuthModelAlias,
			http.MethodPut,
			"/oauth-model-alias",
			gin.H{"codex": []gin.H{{"name": "alias-model", "alias": "alias-name"}}},
		)
	}()
	go func() {
		defer wg.Done()
		<-start
		responses <- performManagementJSONRequest(
			t,
			h.PutModelPrices,
			http.MethodPut,
			"/model-prices",
			gin.H{"prices": gin.H{"priced-model": gin.H{"prompt": 7, "completion": 8, "cache": 2}}},
		)
	}()
	close(start)
	wg.Wait()
	close(responses)
	for response := range responses {
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", response.Code, response.Body.String())
		}
	}
	doc, err := modelcatalog.Load(configPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, ok := doc.Find("concurrent-model"); !ok {
		t.Fatal("concurrent-model missing")
	}
	if _, ok := doc.Find("alias-model"); !ok {
		t.Fatal("alias-model missing")
	}
	aliases := doc.OAuthModelAlias["codex"]
	if len(aliases) != 1 || aliases[0].Name != "alias-model" || aliases[0].Alias != "alias-name" || aliases[0].Fork {
		t.Fatalf("codex aliases = %#v", aliases)
	}
	pricedModel, ok := doc.Find("priced-model")
	if !ok || pricedModel.Price == nil || pricedModel.Price.Prompt != 7 || pricedModel.Price.Completion != 8 || pricedModel.Price.Cache != 2 {
		t.Fatalf("priced-model = %#v", pricedModel)
	}
}

func TestPutModelCatalogPreservesStoredOAuthAliasSemantics(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("port: 8317\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	doc := modelcatalog.File{}
	modelcatalog.ApplyOAuthModelAlias(
		&doc,
		map[string][]config.OAuthModelAlias{
			"vertex": {{Name: "gemini-3-pro-preview", Alias: "rename-only", ForceMapping: true}},
		},
	)
	if err := modelcatalog.Save(configPath, doc); err != nil {
		t.Fatalf("Save: %v", err)
	}
	h := &Handler{cfg: &config.Config{}, configFilePath: configPath}
	response := performManagementJSONRequest(
		t,
		h.PutModelCatalog,
		http.MethodPut,
		"/model-catalog",
		gin.H{"models": doc.Models},
	)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	reloaded, err := modelcatalog.Load(configPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	aliases := reloaded.OAuthModelAlias["vertex"]
	if len(aliases) != 1 || aliases[0].Alias != "rename-only" || aliases[0].Fork || !aliases[0].ForceMapping {
		t.Fatalf("vertex aliases = %#v", aliases)
	}
	for _, channel := range []string{"gemini-cli", "aistudio", "antigravity"} {
		if len(reloaded.OAuthModelAlias[channel]) != 0 {
			t.Fatalf("aliases[%s] = %#v, want none", channel, reloaded.OAuthModelAlias[channel])
		}
	}
}

func TestPatchModelCatalogAliasesReplacesStoredOAuthAliases(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("port: 8317\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	doc := modelcatalog.File{}
	modelcatalog.ApplyOAuthModelAlias(
		&doc,
		map[string][]config.OAuthModelAlias{
			"codex": {{Name: "alias-model", Alias: "rename-only"}},
		},
	)
	if err := modelcatalog.Save(configPath, doc); err != nil {
		t.Fatalf("Save: %v", err)
	}
	h := &Handler{cfg: &config.Config{}, configFilePath: configPath}
	response := performManagementJSONRequest(
		t,
		h.PatchModelCatalogModel,
		http.MethodPatch,
		"/model-catalog",
		gin.H{"name": "alias-model", "aliases": []string{"fork-only"}},
	)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	reloaded, err := modelcatalog.Load(configPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	aliases := reloaded.OAuthModelAlias["codex"]
	if len(aliases) != 1 || aliases[0].Alias != "fork-only" || !aliases[0].Fork {
		t.Fatalf("codex aliases = %#v", aliases)
	}

	response = performManagementJSONRequest(
		t,
		h.PatchModelCatalogModel,
		http.MethodPatch,
		"/model-catalog",
		gin.H{"name": "alias-model", "aliases": []string{}},
	)
	if response.Code != http.StatusOK {
		t.Fatalf("clear status = %d, body = %s", response.Code, response.Body.String())
	}
	reloaded, err = modelcatalog.Load(configPath)
	if err != nil {
		t.Fatalf("Load after clear: %v", err)
	}
	if len(reloaded.OAuthModelAlias) != 0 {
		t.Fatalf("aliases after clear = %#v", reloaded.OAuthModelAlias)
	}
}

func TestModelCatalogResponseIncludesStoredOAuthAliases(t *testing.T) {
	doc := modelcatalog.File{}
	modelcatalog.ApplyOAuthModelAlias(
		&doc,
		map[string][]config.OAuthModelAlias{
			"codex": {{Name: "alias-model", Alias: "rename-only"}},
		},
	)
	response := (&Handler{}).modelCatalogResponse(&config.Config{}, doc)
	if len(response.Models) != 1 {
		t.Fatalf("models = %#v", response.Models)
	}
	aliases := response.Models[0].Aliases
	if len(aliases) != 1 || aliases[0] != "rename-only" {
		t.Fatalf("aliases = %#v", aliases)
	}
}

func TestGetModelCatalogDoesNotPublishRuntime(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := modelcatalog.Save(
		configPath,
		modelcatalog.File{Models: []modelcatalog.Model{{Name: "read-only-model", Enabled: true}}},
	); err != nil {
		t.Fatalf("Save: %v", err)
	}
	var runtimeCalls atomic.Int32
	h := &Handler{
		cfg:            &config.Config{},
		configFilePath: configPath,
		modelCatalogRuntimeSync: func(*config.Config) {
			runtimeCalls.Add(1)
		},
	}
	response := performManagementJSONRequest(t, h.GetModelCatalog, http.MethodGet, "/model-catalog", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", response.Code, response.Body.String())
	}
	if runtimeCalls.Load() != 0 {
		t.Fatalf("runtime sync calls = %d, want 0", runtimeCalls.Load())
	}
	if h.configGeneration != 0 {
		t.Fatalf("config generation = %d, want 0", h.configGeneration)
	}
}

func TestPatchModelCatalogPriceStartsBackgroundRecalculation(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("port: 8317\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	initialPrice := modelcatalog.Price{Prompt: 1, Completion: 2, Cache: 0.5}
	if err := modelcatalog.Save(
		configPath,
		modelcatalog.File{Models: []modelcatalog.Model{{Name: "priced-model", Enabled: true, Price: &initialPrice}}},
	); err != nil {
		t.Fatalf("save catalog: %v", err)
	}
	persister, releaseDB := newLockedCatalogRecalculation(t, "priced-model")
	h := &Handler{cfg: &config.Config{}, configFilePath: configPath, usagePersister: persister}

	responseCh := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		responseCh <- performManagementJSONRequest(
			t,
			h.PatchModelCatalogModel,
			http.MethodPatch,
			"/model-catalog",
			gin.H{"name": "priced-model", "price": gin.H{"prompt": 3, "completion": 4, "cache": 1}},
		)
	}()
	var response *httptest.ResponseRecorder
	select {
	case response = <-responseCh:
	case <-time.After(time.Second):
		releaseDB()
		<-responseCh
		t.Fatal("price patch waited for historical cost recalculation")
	}
	if response.Code != http.StatusOK {
		releaseDB()
		t.Fatalf("status = %d, want 200; body=%s", response.Code, response.Body.String())
	}
	var responseBody modelCatalogResponse
	if err := json.Unmarshal(response.Body.Bytes(), &responseBody); err != nil {
		releaseDB()
		t.Fatalf("decode response: %v", err)
	}
	if !responseBody.RecalculationPending {
		releaseDB()
		t.Fatalf("recalculation_pending = false, want true; body=%s", response.Body.String())
	}
	status := persister.RecalculateCostsStatus()
	if status.Status != "running" || !status.Running {
		releaseDB()
		t.Fatalf("recalculation status = %+v, want running", status)
	}
	releaseDB()
	waitForCatalogRecalculation(t, persister)
}

func TestPatchModelCatalogMetadataDoesNotStartRecalculation(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("port: 8317\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := modelcatalog.Save(
		configPath,
		modelcatalog.File{Models: []modelcatalog.Model{{Name: "metadata-model", Enabled: true}}},
	); err != nil {
		t.Fatalf("save catalog: %v", err)
	}
	persister := usage.NewPersister(filepath.Join(dir, "usage"), config.UsageRetention{})
	h := &Handler{cfg: &config.Config{}, configFilePath: configPath, usagePersister: persister}

	response := performManagementJSONRequest(
		t,
		h.PatchModelCatalogModel,
		http.MethodPatch,
		"/model-catalog",
		gin.H{"name": "metadata-model", "display_name": "Metadata Model"},
	)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", response.Code, response.Body.String())
	}
	var responseBody modelCatalogResponse
	if err := json.Unmarshal(response.Body.Bytes(), &responseBody); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if responseBody.RecalculationPending {
		t.Fatalf("recalculation_pending = true, want false; body=%s", response.Body.String())
	}
	status := persister.RecalculateCostsStatus()
	if status.Status != "idle" || status.Running {
		t.Fatalf("metadata-only patch status = %+v, want idle", status)
	}
}

func TestApplyModelCatalogDefaultUpdateStartsBackgroundRecalculation(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("port: 8317\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	defaults := modelcatalog.DefaultFile()
	var target modelcatalog.Model
	for _, model := range defaults.Models {
		if model.Price != nil {
			target = model
			break
		}
	}
	if target.Name == "" || target.Price == nil {
		t.Fatal("default catalog has no priced model")
	}
	changedPrice := *target.Price
	changedPrice.Prompt++
	target.Price = &changedPrice
	if err := modelcatalog.Save(
		configPath,
		modelcatalog.File{
			SchemaVersion:   modelcatalog.SchemaVersion,
			DefaultsVersion: "previous-defaults",
			Models:          []modelcatalog.Model{target},
		},
	); err != nil {
		t.Fatalf("save catalog: %v", err)
	}
	persister, releaseDB := newLockedCatalogRecalculation(t, target.Name)
	h := &Handler{cfg: &config.Config{}, configFilePath: configPath, usagePersister: persister}

	responseCh := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		responseCh <- performManagementJSONRequest(
			t,
			h.ApplyModelCatalogDefaultUpdate,
			http.MethodPost,
			"/model-catalog/default-update/apply",
			gin.H{"decisions": []gin.H{{"name": target.Name, "action": "use_default"}}},
		)
	}()
	var response *httptest.ResponseRecorder
	select {
	case response = <-responseCh:
	case <-time.After(time.Second):
		releaseDB()
		<-responseCh
		t.Fatal("default update waited for historical cost recalculation")
	}
	if response.Code != http.StatusOK {
		releaseDB()
		t.Fatalf("status = %d, want 200; body=%s", response.Code, response.Body.String())
	}
	var responseBody struct {
		Catalog modelCatalogResponse `json:"catalog"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &responseBody); err != nil {
		releaseDB()
		t.Fatalf("decode response: %v", err)
	}
	if !responseBody.Catalog.RecalculationPending {
		releaseDB()
		t.Fatalf("recalculation_pending = false, want true; body=%s", response.Body.String())
	}
	status := persister.RecalculateCostsStatus()
	if status.Status != "running" || !status.Running {
		releaseDB()
		t.Fatalf("recalculation status = %+v, want running", status)
	}
	releaseDB()
	waitForCatalogRecalculation(t, persister)
}

func newLockedCatalogRecalculation(t *testing.T, model string) (*usage.Persister, func()) {
	t.Helper()
	baseDir := t.TempDir()
	persister := usage.NewPersister(baseDir, config.UsageRetention{})
	persister.Start(context.Background())
	if !persister.IsReady() {
		t.Fatal("persister did not become ready")
	}
	t.Cleanup(persister.Stop)
	payload, err := json.Marshal([]map[string]any{{
		"timestamp": "2026-07-31T00:00:00Z",
		"model":     model,
		"completed": true,
		"tokens": map[string]int64{
			"input_tokens": 1,
			"total_tokens": 1,
		},
	}})
	if err != nil {
		t.Fatalf("marshal usage event: %v", err)
	}
	if _, err := persister.Import(payload); err != nil {
		t.Fatalf("seed usage event: %v", err)
	}
	locker, err := sql.Open("sqlite", filepath.Join(baseDir, "events.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open lock connection: %v", err)
	}
	if _, err := locker.Exec("BEGIN IMMEDIATE"); err != nil {
		_ = locker.Close()
		t.Fatalf("lock usage database: %v", err)
	}
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() {
			_, _ = locker.Exec("ROLLBACK")
			_ = locker.Close()
		})
	}
	t.Cleanup(release)
	return persister, release
}

func waitForCatalogRecalculation(t *testing.T, persister *usage.Persister) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		status := persister.RecalculateCostsStatus()
		if !status.Running {
			if status.Status != "ok" || status.Error != "" {
				t.Fatalf("completed status = %+v", status)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("recalculation did not finish: %+v", status)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func performManagementJSONRequest(
	t *testing.T,
	handler gin.HandlerFunc,
	method string,
	target string,
	body any,
) *httptest.ResponseRecorder {
	t.Helper()
	var requestBody bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&requestBody).Encode(body); err != nil {
			t.Fatalf("encode request body: %v", err)
		}
	}
	response := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(response)
	ctx.Request = httptest.NewRequest(method, target, &requestBody)
	if body != nil {
		ctx.Request.Header.Set("Content-Type", "application/json")
	}
	handler(ctx)
	return response
}
