//go:build windows

package store

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/go-git/go-git/v6"
	"golang.org/x/sys/windows"
)

func TestGitTokenStoreSaveProtectsAuthDirectoryAndFileDACL(t *testing.T) {
	remoteDir := filepath.Join(t.TempDir(), "remote.git")
	if _, errInit := git.PlainInit(remoteDir, true); errInit != nil {
		t.Fatalf("initialize remote repository: %v", errInit)
	}
	authDir := filepath.Join(t.TempDir(), "auths")
	store := NewGitTokenStore(remoteDir, "", "", "")
	store.SetBaseDir(authDir)
	path, errSave := store.Save(context.Background(), testWindowsStoreAuth("git.json"))
	if errSave != nil {
		t.Fatalf("Save() error = %v", errSave)
	}
	assertStoreOwnerOnlyProtectedDACL(t, authDir)
	assertStoreOwnerOnlyProtectedDACL(t, path)
}

func TestObjectTokenStoreSaveProtectsAuthDirectoryAndFileDACL(t *testing.T) {
	store := newObjectStoreForTest(t, http.HandlerFunc(successfulObjectPut))
	path, errSave := store.Save(context.Background(), testWindowsStoreAuth("object.json"))
	if errSave != nil {
		t.Fatalf("Save() error = %v", errSave)
	}
	assertStoreOwnerOnlyProtectedDACL(t, store.AuthDir())
	assertStoreOwnerOnlyProtectedDACL(t, path)
}

func TestPostgresStoreSaveProtectsAuthDirectoryAndFileDACL(t *testing.T) {
	store, _ := newPostgresStoreForTest(t)
	path, errSave := store.Save(context.Background(), testWindowsStoreAuth("postgres.json"))
	if errSave != nil {
		t.Fatalf("Save() error = %v", errSave)
	}
	assertStoreOwnerOnlyProtectedDACL(t, store.AuthDir())
	assertStoreOwnerOnlyProtectedDACL(t, path)
}

func testWindowsStoreAuth(fileName string) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{
		ID:       fileName,
		FileName: fileName,
		Metadata: map[string]any{"type": "codex", "access_token": "secret"},
	}
}

func assertStoreOwnerOnlyProtectedDACL(t *testing.T, path string) {
	t.Helper()
	if _, errStat := os.Stat(path); errStat != nil {
		t.Fatalf("stat %s: %v", path, errStat)
	}
	securityDescriptor, errSecurity := windows.GetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION,
	)
	if errSecurity != nil {
		t.Fatalf("read %s security descriptor: %v", path, errSecurity)
	}
	owner, ownerDefaulted, errOwner := securityDescriptor.Owner()
	if errOwner != nil || owner == nil || ownerDefaulted {
		t.Fatalf(
			"read %s owner: owner=%v defaulted=%t error=%v",
			path,
			owner,
			ownerDefaulted,
			errOwner,
		)
	}
	control, _, errControl := securityDescriptor.Control()
	if errControl != nil {
		t.Fatalf("read %s DACL control: %v", path, errControl)
	}
	dacl, _, errDACL := securityDescriptor.DACL()
	if errDACL != nil {
		t.Fatalf("read %s DACL: %v", path, errDACL)
	}
	if control&windows.SE_DACL_PROTECTED == 0 || dacl == nil || dacl.AceCount != 1 {
		t.Fatalf("%s DACL control = %#x, DACL = %#v", path, control, dacl)
	}
}
