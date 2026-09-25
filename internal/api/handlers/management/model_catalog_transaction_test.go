package management

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/usage"
	"github.com/gin-gonic/gin"
)

func TestApplyModelCatalogRuntimeReturnsSyncError(t *testing.T) {
	wantErr := errors.New("runtime sync failed")
	h := &Handler{
		modelCatalogRuntimeSync: func(*config.Config) error {
			return wantErr
		},
	}
	t.Cleanup(func() { config.SetRuntimeModelCatalog(nil) })

	errApply := h.applyModelCatalogRuntime(&config.Config{}, modelcatalog.File{})
	if !errors.Is(errApply, wantErr) {
		t.Fatalf("applyModelCatalogRuntime() error = %v, want %v", errApply, wantErr)
	}
}

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
		modelCatalogRuntimeSync: func(*config.Config) error {
			runtimeCalls.Add(1)
			return nil
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
	if _, err := os.Stat(modelcatalog.PreviousPath(configPath)); !os.IsNotExist(err) {
		t.Fatalf("models.previous.json still exists after failed first commit: %v", err)
	}
	if h.configGeneration != 0 {
		t.Fatalf("config generation = %d, want 0", h.configGeneration)
	}
}

func TestModelCatalogRuntimePanicRestoresConfigRuntimeAndCatalog(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("port: 8317\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	originalPrice := modelcatalog.Price{Prompt: 1, Completion: 2, Cache: 0.5}
	if err := modelcatalog.Save(
		configPath,
		modelcatalog.File{
			Models: []modelcatalog.Model{
				{
					Name: "original-model", Enabled: false, Price: &originalPrice,
				},
			},
		},
	); err != nil {
		t.Fatalf("Save original catalog: %v", err)
	}
	if _, err := modelcatalog.Load(configPath); err != nil {
		t.Fatalf("initialize previous catalog: %v", err)
	}
	previousBefore, err := os.ReadFile(modelcatalog.PreviousPath(configPath))
	if err != nil {
		t.Fatalf("read previous catalog: %v", err)
	}
	var commits []ConfigCommit
	var modelRuntimeCalls atomic.Int32
	h := &Handler{
		cfg:            &config.Config{},
		configFilePath: configPath,
		configCommitted: func(_ context.Context, commit ConfigCommit) error {
			commits = append(commits, commit)
			return nil
		},
		modelCatalogRuntimeSync: func(*config.Config) error {
			modelRuntimeCalls.Add(1)
			panic("runtime panic")
		},
	}

	response := performManagementJSONRequest(
		t,
		h.PatchModelCatalogModel,
		http.MethodPatch,
		"/model-catalog",
		gin.H{"name": "original-model", "enabled": true},
	)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusServiceUnavailable, response.Body.String())
	}
	if len(commits) != 2 {
		t.Fatalf("config committed calls = %d, want 2", len(commits))
	}
	if !commits[0].ModelCatalogPrepared || !commits[1].ModelCatalogPrepared {
		t.Fatalf("prepared flags = %t, %t", commits[0].ModelCatalogPrepared, commits[1].ModelCatalogPrepared)
	}
	if commits[0].Generation != 1 || commits[1].Generation != 2 {
		t.Fatalf("commit generations = %d, %d, want 1, 2", commits[0].Generation, commits[1].Generation)
	}
	if modelRuntimeCalls.Load() != 2 {
		t.Fatalf("model runtime calls = %d, want 2", modelRuntimeCalls.Load())
	}
	if h.configGeneration != 2 {
		t.Fatalf("config generation = %d, want 2", h.configGeneration)
	}
	doc, err := modelcatalog.Load(configPath)
	if err != nil {
		t.Fatalf("Load restored catalog: %v", err)
	}
	restoredModel, ok := doc.Find("original-model")
	if !ok || restoredModel.Enabled {
		t.Fatalf("restored model = %#v, %t", restoredModel, ok)
	}
	previousAfter, err := os.ReadFile(modelcatalog.PreviousPath(configPath))
	if err != nil {
		t.Fatalf("read restored previous catalog: %v", err)
	}
	if !bytes.Equal(previousAfter, previousBefore) {
		t.Fatal("previous catalog advanced before runtime commit succeeded")
	}
	restoredConfig := h.configSnapshot()
	var restoredConfigModel *config.ModelCatalogModel
	for index := range restoredConfig.ModelCatalog.Models {
		if strings.EqualFold(restoredConfig.ModelCatalog.Models[index].Name, "original-model") {
			restoredConfigModel = &restoredConfig.ModelCatalog.Models[index]
			break
		}
	}
	if restoredConfigModel == nil || restoredConfigModel.Enabled == nil || *restoredConfigModel.Enabled {
		t.Fatalf("handler config model = %#v", restoredConfigModel)
	}
	if restoredConfig.ModelCatalogModelEnabled("original-model") {
		t.Fatal("runtime catalog retained failed enabled state")
	}
}

