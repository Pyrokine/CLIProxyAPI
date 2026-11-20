package management

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/modelcatalog"
	"github.com/gin-gonic/gin"
)

func TestConfigYAMLCatalogReplacementUsesPreparedTransaction(t *testing.T) {
	for _, mode := range []string{
		"replace", "empty", "null", "absent", "callback-failure", "runtime-failure", "catalog-publication-failure",
	} {
		t.Run(
			mode, func(t *testing.T) {
				t.Setenv("WRITABLE_PATH", "")
				root := t.TempDir()
				path := filepath.Join(root, "config.yaml")
				original := []byte("# retain original formatting\nport: 8317\ndebug: false\n")
				if err := os.WriteFile(path, original, 0600); err != nil {
					t.Fatal(err)
				}
				price := modelcatalog.Price{Prompt: 1, Completion: 2}
				doc := modelcatalog.File{
					Models: []modelcatalog.Model{
						{
							Name: "catalog-model", Channel: "claude", Enabled: true,
							DisplayName: "preserved catalog model", Aliases: []string{"old-local"}, Price: &price,
						},
					},
				}
				modelcatalog.ApplyOAuthModelAlias(
					&doc,
					map[string][]config.OAuthModelAlias{"claude": {{Name: "external-model", Alias: "old-global"}}},
				)
				if err := modelcatalog.Save(path, doc); err != nil {
					t.Fatal(err)
				}
				before, err := os.ReadFile(modelcatalog.Path(path))
				if err != nil {
					t.Fatal(err)
				}
				cfg := &config.Config{Port: 8317, OAuthModelAlias: doc.ToOAuthModelAlias()}
				h := &Handler{cfg: cfg, configFilePath: path}
				var commits []ConfigCommit
				h.configCommitted = func(_ context.Context, commit ConfigCommit) error {
					commits = append(commits, commit)
					if commit.Config.Port == 8318 && mode == "callback-failure" {
						return errors.New("synthetic callback failure")
					}
					if commit.Config.Port == 8318 && mode == "catalog-publication-failure" {
						if err := os.Remove(modelcatalog.PreviousPath(path)); err != nil && !errors.Is(
							err, os.ErrNotExist,
						) {
							return err
						}
						if err := os.Mkdir(modelcatalog.PreviousPath(path), 0700); err != nil {
							return err
						}
					}
					if !commit.ModelCatalogPrepared {
						authoritative, err := modelcatalog.LoadOrInit(path, commit.Config)
						if err != nil {
							return err
						}
						commit.Config.OAuthModelAlias = authoritative.ToOAuthModelAlias()
					}
					return nil
				}
				h.modelCatalogRuntimeSync = func(actual *config.Config) error {
					if actual.Port == 8318 && mode == "runtime-failure" {
						return errors.New("synthetic handler publication failure")
					}
					return nil
				}
				t.Cleanup(func() { config.SetRuntimeModelCatalog(nil) })
				body := "port: 8318\noauth-model-alias:\n  claude:\n    - name: catalog-model\n" +
					"      alias: fresh-alias\n      force-mapping: true\n"
				switch mode {
				case "empty":
					body = "port: 8318\noauth-model-alias: {}\n"
				case "null":
					body = "port: 8318\noauth-model-alias: null\n"
				case "absent":
					body = "port: 8318\n"
				}
				recorder := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(recorder)
				c.Request = httptest.NewRequest(http.MethodPut, "/v0/management/config.yaml", strings.NewReader(body))
				key, err := h.configYAMLRedactionKey()
				if err != nil {
					t.Fatal(err)
				}
				c.Request.Header.Set("If-Match", configYAMLETag(original, key))
				h.PutConfigYAML(c)
				failure := strings.HasSuffix(mode, "failure")
				wantStatus := 200
				if failure {
					wantStatus = 503
				}
				if recorder.Code != wantStatus {
					t.Fatalf("status=%d want=%d body=%s", recorder.Code, wantStatus, recorder.Body.String())
				}
				stored, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				after, err := os.ReadFile(modelcatalog.Path(path))
				if err != nil {
					t.Fatal(err)
				}
				if failure {
					if !bytes.Equal(stored, original) || !bytes.Equal(
						before, after,
					) || h.configSnapshot().Port != 8317 {
						t.Fatal("failed compound save did not restore exact config/catalog and published config")
					}
					if len(commits) != 2 || !commits[0].ModelCatalogPrepared || !commits[1].ModelCatalogPrepared {
						t.Fatalf("rollback prepared commits=%+v", commits)
					}
					return
				}
				if len(commits) != 1 || commits[0].ModelCatalogPrepared == (mode == "absent") {
					t.Fatalf("prepared contract=%+v", commits)
				}
				loaded, err := modelcatalog.Load(path)
				if err != nil {
					t.Fatal(err)
				}
				model, ok := loaded.Find("catalog-model")
				actualPrice, hasPrice := loaded.Price("catalog-model")
				if !ok || model.DisplayName != "preserved catalog model" || !hasPrice || actualPrice.Prompt != 1 {
					t.Fatal("YAML aliases replaced authoritative catalog model data")
				}
				aliases := loaded.ToOAuthModelAlias()
				if mode == "absent" {
					if !bytes.Equal(before, after) || len(h.configSnapshot().OAuthModelAlias) == 0 {
						t.Fatal("omitted YAML aliases erased authoritative catalog aliases")
					}
					return
				}
				if mode == "empty" || mode == "null" {
					if len(aliases) != 0 || len(model.Aliases) != 0 || len(model.ForceMappedAliases) != 0 {
						t.Fatalf("explicit empty did not remove global and local aliases: %v", aliases)
					}
					return
				}
				found := false
				for _, entries := range aliases {
					for _, alias := range entries {
						if alias.Alias == "old-local" || alias.Alias == "old-global" {
							t.Fatal("replacement retained obsolete aliases")
						}
						if alias.Alias == "fresh-alias" && alias.ForceMapping {
							found = true
						}
					}
				}
				if !found {
					t.Fatalf("fresh alias was not persisted: %v", aliases)
				}
				if len(h.configSnapshot().OAuthModelAlias) == 0 {
					t.Fatal("prepared config did not publish replacement aliases")
				}
			},
		)
	}
}
