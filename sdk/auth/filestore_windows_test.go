package auth

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/watcher/synthesizer"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"golang.org/x/sys/windows"
)

func TestFileTokenStoreSecuresUnchangedCredentialDACL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "unchanged.json")
	if errWrite := os.WriteFile(path, []byte(`{"disabled":false,"type":"codex"}`), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	store := NewFileTokenStore()
	store.SetBaseDir(dir)
	if _, errSave := store.Save(
		context.Background(),
		&cliproxyauth.Auth{
			ID:       "unchanged.json",
			FileName: "unchanged.json",
			Metadata: map[string]any{"disabled": false, "type": "codex"},
		},
	); errSave != nil {
		t.Fatalf("Save() error = %v", errSave)
	}
	assertFileTokenStoreOwnerOnlyProtectedDACL(t, dir)
	assertFileTokenStoreOwnerOnlyProtectedDACL(t, path)
}

func assertFileTokenStoreOwnerOnlyProtectedDACL(t *testing.T, path string) {
	t.Helper()
	file, errOpen := os.Open(path)
	if errOpen != nil {
		t.Fatal(errOpen)
	}
	defer func() { _ = file.Close() }()
	securityDescriptor, errSecurity := windows.GetSecurityInfo(
		windows.Handle(file.Fd()),
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION,
	)
	if errSecurity != nil {
		t.Fatal(errSecurity)
	}
	control, _, errControl := securityDescriptor.Control()
	if errControl != nil {
		t.Fatal(errControl)
	}
	dacl, _, errDACL := securityDescriptor.DACL()
	if errDACL != nil {
		t.Fatal(errDACL)
	}
	if control&windows.SE_DACL_PROTECTED == 0 || dacl == nil || dacl.AceCount != 1 {
		t.Fatalf("%s DACL control = %#x, DACL = %#v", path, control, dacl)
	}
}

func TestFileTokenStoreListCanonicalizesWindowsCaseVariantID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "SHARED.json")
	if errWrite := os.WriteFile(
		path,
		[]byte(`{"type":"codex","refresh_token":"refresh"}`),
		0o600,
	); errWrite != nil {
		t.Fatal(errWrite)
	}
	store := NewFileTokenStore()
	store.SetBaseDir(dir)
	auths, errList := store.List(context.Background())
	if errList != nil {
		t.Fatal(errList)
	}
	if len(auths) != 1 || auths[0] == nil || auths[0].ID != "shared.json" ||
		auths[0].FileName != "shared.json" {
		t.Fatalf("FileTokenStore.List() = %#v", auths)
	}
	synthesized, errSynthesize := synthesizer.SynthesizeAuthFile(
		&synthesizer.SynthesisContext{AuthDir: dir, Now: time.Now()},
		path,
		[]byte(`{"type":"codex","refresh_token":"refresh"}`),
	)
	if errSynthesize != nil {
		t.Fatal(errSynthesize)
	}
	if len(synthesized) != 1 || synthesized[0] == nil || synthesized[0].ID != auths[0].ID {
		t.Fatalf("startup and watcher identities differ: startup=%#v watcher=%#v", auths, synthesized)
	}
}
