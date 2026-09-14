package modelcatalog

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
)

func TestLoadOrInitMigratesLegacyPricesAliasesAndEnabled(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(
		filepath.Join(dir, "model-prices.json"), []byte(`{
  "gpt-5.2": {"prompt": 9, "completion": 19, "cache": 0.9},
  "legacy-extra": {"prompt": 1, "completion": 2, "cache": 0.1}
}`), 0o600,
	); err != nil {
		t.Fatalf("write legacy prices: %v", err)
	}
	disabled := false
	cfg := &config.Config{}
	cfg.OAuthModelAlias = map[string][]config.OAuthModelAlias{
		"codex": {{Name: "gpt-image-2", Alias: "gpt-image"}},
	}
	cfg.ModelCatalog.Models = []config.ModelCatalogModel{{Name: "gpt-5.2", Enabled: &disabled, Group: "GPT 5"}}
	doc, err := LoadOrInit(configPath, cfg)
	if err != nil {
		t.Fatalf("LoadOrInit: %v", err)
	}
	gpt52, ok := doc.Find("gpt-5.2")
	if !ok {
		t.Fatal("gpt-5.2 missing")
	}
	if gpt52.Enabled {
		t.Fatal("gpt-5.2 enabled migrated true, want false")
	}
	gpt52Price, ok := doc.Price("gpt-5.2")
	if !ok || gpt52Price.Prompt != 9 || gpt52Price.Completion != 19 || gpt52Price.Cache != 0.9 {
		t.Fatalf("gpt-5.2 price = %#v, %v", gpt52Price, ok)
	}
	if _, ok := doc.Find("gpt-image-2"); !ok {
		t.Fatal("gpt-image-2 missing")
	}
	aliases := doc.OAuthModelAlias["codex"]
	if len(aliases) != 1 || aliases[0].Name != "gpt-image-2" || aliases[0].Alias != "gpt-image" || aliases[0].Fork {
		t.Fatalf("gpt-image-2 aliases = %#v", aliases)
	}
	if _, ok := doc.Find("legacy-extra"); ok {
		t.Fatal("legacy price-only entry created an inventory model")
	}
	if legacyPrice, ok := doc.Price("legacy-extra"); !ok || legacyPrice.Prompt != 1 {
		t.Fatalf("legacy-extra price = %#v, %v", legacyPrice, ok)
	}
	if _, err := os.Stat(filepath.Join(dir, FileName)); err != nil {
		t.Fatalf("models.json not written: %v", err)
	}
}

