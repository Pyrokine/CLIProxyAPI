package management

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/usage"
	"github.com/gin-gonic/gin"
)

func TestAPIKeyAliasSnapshotsAreSafeDuringManagementUpdates(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("api-key-aliases: {}\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	h := NewHandler(
		&config.Config{SDKConfig: config.SDKConfig{APIKeyAliases: map[string]string{"initial-key": "initial"}}},
		configPath,
		nil,
		WithEnvSecret(""),
	)
	defer func() { _ = h.Close() }()

	var wg sync.WaitGroup
	errCh := make(chan error, 3)
	wg.Add(3)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			aliases := h.apiKeyAliasesSnapshot()
			_ = maskUsageEventAPIKeys([]usage.FlatDetail{{APIKey: "initial-key"}}, aliases)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(
				http.MethodPut,
				"/v0/management/api-key-aliases",
				bytes.NewBufferString(fmt.Sprintf(`{"initial-key":"alias-%d"}`, i)),
			)
			ctx.Request.Header.Set("Content-Type", "application/json")
			h.PutAPIKeyAliases(ctx)
			if recorder.Code != http.StatusOK {
				errCh <- fmt.Errorf("PutAPIKeyAliases status = %d: %s", recorder.Code, recorder.Body.String())
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			h.SetConfig(
				&config.Config{
					SDKConfig: config.SDKConfig{
						APIKeyAliases: map[string]string{"initial-key": fmt.Sprintf("reload-%d", i)},
					},
				},
			)
		}
	}()
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatal(err)
		}
	}
}
