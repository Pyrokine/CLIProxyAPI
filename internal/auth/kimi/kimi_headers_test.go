package kimi

import (
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/buildinfo"
)

func TestCommonHeadersUseBuildVersion(t *testing.T) {
	client := NewDeviceFlowClientWithDeviceID(nil, "device-1")
	headers := client.commonHeaders()

	if got := headers["X-Msh-Platform"]; got != "CLIProxyAPI" {
		t.Fatalf("X-Msh-Platform = %q, want CLIProxyAPI", got)
	}
	if got := headers["X-Msh-Version"]; got != buildinfo.Version {
		t.Fatalf("X-Msh-Version = %q, want %q", got, buildinfo.Version)
	}
	if got := headers["X-Msh-Device-Id"]; got != "device-1" {
		t.Fatalf("X-Msh-Device-Id = %q, want device-1", got)
	}
}