func TestSaveInitializesPreviousCatalog(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := Save(configPath, File{Models: []Model{{Name: "initial-model", Enabled: true}}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	currentData, err := os.ReadFile(Path(configPath))
	if err != nil {
		t.Fatalf("read current catalog: %v", err)
	}
	previousData, err := os.ReadFile(PreviousPath(configPath))
	if err != nil {
		t.Fatalf("read previous catalog: %v", err)
	}
	if string(previousData) != string(currentData) {
		t.Fatal("initial previous catalog differs from current catalog")
	}
	previous, err := decodeFile(previousData)
	if err != nil {
		t.Fatalf("decode previous catalog: %v", err)
	}
	if _, ok := previous.Find("initial-model"); !ok {
		t.Fatalf("previous models = %#v", previous.Models)
	}
}

func TestLoadOrInitKeepsExistingCatalogAuthoritative(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	doc := File{Models: []Model{{Name: "gpt-5", Provider: "GPT", Channel: "codex", Enabled: true}}}
	if err := Save(configPath, doc); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := LoadOrInit(
		configPath, &config.Config{
			OAuthModelAlias: map[string][]config.OAuthModelAlias{
				"codex": {{Name: "gpt-5", Alias: "stale-alias"}},
			},
		},
	)
	if err != nil {
		t.Fatalf("LoadOrInit: %v", err)
	}
	model, ok := loaded.Find("gpt-5")
	if !ok {
		t.Fatal("gpt-5 missing")
	}
	if len(model.Aliases) != 0 {
		t.Fatalf("gpt-5 aliases = %#v, want no stale YAML aliases", model.Aliases)
	}
	reloaded, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	model, ok = reloaded.Find("gpt-5")
	if !ok || len(model.Aliases) != 0 {
		t.Fatalf("reloaded gpt-5 = %#v", model)
	}
}

func TestLoadSchemaVersionOneAliasesRemainForked(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(
		Path(configPath),
		[]byte(`{
  "schema_version": 1,
  "defaults_version": "legacy",
  "models": [
    {
      "name": "gemini-3-pro-preview",
      "provider": "Gemini",
      "channel": "gemini",
      "enabled": true,
      "aliases": ["legacy-fork"]
    }
  ]
}
`),
		0o600,
	); err != nil {
		t.Fatalf("write legacy catalog: %v", err)
	}

	doc, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if doc.SchemaVersion != SchemaVersion {
		t.Fatalf("schema version = %d, want %d", doc.SchemaVersion, SchemaVersion)
	}
	aliases := doc.ToOAuthModelAlias()
	for _, channel := range []string{"gemini-cli", "vertex", "aistudio", "antigravity"} {
		entries := aliases[channel]
		if len(entries) != 1 || entries[0].Alias != "legacy-fork" || !entries[0].Fork {
			t.Fatalf("aliases[%s] = %#v", channel, entries)
		}
	}
}

func TestLoadOrInitPersistsSchemaUpgradeAndAliasMigration(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(
		Path(configPath),
		[]byte(`{
  "schema_version": 1,
  "defaults_version": "legacy",
  "models": [
    {
      "name": "gemini-3-pro-preview",
      "provider": "Gemini",
      "channel": "gemini",
      "enabled": true,
      "aliases": ["legacy-fork"]
    }
  ]
}
`),
		0o600,
	); err != nil {
		t.Fatalf("write legacy catalog: %v", err)
	}

	if _, err := LoadOrInit(configPath, nil); err != nil {
		t.Fatalf("LoadOrInit: %v", err)
	}
	data, err := os.ReadFile(Path(configPath))
	if err != nil {
		t.Fatalf("read migrated catalog: %v", err)
	}
	var persisted File
	if err = json.Unmarshal(data, &persisted); err != nil {
		t.Fatalf("decode migrated catalog: %v", err)
	}
	if persisted.SchemaVersion != SchemaVersion {
		t.Fatalf("persisted schema version = %d, want %d", persisted.SchemaVersion, SchemaVersion)
	}
	if len(persisted.Models) != 1 || len(persisted.Models[0].Aliases) != 0 {
		t.Fatalf("persisted models = %#v, want aliases externalized", persisted.Models)
	}
	for _, channel := range []string{"gemini-cli", "vertex", "aistudio", "antigravity"} {
		entries := persisted.OAuthModelAlias[channel]
		if len(entries) != 1 || entries[0].Alias != "legacy-fork" || !entries[0].Fork {
			t.Fatalf("persisted aliases[%s] = %#v", channel, entries)
		}
	}
}

func TestLoadLegacyGoogleChannelAliasesExpandsGeminiOAuthChannels(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(
		Path(configPath),
		[]byte(`{
  "schema_version": 1,
  "defaults_version": "legacy",
  "models": [
    {
      "name": "gemini-3-pro-preview",
      "provider": "Gemini",
      "channel": "google",
      "enabled": true,
      "aliases": ["legacy-google"]
    }
  ]
}
`),
		0o600,
	); err != nil {
		t.Fatalf("write legacy catalog: %v", err)
	}

	doc, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	aliases := doc.ToOAuthModelAlias()
	for _, channel := range []string{"gemini-cli", "vertex", "aistudio", "antigravity"} {
		entries := aliases[channel]
		if len(entries) != 1 || entries[0].Alias != "legacy-google" || !entries[0].Fork {
			t.Fatalf("aliases[%s] = %#v", channel, entries)
		}
	}
	if len(aliases["google"]) != 0 {
		t.Fatalf("google aliases = %#v, want none", aliases["google"])
	}
}

func TestOAuthModelAliasRoundTripPreservesChannelAndFork(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	doc := File{}
	ApplyOAuthModelAlias(
		&doc,
		map[string][]config.OAuthModelAlias{
			"gemini-cli": {{Name: "gemini-3-pro-preview", Alias: "rename-only"}},
			"vertex":     {{Name: "gemini-3-pro-preview", Alias: "fork-only", Fork: true}},
		},
	)
	if err := Save(configPath, doc); err != nil {
		t.Fatalf("Save: %v", err)
	}
	reloaded, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	aliases := reloaded.ToOAuthModelAlias()
	geminiCLI := aliases["gemini-cli"]
	if len(geminiCLI) != 1 || geminiCLI[0].Alias != "rename-only" || geminiCLI[0].Fork {
		t.Fatalf("gemini-cli aliases = %#v", geminiCLI)
	}
	vertex := aliases["vertex"]
	if len(vertex) != 1 || vertex[0].Alias != "fork-only" || !vertex[0].Fork {
		t.Fatalf("vertex aliases = %#v", vertex)
	}
	for _, channel := range []string{"aistudio", "antigravity"} {
		if len(aliases[channel]) != 0 {
			t.Fatalf("aliases[%s] = %#v, want none", channel, aliases[channel])
		}
	}
}

func TestToOAuthModelAliasExpandsGeminiOAuthChannelsAndForks(t *testing.T) {
	t.Parallel()
	doc := File{
		Models: []Model{
			{
				Name:     "gemini-3-pro-preview",
				Provider: "Gemini",
				Channel:  "gemini",
				Aliases:  []string{"py-gemini"},
			},
		},
	}
	aliases := doc.ToOAuthModelAlias()
	for _, channel := range []string{"gemini-cli", "vertex", "aistudio", "antigravity"} {
		entries := aliases[channel]
		if len(entries) != 1 {
			t.Fatalf("aliases[%s] = %#v, want one entry", channel, entries)
		}
		if entries[0].Name != "gemini-3-pro-preview" || entries[0].Alias != "py-gemini" || !entries[0].Fork {
			t.Fatalf("aliases[%s][0] = %#v", channel, entries[0])
		}
	}
	if _, exists := aliases["gemini"]; exists {
		t.Fatalf("gemini API-key channel should not receive OAuth aliases: %#v", aliases["gemini"])
	}
}

func TestRemoveModelClearsStoredOAuthAliases(t *testing.T) {
	t.Parallel()
	doc := File{}
	ApplyOAuthModelAlias(
		&doc,
		map[string][]config.OAuthModelAlias{
			"codex": {{Name: "alias-model", Alias: "alias-name"}},
		},
	)
	if !doc.RemoveModel("alias-model", File{}) {
		t.Fatal("RemoveModel changed = false, want true")
	}
	if len(doc.OAuthModelAlias) != 0 {
		t.Fatalf("OAuthModelAlias = %#v, want empty", doc.OAuthModelAlias)
	}
}

func TestRemoveDefaultModelRecordsTombstoneAndPreviewCanRestore(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	doc := DefaultFile()
	if !doc.RemoveModel("gpt-5.6-sol", DefaultFile()) {
		t.Fatal("expected gpt-5.6-sol to be removed")
	}
	if err := Save(configPath, doc); err != nil {
		t.Fatalf("Save: %v", err)
	}
	preview, err := PreviewDefaultUpdate(configPath)
	if err != nil {
		t.Fatalf("PreviewDefaultUpdate: %v", err)
	}
	found := false
	for _, change := range preview.Changes {
		if change.Name == "gpt-5.6-sol" && change.Type == "restore_removed_default" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("restore change for gpt-5.6-sol not found: %#v", preview.Changes)
	}
	result, err := applyDefaultUpdate(configPath, []ApplyDecision{{Name: "gpt-5.6-sol", Action: "restore"}})
	if err != nil {
		t.Fatalf("ApplyDefaultUpdate: %v", err)
	}
	if result.Applied != 1 || result.BackupPath == "" {
		t.Fatalf("ApplyDefaultUpdate result = %#v", result)
	}
	reloaded, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, ok := reloaded.Find("gpt-5.6-sol"); !ok {
		t.Fatal("gpt-5.6-sol was not restored")
	}
	for _, name := range reloaded.RemovedDefaultModels {
		if name == "gpt-5.6-sol" {
			t.Fatal("gpt-5.6-sol tombstone still present after restore")
		}
	}
}

func TestUpdateSerializesConcurrentReadModifyWrite(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	const updates = 16
	start := make(chan struct{})
	errs := make(chan error, updates)
	var wg sync.WaitGroup
	for i := range updates {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			_, _, err := update(
				configPath,
				nil,
				UpdateOptions{},
				func(doc *File) error {
					doc.EnsureModel(fmt.Sprintf("concurrent-model-%02d", index), true)
					return nil
				},
			)
			errs <- err
		}(i)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("Update: %v", err)
		}
	}

	doc, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for i := range updates {
		name := fmt.Sprintf("concurrent-model-%02d", i)
		if _, ok := doc.Find(name); !ok {
			t.Fatalf("%s missing after concurrent updates", name)
		}
	}
}

