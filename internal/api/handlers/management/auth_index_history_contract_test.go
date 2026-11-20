package management

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/usage"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/gin-gonic/gin"
)

func TestAuthIndexHistoryRestoreKeepsPublishedAndLegacyCredentialReferences(t *testing.T) {
	for _, kind := range []string{"published index", "legacy API key index", "OAuth file index"} {
		t.Run(
			kind, func(t *testing.T) {
				auth := &coreauth.Auth{
					ID: "fixture-credential", Provider: "gemini", Status: coreauth.StatusActive,
					Attributes: map[string]string{"runtime_only": "true"},
				}
				reference := "legacy-published-index"
				switch kind {
				case "published index":
					auth.Index = reference
				case "legacy API key index":
					auth.Attributes["api_key"] = "synthetic-key"
					auth.Attributes["base_url"] = "https://synthetic.invalid/v1"
					auth.Attributes["source"] = "synthetic-source"
					sum := sha256.Sum256([]byte("gemini-api-key:https://synthetic.invalid/v1+synthetic-key"))
					reference = hex.EncodeToString(sum[:8])
				case "OAuth file index":
					auth.Provider = "gemini-cli"
					auth.FileName = "test-oauth.json"
					auth.Attributes["path"] = "test-oauth.json"
					auth.Metadata = map[string]any{"type": "gemini"}
					reference = auth.EnsureIndex()
				}
				manager := coreauth.NewManager(nil, nil, nil)
				published, err := manager.Register(t.Context(), auth)
				if err != nil || published.Index == "" {
					t.Fatalf("register credential=%+v error=%v", published, err)
				}
				originalIndex := published.Index
				at := time.Now().UTC().Add(-time.Hour).Truncate(time.Hour).Add(10 * time.Minute)
				payload, err := json.Marshal(
					[]usage.FlatDetail{
						{
							Timestamp: at, Provider: auth.Provider, Model: "fixture-model", Source: "fixture-source",
							AuthIndex: reference, APIKey: "fixture-client-key", RequestID: "fixture-request",
							Completed: true,
						},
					},
				)
				if err != nil {
					t.Fatal(err)
				}
				original := usage.NewPersister(t.TempDir(), config.UsageRetention{Days: -1})
				original.Start(t.Context())
				defer original.Stop()
				if !original.IsReady() {
					t.Fatal("original usage store did not initialize")
				}
				if _, err := original.ImportContext(t.Context(), payload); err != nil {
					t.Fatal(err)
				}
				snapshot, err := original.QuerySnapshotLimitedContext(t.Context(), 10)
				if err != nil {
					t.Fatal(err)
				}
				exported, err := json.Marshal(
					struct {
						Version int                      `json:"version"`
						Usage   usage.StatisticsSnapshot `json:"usage"`
					}{Version: 1, Usage: snapshot},
				)
				if err != nil {
					t.Fatal(err)
				}
				restored := usage.NewPersister(t.TempDir(), config.UsageRetention{Days: -1})
				restored.Start(t.Context())
				defer restored.Stop()
				if !restored.IsReady() {
					t.Fatal("restored usage store did not initialize")
				}
				if _, err := restored.ImportContext(t.Context(), exported); err != nil {
					t.Fatal(err)
				}
				handler := NewHandler(&config.Config{}, "", manager, withEnvSecret(""), withUsagePersister(restored))
				defer func() {
					if err := handler.Close(); err != nil {
						t.Error(err)
					}
				}()
				recorder := httptest.NewRecorder()
				request, _ := gin.CreateTestContext(recorder)
				request.Request = httptest.NewRequest(
					http.MethodGet, fmt.Sprintf(
						"/usage/summary?from=%s&to=%s&granularity=hourly&auth_index_groups=true",
						at.Add(-time.Minute).Format(time.RFC3339), at.Add(time.Minute).Format(time.RFC3339),
					), nil,
				)
				handler.GetUsageSummary(request)
				var response summaryResponse
				if err := json.Unmarshal(
					recorder.Body.Bytes(), &response,
				); err != nil || recorder.Code != http.StatusOK {
					t.Fatalf("summary status=%d body=%s error=%v", recorder.Code, recorder.Body.Bytes(), err)
				}
				group := response.ByAuthIndex[reference]
				if response.Totals.Requests != 1 || len(response.ByAuthIndex) != 1 || group == nil ||
					group.Requests != 1 {
					t.Fatalf("restored index groups=%+v totals=%+v", response.ByAuthIndex, response.Totals)
				}
				selected := handler.authByIndex(reference)
				if selected == nil || selected.ID != auth.ID || selected.Index != originalIndex {
					t.Fatalf(
						"historical index selected=%+v, expected credential=%q index=%q", selected, auth.ID,
						originalIndex,
					)
				}
			},
		)
	}
}
