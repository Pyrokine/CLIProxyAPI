package store

import "testing"

func TestGitTokenStoreAuthIDCanonicalizesWindowsCase(t *testing.T) {
	store := &GitTokenStore{}
	if got := store.idFor(`C:\auth\SHARED.json`, `c:\AUTH`); got != "shared.json" {
		t.Fatalf("GitTokenStore.idFor() = %q", got)
	}
}