func TestUpdateRestoresExistingCatalogWhenCommitFails(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := Save(configPath, File{Models: []Model{{Name: "original-model", Enabled: true}}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	commitErr := errors.New("commit failed")
	_, _, err := update(
		configPath,
		nil,
		UpdateOptions{Commit: func(File) error { return commitErr }},
		func(doc *File) error {
			doc.EnsureModel("new-model", true)
			return nil
		},
	)
	if !errors.Is(err, commitErr) {
		t.Fatalf("Update error = %v, want %v", err, commitErr)
	}
	doc, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, ok := doc.Find("original-model"); !ok {
		t.Fatal("original-model missing after rollback")
	}
	if _, ok := doc.Find("new-model"); ok {
		t.Fatal("new-model persisted after rollback")
	}
}

func TestUpdateRemovesNewCatalogWhenCommitFails(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	commitErr := errors.New("commit failed")
	_, _, err := update(
		configPath,
		nil,
		UpdateOptions{Commit: func(File) error { return commitErr }},
		func(doc *File) error {
			doc.EnsureModel("new-model", true)
			return nil
		},
	)
	if !errors.Is(err, commitErr) {
		t.Fatalf("Update error = %v, want %v", err, commitErr)
	}
	if _, err = os.Stat(Path(configPath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("models.json still exists after rollback: %v", err)
	}
}

func TestUpdateBackupUsesPreMutationSnapshotAndUniquePath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	_, firstBackup, err := update(
		configPath,
		nil,
		UpdateOptions{Backup: true},
		func(doc *File) error {
			doc.EnsureModel("new-only-model", true)
			return nil
		},
	)
	if err != nil {
		t.Fatalf("first Update: %v", err)
	}
	backupData, err := os.ReadFile(firstBackup)
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	backupDoc, err := decodeFile(backupData)
	if err != nil {
		t.Fatalf("decode backup: %v", err)
	}
	if _, ok := backupDoc.Find("new-only-model"); ok {
		t.Fatal("backup contains post-mutation model")
	}
	_, secondBackup, err := update(
		configPath,
		nil,
		UpdateOptions{Backup: true},
		nil,
	)
	if err != nil {
		t.Fatalf("second Update: %v", err)
	}
	if firstBackup == secondBackup {
		t.Fatalf("backup paths are identical: %s", firstBackup)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "."+FileName+".tmp-") {
			t.Fatalf("atomic temp file was not cleaned: %s", entry.Name())
		}
	}
}

func TestLoadOrInitRecoversCorruptCurrentFromPrevious(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := Save(configPath, File{Models: []Model{{Name: "previous-model", Enabled: true}}}); err != nil {
		t.Fatalf("Save previous: %v", err)
	}
	if err := Save(configPath, File{Models: []Model{{Name: "current-model", Enabled: true}}}); err != nil {
		t.Fatalf("Save current: %v", err)
	}
	if err := os.WriteFile(Path(configPath), []byte(`{invalid`), 0o600); err != nil {
		t.Fatalf("corrupt current: %v", err)
	}

	doc, err := LoadOrInit(configPath, nil)
	if err != nil {
		t.Fatalf("LoadOrInit: %v", err)
	}
	if _, ok := doc.Find("previous-model"); !ok {
		t.Fatalf("recovered catalog = %#v", doc.Models)
	}
	if doc.LoadMetadata.Source != LoadSourcePrevious || !doc.LoadMetadata.RecoveredFromPrevious ||
		!strings.Contains(doc.LoadMetadata.CurrentError, "invalid current model catalog") {
		t.Fatalf("load metadata = %#v", doc.LoadMetadata)
	}
	for _, path := range []string{Path(configPath), PreviousPath(configPath)} {
		info, errStat := os.Stat(path)
		if errStat != nil {
			t.Fatalf("stat %s: %v", path, errStat)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("mode %s = %o, want 600", path, info.Mode().Perm())
		}
	}
	reloaded, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load repaired current: %v", err)
	}
	if reloaded.LoadMetadata.Source != LoadSourceCurrent || reloaded.LoadMetadata.RecoveredFromPrevious {
		t.Fatalf("reloaded metadata = %#v", reloaded.LoadMetadata)
	}
	if _, ok := reloaded.Find("previous-model"); !ok {
		t.Fatalf("repaired current = %#v", reloaded.Models)
	}
}

