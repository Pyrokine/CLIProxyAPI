//go:build windows

package util

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestSecurePrivateFileCanSetDACLWithReopenedHandle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credential")
	if err := os.WriteFile(path, []byte("secret"), 0o600); err != nil {
		t.Fatalf("write credential: %v", err)
	}
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open credential: %v", err)
	}
	if err = SecurePrivateFile(file, false); err != nil {
		_ = file.Close()
		t.Fatalf("SecurePrivateFile() error = %v", err)
	}
	securityDescriptor, err := windows.GetSecurityInfo(
		windows.Handle(file.Fd()),
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION,
	)
	if err != nil {
		_ = file.Close()
		t.Fatalf("GetSecurityInfo() error = %v", err)
	}
	dacl, _, err := securityDescriptor.DACL()
	if err != nil {
		_ = file.Close()
		t.Fatalf("read credential DACL: %v", err)
	}
	if dacl == nil {
		_ = file.Close()
		t.Fatal("credential DACL is nil")
	}
	control, _, errControl := securityDescriptor.Control()
	if errControl != nil {
		_ = file.Close()
		t.Fatalf("read credential DACL control: %v", errControl)
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		_ = file.Close()
		t.Fatalf("credential DACL control = %#x, want protected", control)
	}
	if dacl.AceCount != 1 {
		_ = file.Close()
		t.Fatalf("credential DACL ACE count = %d, want owner-only ACE", dacl.AceCount)
	}
	if err = file.Close(); err != nil {
		t.Fatalf("close credential: %v", err)
	}
}
