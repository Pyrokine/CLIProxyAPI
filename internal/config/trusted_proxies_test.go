package config

import (
	"reflect"
	"strings"
	"testing"
)

func TestNormalizeTrustedProxies(t *testing.T) {
	got, err := normalizeTrustedProxies(
		[]string{
			" 127.0.0.1 ",
			"127.0.0.1/32",
			"10.0.0.7/8",
			"::1",
			"::1",
		},
	)
	if err != nil {
		t.Fatalf("NormalizeTrustedProxies: %v", err)
	}
	want := []string{"127.0.0.1", "127.0.0.1/32", "10.0.0.0/8", "::1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("NormalizeTrustedProxies = %v, want %v", got, want)
	}
}

func TestParseConfigBytesRejectsInvalidTrustedProxy(t *testing.T) {
	_, err := ParseConfigBytes([]byte("remote-management:\n  trusted-proxies:\n    - not-an-ip\n"))
	if err == nil || !strings.Contains(err.Error(), "remote-management.trusted-proxies") {
		t.Fatalf("ParseConfigBytes error = %v, want trusted proxy validation error", err)
	}
}

func TestParseConfigBytesNormalizesTrustedProxies(t *testing.T) {
	cfg, err := ParseConfigBytes([]byte("remote-management:\n  trusted-proxies:\n    - 10.1.2.3/8\n    - ::1\n"))
	if err != nil {
		t.Fatalf("ParseConfigBytes: %v", err)
	}
	want := []string{"10.0.0.0/8", "::1"}
	if !reflect.DeepEqual(cfg.RemoteManagement.TrustedProxies, want) {
		t.Fatalf("TrustedProxies = %v, want %v", cfg.RemoteManagement.TrustedProxies, want)
	}
}

func TestNormalizeTrustedProxiesRejectsFullAddressSpace(t *testing.T) {
	for _, value := range []string{"0.0.0.0/0", "::/0", "10.1.2.3/0"} {
		t.Run(
			value, func(t *testing.T) {
				_, err := normalizeTrustedProxies([]string{value})
				if err == nil || !strings.Contains(err.Error(), "full address space") {
					t.Fatalf("NormalizeTrustedProxies(%q) error = %v, want full address space rejection", value, err)
				}
			},
		)
	}
}