func TestLoadOrInitRecoversMissingCurrentFromPrevious(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := Save(configPath, File{Models: []Model{{Name: "previous-model", Enabled: true}}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := Load(configPath); err != nil {
		t.Fatalf("initialize previous catalog: %v", err)
	}
	if err := os.Remove(Path(configPath)); err != nil {
		t.Fatalf("remove current catalog: %v", err)
	}

	doc, err := LoadOrInit(configPath, nil)
	if err != nil {
		t.Fatalf("LoadOrInit: %v", err)
	}
	if _, ok := doc.Find("previous-model"); !ok {
		t.Fatalf("recovered catalog = %#v", doc.Models)
	}
	if doc.LoadMetadata.Source != LoadSourcePrevious || !doc.LoadMetadata.RecoveredFromPrevious ||
		!strings.Contains(doc.LoadMetadata.CurrentError, "current model catalog is missing") {
		t.Fatalf("load metadata = %#v", doc.LoadMetadata)
	}
	if _, err = os.Stat(Path(configPath)); err != nil {
		t.Fatalf("restored current catalog: %v", err)
	}
}

func TestLoadOrInitFailsWhenCurrentAndPreviousAreInvalid(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(Path(configPath), []byte(`{invalid-current`), 0o600); err != nil {
		t.Fatalf("write current: %v", err)
	}
	if err := os.WriteFile(PreviousPath(configPath), []byte(`{invalid-previous`), 0o600); err != nil {
		t.Fatalf("write previous: %v", err)
	}

	_, err := LoadOrInit(configPath, nil)
	if err == nil || !strings.Contains(err.Error(), "invalid current model catalog") ||
		!strings.Contains(err.Error(), "invalid previous model catalog") {
		t.Fatalf("LoadOrInit error = %v", err)
	}
}

func TestLoadRejectsFutureSchemaAndDuplicateModelIdentity(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "future schema",
			raw:  `{"schema_version": 4, "defaults_version": "future", "models": []}`,
			want: "unsupported schema_version 4",
		},
		{
			name: "unknown field",
			raw:  `{"schema_version":3,"defaults_version":"test","models":[],"unexpected":true}`,
			want: `unknown field "unexpected"`,
		},
		{
			name: "duplicate model",
			raw: `{
			  "schema_version": 3,
			  "defaults_version": "test",
			  "models": [
			    {"name": "duplicate", "provider": "Other", "group": "Other", "user_created": true, "enabled": true},
			    {"name": "DUPLICATE", "provider": "Other", "group": "Other", "user_created": true, "enabled": true}
			  ]
			}`,
			want: "duplicate model name",
		},
	}
	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				dir := t.TempDir()
				configPath := filepath.Join(dir, "config.yaml")
				if err := os.WriteFile(Path(configPath), []byte(test.raw), 0o600); err != nil {
					t.Fatalf("write catalog: %v", err)
				}
				if _, err := LoadOrInit(configPath, nil); err == nil || !strings.Contains(err.Error(), test.want) {
					t.Fatalf("LoadOrInit error = %v, want %q", err, test.want)
				}
			},
		)
	}
}

