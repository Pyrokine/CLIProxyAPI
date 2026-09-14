package artifact

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strings"
)

const accessTokenVersion = "v1"

type AccessSigner struct {
	namespace string
	key       [sha256.Size]byte
}

func NewAccessSigner(namespace string, key [sha256.Size]byte) (*AccessSigner, error) {
	namespace = strings.TrimSpace(namespace)
	if namespace == "" {
		return nil, fmt.Errorf("artifact access namespace is empty")
	}
	return &AccessSigner{namespace: namespace, key: key}, nil
}

func (s *AccessSigner) Sign(principal string, artifactID string) (string, error) {
	if s == nil {
		return "", fmt.Errorf("artifact access signer is unavailable")
	}
	principal = strings.TrimSpace(principal)
	if principal == "" {
		return "", fmt.Errorf("artifact principal is empty")
	}
	if err := ValidateID(artifactID); err != nil {
		return "", err
	}
	mac := s.mac(principal, artifactID)
	return accessTokenVersion + "." + base64.RawURLEncoding.EncodeToString(mac), nil
}

func (s *AccessSigner) Verify(principal string, artifactID string, token string) bool {
	if s == nil {
		return false
	}
	principal = strings.TrimSpace(principal)
	if principal == "" || ValidateID(artifactID) != nil {
		return false
	}
	version, encodedMAC, ok := strings.Cut(strings.TrimSpace(token), ".")
	if !ok || version != accessTokenVersion || encodedMAC == "" {
		return false
	}
	providedMAC, err := base64.RawURLEncoding.DecodeString(encodedMAC)
	if err != nil || len(providedMAC) != sha256.Size {
		return false
	}
	return hmac.Equal(providedMAC, s.mac(principal, artifactID))
}

func (s *AccessSigner) mac(principal string, artifactID string) []byte {
	principalDigest := sha256.Sum256([]byte(strings.TrimSpace(principal)))
	mac := hmac.New(sha256.New, s.key[:])
	_, _ = mac.Write([]byte(accessTokenVersion))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(s.namespace))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write(principalDigest[:])
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(artifactID))
	return mac.Sum(nil)
}
