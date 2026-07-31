//go:build windows

package util

import (
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/windows"
)

var reOpenFileProc = windows.NewLazySystemDLL("kernel32.dll").NewProc("ReOpenFile")

// ValidateFileOwner requires file to be owned by the current process user.
func ValidateFileOwner(file *os.File) error {
	if file == nil {
		return fmt.Errorf("file is nil")
	}
	securityDescriptor, err := windows.GetSecurityInfo(
		windows.Handle(file.Fd()),
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION,
	)
	if err != nil {
		return fmt.Errorf("inspect file owner: %w", err)
	}
	owner, _, err := securityDescriptor.Owner()
	if err != nil {
		return fmt.Errorf("read file owner: %w", err)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return fmt.Errorf("read process user: %w", err)
	}
	if owner == nil || !windows.EqualSid(owner, user.User.Sid) {
		return fmt.Errorf("file is not owned by the current process user")
	}
	return nil
}

// SecurePrivateFile applies an owner-only protected DACL using a handle with
// WRITE_DAC access reopened from the already validated file handle.
func SecurePrivateFile(file *os.File, directory bool) error {
	if err := ValidateFileOwner(file); err != nil {
		return err
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	inheritance := uint32(windows.NO_INHERITANCE)
	if directory {
		inheritance = windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT
	}
	acl, err := windows.ACLFromEntries(
		[]windows.EXPLICIT_ACCESS{
			{
				AccessPermissions: windows.GENERIC_ALL,
				AccessMode:        windows.SET_ACCESS,
				Inheritance:       inheritance,
				Trustee: windows.TRUSTEE{
					TrusteeForm:  windows.TRUSTEE_IS_SID,
					TrusteeType:  windows.TRUSTEE_IS_USER,
					TrusteeValue: windows.TrusteeValueFromSID(user.User.Sid),
				},
			},
		},
		nil,
	)
	if err != nil {
		return err
	}
	handle, err := reOpenFileWithWriteDAC(file)
	if err != nil {
		return err
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	return windows.SetSecurityInfo(
		handle,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		acl,
		nil,
	)
}

func reOpenFileWithWriteDAC(file *os.File) (windows.Handle, error) {
	handle, _, callErr := reOpenFileProc.Call(
		file.Fd(),
		uintptr(windows.WRITE_DAC|windows.READ_CONTROL),
		uintptr(windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE),
		uintptr(windows.FILE_FLAG_BACKUP_SEMANTICS),
	)
	if windows.Handle(handle) != windows.InvalidHandle {
		return windows.Handle(handle), nil
	}
	if callErr != nil && callErr != syscall.Errno(0) {
		return windows.InvalidHandle, callErr
	}
	return windows.InvalidHandle, windows.ERROR_INVALID_HANDLE
}