func TestLoadRejectsSymbolicLinkCatalog(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	target := filepath.Join(dir, "target.json")
	if err := os.WriteFile(
		target, []byte(`{"schema_version":3,"defaults_version":"test","models":[]}`), 0o600,
	); err != nil {
		t.Fatalf("write target: %v", err)
	}
	if err := os.Symlink(target, Path(configPath)); err != nil {
		t.Skipf("create symbolic link: %v", err)
	}
	if _, err := LoadOrInit(configPath, nil); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("LoadOrInit error = %v", err)
	}
}

func TestLoadRejectsSymbolicLinkPreviousCatalog(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := Save(configPath, File{Models: []Model{{Name: "current-model", Enabled: true}}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	target := filepath.Join(dir, "previous-target.json")
	if err := os.WriteFile(
		target,
		[]byte(`{"schema_version":3,"defaults_version":"test","models":[]}`),
		0o600,
	); err != nil {
		t.Fatalf("write target: %v", err)
	}
	if err := os.Remove(PreviousPath(configPath)); err != nil {
		t.Fatalf("remove previous catalog: %v", err)
	}
	if err := os.Symlink(target, PreviousPath(configPath)); err != nil {
		t.Skipf("create symbolic link: %v", err)
	}
	if _, err := Load(configPath); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("Load error = %v", err)
	}
}

func TestUpdateCommitFailureKeepsPreviousSnapshot(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := Save(configPath, File{Models: []Model{{Name: "previous-model", Enabled: true}}}); err != nil {
		t.Fatalf("Save previous: %v", err)
	}
	if err := Save(configPath, File{Models: []Model{{Name: "current-model", Enabled: true}}}); err != nil {
		t.Fatalf("Save current: %v", err)
	}
	commitErr := errors.New("commit failed")
	_, _, err := update(
		configPath,
		nil,
		UpdateOptions{Commit: func(File) error { return commitErr }},
		func(doc *File) error {
			doc.EnsureModel("candidate-model", true)
			return nil
		},
	)
	if !errors.Is(err, commitErr) {
		t.Fatalf("Update error = %v, want %v", err, commitErr)
	}
	current, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load current: %v", err)
	}
	if _, ok := current.Find("current-model"); !ok {
		t.Fatalf("current models = %#v", current.Models)
	}
	previousData, err := os.ReadFile(PreviousPath(configPath))
	if err != nil {
		t.Fatalf("read previous: %v", err)
	}
	previous, err := decodeFile(previousData)
	if err != nil {
		t.Fatalf("decode previous: %v", err)
	}
	if _, ok := previous.Find("previous-model"); !ok {
		t.Fatalf("previous models = %#v", previous.Models)
	}
}

