package executor

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	codexauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/codex"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	fileauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/auth"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func makeTestCodexRefreshJWT(t *testing.T, planType, accountID string) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	authInfo := map[string]any{"chatgpt_account_id": accountID}
	if planType != "" {
		authInfo["chatgpt_plan_type"] = planType
	}
	payload, err := json.Marshal(
		map[string]any{
			"email": "user@example.com", "https://api.openai.com/auth": authInfo,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + "."
}

func codexRefreshTestContext(t *testing.T, idToken, refreshToken string) context.Context {
	t.Helper()
	calls := 0
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	t.Cleanup(
		func() {
			if calls != 1 {
				t.Errorf("refresh request count = %d, want 1", calls)
			}
		},
	)
	return util.WithRoundTripper(
		ctx, devinTestRoundTripper(
			func(request *http.Request) (*http.Response, error) {
				calls++
				if request.Method != http.MethodPost || request.URL.String() != "https://auth.openai.com/oauth/token" {
					return nil, fmt.Errorf("unexpected synthetic OAuth request: %s %s", request.Method, request.URL)
				}
				if err := request.ParseForm(); err != nil {
					return nil, err
				}
				if request.Form.Get("refresh_token") != refreshToken {
					return nil, fmt.Errorf("refresh request did not preserve its selected credential")
				}
				body, err := json.Marshal(
					map[string]any{
						"access_token": "new-mock-access-token", "refresh_token": "new-mock-refresh-token",
						"id_token": idToken, "token_type": "Bearer", "expires_in": 3600,
					},
				)
				if err != nil {
					return nil, err
				}
				return &http.Response{
					StatusCode: http.StatusOK, Header: make(http.Header),
					Body: io.NopCloser(strings.NewReader(string(body))), Request: request,
				}, nil
			},
		),
	)
}

func saveCodexRefreshPlan(
	t *testing.T, ctx context.Context, auth *cliproxyauth.Auth,
	filename, wantPlan string,
) map[string]any {
	t.Helper()
	dir := t.TempDir()
	store := fileauth.NewFileTokenStore()
	store.SetBaseDir(dir)
	auth.Attributes[cliproxyauth.AttributePath] = filepath.Join(dir, filename)
	path, err := store.Save(ctx, auth)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var saved map[string]any
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	if got := saved["plan_type"]; got != wantPlan {
		t.Errorf("credential file plan_type = %v, want %q", got, wantPlan)
	}
	return saved
}

func TestCodexExecutorRefresh_MissingPlanTypeDefaultsToFree(t *testing.T) {
	idToken := makeTestCodexRefreshJWT(t, "", "acc-codex-test")
	ctx := codexRefreshTestContext(t, idToken, "valid-refresh-token")
	storage := &codexauth.TokenStorage{
		IDToken: "old-id-token", AccessToken: "old-access-token",
		RefreshToken: "old-refresh-token", PlanType: "unknown",
	}
	auth := &cliproxyauth.Auth{
		ID: "test-codex-refresh", Provider: "codex", Storage: storage,
		Metadata:   map[string]any{"refresh_token": "valid-refresh-token"},
		Attributes: map[string]string{"plan_type": "unknown"},
	}
	refreshed, err := newCodexExecutor(&config.Config{}).Refresh(ctx, auth)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.Attributes["plan_type"] != "free" || refreshed.Metadata["plan_type"] != "free" {
		t.Errorf(
			"refreshed plan_type = %q / %v, want free",
			refreshed.Attributes["plan_type"], refreshed.Metadata["plan_type"],
		)
	}
	if storage.PlanType != "unknown" {
		t.Errorf("original storage plan_type = %q, want unknown", storage.PlanType)
	}
	updated, ok := refreshed.Storage.(*codexauth.TokenStorage)
	if !ok || updated == nil || updated == storage || updated.PlanType != "free" {
		t.Fatalf("refreshed storage = %#v, want independent free plan storage", refreshed.Storage)
	}
	saveCodexRefreshPlan(t, ctx, refreshed, "codex-refreshed.json", "free")
}

func TestCodexExecutorRefresh_ExtractsPlanTypeWhenPresent(t *testing.T) {
	idToken := makeTestCodexRefreshJWT(t, "team", "acc-codex-test-team")
	ctx := codexRefreshTestContext(t, idToken, "valid-refresh-token-team")
	storage := &codexauth.TokenStorage{
		IDToken: "old-id-token", AccessToken: "old-access-token",
		RefreshToken: "old-refresh-token", PlanType: "free",
	}
	auth := &cliproxyauth.Auth{
		ID: "test-codex-refresh-team", Provider: "codex", Storage: storage,
		Metadata:   map[string]any{"refresh_token": "valid-refresh-token-team"},
		Attributes: map[string]string{"plan_type": "free"},
	}
	refreshed, err := newCodexExecutor(&config.Config{}).Refresh(ctx, auth)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.Attributes["plan_type"] != "team" || refreshed.Metadata["plan_type"] != "team" {
		t.Errorf(
			"refreshed plan_type = %q / %v, want team",
			refreshed.Attributes["plan_type"], refreshed.Metadata["plan_type"],
		)
	}
	if storage.PlanType != "free" {
		t.Errorf("original storage plan_type = %q, want free", storage.PlanType)
	}
	updated, ok := refreshed.Storage.(*codexauth.TokenStorage)
	if !ok || updated == nil || updated == storage || updated.PlanType != "team" {
		t.Fatalf("refreshed storage = %#v, want independent team plan storage", refreshed.Storage)
	}
	saveCodexRefreshPlan(t, ctx, refreshed, "codex-team-refreshed.json", "team")
}

func TestCodexExecutorRefresh_EmptyAttributesSnapshotIsolation(t *testing.T) {
	idToken := makeTestCodexRefreshJWT(t, "", "acc-codex-empty-attrs")
	ctx := codexRefreshTestContext(t, idToken, "valid-refresh-token")
	emptyAttrs := map[string]string{}
	storage := &codexauth.TokenStorage{
		IDToken: "old-id-token", AccessToken: "old-access-token",
		RefreshToken: "old-refresh-token",
	}
	auth := &cliproxyauth.Auth{
		ID: "test-codex-empty-attrs", Provider: "codex", Storage: storage,
		Attributes: emptyAttrs, Metadata: map[string]any{"refresh_token": "valid-refresh-token"},
	}
	refreshed, err := newCodexExecutor(&config.Config{}).Refresh(ctx, auth)
	if err != nil {
		t.Fatal(err)
	}
	if got := refreshed.Attributes["plan_type"]; got != "free" {
		t.Errorf("refreshed plan_type = %q, want free", got)
	}
	if _, exists := emptyAttrs["plan_type"]; exists || len(emptyAttrs) != 0 {
		t.Errorf("original empty attributes changed: %#v", emptyAttrs)
	}
}

func TestCodexRefreshClonePreservesMetadataThroughFileTokenStore(t *testing.T) {
	for _, plan := range []string{"free", "team", ""} {
		t.Run(
			"claim="+plan, func(t *testing.T) {
				ctx := codexRefreshTestContext(t, makeTestCodexRefreshJWT(t, plan, "acc-codex-snapshot"), "old-refresh")
				originalMetadata := map[string]any{
					"refresh_token": "old-refresh", "plan_type": "pro",
					"custom_metadata": "preserved", "account_id": "old-account",
				}
				originalAttributes := map[string]string{"plan_type": "pro"}
				originalStorage := &codexauth.TokenStorage{
					PlanType: "pro", AccessToken: "old-access",
					RefreshToken: "old-refresh", Metadata: originalMetadata,
				}
				original := &cliproxyauth.Auth{
					ID: "codex-snapshot", Provider: "codex",
					Attributes: originalAttributes, Metadata: originalMetadata, Storage: originalStorage,
				}
				beforeMetadata := map[string]any{
					"refresh_token": "old-refresh", "plan_type": "pro",
					"custom_metadata": "preserved", "account_id": "old-account",
				}
				refreshed, err := newCodexExecutor(&config.Config{}).Refresh(ctx, original.Clone())
				if err != nil {
					t.Fatal(err)
				}
				wantPlan := plan
				if wantPlan == "" {
					wantPlan = "free"
				}
				beforeSave := refreshed.Storage
				saved := saveCodexRefreshPlan(t, ctx, refreshed, "codex-snapshot.json", wantPlan)
				if refreshed.Storage == beforeSave {
					t.Fatal("FileTokenStore did not prepare an independent token storage snapshot")
				}
				if saved["custom_metadata"] != "preserved" || saved["access_token"] != "new-mock-access-token" ||
					saved["refresh_token"] != "new-mock-refresh-token" || saved["account_id"] != "acc-codex-snapshot" {
					t.Fatal("FileTokenStore lost refreshed fields or original custom metadata")
				}
				if !reflect.DeepEqual(original.Metadata, beforeMetadata) ||
					!reflect.DeepEqual(originalStorage.Metadata, beforeMetadata) || len(originalAttributes) != 1 ||
					originalAttributes["plan_type"] != "pro" || originalStorage.PlanType != "pro" ||
					originalStorage.AccessToken != "old-access" || original.Storage != originalStorage {
					t.Fatal("refresh or file persistence changed the original credential snapshot")
				}
			},
		)
	}
}
