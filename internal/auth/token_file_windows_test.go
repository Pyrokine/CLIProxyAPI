//go:build windows

package auth

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestPublishTokenFileProtectsDirectoryAndFileDACL(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "auth")
	path := filepath.Join(dir, "token.json")
	if errWrite := WriteTokenFileAtomic(path, []byte(`{"type":"codex"}`)); errWrite != nil {
		t.Fatalf("WriteTokenFileAtomic() error = %v", errWrite)
	}
	assertOwnerOnlyProtectedDACL(t, dir)
	assertOwnerOnlyProtectedDACL(t, path)
}

func assertOwnerOnlyProtectedDACL(t *testing.T, path string) {
	t.Helper()
	file, errOpen := os.Open(path)
	if errOpen != nil {
		t.Fatalf("open %s: %v", path, errOpen)
	}
	defer func() { _ = file.Close() }()
	securityDescriptor, errSecurity := windows.GetSecurityInfo(
		windows.Handle(file.Fd()),
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION,
	)
	if errSecurity != nil {
		t.Fatalf("read %s security descriptor: %v", path, errSecurity)
	}
	control, _, errControl := securityDescriptor.Control()
	if errControl != nil {
		t.Fatalf("read %s security descriptor control: %v", path, errControl)
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		t.Fatalf("%s DACL control = %#x, want protected", path, control)
	}
	dacl, _, errDACL := securityDescriptor.DACL()
	if errDACL != nil {
		t.Fatalf("read %s DACL: %v", path, errDACL)
	}
	if dacl == nil || dacl.AceCount != 1 {
		t.Fatalf("%s DACL = %#v, want one owner ACE", path, dacl)
	}
}
