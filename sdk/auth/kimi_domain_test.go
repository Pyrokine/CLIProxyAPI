package auth

import (
	"strings"
	"testing"
	"time"

	kimiauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/kimi"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestKimiRecordFromBundleSeparatesBothServices(t *testing.T) {
	for _, tc := range []struct {
		domain   string
		provider string
		baseURL  string
	}{
		{domain: kimiauth.KimiDefaultDomain, provider: "kimi", baseURL: kimiauth.APIBaseURL},
		{domain: kimiauth.KimiAIDomain, provider: "kimi-ai", baseURL: kimiauth.KimiAIAPIBaseURL},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			svc := kimiauth.NewAuthWithDomain(nil, tc.domain)
			bundle := &kimiauth.AuthBundle{DeviceID: "device-1", TokenData: &kimiauth.TokenData{
				AccessToken: "test-access", RefreshToken: "test-refresh", ExpiresAt: time.Now().Add(time.Hour).Unix(),
			}}
			record, err := KimiRecordFromBundle(svc, bundle)
			if err != nil {
				t.Fatalf("convert credential: %v", err)
			}
			storage, ok := record.Storage.(*kimiauth.TokenStorage)
			if !ok || record.Provider != tc.provider || !strings.HasPrefix(record.FileName, tc.provider+"-") ||
				record.Metadata["type"] != tc.provider || record.Metadata["domain"] != tc.domain ||
				record.Metadata["base_url"] != tc.baseURL || record.Attributes["domain"] != tc.domain ||
				record.Attributes["base_url"] != tc.baseURL || storage.Type != tc.provider {
				t.Fatalf("incorrect Kimi service credential: %+v, storage=%+v", record, storage)
			}
			if coreauth.ProviderRefreshLead(record.Provider, nil) == nil {
				t.Fatal("refresh threshold is not registered")
			}
		})
	}
	if NewKimiAIAuthenticator().Provider() != "kimi-ai" || NewKimiAuthenticator().Provider() != "kimi" {
		t.Fatal("Kimi authenticators share a provider identifier")
	}
}

func TestKimiRecordFromBundleRejectsMissingToken(t *testing.T) {
	if _, err := KimiRecordFromBundle(kimiauth.NewAuth(nil), &kimiauth.AuthBundle{TokenData: &kimiauth.TokenData{}}); err == nil {
		t.Fatal("empty token was accepted as an OAuth credential")
	}
}
