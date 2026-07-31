package watcher

import (
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/watcher/synthesizer"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestAuthEqualDetectsGeminiVirtualCredentialOwnerChange(t *testing.T) {
	ctx := &synthesizer.SynthesisContext{AuthDir: t.TempDir(), Now: time.Now()}
	path := ctx.AuthDir + "/gemini.json"
	oldAuths, errOld := synthesizer.SynthesizeAuthFile(
		ctx,
		path,
		[]byte(`{"type":"gemini","email":"user@example.com","project_id":"one,two","token":{"refresh_token":"old-refresh"}}`),
	)
	if errOld != nil {
		t.Fatal(errOld)
	}
	newAuths, errNew := synthesizer.SynthesizeAuthFile(
		ctx,
		path,
		[]byte(`{"type":"gemini","email":"user@example.com","project_id":"one,two","token":{"refresh_token":"new-refresh"}}`),
	)
	if errNew != nil {
		t.Fatal(errNew)
	}
	oldVirtual := findAuthByID(oldAuths, "gemini.json::one")
	newVirtual := findAuthByID(newAuths, "gemini.json::one")
	if oldVirtual == nil || newVirtual == nil {
		t.Fatalf("virtual auths missing: old=%#v new=%#v", oldAuths, newAuths)
	}
	if authEqual(oldVirtual, newVirtual) {
		t.Fatal("virtual auth owner change was treated as unchanged")
	}
}

func findAuthByID(auths []*coreauth.Auth, id string) *coreauth.Auth {
	for _, auth := range auths {
		if auth != nil && auth.ID == id {
			return auth
		}
	}
	return nil
}
