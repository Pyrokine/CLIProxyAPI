package signature

import (
	"crypto/sha256"
	"encoding/base64"
	"testing"
)

func validGrokEncryptedContentForTest() string {
	payload := make([]byte, 0, 128)
	for i := byte(0); len(payload) < 128; i++ {
		sum := sha256.Sum256([]byte{i, i + 17, i + 43})
		payload = append(payload, sum[:]...)
	}
	payload = payload[:128]
	return base64.RawStdEncoding.EncodeToString(payload)
}

func TestInspectGrokEncryptedContentAcceptsNativeShape(t *testing.T) {
	sig := validGrokEncryptedContentForTest()
	info, err := InspectGrokEncryptedContent(sig)
	if err != nil {
		t.Fatalf("InspectGrokEncryptedContent error: %v", err)
	}
	if info.RawLen != len(sig) || info.DecodedLen != 128 {
		t.Fatalf("info = %+v, want raw len %d and decoded len 128", info, len(sig))
	}
}

func TestInspectGrokEncryptedContentRejectsForeignSignatures(t *testing.T) {
	for _, tt := range []struct {
		name string
		sig  string
	}{
		{name: "gpt", sig: "gAAAA" + validGrokEncryptedContentForTest()},
		{name: "padded base64", sig: validGrokEncryptedContentForTest() + "="},
		{name: "invalid character", sig: validGrokEncryptedContentForTest() + "-"},
	} {
		t.Run(
			tt.name, func(t *testing.T) {
				if _, err := InspectGrokEncryptedContent(tt.sig); err == nil {
					t.Fatalf("InspectGrokEncryptedContent accepted %q", tt.name)
				}
			},
		)
	}
}
