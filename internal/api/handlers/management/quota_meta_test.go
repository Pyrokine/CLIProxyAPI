package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/gin-gonic/gin"
)

func TestMetaQuotaStatusUsesAuthIndexWithoutExposingCredentials(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	record, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "meta-account.json",
		FileName: "meta-private@example.com.json",
		Provider: "meta",
		Attributes: map[string]string{
			"auth_kind": "oauth",
			"path":      "/tmp/meta-private@example.com.json",
			"api_key":   "api-secret-marker",
			"dca_token": "dca:secret-marker",
		},
		Metadata: map[string]any{
			"subs_tier_name": "Muse Pro",
			"is_subs_active": true,
			"email":          "private@example.com",
			"api_key":        "api-secret-marker",
			"dca_token":      "dca:secret-marker",
		},
	})
	if err != nil {
		t.Fatalf("register Meta credential: %v", err)
	}
	h := &Handler{authManager: manager}

	quotaRecorder := httptest.NewRecorder()
	quotaContext, _ := gin.CreateTestContext(quotaRecorder)
	quotaContext.Request = httptest.NewRequest(http.MethodGet, "/v0/management/quota/status", nil)
	h.GetQuotaStatus(quotaContext)
	if quotaRecorder.Code != http.StatusOK {
		t.Fatalf("quota status = %d, response = %s", quotaRecorder.Code, quotaRecorder.Body.String())
	}
	for _, secret := range []string{"api-secret-marker", "dca:secret-marker", "private@example.com"} {
		if strings.Contains(quotaRecorder.Body.String(), secret) {
			t.Fatalf("quota status exposed Meta private data: %q", secret)
		}
	}
	var response struct {
		UpdatedAt   time.Time `json:"updated_at"`
		Credentials map[string]struct {
			AuthIndex     string    `json:"auth_index"`
			FileName      string    `json:"file_name"`
			Status        string    `json:"status"`
			Freshness     string    `json:"freshness"`
			Source        string    `json:"source"`
			Generation    uint64    `json:"generation"`
			StateRevision uint64    `json:"state_revision"`
			ObservedAt    time.Time `json:"observed_at"`
			Data          struct {
				PlanName             string   `json:"planName"`
				IsSubscriptionActive bool     `json:"isSubscriptionActive"`
				Windows              []string `json:"windows"`
				Available            bool     `json:"available"`
				Reason               string   `json:"reason"`
			} `json:"data"`
		} `json:"credentials"`
	}
	if err := json.Unmarshal(quotaRecorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode quota status: %v", err)
	}
	index := record.EnsureIndex()
	entry, ok := response.Credentials["meta::"+index]
	if !ok || entry.AuthIndex != index || entry.FileName != "" || entry.Status != "idle" ||
		entry.Freshness != "outdated" || entry.Source != "meta_subscription_metadata" ||
		entry.Generation == 0 || entry.Generation > 1<<53-1 || entry.StateRevision != record.Generation ||
		entry.ObservedAt.IsZero() || !entry.ObservedAt.Equal(response.UpdatedAt) ||
		entry.Data.PlanName != "Muse Pro" || !entry.Data.IsSubscriptionActive ||
		entry.Data.Windows == nil || len(entry.Data.Windows) != 0 || entry.Data.Available ||
		entry.Data.Reason != "usage_not_provided" {
		t.Fatalf("Meta subscription status = %+v, found = %v", entry, ok)
	}

	authRecorder := httptest.NewRecorder()
	authContext, _ := gin.CreateTestContext(authRecorder)
	authContext.Request = httptest.NewRequest(http.MethodGet, "/v0/management/auth-files", nil)
	h.ListAuthFiles(authContext)
	if authRecorder.Code != http.StatusOK || !strings.Contains(authRecorder.Body.String(), `"auth_index":"`+index+`"`) {
		t.Fatalf("auth-files index differs from quota status: %d, %s", authRecorder.Code, authRecorder.Body.String())
	}
}

func TestMetaQuotaStatusDoesNotInventPlanOrUsage(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "meta-unknown.json",
		Provider: "meta",
		Attributes: map[string]string{
			"auth_kind": "oauth",
			"dca_token": "dca:secret-marker",
		},
		Metadata: map[string]any{"subs_tier_name": "secret@example.com"},
	}); err != nil {
		t.Fatalf("register Meta credential: %v", err)
	}
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/quota/status", nil)
	(&Handler{authManager: manager}).GetQuotaStatus(ctx)
	if recorder.Code != http.StatusOK || strings.Contains(recorder.Body.String(), "secret@example.com") ||
		strings.Contains(recorder.Body.String(), "usedPercent") || strings.Contains(recorder.Body.String(), "planName") {
		t.Fatalf("Meta status invented or leaked quota data: %d, %s", recorder.Code, recorder.Body.String())
	}
}

func TestMetaQuotaStatusVersionsAdvanceAcrossObservationsAndUpdates(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	const id = "meta-version.json"
	register := func(plan string) *coreauth.Auth {
		t.Helper()
		result, err := manager.Register(context.Background(), &coreauth.Auth{
			ID: id, Provider: "meta",
			Attributes: map[string]string{"auth_kind": "oauth"},
			Metadata:   map[string]any{"subs_tier_name": plan},
		})
		if err != nil {
			t.Fatalf("register Meta credential: %v", err)
		}
		return result
	}
	record := register("Muse Basic")
	h := &Handler{authManager: manager}
	status := func() (generation, revision uint64) {
		t.Helper()
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/quota/status", nil)
		h.GetQuotaStatus(ctx)
		if recorder.Code != http.StatusOK {
			t.Fatalf("quota status = %d, %s", recorder.Code, recorder.Body.String())
		}
		var response struct {
			Credentials map[string]struct {
				Generation    uint64 `json:"generation"`
				StateRevision uint64 `json:"state_revision"`
			} `json:"credentials"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatalf("decode quota status: %v", err)
		}
		entry, exists := response.Credentials["meta::"+record.EnsureIndex()]
		if !exists {
			t.Fatalf("Meta quota entry missing: %s", recorder.Body.String())
		}
		return entry.Generation, entry.StateRevision
	}
	firstGeneration, firstRevision := status()
	secondGeneration, secondRevision := status()
	if firstGeneration == 0 || secondGeneration <= firstGeneration ||
		firstRevision != record.Generation || secondRevision != firstRevision {
		t.Fatalf("unchanged record versions = %d/%d then %d/%d", firstGeneration, firstRevision, secondGeneration, secondRevision)
	}
	record = register("Muse Pro")
	thirdGeneration, thirdRevision := status()
	if thirdGeneration <= secondGeneration || thirdRevision != record.Generation || thirdRevision <= secondRevision {
		t.Fatalf("updated record versions = %d/%d after %d/%d", thirdGeneration, thirdRevision, secondGeneration, secondRevision)
	}
	// A new handler (as after a restart) derives its first generation from UTC microseconds.
	// Its generation is newer while the system clock has advanced beyond the last observation.
	time.Sleep(time.Millisecond)
	h = &Handler{authManager: manager}
	restartedGeneration, _ := status()
	if restartedGeneration <= thirdGeneration {
		t.Fatalf("new handler generation = %d, previous = %d", restartedGeneration, thirdGeneration)
	}
}