func TestConfigCallbackFailurePublishesCompensatingGeneration(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("port: 8317\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	originalPrice := modelcatalog.Price{Prompt: 1, Completion: 2, Cache: 0.5}
	if err := modelcatalog.Save(
		configPath,
		modelcatalog.File{
			Models: []modelcatalog.Model{
				{
					Name: "original-model", Enabled: true, Price: &originalPrice,
				},
			},
		},
	); err != nil {
		t.Fatalf("Save original catalog: %v", err)
	}
	if _, err := modelcatalog.Load(configPath); err != nil {
		t.Fatalf("initialize previous catalog: %v", err)
	}
	var commits []ConfigCommit
	var runtimeConfig *config.Config
	h := &Handler{
		cfg:            &config.Config{},
		configFilePath: configPath,
		configCommitted: func(_ context.Context, commit ConfigCommit) error {
			commits = append(commits, commit)
			if len(commits) == 1 {
				panic("config callback panic")
			}
			return nil
		},
		modelCatalogRuntimeSync: func(cfg *config.Config) error {
			runtimeConfig = cfg
			return nil
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
	if len(commits) != 2 {
		t.Fatalf("commits = %d, want 2", len(commits))
	}
	candidateCommit := commits[0]
	rollbackCommit := commits[1]
	if candidateCommit.Generation != 1 || rollbackCommit.Generation != 2 {
		t.Fatalf("commit generations = %d, %d, want 1, 2", candidateCommit.Generation, rollbackCommit.Generation)
	}
	if !candidateCommit.ModelCatalogPrepared || !rollbackCommit.ModelCatalogPrepared {
		t.Fatalf(
			"prepared flags = %t, %t",
			candidateCommit.ModelCatalogPrepared,
			rollbackCommit.ModelCatalogPrepared,
		)
	}
	if runtimeConfig != rollbackCommit.Config {
		t.Fatalf("model runtime config = %p, want rollback config %p", runtimeConfig, rollbackCommit.Config)
	}
	if h.configSnapshot() != rollbackCommit.Config {
		t.Fatal("handler did not retain compensating config generation")
	}
	if h.configSnapshot() == candidateCommit.Config {
		t.Fatal("handler retained failed candidate config")
	}
	doc, err := modelcatalog.Load(configPath)
	if err != nil {
		t.Fatalf("Load restored catalog: %v", err)
	}
	if _, ok := doc.Price("original-model"); !ok {
		t.Fatal("original-model price missing after callback rollback")
	}
	if _, ok := doc.Price("callback-model"); ok {
		t.Fatal("callback-model price persisted after callback rollback")
	}
}

func TestModelCatalogFileConflictRestoresCompensatingConfigAndRuntime(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("port: 8317\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := modelcatalog.Save(
		configPath,
		modelcatalog.File{Models: []modelcatalog.Model{{Name: "base-model", Enabled: false}}},
	); err != nil {
		t.Fatalf("Save base catalog: %v", err)
	}
	currentBefore, err := os.ReadFile(modelcatalog.Path(configPath))
	if err != nil {
		t.Fatalf("read current catalog: %v", err)
	}
	var commitCalls atomic.Int32
	var runtimeCalls atomic.Int32
	h := &Handler{
		cfg:            &config.Config{},
		configFilePath: configPath,
		configCommitted: func(_ context.Context, _ ConfigCommit) error {
			if commitCalls.Add(1) != 1 {
				return nil
			}
			return os.WriteFile(
				modelcatalog.PreviousPath(configPath),
				[]byte(`{"schema_version":3,"defaults_version":"external","models":[{"name":"external-model","provider":"Other","channel":"","group":"Other","user_created":true,"enabled":true}]}`),
				0o600,
			)
		},
		modelCatalogRuntimeSync: func(*config.Config) error {
			runtimeCalls.Add(1)
			return nil
		},
	}

	response := performManagementJSONRequest(
		t,
		h.PatchModelCatalogModel,
		http.MethodPatch,
		"/model-catalog",
		gin.H{"name": "base-model", "enabled": true},
	)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", response.Code, response.Body.String())
	}
	if commitCalls.Load() != 2 || runtimeCalls.Load() != 2 {
		t.Fatalf("commit calls = %d, runtime calls = %d, want 2 and 2", commitCalls.Load(), runtimeCalls.Load())
	}
	if h.configGeneration != 2 {
		t.Fatalf("config generation = %d, want 2", h.configGeneration)
	}
	currentAfter, err := os.ReadFile(modelcatalog.Path(configPath))
	if err != nil {
		t.Fatalf("read restored current catalog: %v", err)
	}
	if !bytes.Equal(currentAfter, currentBefore) {
		t.Fatal("current catalog changed after file finalization conflict")
	}
	previousAfter, err := os.ReadFile(modelcatalog.PreviousPath(configPath))
	if err != nil {
		t.Fatalf("read conflicting previous catalog: %v", err)
	}
	if !bytes.Contains(previousAfter, []byte("external-model")) || bytes.Contains(
		previousAfter, []byte("base-model\""),
	) {
		t.Fatalf("previous catalog conflict was overwritten: %s", previousAfter)
	}
	restoredConfig := h.configSnapshot()
	if restoredConfig.ModelCatalogModelEnabled("base-model") {
		t.Fatal("runtime retained failed enabled state after file conflict")
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
	price, ok := doc.Price("priced-model")
	if !ok || price.Prompt != 7 || price.Completion != 8 || price.Cache != 2 {
		t.Fatalf("priced-model price = %#v, %v", price, ok)
	}
	if _, ok = doc.Find("priced-model"); ok {
		t.Fatal("price-only priced-model created an inventory entry")
	}
}

func TestOAuthModelAliasManagementRoundTripPreservesDisplayName(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("port: 8317\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	h := &Handler{cfg: &config.Config{}, configFilePath: configPath}
	putResponse := performManagementJSONRequest(
		t,
		h.PutOAuthModelAlias,
		http.MethodPut,
		"/oauth-model-alias",
		gin.H{
			"codex": []gin.H{
				{
					"name": "gpt-5.5", "alias": "oauth-gpt", "display-name": "OAuth GPT",
				},
			},
		},
	)
	if putResponse.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, body = %s", putResponse.Code, putResponse.Body.String())
	}

	getResponse := performManagementJSONRequest(
		t,
		h.GetOAuthModelAlias,
		http.MethodGet,
		"/oauth-model-alias",
		nil,
	)
	if getResponse.Code != http.StatusOK {
		t.Fatalf("GET status = %d, body = %s", getResponse.Code, getResponse.Body.String())
	}
	var payload struct {
		OAuthModelAlias map[string][]config.OAuthModelAlias `json:"oauth-model-alias"`
	}
	if err := json.Unmarshal(getResponse.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode GET response: %v", err)
	}
	aliases := payload.OAuthModelAlias["codex"]
	if len(aliases) != 1 || aliases[0].DisplayName != "OAuth GPT" {
		t.Fatalf("GET aliases = %#v", aliases)
	}

	reloaded, err := modelcatalog.Load(configPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	aliases = reloaded.ToOAuthModelAlias()["codex"]
	if len(aliases) != 1 || aliases[0].DisplayName != "OAuth GPT" {
		t.Fatalf("persisted aliases = %#v", aliases)
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

func TestModelCatalogResponseIncludesRuntimeOnlyAvailability(t *testing.T) {
	const (
		clientID = "management-runtime-overlay-client"
		modelID  = "deepseek-v4-pro-runtime-overlay-test"
	)
	registry.GetGlobalRegistry().RegisterClient(
		clientID,
		"claude",
		[]*registry.ModelInfo{{ID: modelID, DisplayName: "DeepSeek V4 Pro Runtime"}},
	)
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(clientID) })

	response := (&Handler{}).modelCatalogResponse(&config.Config{}, modelcatalog.File{})
	row, ok := findModelCatalogResponseRow(response.Models, modelID)
	if !ok {
		t.Fatalf("runtime-only model missing: %#v", response.Models)
	}
	if !row.RuntimeOnly || !row.RuntimeAvailable || !row.Requestable || row.Provider != "DeepSeek" || row.Group != "V4" {
		t.Fatalf("runtime-only row = %#v", row)
	}
	if row.RuntimeConfiguredClients != 1 || row.RuntimeAvailableClients != 1 ||
		len(row.RuntimeAvailableProviders) != 1 || row.RuntimeAvailableProviders[0] != "claude" {
		t.Fatalf("runtime availability = %#v", row)
	}

	registry.GetGlobalRegistry().SuspendClientModel(clientID, modelID, "quota")
	response = (&Handler{}).modelCatalogResponse(&config.Config{}, modelcatalog.File{})
	row, ok = findModelCatalogResponseRow(response.Models, modelID)
	if !ok || row.RuntimeAvailable || row.Requestable || row.NotRequestableReason != "quota_cooldown" ||
		row.RuntimeQuotaCooldown != 1 {
		t.Fatalf("quota-cooling row = %#v, %v", row, ok)
	}
}

func TestModelCatalogAstraPriceDoesNotImplyRuntimeAvailability(t *testing.T) {
	doc := modelcatalog.File{
		Models: []modelcatalog.Model{
			{
				Name: "gpt-6-astra", Provider: "OpenAI", Channel: "openai-compatibility", Group: "GPT 6", Enabled: true,
			},
		},
	}
	price, ok := modelcatalog.DefaultPrices()["gpt-6-astra"]
	if !ok {
		t.Fatal("default Astra price is missing")
	}
	doc.SetPrice("gpt-6-astra", price)

	response := (&Handler{}).modelCatalogResponse(&config.Config{}, doc)
	row, ok := findModelCatalogResponseRow(response.Models, "gpt-6-astra")
	if !ok {
		t.Fatalf("Astra catalog row is missing: %#v", response.Models)
	}
	if row.Price == nil || row.Price.Prompt != price.Prompt || row.RuntimeAvailable || row.Requestable ||
		row.RuntimeConfiguredClients != 0 || row.NotRequestableReason != "not_runtime" {
		t.Fatalf("Astra price-only runtime state = %#v", row)
	}
}

func TestModelCatalogDocumentedCompatibilityModelsRequireRuntime(t *testing.T) {
	response := (&Handler{}).modelCatalogResponse(&config.Config{}, modelcatalog.DefaultFile())
	models := map[string]struct {
		provider string
		channel  string
	}{
		"qwen3.8-max":            {provider: "Qwen", channel: "openai-compatibility"},
		"qwen3.8-max-0902":       {provider: "Qwen", channel: "openai-compatibility"},
		"qwen3.8-flash":          {provider: "Qwen", channel: "openai-compatibility"},
		"qwen3-coder-next":       {provider: "Qwen", channel: "openai-compatibility"},
		"MiniMax-M3":             {provider: "MiniMax", channel: "openai-compatibility"},
		"MiniMax-M2.7":           {provider: "MiniMax", channel: "openai-compatibility"},
		"MiniMax-M2.7-highspeed": {provider: "MiniMax", channel: "openai-compatibility"},
	}
	for modelID, want := range models {
		row, ok := findModelCatalogResponseRow(response.Models, modelID)
		if !ok {
			t.Errorf("documented compatibility model %q is missing", modelID)
			continue
		}
		if row.Provider != want.provider || row.Channel != want.channel || !row.Enabled || row.RuntimeOnly ||
			row.RuntimeAvailable || row.Requestable || row.RuntimeConfiguredClients != 0 ||
			row.RuntimeAvailableClients != 0 || row.NotRequestableReason != "not_runtime" {
			t.Errorf("documented compatibility model %q runtime state = %#v", modelID, row)
		}
	}
}

func TestModelCatalogResponseMapsRuntimeAliasToCatalogModel(t *testing.T) {
	const (
		clientID = "management-runtime-alias-client"
		aliasID  = "management-runtime-alias"
	)
	registry.GetGlobalRegistry().RegisterClient(clientID, "codex", []*registry.ModelInfo{{ID: aliasID}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(clientID) })

	doc := modelcatalog.File{
		Models: []modelcatalog.Model{
			{
				Name: "management-canonical-model", Provider: "GPT", Channel: "codex", Group: "GPT Other",
				Enabled: true,
			},
		},
	}
	modelcatalog.ApplyOAuthModelAlias(
		&doc, map[string][]config.OAuthModelAlias{
			"codex": {{Name: "management-canonical-model", Alias: aliasID}},
		},
	)
	response := (&Handler{}).modelCatalogResponse(&config.Config{}, doc)
	row, ok := findModelCatalogResponseRow(response.Models, "management-canonical-model")
	if !ok || row.RuntimeOnly || !row.RuntimeAvailable || row.RuntimeConfiguredClients != 1 {
		t.Fatalf("canonical row = %#v, %v", row, ok)
	}
	if _, ok := findModelCatalogResponseRow(response.Models, aliasID); ok {
		t.Fatalf("runtime alias %q created a duplicate inventory row", aliasID)
	}
}

func findModelCatalogResponseRow(rows []modelCatalogRow, name string) (modelCatalogRow, bool) {
	for _, row := range rows {
		if strings.EqualFold(row.Name, name) {
			return row, true
		}
	}
	return modelCatalogRow{}, false
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
		modelCatalogRuntimeSync: func(*config.Config) error {
			runtimeCalls.Add(1)
			return nil
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

func TestGetModelCatalogReportsPreviousRecovery(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := modelcatalog.Save(
		configPath,
		modelcatalog.File{Models: []modelcatalog.Model{{Name: "previous-model", Enabled: true}}},
	); err != nil {
		t.Fatalf("Save previous catalog: %v", err)
	}
	if _, err := modelcatalog.Load(configPath); err != nil {
		t.Fatalf("initialize previous catalog: %v", err)
	}
	if err := modelcatalog.Save(
		configPath,
		modelcatalog.File{Models: []modelcatalog.Model{{Name: "current-model", Enabled: true}}},
	); err != nil {
		t.Fatalf("Save current catalog: %v", err)
	}
	if err := os.WriteFile(modelcatalog.Path(configPath), []byte("{invalid"), 0o600); err != nil {
		t.Fatalf("corrupt current catalog: %v", err)
	}
	h := &Handler{cfg: &config.Config{}, configFilePath: configPath}

	response := performManagementJSONRequest(t, h.GetModelCatalog, http.MethodGet, "/model-catalog", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", response.Code, response.Body.String())
	}
	var payload struct {
		Models []modelCatalogRow `json:"models"`
		Meta   struct {
			ModelsConfig          string                  `json:"models_config"`
			PreviousModelsConfig  string                  `json:"previous_models_config"`
			LoadSource            modelcatalog.LoadSource `json:"load_source"`
			RecoveredFromPrevious bool                    `json:"recovered_from_previous"`
			CurrentError          string                  `json:"current_error"`
		} `json:"meta"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if payload.Meta.ModelsConfig != modelcatalog.FileName ||
		payload.Meta.PreviousModelsConfig != modelcatalog.PreviousFileName ||
		payload.Meta.LoadSource != modelcatalog.LoadSourcePrevious ||
		!payload.Meta.RecoveredFromPrevious || payload.Meta.CurrentError == "" {
		t.Fatalf("meta = %#v", payload.Meta)
	}
	if _, ok := findModelCatalogResponseRow(payload.Models, "previous-model"); !ok {
		t.Fatalf("recovered models = %#v", payload.Models)
	}
	if _, ok := findModelCatalogResponseRow(payload.Models, "current-model"); ok {
		t.Fatalf("corrupt current model survived recovery: %#v", payload.Models)
	}
	reloaded, err := modelcatalog.Load(configPath)
	if err != nil {
		t.Fatalf("Load repaired catalog: %v", err)
	}
	if reloaded.LoadMetadata.Source != modelcatalog.LoadSourceCurrent {
		t.Fatalf("reloaded source = %q, want current", reloaded.LoadMetadata.Source)
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
	var changedPrice modelcatalog.Price
	for _, model := range defaults.Models {
		if price, ok := defaults.Price(model.Name); ok {
			target = model
			changedPrice = price
			break
		}
	}
	if target.Name == "" {
		t.Fatal("default catalog has no priced inventory model")
	}
	changedPrice.Prompt++
	if err := modelcatalog.Save(
		configPath,
		modelcatalog.File{
			SchemaVersion:   modelcatalog.SchemaVersion,
			DefaultsVersion: "previous-defaults",
			Prices:          map[string]modelcatalog.Price{target.Name: changedPrice},
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
	payload, err := json.Marshal(
		[]map[string]any{
			{
				"timestamp": "2026-07-31T00:00:00Z",
				"model":     model,
				"completed": true,
				"tokens": map[string]int64{
					"input_tokens": 1,
					"total_tokens": 1,
				},
			},
		},
	)
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
		releaseOnce.Do(
			func() {
				_, _ = locker.Exec("ROLLBACK")
				_ = locker.Close()
			},
		)
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
