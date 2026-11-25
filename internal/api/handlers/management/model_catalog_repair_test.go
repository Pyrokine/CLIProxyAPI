package management

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/modelcatalog"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/gin-gonic/gin"
)

func repairCatalogFixture(t *testing.T, doc modelcatalog.File) *Handler {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("port: 8317\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := modelcatalog.Save(path, doc); err != nil {
		t.Fatal(err)
	}
	return &Handler{cfg: &config.Config{}, configFilePath: path}
}

func repairCatalogJSON[T any](t *testing.T, response *httptest.ResponseRecorder) T {
	t.Helper()
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var result T
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestCatalogRepairKeepsReferencePricesAndAliasRules(t *testing.T) {
	const name = "fixture-catalog-model"
	rules := map[string][]config.OAuthModelAlias{
		"codex":  {{Name: name, Alias: "codex-fixture", DisplayName: "Codex fixture", ForceMapping: true}},
		"claude": {{Name: name, Alias: "claude-fixture", Fork: true}},
	}
	var reference modelcatalog.Price
	if err := json.Unmarshal([]byte(`{"prompt":2,"status":"partial","auto_apply":false}`), &reference); err != nil {
		t.Fatal(err)
	}
	h := repairCatalogFixture(
		t, modelcatalog.File{
			Models:          []modelcatalog.Model{{Name: name, Channel: "codex", Enabled: true}},
			OAuthModelAlias: rules,
			Prices:          map[string]modelcatalog.Price{name: reference},
		},
	)
	persister, release := newLockedCatalogRecalculation(t, name)
	defer release()
	h.usagePersister = persister
	patch := func(body gin.H) modelCatalogResponse {
		return repairCatalogJSON[modelCatalogResponse](
			t, performManagementJSONRequest(t, h.PatchModelCatalogModel, http.MethodPatch, "/model-catalog", body),
		)
	}
	state := patch(gin.H{"name": name, "display_name": "Renamed", "enabled": false})
	if row := state.Models[0]; row.Enabled || row.DisplayName != "Renamed" || row.Price == nil || row.Price.Automatic() || state.RecalculationPending {
		t.Fatalf("metadata-only edit changed catalog semantics: %#v", state)
	}
	// Legacy flat and explicit channel callers both retain the original rule metadata.
	patch(gin.H{"name": name, "aliases": []string{"codex-fixture", "claude-fixture"}})
	patch(gin.H{"name": name, "aliases": []string{"codex-fixture", "new-fixture"}, "alias_channel": " CODEX "})
	rules["codex"] = append(rules["codex"], config.OAuthModelAlias{Name: name, Alias: "new-fixture", Fork: true})
	aliasResponse := repairCatalogJSON[map[string]map[string][]config.OAuthModelAlias](
		t, performManagementJSONRequest(t, h.GetOAuthModelAlias, http.MethodGet, "/oauth-model-alias", nil),
	)
	if !reflect.DeepEqual(aliasResponse["oauth-model-alias"], rules) {
		t.Fatalf("rules = %#v, want %#v", aliasResponse["oauth-model-alias"], rules)
	}
	if status := persister.RecalculateCostsStatus(); status.Status != "idle" || status.Running || status.TotalCost != 0 {
		t.Fatalf("non-price edits started repricing: %+v", status)
	}
	catalog := repairCatalogJSON[struct {
		Models []struct{ Price map[string]any } `json:"models"`
	}](t, performManagementJSONRequest(t, h.GetModelCatalog, http.MethodGet, "/model-catalog", nil))
	if price := catalog.Models[0].Price; price["auto_apply"] != false || price["prompt"] != float64(2) {
		t.Fatalf("reference price wire value = %#v", price)
	}
}

func TestCatalogRepairRejectsUnroutableAliases(t *testing.T) {
	h := repairCatalogFixture(t, modelcatalog.File{Models: []modelcatalog.Model{{Name: "existing-fixture"}}})
	const name = "new-fixture-model"
	for _, extra := range []gin.H{
		{"channel": " "}, {"provider": "Gemini", "channel": " "}, {"alias_channel": " "},
		{"alias_channel": "gemini"}, {"alias_channel": "openai-compatibility"}, {"alias_channel": "invalid channel"},
	} {
		extra["name"], extra["aliases"] = name, []string{"fixture-alias"}
		response := performManagementJSONRequest(t, h.PatchModelCatalogModel, http.MethodPatch, "/model-catalog", extra)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("blank channel accepted: %d %s", response.Code, response.Body.String())
		}
	}
	doc, err := modelcatalog.Load(h.configFilePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := doc.Find(name); exists || len(doc.ToOAuthModelAlias()) > 0 {
		t.Fatalf("rejected creation persisted a model or alias: %#v", doc)
	}
	patch := repairCatalogJSON[modelCatalogResponse](
		t, performManagementJSONRequest(
			t, h.PatchModelCatalogModel, http.MethodPatch, "/model-catalog", gin.H{
				"name": name, "channel": " codex ", "alias_channel": " codex ", "enabled": false,
				"aliases": []string{"fixture-alias"},
			},
		),
	)
	if row, ok := findModelCatalogResponseRow(patch.Models, name); !ok || row.Enabled || !reflect.DeepEqual(
		row.Aliases, []string{"fixture-alias"},
	) {
		t.Fatalf("creation readback = %#v", patch)
	}
}

func TestCatalogRepairPreviewKeepsLegacyAliasesOnFirstLoad(t *testing.T) {
	const name = "fixture-legacy-model"
	rules := map[string][]config.OAuthModelAlias{
		"codex": {
			{
				Name: name, Alias: "fixture-legacy-alias", ForceMapping: true,
			},
		},
	}
	h := &Handler{
		cfg: &config.Config{OAuthModelAlias: rules}, configFilePath: filepath.Join(t.TempDir(), "config.yaml"),
	}
	repairCatalogJSON[map[string]any](
		t, performManagementJSONRequest(
			t, h.GetModelCatalogDefaultUpdate, http.MethodGet, "/model-catalog/default-update", nil,
		),
	)
	doc, err := modelcatalog.Load(h.configFilePath)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(doc.ToOAuthModelAlias(), rules) {
		t.Fatalf("first preview lost legacy alias rules: %#v", doc.ToOAuthModelAlias())
	}
}

func TestCatalogRepairPreservesExplicitPriceBooleans(t *testing.T) {
	const name = "fixture-price-flags"
	h := repairCatalogFixture(t, modelcatalog.File{Models: []modelcatalog.Model{{Name: name}}})
	patch := func(price gin.H) {
		repairCatalogJSON[modelCatalogResponse](
			t, performManagementJSONRequest(
				t, h.PatchModelCatalogModel, http.MethodPatch, "/model-catalog", gin.H{
					"name": name, "price": price, "recalculate_costs": false,
				},
			),
		)
	}
	read := func() map[string]any {
		result := repairCatalogJSON[struct {
			Models []struct{ Price map[string]any }
		}](t, performManagementJSONRequest(t, h.GetModelCatalog, http.MethodGet, "/model-catalog", nil))
		return result.Models[0].Price
	}
	price := gin.H{"prompt": 0, "status": "partial", "auto_apply": false, "request_scoped": false}
	patch(price)
	if got := read(); got["request_scoped"] != false || got["auto_apply"] != false || got["prompt"] != float64(0) {
		t.Fatalf("explicit false and zero not preserved: %#v", got)
	}
	delete(price, "request_scoped")
	patch(price)
	if got := read(); got["request_scoped"] != nil || got["auto_apply"] != false {
		t.Fatalf("absent request-scoped setting not preserved: %#v", got)
	}
}

func TestCatalogRepairDefaultsPreviewSkipAndConditionalApply(t *testing.T) {
	defaults := modelcatalog.DefaultFile()
	var target modelcatalog.Model
	var price modelcatalog.Price
	for _, model := range defaults.Models {
		if candidate, ok := defaults.Price(model.Name); ok && candidate.Automatic() && candidate.PromptRateKnown() {
			target, price = model, candidate
			break
		}
	}
	if target.Name == "" {
		t.Fatal("missing priced default fixture")
	}
	custom := price
	custom.Prompt += 17
	for _, restore := range []bool{false, true} {
		label := "adopt"
		if restore {
			label = "restore"
		}
		t.Run(
			label, func(t *testing.T) {
				doc := modelcatalog.File{
					Prices: map[string]modelcatalog.Price{target.Name: custom}, Models: []modelcatalog.Model{},
				}
				if restore {
					doc.RemovedDefaultModels = []string{target.Name}
				}
				h := repairCatalogFixture(t, doc)
				snapshot := func() []byte {
					data, err := os.ReadFile(modelcatalog.Path(h.configFilePath))
					if err != nil {
						t.Fatal(err)
					}
					return data
				}
				preview := func() string {
					t.Helper()
					response := performManagementJSONRequest(
						t, h.GetModelCatalogDefaultUpdate, http.MethodGet, "/model-catalog/default-update", nil,
					)
					result := repairCatalogJSON[struct {
						Revision string `json:"revision"`
						Changes  []struct {
							Name, Type string
							Default    *modelcatalog.Model
							Fields     map[string]struct{ Current, Default modelcatalog.Price }
						}
					}](t, response)
					if result.Revision == "" || result.Revision != response.Header().Get("ETag") {
						t.Fatalf(
							"missing or inconsistent revision: %q %q", result.Revision, response.Header().Get("ETag"),
						)
					}
					for _, change := range result.Changes {
						if change.Name != target.Name {
							continue
						}
						wantType := "new_default"
						if restore {
							wantType = "restore_removed_default"
						}
						if wantType != change.Type || change.Default == nil || change.Fields["price"].Current.Prompt != custom.Prompt || change.Fields["price"].Default.Prompt != price.Prompt {
							t.Fatalf("preview does not show the model and both prices: %#v", change)
						}
						return result.Revision
					}
					t.Fatal("missing default diff")
					return ""
				}
				apply := func(action, revision string) *httptest.ResponseRecorder {
					return performManagementJSONRequest(
						t, func(c *gin.Context) {
							if revision != "" {
								c.Request.Header.Set("If-Match", revision)
							}
							h.ApplyModelCatalogDefaultUpdate(c)
						}, http.MethodPost, "/model-catalog/default-update/apply",
						gin.H{"decisions": []modelcatalog.ApplyDecision{{Name: target.Name, Action: action}}},
					)
				}
				type result struct {
					Result  modelcatalog.ApplyResult
					Catalog modelCatalogResponse
				}
				revision, before := preview(), snapshot()
				persister, release := newLockedCatalogRecalculation(t, target.Name)
				defer release()
				h.usagePersister = persister
				for _, action := range []string{"keep_current", "skip"} {
					skipped := repairCatalogJSON[result](t, apply(action, revision))
					if skipped.Result.Applied != 0 || skipped.Result.BackupPath != "" || skipped.Catalog.RecalculationPending || !bytes.Equal(
						before, snapshot(),
					) || persister.RecalculateCostsStatus().Status != "idle" || h.configGeneration != 0 {
						t.Fatalf("skip changed catalog or usage: %#v", skipped)
					}
				}
				repairCatalogJSON[modelCatalogResponse](
					t, performManagementJSONRequest(
						t, h.PatchModelCatalogModel, http.MethodPatch, "/model-catalog",
						gin.H{"name": "other-fixture", "display_name": "Concurrent edit"},
					),
				)
				before = snapshot()
				stale := apply(label, revision)
				if stale.Code != http.StatusPreconditionFailed || !bytes.Equal(
					before, snapshot(),
				) || persister.RecalculateCostsStatus().Status != "idle" {
					t.Fatalf("old revision applied: %d %s", stale.Code, stale.Body.String())
				}
				applied := repairCatalogJSON[result](t, apply(label, preview()))
				row, ok := findModelCatalogResponseRow(applied.Catalog.Models, target.Name)
				if applied.Result.Applied != 1 || applied.Result.BackupPath == "" || !ok || row.Price == nil || row.Price.Prompt != price.Prompt || !applied.Catalog.RecalculationPending {
					t.Fatalf("conditional apply = %#v", applied)
				}
				if data, err := os.ReadFile(applied.Result.BackupPath); err != nil || !bytes.Equal(before, data) {
					t.Fatalf("backup does not match the pre-apply catalog: %v", err)
				}
				release()
				waitForCatalogRecalculation(t, persister)
				// Existing callers without a conditional header still work, including a metadata-only default update.
				repairCatalogJSON[modelCatalogResponse](
					t, performManagementJSONRequest(
						t, h.PatchModelCatalogModel, http.MethodPatch, "/model-catalog",
						gin.H{"name": target.Name, "display_name": "Custom label", "enabled": false},
					),
				)
				metadata := repairCatalogJSON[result](t, apply("use_default", ""))
				if row, ok = findModelCatalogResponseRow(
					metadata.Catalog.Models, target.Name,
				); !ok || row.DisplayName != target.DisplayName || row.Enabled || metadata.Catalog.RecalculationPending {
					t.Fatalf("metadata-only default apply changed pricing or enablement: %#v", metadata)
				}
			},
		)
	}
}

func TestCatalogRepairRuntimeAliasesUseRegistrationOwnership(t *testing.T) {
	const first, second, public = "fixture-canonical-codex", "fixture-canonical-claude", "fixture-public-alias"
	reg := registry.GetGlobalRegistry()
	register := func(id, provider string, models ...*registry.ModelInfo) {
		reg.RegisterClient(id, provider, models)
		t.Cleanup(func() { reg.UnregisterClient(id) })
	}
	register("fixture-codex-client", "codex", &registry.ModelInfo{ID: public}, &registry.ModelInfo{ID: first})
	register("fixture-claude-client", "claude", &registry.ModelInfo{ID: public})
	reg.SetModelQuotaExceeded("fixture-claude-client", public)
	h := repairCatalogFixture(
		t, modelcatalog.File{
			Models: []modelcatalog.Model{
				{Name: first, Channel: "codex", Enabled: true},
				{Name: second, Channel: "claude", Enabled: true},
			},
			OAuthModelAlias: map[string][]config.OAuthModelAlias{
				"codex":  {{Name: first, Alias: public, Fork: true}},
				"claude": {{Name: second, Alias: public}},
			},
		},
	)
	get := func() *httptest.ResponseRecorder {
		return performManagementJSONRequest(t, h.GetModelCatalog, http.MethodGet, "/model-catalog", nil)
	}
	rows := repairCatalogJSON[modelCatalogResponse](t, get()).Models
	codex, _ := findModelCatalogResponseRow(rows, first)
	claude, _ := findModelCatalogResponseRow(rows, second)
	if codex.RuntimeConfiguredClients != 1 || codex.RuntimeAvailableClients != 1 || !reflect.DeepEqual(
		codex.RuntimeProviders, []string{"codex"},
	) || !codex.Requestable ||
		claude.RuntimeConfiguredClients != 1 || claude.RuntimeQuotaCooldown != 1 || !reflect.DeepEqual(
		claude.RuntimeProviders, []string{"claude"},
	) || claude.Requestable {
		t.Fatalf("wrong channel or duplicate clients: codex=%#v claude=%#v", codex, claude)
	}
	register("fixture-provable-client", "plugin-fixture", &registry.ModelInfo{ID: public, CatalogName: second})
	register("fixture-ambiguous-client", "", &registry.ModelInfo{ID: public})
	reg.SuspendClientModel("fixture-provable-client", public, "maintenance")
	response := get()
	rows = repairCatalogJSON[modelCatalogResponse](t, response).Models
	claude, _ = findModelCatalogResponseRow(rows, second)
	if claude.RuntimeConfiguredClients != 2 || claude.RuntimeSuspended != 1 || claude.NotRequestableReason != "ambiguous_alias" || !strings.Contains(
		response.Body.String(), `"runtime_ambiguous_aliases":["`+public+`"]`,
	) || strings.Contains(response.Body.String(), "fixture-ambiguous-client") {
		t.Fatalf("ambiguous or known registration incorrectly projected: %s", response.Body.String())
	}
}
