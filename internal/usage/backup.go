package usage

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"golang.org/x/crypto/scrypt"
)

const (
	usageBackupFormat       = "cliproxy-usage-backup"
	usageBackupVersion      = 3
	usageBackupKDFAlgorithm = "scrypt"
	usageBackupKDFN         = 1 << 15
	usageBackupKDFR         = 8
	usageBackupKDFP         = 1
	usageBackupKeyBytes     = 32
	usageBackupSaltBytes    = 16
	usageBackupIdentityType = "hmac-sha256:"
)

// MaxPortableBackupBytes is the maximum encoded backup accepted by every
// portable usage import path and emitted by the management export endpoint.
const MaxPortableBackupBytes int64 = 100 << 20

// MaxPortableBackupPlaintextBytes leaves enough room for AES-GCM overhead,
// base64 expansion and the visible envelope before the final file reaches the
// portable limit.
const MaxPortableBackupPlaintextBytes = ((MaxPortableBackupBytes - 1024) * 3 / 4) - 16

var ErrPortableBackupTooLarge = errors.New("usage: portable backup exceeds size limit")

type usageBackupKeyDerivation struct {
	Algorithm string `json:"algorithm"`
	Salt      string `json:"salt"`
	N         int    `json:"n"`
	R         int    `json:"r"`
	P         int    `json:"p"`
	KeyBytes  int    `json:"key_bytes"`
}

type usageBackupEvent struct {
	Timestamp           time.Time   `json:"timestamp"`
	Model               string      `json:"model"`
	Source              string      `json:"source"`
	AuthIndex           string      `json:"auth_index"`
	APIKeyIdentity      string      `json:"api_key_identity,omitempty"`
	Provider            string      `json:"provider"`
	LatencyMs           int64       `json:"latency_ms,omitempty"`
	RequestID           string      `json:"request_id,omitempty"`
	TimeToFirstByteMs   int64       `json:"time_to_first_byte_ms,omitempty"`
	TotalDurationMs     int64       `json:"total_duration_ms,omitempty"`
	Completed           bool        `json:"completed"`
	MetadataRecorded    bool        `json:"metadata_recorded,omitempty"`
	ReasoningEffort     string      `json:"reasoning_effort,omitempty"`
	ServiceTier         string      `json:"service_tier,omitempty"`
	RequestServiceTier  string      `json:"request_service_tier,omitempty"`
	ResponseServiceTier string      `json:"response_service_tier,omitempty"`
	ExecutorType        string      `json:"executor_type,omitempty"`
	Thinking            *Thinking   `json:"thinking,omitempty"`
	ResponseHeaders     http.Header `json:"response_headers,omitempty"`
	Tokens              tokenStats  `json:"tokens"`
	CostMicro           int64       `json:"cost_micro"`
	Failed              bool        `json:"failed"`
}

type usageBackupContent struct {
	ExportedAt time.Time          `json:"exported_at"`
	APIKeys    map[string]string  `json:"api_keys,omitempty"`
	Events     []usageBackupEvent `json:"events"`
}

type usageBackupEnvelope struct {
	Format        string                   `json:"format"`
	Version       int                      `json:"version"`
	KeyDerivation usageBackupKeyDerivation `json:"key_derivation"`
	Nonce         string                   `json:"nonce"`
	Ciphertext    string                   `json:"ciphertext"`
}

