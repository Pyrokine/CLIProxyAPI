package devin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestCreateAuthRecordKeepsTokenServerSideAndSanitizesFilename(t *testing.T) {
	server := httptest.NewServer(
		http.HandlerFunc(
			func(response http.ResponseWriter, request *http.Request) {
				switch request.URL.Path {
				case "/v3/self":
					_, _ = response.Write(
						[]byte(`{"user_name":"../../unsafe user","user_id":"user-1","org_id":"org-1"}`),
					)
				case GetUserStatusPath:
					_, _ = response.Write(testDevinUserStatusResponse(0, 75))
				default:
					http.NotFound(response, request)
				}
			},
		),
	)
	defer server.Close()

	service := NewDevinAuthService(server.Client())
	service.SetAPIBaseURL(server.URL)
	service.SetServerBaseURL(server.URL)
	record, err := service.CreateAuthRecord(context.Background(), "raw-token")
	if err != nil {
		t.Fatalf("CreateAuthRecord(): %v", err)
	}
	if record.Provider != "devin" || record.Prefix != "" || record.Status != "active" {
		t.Fatalf("record identity = %+v", record)
	}
	if record.Metadata["user_id"] != "user-1" || record.Metadata["org_id"] != "org-1" {
		t.Fatalf("record user/org identity = %#v", record.Metadata)
	}
	if strings.ContainsAny(record.FileName, `/\\`) || !strings.HasPrefix(record.FileName, "devin-user-") ||
		!strings.HasSuffix(record.FileName, ".json") {
		t.Fatalf("unsafe filename = %q", record.FileName)
	}
	token := devinTokenPrefix + "raw-token"
	if record.Attributes["session_token"] != token || record.Metadata["session_token"] != token {
		t.Fatalf("record token metadata is missing")
	}
	if record.Attributes["api_key"] != token || record.Metadata["api_key"] != token {
		t.Fatal("formatted SDK token aliases are inconsistent")
	}
	if record.AuthKind() != coreauth.AuthKindOAuth {
		t.Fatal("session token was classified as an API key")
	}
	if _, account := record.AccountInfo(); strings.Contains(account, token) {
		t.Fatal("account information exposes the session token")
	}
	if wireToken, _, _ := CredentialsFromAuth(record); wireToken != token {
		t.Fatal("executor credentials differ from the SDK token")
	}
	for _, key := range []string{
		"daily_quota_present",
		"daily_quota_remaining_percent",
		"weekly_quota_present",
		"weekly_quota_remaining_percent",
	} {
		if _, exists := record.Metadata[key]; exists {
			t.Fatalf("transient quota key %q was persisted in metadata", key)
		}
	}
	if record.Quota.Signals["daily_quota_present"] != "true" ||
		record.Quota.Signals["daily_quota_remaining_percent"] != "0%" ||
		record.Quota.Signals["weekly_quota_present"] != "true" ||
		record.Quota.Signals["weekly_quota_remaining_percent"] != "75%" {
		t.Fatalf("record quota signals = %#v", record.Quota.Signals)
	}
	if record.Quota.Family != "devin" || record.Quota.Exceeded || record.Quota.ObservedAt.IsZero() {
		t.Fatalf("record quota state = %+v", record.Quota)
	}
	manager := coreauth.NewManager(nil, nil, nil)
	record.Status, record.Unavailable = coreauth.StatusError, true
	record.NextRetryAfter = time.Now().Add(time.Hour)
	registered, err := manager.Register(coreauth.WithSkipPersist(context.Background()), record)
	if err != nil {
		t.Fatal(err)
	}
	store := coreauth.NewFileCooldownStateStore(filepath.Join(t.TempDir(), "cds"))
	manager.SetCooldownStateStore(store)
	manager.PersistCooldownStates(context.Background())
	records, err := store.Load(context.Background())
	if err != nil || len(records) != 1 {
		t.Fatalf("CDS records=%+v error=%v", records, err)
	}
	if records[0].AuthID != registered.ID || !reflect.DeepEqual(records[0].Quota.Signals, record.Quota.Signals) ||
		!records[0].Quota.ObservedAt.Equal(record.Quota.ObservedAt) {
		t.Fatalf("record -> Manager -> CDS lost Devin observation: %+v", records[0])
	}
}

