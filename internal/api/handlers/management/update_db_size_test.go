package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/usage"
	"github.com/gin-gonic/gin"
)

func TestUpdateCompatibilityRejectsUnavailableUsageSize(t *testing.T) {
	for _, withPersister := range []bool{false, true} {
		name := "without_persister"
		if withPersister {
			name = "with_persister"
		}
		t.Run(
			name, func(t *testing.T) {
				dir := t.TempDir()
				dataDir := filepath.Join(dir, "usage")
				if err := os.WriteFile(dataDir, []byte("blocks database directory"), 0o600); err != nil {
					t.Fatal(err)
				}
				var persister *usage.Persister
				if withPersister {
					persister = usage.NewPersister(dataDir, config.UsageRetention{Days: -1})
				}
				h := NewHandler(
					&config.Config{UsageDataDir: dataDir}, filepath.Join(dir, "config.yaml"), nil,
					withUsagePersister(persister),
				)
				t.Cleanup(
					func() {
						if err := h.Close(); err != nil {
							t.Errorf("close handler: %v", err)
						}
					},
				)
				read := func() updateCompatibilityResponse {
					t.Helper()
					recorder := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(recorder)
					c.Request = httptest.NewRequest(
						http.MethodGet, "/v0/management/update/compatibility?version=v9.9.9", nil,
					)
					h.GetUpdateCompatibility(c)
					if recorder.Code != http.StatusOK {
						t.Fatalf("HTTP = %d, body = %s", recorder.Code, recorder.Body.String())
					}
					var result updateCompatibilityResponse
					if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
						t.Fatal(err)
					}
					return result
				}
				failed := read()
				if failed.Compatible {
					t.Fatalf("unavailable database size accepted: %+v", failed)
				}
				if failed.Usage.DBSizeBytes != -1 {
					t.Fatalf("unavailable size = %d, want -1", failed.Usage.DBSizeBytes)
				}
				warning := "failed to stat usage db"
				if withPersister {
					warning = "failed to read usage database size"
				}
				if !strings.Contains(strings.Join(failed.Warnings, "\n"), warning) {
					t.Fatalf("missing %q warning: %v", warning, failed.Warnings)
				}
				if err := os.Remove(dataDir); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(dataDir, 0o700); err != nil {
					t.Fatal(err)
				}
				recovered := read()
				if !recovered.Compatible || recovered.Usage.DBSizeBytes != 0 || len(recovered.Warnings) != 0 {
					t.Fatalf("available empty directory did not recover: %+v", recovered)
				}
				t.Logf(
					"read error: compatible=%v size=%d warnings=%v; recovery: compatible=%v size=%d",
					failed.Compatible, failed.Usage.DBSizeBytes, failed.Warnings,
					recovered.Compatible, recovered.Usage.DBSizeBytes,
				)
			},
		)
	}
}