// IsUsageBackup reports whether data declares the encrypted usage backup format.
func IsUsageBackup(data []byte) bool {
	var probe struct {
		Format  string `json:"format"`
		Version int    `json:"version"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return false
	}
	return probe.Format == usageBackupFormat && probe.Version == usageBackupVersion
}

// MarshalUsageBackup encrypts the complete canonical backup content, including
// API keys, event metadata and API key identities.
func MarshalUsageBackup(details []FlatDetail, secret string, exportedAt time.Time) ([]byte, error) {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return nil, errors.New("usage: backup encryption key is empty")
	}

	salt := make([]byte, usageBackupSaltBytes)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("usage: generate backup salt: %w", err)
	}
	kdf := usageBackupKeyDerivation{
		Algorithm: usageBackupKDFAlgorithm,
		Salt:      base64.RawStdEncoding.EncodeToString(salt),
		N:         usageBackupKDFN,
		R:         usageBackupKDFR,
		P:         usageBackupKDFP,
		KeyBytes:  usageBackupKeyBytes,
	}
	encryptionKey, identityKey, err := deriveUsageBackupKeys(secret, salt)
	if err != nil {
		return nil, err
	}
	gcm, err := newUsageBackupAEAD(encryptionKey)
	if err != nil {
		return nil, err
	}

	content := usageBackupContent{
		ExportedAt: exportedAt.UTC(),
		APIKeys:    make(map[string]string),
		Events:     make([]usageBackupEvent, 0, len(details)),
	}
	for i := range details {
		detail := details[i]
		if err = validateSanitizedResponseHeaders(detail.ResponseHeaders); err != nil {
			return nil, fmt.Errorf("usage: backup event[%d]: %w", i, err)
		}
		if detail.costTrusted && detail.costMicro < 0 {
			return nil, fmt.Errorf("usage: backup event[%d]: negative cost_micro", i)
		}
		identity := ""
		if detail.APIKey != "" {
			identity = usageBackupAPIKeyIdentity(identityKey, detail.APIKey)
			if previous, exists := content.APIKeys[identity]; exists && previous != detail.APIKey {
				return nil, errors.New("usage: API key identity collision")
			}
			content.APIKeys[identity] = detail.APIKey
		}
		content.Events = append(content.Events, usageBackupEventFromFlatDetail(detail, identity))
	}
	if len(content.APIKeys) == 0 {
		content.APIKeys = nil
	}
	plaintext, err := json.Marshal(content)
	if err != nil {
		return nil, fmt.Errorf("usage: encode backup content: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("usage: generate backup nonce: %w", err)
	}
	ciphertext := gcm.Seal(nil, nonce, plaintext, usageBackupAAD(kdf))
	return json.Marshal(
		usageBackupEnvelope{
			Format:        usageBackupFormat,
			Version:       usageBackupVersion,
			KeyDerivation: kdf,
			Nonce:         base64.RawStdEncoding.EncodeToString(nonce),
			Ciphertext:    base64.RawStdEncoding.EncodeToString(ciphertext),
		},
	)
}

// ParseUsageBackup authenticates and decrypts a version 3 backup before
// reconstructing canonical FlatDetail records.
func ParseUsageBackup(data []byte, secret string) ([]FlatDetail, error) {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return nil, errors.New("usage: backup decryption key is empty")
	}
	var envelope usageBackupEnvelope
	if err := decodeSingleJSONValue(data, &envelope); err != nil {
		return nil, fmt.Errorf("usage: decode backup: %w", err)
	}
	if envelope.Format != usageBackupFormat || envelope.Version != usageBackupVersion {
		return nil, fmt.Errorf("usage: unsupported backup format %q version %d", envelope.Format, envelope.Version)
	}
	if err := validateUsageBackupKDF(envelope.KeyDerivation); err != nil {
		return nil, err
	}
	salt, err := base64.RawStdEncoding.DecodeString(envelope.KeyDerivation.Salt)
	if err != nil || len(salt) != usageBackupSaltBytes {
		return nil, errors.New("usage: invalid backup KDF salt")
	}
	encryptionKey, identityKey, err := deriveUsageBackupKeys(secret, salt)
	if err != nil {
		return nil, err
	}
	gcm, err := newUsageBackupAEAD(encryptionKey)
	if err != nil {
		return nil, err
	}
	nonce, err := base64.RawStdEncoding.DecodeString(envelope.Nonce)
	if err != nil || len(nonce) != gcm.NonceSize() {
		return nil, errors.New("usage: invalid backup nonce")
	}
	ciphertext, err := base64.RawStdEncoding.DecodeString(envelope.Ciphertext)
	if err != nil {
		return nil, errors.New("usage: invalid backup ciphertext")
	}
	plaintext, err := gcm.Open(nil, nonce, ciphertext, usageBackupAAD(envelope.KeyDerivation))
	if err != nil {
		return nil, errors.New("usage: backup decryption failed; use the key that created the backup")
	}

	var content usageBackupContent
	if err = decodeSingleJSONValue(plaintext, &content); err != nil {
		return nil, fmt.Errorf("usage: decode backup content: %w", err)
	}
	apiKeys := make(map[string]string, len(content.APIKeys))
	for identity, rawAPIKey := range content.APIKeys {
		if !validUsageBackupAPIKeyIdentity(identity) {
			return nil, errors.New("usage: invalid backup API key identity")
		}
		if !hmac.Equal([]byte(identity), []byte(usageBackupAPIKeyIdentity(identityKey, rawAPIKey))) {
			return nil, errors.New("usage: backup API key identity verification failed")
		}
		apiKeys[identity] = rawAPIKey
	}

	details := make([]FlatDetail, 0, len(content.Events))
	for i := range content.Events {
		event := content.Events[i]
		if event.CostMicro < 0 {
			return nil, fmt.Errorf("usage: backup event[%d] has negative cost_micro", i)
		}
		if err = validateSanitizedResponseHeaders(event.ResponseHeaders); err != nil {
			return nil, fmt.Errorf("usage: backup event[%d]: %w", i, err)
		}
		rawAPIKey := ""
		if event.APIKeyIdentity != "" {
			var ok bool
			rawAPIKey, ok = apiKeys[event.APIKeyIdentity]
			if !ok {
				return nil, fmt.Errorf("usage: backup event[%d] references an unknown API key identity", i)
			}
		}
		details = append(details, event.flatDetail(rawAPIKey))
	}
	return details, nil
}

func decodeSingleJSONValue(data []byte, dst any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			err = errors.New("unexpected trailing JSON value")
		}
		return err
	}
	return nil
}

func validateUsageBackupKDF(kdf usageBackupKeyDerivation) error {
	if kdf.Algorithm != usageBackupKDFAlgorithm ||
		kdf.N != usageBackupKDFN ||
		kdf.R != usageBackupKDFR ||
		kdf.P != usageBackupKDFP ||
		kdf.KeyBytes != usageBackupKeyBytes {
		return errors.New("usage: unsupported backup key derivation parameters")
	}
	return nil
}

func deriveUsageBackupKeys(secret string, salt []byte) ([]byte, []byte, error) {
	master, err := scrypt.Key(
		[]byte(secret),
		salt,
		usageBackupKDFN,
		usageBackupKDFR,
		usageBackupKDFP,
		usageBackupKeyBytes,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("usage: derive backup key: %w", err)
	}
	return usageBackupSubkey(master, "encryption-v1"), usageBackupSubkey(master, "identity-v1"), nil
}

func usageBackupSubkey(master []byte, label string) []byte {
	mac := hmac.New(sha256.New, master)
	_, _ = mac.Write([]byte("cliproxy-usage-backup-subkey-v1"))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(label))
	return mac.Sum(nil)
}

func newUsageBackupAEAD(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("usage: initialize backup cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("usage: initialize backup AEAD: %w", err)
	}
	return gcm, nil
}

func usageBackupAPIKeyIdentity(identityKey []byte, apiKey string) string {
	mac := hmac.New(sha256.New, identityKey)
	_, _ = mac.Write([]byte("cliproxy-usage-api-key-identity-v1"))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(apiKey))
	return usageBackupIdentityType + hex.EncodeToString(mac.Sum(nil))
}

func validUsageBackupAPIKeyIdentity(identity string) bool {
	if !strings.HasPrefix(identity, usageBackupIdentityType) {
		return false
	}
	digest := strings.TrimPrefix(identity, usageBackupIdentityType)
	if len(digest) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(digest)
	return err == nil
}

func usageBackupAAD(kdf usageBackupKeyDerivation) []byte {
	return fmt.Appendf(
		nil,
		"%s\x00%d\x00%s\x00%s\x00%d\x00%d\x00%d\x00%d",
		usageBackupFormat,
		usageBackupVersion,
		kdf.Algorithm,
		kdf.Salt,
		kdf.N,
		kdf.R,
		kdf.P,
		kdf.KeyBytes,
	)
}

func estimateUsageBackupDetailBytes(detail FlatDetail, includeAPIKey bool) (int64, error) {
	identity := ""
	if detail.APIKey != "" {
		identity = usageBackupIdentityType + strings.Repeat("0", sha256.Size*2)
	}
	event, err := json.Marshal(usageBackupEventFromFlatDetail(detail, identity))
	if err != nil {
		return 0, err
	}
	estimated := int64(len(event) + 1)
	if includeAPIKey {
		apiKey, errMarshal := json.Marshal(detail.APIKey)
		if errMarshal != nil {
			return 0, errMarshal
		}
		estimated += int64(len(identity) + len(apiKey) + 4)
	}
	return estimated, nil
}

func usageBackupEventFromFlatDetail(detail FlatDetail, identity string) usageBackupEvent {
	return usageBackupEvent{
		Timestamp:           detail.Timestamp,
		Model:               detail.Model,
		Source:              detail.Source,
		AuthIndex:           detail.AuthIndex,
		APIKeyIdentity:      identity,
		Provider:            detail.Provider,
		LatencyMs:           detail.LatencyMs,
		RequestID:           detail.RequestID,
		TimeToFirstByteMs:   detail.TimeToFirstByteMs,
		TotalDurationMs:     detail.TotalDurationMs,
		Completed:           detail.Completed,
		MetadataRecorded:    detail.MetadataRecorded,
		ReasoningEffort:     detail.ReasoningEffort,
		ServiceTier:         detail.ServiceTier,
		RequestServiceTier:  detail.RequestServiceTier,
		ResponseServiceTier: detail.ResponseServiceTier,
		ExecutorType:        detail.ExecutorType,
		Thinking:            cloneThinking(detail.Thinking),
		ResponseHeaders:     cloneHTTPHeader(detail.ResponseHeaders),
		Tokens:              detail.Tokens,
		CostMicro:           detail.costMicro,
		Failed:              detail.Failed,
	}
}

func (event usageBackupEvent) flatDetail(rawAPIKey string) FlatDetail {
	return FlatDetail{
		Timestamp:           event.Timestamp,
		Model:               event.Model,
		Source:              event.Source,
		AuthIndex:           event.AuthIndex,
		APIKey:              rawAPIKey,
		Provider:            event.Provider,
		LatencyMs:           event.LatencyMs,
		RequestID:           event.RequestID,
		TimeToFirstByteMs:   event.TimeToFirstByteMs,
		TotalDurationMs:     event.TotalDurationMs,
		Completed:           event.Completed,
		MetadataRecorded:    event.MetadataRecorded,
		ReasoningEffort:     event.ReasoningEffort,
		ServiceTier:         event.ServiceTier,
		RequestServiceTier:  event.RequestServiceTier,
		ResponseServiceTier: event.ResponseServiceTier,
		ExecutorType:        event.ExecutorType,
		Thinking:            cloneThinking(event.Thinking),
		ResponseHeaders:     cloneHTTPHeader(event.ResponseHeaders),
		Tokens:              event.Tokens,
		Failed:              event.Failed,
		metadataTrusted:     true,
		costMicro:           event.CostMicro,
		costTrusted:         true,
	}
}