func TestCredentialFileNameIncludesUserAndOrganizationIdentity(t *testing.T) {
	first := CredentialFileName("user-1", "org-1", "token-a")
	if sameIdentity := CredentialFileName("user-1", "org-1", "token-b"); sameIdentity != first {
		t.Fatalf("same user/org identity changed with token: %q/%q", first, sameIdentity)
	}
	if otherOrg := CredentialFileName("user-1", "org-2", "token-a"); otherOrg == first {
		t.Fatalf("different organizations share filename %q", first)
	}
	fallback := CredentialFileName("user-1", "", "private-token-value")
	if fallback != CredentialFileName("", "org-1", "private-token-value") ||
		strings.Contains(fallback, "private-token-value") {
		t.Fatalf("token digest fallback = %q", fallback)
	}
}

func TestApplyQuotaStatusReplacesObservationAndPreservesGeneration(t *testing.T) {
	observedAt := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	quota := coreauth.QuotaState{
		Generation: 7,
		Signals: map[string]string{
			"weekly_quota_remaining_percent": "90%",
			"weekly_quota_reset_at":          "stale",
		},
	}
	ApplyQuotaStatus(
		&quota, &DevinUserStatus{
			Plan:                       "Pro",
			DailyQuotaPresent:          true,
			DailyQuotaRemainingPercent: 0,
			WeeklyQuotaPresent:         false,
			PlanStart:                  observedAt.Add(-time.Hour),
		}, observedAt,
	)

	if quota.Generation != 7 || !quota.ObservedAt.Equal(observedAt) {
		t.Fatalf("quota ownership changed: %+v", quota)
	}
	if quota.Signals["daily_quota_present"] != "true" ||
		quota.Signals["daily_quota_remaining_percent"] != "0%" ||
		quota.Signals["weekly_quota_present"] != "false" {
		t.Fatalf("quota presence signals = %#v", quota.Signals)
	}
	if _, exists := quota.Signals["weekly_quota_remaining_percent"]; exists {
		t.Fatal("absent weekly quota retained a stale percentage")
	}
	if _, exists := quota.Signals["weekly_quota_reset_at"]; exists {
		t.Fatal("absent weekly quota retained a stale reset time")
	}
}

func TestCreateAuthRecordFallsBackToTokenDigestWithoutExposingToken(t *testing.T) {
	server := httptest.NewServer(
		http.HandlerFunc(
			func(response http.ResponseWriter, _ *http.Request) {
				http.Error(response, "failed", http.StatusBadGateway)
			},
		),
	)
	defer server.Close()
	service := NewDevinAuthService(server.Client())
	service.SetAPIBaseURL(server.URL)
	service.SetServerBaseURL(server.URL)

	record, err := service.CreateAuthRecord(context.Background(), "private-token-value")
	if err != nil {
		t.Fatalf("CreateAuthRecord(): %v", err)
	}
	if strings.Contains(record.FileName, "private-token-value") || !strings.HasPrefix(record.FileName, "devin-user-") {
		t.Fatalf("fallback filename = %q", record.FileName)
	}
	if accountType, account := record.AccountInfo(); accountType != coreauth.AuthKindOAuth || account != "" {
		t.Fatalf("browser-facing account info exposes fallback credential: %q/%q", accountType, account)
	}
	if !record.Quota.ObservedAt.IsZero() || len(record.Quota.Signals) != 0 {
		t.Fatalf("failed quota enrichment produced an observation: %+v", record.Quota)
	}
}
