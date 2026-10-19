package auth

import (
	"testing"

	metaauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/meta"
)

func TestMetaRecordFromBundleKeepsDCASeparateFromAPIKey(t *testing.T) {
	bundle := &metaauth.MetaAuthBundle{
		TokenData: &metaauth.TokenData{AccessToken: "dca:fixture", ExpiresAt: 1750000000},
		MintedKey: &metaauth.MintedKeyResponse{
			APIKey: "fixture-api-key", UserEmail: "fixture@example.invalid", SubsTierName: "Muse Pro",
		},
	}
	record, err := MetaRecordFromBundle(metaauth.NewMetaAuth(nil), bundle)
	if err != nil {
		t.Fatalf("MetaRecordFromBundle: %v", err)
	}
	if record.Provider != "meta" || record.Attributes["auth_kind"] != "oauth" ||
		record.Attributes["api_key"] != "fixture-api-key" || record.Attributes["dca_token"] != "dca:fixture" ||
		record.Metadata["subs_tier_name"] != "Muse Pro" {
		t.Fatalf("Meta record did not preserve separate credential and subscription fields")
	}
	storage, ok := record.Storage.(*metaauth.MetaTokenStorage)
	if !ok || storage.AccessToken != "fixture-api-key" || storage.DCAToken != "dca:fixture" {
		t.Fatal("Meta storage must use minted API key for execution and retain DCA only for refresh")
	}
}

func TestMetaRecordFromBundleRequiresMintedAPIKey(t *testing.T) {
	for _, bundle := range []*metaauth.MetaAuthBundle{
		{TokenData: &metaauth.TokenData{AccessToken: "dca:fixture"}},
		{TokenData: &metaauth.TokenData{AccessToken: "dca:fixture"}, MintedKey: &metaauth.MintedKeyResponse{APIKey: "dca:fixture"}},
	} {
		if record, err := MetaRecordFromBundle(metaauth.NewMetaAuth(nil), bundle); record != nil || err == nil {
			t.Fatalf("invalid Meta bundle returned a usable record: %v", err)
		}
	}
}
