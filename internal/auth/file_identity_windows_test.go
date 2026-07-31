package auth

import "testing"

func TestFileAuthIDCanonicalizesWindowsCase(t *testing.T) {
	got := FileAuthID(`C:\auth\SHARED.json`, `c:\AUTH`)
	if got != "shared.json" {
		t.Fatalf("FileAuthID() = %q", got)
	}
}
