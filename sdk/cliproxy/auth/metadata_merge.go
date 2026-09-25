package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"strings"
)

type authCreationIntentContextKey struct{}

// WithAuthCreationIntent marks the immediate persistence performed by an
// explicit login or credential migration. Token stores use the marker to
// distinguish an intentional create from a late runtime update.
func WithAuthCreationIntent(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, authCreationIntentContextKey{}, true)
}

// HasAuthCreationIntent reports whether a save may intentionally create a
// missing disabled credential.
func HasAuthCreationIntent(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	enabled, ok := ctx.Value(authCreationIntentContextKey{}).(bool)
	return ok && enabled
}

// IsAuthTokenPayloadKey reports whether key belongs to credentials or token
// lifecycle state that must come from the newly authenticated record.
func IsAuthTokenPayloadKey(key string) bool {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "access_token", "refresh_token", "id_token", "session_id", "session_token",
		"api_key", "token", "raw_token", "token_data", "cookie", "token_type", "scope",
		"expires_in", "expires_at", "expired", "expiry", "last_refresh", "timestamp",
		"client_id", "client_secret", "service_account", "device_id", "device_code",
		"user_code", "verification_uri", "verification_uri_complete":
		return true
	default:
		return false
	}
}

// MergeExistingAuthMetadata preserves user-owned credential settings while
// keeping newly acquired credentials and identity authoritative.
func MergeExistingAuthMetadata(target *Auth, existingMap map[string]any) error {
	if target == nil || len(existingMap) == 0 {
		return nil
	}

	existing := maps.Clone(existingMap)
	NormalizeCredentialMetadata(existing)
	if target.Metadata == nil {
		target.Metadata = make(map[string]any)
	}
	NormalizeCredentialMetadata(target.Metadata)
	authoritativeStorageKeys := make(map[string]struct{})
	if target.Storage != nil {
		rawStorage, errMarshal := json.Marshal(target.Storage)
		if errMarshal != nil {
			return fmt.Errorf("marshal newly authenticated storage: %w", errMarshal)
		}
		var storagePayload map[string]any
		if errUnmarshal := json.Unmarshal(rawStorage, &storagePayload); errUnmarshal != nil {
			return fmt.Errorf("decode newly authenticated storage: %w", errUnmarshal)
		}
		for key := range storagePayload {
			authoritativeStorageKeys[strings.ToLower(strings.TrimSpace(key))] = struct{}{}
		}
	}
	_, disabledExplicit := target.Metadata["disabled"]
	for key, value := range existing {
		if IsAuthTokenPayloadKey(key) {
			continue
		}
		if _, authoritative := authoritativeStorageKeys[strings.ToLower(strings.TrimSpace(key))]; authoritative {
			continue
		}
		if _, exists := target.Metadata[key]; !exists {
			target.Metadata[key] = value
		}
	}

	if disabled, ok := target.Metadata["disabled"].(bool); disabledExplicit && ok {
		target.Disabled = disabled
	} else if !disabledExplicit {
		if disabled, ok := existing["disabled"].(bool); ok {
			target.Disabled = disabled
		}
	}
	if target.Disabled {
		target.Status = StatusDisabled
	} else if target.Status == StatusDisabled {
		target.Status = StatusActive
	}
	if target.ProxyURL == "" {
		if proxyURL, ok := target.Metadata["proxy_url"].(string); ok {
			target.ProxyURL = strings.TrimSpace(proxyURL)
		}
	}
	if target.Prefix == "" {
		if prefix, ok := target.Metadata["prefix"].(string); ok {
			prefix = strings.Trim(strings.TrimSpace(prefix), "/")
			if prefix != "" && !strings.Contains(prefix, "/") {
				target.Prefix = prefix
			}
		}
	}
	if errHeaders := ApplyCustomHeadersFromMetadata(target); errHeaders != nil {
		return errHeaders
	}
	if errWeight := ApplyAuthWeightMetadata(target, target.Metadata); errWeight != nil {
		return errWeight
	}
	if errProfile := ApplyClaudeFingerprintProfileMetadata(target, target.Metadata); errProfile != nil {
		return errProfile
	}
	if setter, ok := target.Storage.(interface{ SetMetadata(map[string]any) }); ok {
		setter.SetMetadata(target.Metadata)
	}
	return nil
}
