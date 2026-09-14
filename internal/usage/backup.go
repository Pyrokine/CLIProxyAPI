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

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	"golang.org/x/crypto/scrypt"
)

const (
	usageBackupFormat        = "cliproxy-usage-backup"
	usageBackupVersion       = 4
	usageBackupLegacyVersion = 3
	usageBackupKDFAlgorithm  = "scrypt"
	usageBackupKDFN          = 1 << 15
	usageBackupKDFR          = 8
	usageBackupKDFP          = 1
	usageBackupKeyBytes      = 32
	usageBackupSaltBytes     = 16
	usageBackupIdentityType  = "hmac-sha256:"
)

// MaxPortableBackupBytes is the maximum encoded backup accepted by every
// portable usage import path and emitted by the management export endpoint.
const MaxPortableBackupBytes int64 = 100 << 20

// maxPortableBackupPlaintextBytes leaves enough room for AES-GCM overhead,
// base64 expansion and the visible envelope before the final file reaches the
// portable limit.
const maxPortableBackupPlaintextBytes = ((MaxPortableBackupBytes - 1024) * 3 / 4) - 16

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
	Fingerprint         string                     `json:"fingerprint,omitempty"`
	Timestamp           time.Time                  `json:"timestamp"`
	Model               string                     `json:"model"`
	Source              string                     `json:"source"`
	AuthIndex           string                     `json:"auth_index"`
	APIKeyIdentity      string                     `json:"api_key_identity,omitempty"`
	Provider            string                     `json:"provider"`
	LatencyMs           int64                      `json:"latency_ms,omitempty"`
	RequestID           string                     `json:"request_id,omitempty"`
	RequestLogState     logging.RequestLogState    `json:"request_log_state,omitempty"`
	RequestLogLocation  logging.RequestLogLocation `json:"request_log_location,omitempty"`
	RequestLogFileName  string                     `json:"request_log_file_name,omitempty"`
	RequestLogHomeIP    string                     `json:"request_log_home_ip,omitempty"`
	RequestLogReason    string                     `json:"request_log_reason,omitempty"`
	RequestLogRevision  int64                      `json:"request_log_revision,omitempty"`
	RequestLogUpdatedAt time.Time                  `json:"request_log_updated_at,omitzero"`
	TimeToFirstByteMs   int64                      `json:"time_to_first_byte_ms,omitempty"`
	TotalDurationMs     int64                      `json:"total_duration_ms,omitempty"`
	Completed           bool                       `json:"completed"`
	Generate            *bool                      `json:"generate,omitempty"`
	ClientIP            string                     `json:"client_ip,omitempty"`
	XForwardedFor       string                     `json:"x_forwarded_for,omitempty"`
	UserAgent           string                     `json:"user_agent,omitempty"`
	AccountingVersion   int                        `json:"accounting_version,omitempty"`
	TokenBreakdown      *coreusage.TokenBreakdown  `json:"token_breakdown,omitempty"`
	MetadataRecorded    bool                       `json:"metadata_recorded,omitempty"`
	MetadataTruncated   bool                       `json:"metadata_truncated,omitempty"`
	ReasoningEffort     string                     `json:"reasoning_effort,omitempty"`
	ServiceTier         string                     `json:"service_tier,omitempty"`
	RequestServiceTier  string                     `json:"request_service_tier,omitempty"`
	ResponseServiceTier string                     `json:"response_service_tier,omitempty"`
	ExecutorType        string                     `json:"executor_type,omitempty"`
	Thinking            *Thinking                  `json:"thinking,omitempty"`
	ResponseHeaders     http.Header                `json:"response_headers,omitempty"`
	Tokens              tokenStats                 `json:"tokens"`
	CostMicro           int64                      `json:"cost_micro"`
	CostStatus          CostStatus                 `json:"cost_status,omitempty"`
	CostReason          string                     `json:"cost_reason,omitempty"`
	PricingReceipt      *PricingReceipt            `json:"pricing_receipt,omitempty"`
	Failed              bool                       `json:"failed"`
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
	return probe.Format == usageBackupFormat &&
		(probe.Version == usageBackupLegacyVersion || probe.Version == usageBackupVersion)
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
		if err = validatePricingReceipt(detail.PricingReceipt); err != nil {
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
	ciphertext := gcm.Seal(nil, nonce, plaintext, usageBackupAAD(usageBackupVersion, kdf))
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

// ParseUsageBackup authenticates and decrypts a supported portable backup before
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
	if envelope.Format != usageBackupFormat ||
		(envelope.Version != usageBackupLegacyVersion && envelope.Version != usageBackupVersion) {
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
	plaintext, err := gcm.Open(nil, nonce, ciphertext, usageBackupAAD(envelope.Version, envelope.KeyDerivation))
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
		if envelope.Version == usageBackupLegacyVersion && event.PricingReceipt != nil {
			return nil, fmt.Errorf("usage: backup event[%d] has a pricing receipt in version 3", i)
		}
		if err = validatePricingReceipt(event.PricingReceipt); err != nil {
			return nil, fmt.Errorf("usage: backup event[%d]: %w", i, err)
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
		detail, errDetail := event.flatDetail(rawAPIKey)
		if errDetail != nil {
			return nil, fmt.Errorf("usage: backup event[%d]: %w", i, errDetail)
		}
		details = append(details, detail)
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

func usageBackupAAD(version int, kdf usageBackupKeyDerivation) []byte {
	return fmt.Appendf(
		nil,
		"%s\x00%d\x00%s\x00%s\x00%d\x00%d\x00%d\x00%d",
		usageBackupFormat,
		version,
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
	fingerprint := detail.fingerprint
	if !detail.fingerprintTrusted {
		fingerprint = computeFingerprint(detail)
	}
	generate := true
	if detail.generateRecorded {
		generate = detail.Generate
	}
	var tokenBreakdown *coreusage.TokenBreakdown
	accountingVersion := detail.AccountingVersion
	if detail.TokenBreakdown.Valid() {
		breakdown := detail.TokenBreakdown
		tokenBreakdown = &breakdown
		accountingVersion = coreusage.TokenAccountingSchemaVersion
	}
	return usageBackupEvent{
		Fingerprint:         hex.EncodeToString(fingerprint[:]),
		Timestamp:           detail.Timestamp,
		Model:               detail.Model,
		Source:              detail.Source,
		AuthIndex:           detail.AuthIndex,
		APIKeyIdentity:      identity,
		Provider:            detail.Provider,
		LatencyMs:           detail.LatencyMs,
		RequestID:           detail.RequestID,
		RequestLogState:     detail.RequestLogState,
		RequestLogLocation:  detail.RequestLogLocation,
		RequestLogFileName:  detail.RequestLogFileName,
		RequestLogHomeIP:    detail.RequestLogHomeIP,
		RequestLogReason:    detail.RequestLogReason,
		RequestLogRevision:  detail.RequestLogRevision,
		RequestLogUpdatedAt: detail.RequestLogUpdatedAt,
		TimeToFirstByteMs:   detail.TimeToFirstByteMs,
		TotalDurationMs:     detail.TotalDurationMs,
		Completed:           detail.Completed,
		Generate:            &generate,
		ClientIP:            detail.ClientIP,
		XForwardedFor:       detail.XForwardedFor,
		UserAgent:           detail.UserAgent,
		AccountingVersion:   accountingVersion,
		TokenBreakdown:      tokenBreakdown,
		MetadataRecorded:    detail.MetadataRecorded,
		MetadataTruncated:   detail.MetadataTruncated,
		ReasoningEffort:     detail.ReasoningEffort,
		ServiceTier:         detail.ServiceTier,
		RequestServiceTier:  detail.RequestServiceTier,
		ResponseServiceTier: detail.ResponseServiceTier,
		ExecutorType:        detail.ExecutorType,
		Thinking:            cloneThinking(detail.Thinking),
		ResponseHeaders:     cloneHTTPHeader(detail.ResponseHeaders),
		Tokens:              detail.Tokens,
		CostMicro:           detail.costMicro,
		CostStatus:          detail.CostStatus,
		CostReason:          detail.CostReason,
		PricingReceipt:      clonePricingReceipt(detail.PricingReceipt),
		Failed:              detail.Failed,
	}
}

func (event usageBackupEvent) flatDetail(rawAPIKey string) (FlatDetail, error) {
	if event.TokenBreakdown != nil {
		if event.AccountingVersion != coreusage.TokenAccountingSchemaVersion || !event.TokenBreakdown.Valid() {
			return FlatDetail{}, errors.New("invalid token breakdown")
		}
	} else if event.AccountingVersion != 0 {
		return FlatDetail{}, errors.New("accounting version has no token breakdown")
	}
	var fingerprint [eventFingerprintBytes]byte
	fingerprintTrusted := event.Fingerprint != ""
	if fingerprintTrusted {
		fingerprintRaw, err := hex.DecodeString(event.Fingerprint)
		if err != nil || len(fingerprintRaw) != eventFingerprintBytes {
			return FlatDetail{}, errors.New("invalid event fingerprint")
		}
		copy(fingerprint[:], fingerprintRaw)
	}
	generate := event.Generate == nil || *event.Generate
	var tokenBreakdown coreusage.TokenBreakdown
	if event.TokenBreakdown != nil {
		tokenBreakdown = *event.TokenBreakdown
	}
	return FlatDetail{
		Timestamp:           event.Timestamp,
		Model:               event.Model,
		Source:              event.Source,
		AuthIndex:           event.AuthIndex,
		APIKey:              rawAPIKey,
		Provider:            event.Provider,
		LatencyMs:           event.LatencyMs,
		RequestID:           event.RequestID,
		RequestLogState:     event.RequestLogState,
		RequestLogLocation:  event.RequestLogLocation,
		RequestLogFileName:  event.RequestLogFileName,
		RequestLogHomeIP:    event.RequestLogHomeIP,
		RequestLogReason:    event.RequestLogReason,
		RequestLogRevision:  event.RequestLogRevision,
		RequestLogUpdatedAt: event.RequestLogUpdatedAt,
		TimeToFirstByteMs:   event.TimeToFirstByteMs,
		TotalDurationMs:     event.TotalDurationMs,
		Completed:           event.Completed,
		Generate:            generate,
		ClientIP:            event.ClientIP,
		XForwardedFor:       event.XForwardedFor,
		UserAgent:           event.UserAgent,
		AccountingVersion:   event.AccountingVersion,
		TokenBreakdown:      tokenBreakdown,
		MetadataRecorded:    event.MetadataRecorded,
		MetadataTruncated:   event.MetadataTruncated,
		ReasoningEffort:     event.ReasoningEffort,
		ServiceTier:         event.ServiceTier,
		RequestServiceTier:  event.RequestServiceTier,
		ResponseServiceTier: event.ResponseServiceTier,
		ExecutorType:        event.ExecutorType,
		Thinking:            cloneThinking(event.Thinking),
		ResponseHeaders:     cloneHTTPHeader(event.ResponseHeaders),
		Tokens:              event.Tokens,
		PricingReceipt:      clonePricingReceipt(event.PricingReceipt),
		CostStatus:          event.CostStatus,
		CostReason:          event.CostReason,
		Failed:              event.Failed,
		generateRecorded:    event.Generate != nil,
		metadataTrusted:     true,
		fingerprint:         fingerprint,
		fingerprintTrusted:  fingerprintTrusted,
		Cost:                float64(event.CostMicro) / costMicroPerUSD,
		costMicro:           event.CostMicro,
		costTrusted:         true,
	}, nil
}