func TestPreparedUpdateDoesNotPublishBeforeFileCommit(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := Save(configPath, File{Models: []Model{{Name: "base-model", Enabled: false}}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	transaction, err := PrepareUpdate(
		configPath, nil, false, func(doc *File) error {
			baseModel, ok := doc.Find("base-model")
			if !ok {
				return fmt.Errorf("base-model missing")
			}
			baseModel.Enabled = true
			doc.EnsureModel("candidate-model", true)
			return nil
		},
	)
	if err != nil {
		t.Fatalf("PrepareUpdate: %v", err)
	}
	defer func() { _ = transaction.Rollback() }()
	baseDoc := transaction.BaseDocument()
	baseModel, ok := baseDoc.Find("base-model")
	if !ok || baseModel.Enabled {
		t.Fatalf("base document model = %#v, %t", baseModel, ok)
	}
	candidateDoc := transaction.Document()
	candidateModel, ok := candidateDoc.Find("base-model")
	if !ok || !candidateModel.Enabled {
		t.Fatalf("candidate document model = %#v, %t", candidateModel, ok)
	}

	current, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load while prepared: %v", err)
	}
	if _, ok := current.Find("candidate-model"); ok {
		t.Fatalf("candidate published before CommitFiles: %#v", current.Models)
	}
	baseModel, ok = current.Find("base-model")
	if !ok || baseModel.Enabled {
		t.Fatalf("base model while prepared = %#v, %t", baseModel, ok)
	}
	if err = transaction.CommitFiles(); err != nil {
		t.Fatalf("CommitFiles: %v", err)
	}
	if err = transaction.Finish(); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	current, err = Load(configPath)
	if err != nil {
		t.Fatalf("Load committed: %v", err)
	}
	baseModel, ok = current.Find("base-model")
	if !ok || !baseModel.Enabled {
		t.Fatalf("committed base model = %#v, %t", baseModel, ok)
	}
	if _, ok := current.Find("candidate-model"); !ok {
		t.Fatalf("candidate missing after commit: %#v", current.Models)
	}
}

func TestApplyOAuthModelAliasPersistsForceMappedAliases(t *testing.T) {
	t.Parallel()

	doc := File{}
	changed := ApplyOAuthModelAlias(
		&doc,
		map[string][]config.OAuthModelAlias{
			"codex": {{Name: "gpt-5.4", Alias: "gpt-5.4-fast", Fork: true, ForceMapping: true}},
		},
	)
	if !changed {
		t.Fatal("ApplyOAuthModelAlias changed = false, want true")
	}
	if _, ok := doc.Find("gpt-5.4"); !ok {
		t.Fatal("gpt-5.4 missing")
	}
	storedAliases := doc.OAuthModelAlias["codex"]
	if len(storedAliases) != 1 || !storedAliases[0].Fork || !storedAliases[0].ForceMapping {
		t.Fatalf("stored aliases = %#v", storedAliases)
	}

	aliases := doc.ToOAuthModelAlias()["codex"]
	if len(aliases) != 1 || !aliases[0].Fork || !aliases[0].ForceMapping {
		t.Fatalf("oauth aliases = %#v", aliases)
	}
}

func TestDeleteOAuthModelAliasChannelPreservesLegacyAliasesForOtherChannels(t *testing.T) {
	t.Parallel()
	doc := File{
		Models: []Model{
			{
				Name:     "gemini-3-pro-preview",
				Provider: "Gemini",
				Channel:  "gemini",
				Aliases:  []string{"legacy-fork"},
			},
		},
	}
	if !DeleteOAuthModelAliasChannel(&doc, "vertex") {
		t.Fatal("DeleteOAuthModelAliasChannel changed = false, want true")
	}
	aliases := doc.ToOAuthModelAlias()
	if len(aliases["vertex"]) != 0 {
		t.Fatalf("vertex aliases = %#v, want none", aliases["vertex"])
	}
	for _, channel := range []string{"gemini-cli", "aistudio", "antigravity"} {
		entries := aliases[channel]
		if len(entries) != 1 || entries[0].Alias != "legacy-fork" || !entries[0].Fork {
			t.Fatalf("aliases[%s] = %#v", channel, entries)
		}
	}
}

func TestSanitizeMaterializesAliasesForCustomOAuthChannel(t *testing.T) {
	t.Parallel()
	doc := File{
		Models: []Model{
			{
				Name:     "grok-4",
				Provider: "Grok",
				Channel:  "xai",
				Aliases:  []string{"grok-latest"},
			},
		},
	}
	doc.Sanitize()
	model, ok := doc.Find("grok-4")
	if !ok || len(model.Aliases) != 0 {
		t.Fatalf("model = %#v", model)
	}
	entries := doc.OAuthModelAlias["xai"]
	if len(entries) != 1 || entries[0].Alias != "grok-latest" || !entries[0].Fork {
		t.Fatalf("xai aliases = %#v", entries)
	}
}

func TestDeleteOAuthModelAliasChannelClearsForceMappedAliases(t *testing.T) {
	t.Parallel()

	doc := File{}
	ApplyOAuthModelAlias(
		&doc,
		map[string][]config.OAuthModelAlias{
			"codex": {{Name: "gpt-5.4", Alias: "gpt-5.4-fast", Fork: true, ForceMapping: true}},
		},
	)
	if !DeleteOAuthModelAliasChannel(&doc, "codex") {
		t.Fatal("DeleteOAuthModelAliasChannel changed = false, want true")
	}
	if _, ok := doc.Find("gpt-5.4"); !ok {
		t.Fatal("gpt-5.4 missing")
	}
	if len(doc.OAuthModelAlias) != 0 {
		t.Fatalf("aliases = %#v", doc.OAuthModelAlias)
	}
}
