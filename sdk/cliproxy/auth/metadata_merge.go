package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"strings"
)

type authCreationIntentContextKey struct{}

// WithAuthCreationIntent marks the immediate persistence performed by an
// explicit login or credential migration. Token stores use the marker to
// distinguish intentional creation from a late runtime update.
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
//
//goland:noinspection GoUnnecessarilyExportedIdentifiers
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
		if strings.EqualFold(strings.TrimSpace(target.Provider), "meta") {
			switch CanonicalCredentialMetadataKey(key) {
			case "api_key", "dca_token", "dca_expired", "dca_expires_at":
				continue
			}
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

func mergeAuthContent(base, current, updated *Auth) *Auth {
	if current == nil {
		if updated != nil {
			return updated.Clone()
		}
		if base != nil {
			return base.Clone()
		}
		return nil
	}
	if updated == nil {
		return current.Clone()
	}
	if base != nil && current.registrationGeneration != base.registrationGeneration {
		// A previous registration cannot change the current credential.
		return current.Clone()
	}

	merged := current.Clone()
	if merged.Metadata == nil {
		merged.Metadata = make(map[string]any)
	}

	var baseMeta map[string]any
	if base != nil {
		baseMeta = base.Metadata
	}

	// 1. Three-way merge for Metadata (excluding proxy_url which has dedicated canonical merge)
	if updated.Metadata != nil {
		for k, v := range updated.Metadata {
			if strings.EqualFold(strings.TrimSpace(k), "proxy_url") {
				continue
			}
			baseVal, hadInBase := baseMeta[k]
			currentVal, hadInCurrent := current.Metadata[k]

			changedByExecutor := !hadInBase || !reflect.DeepEqual(baseVal, v)
			changedByUser := hadInBase != hadInCurrent || (hadInBase && !reflect.DeepEqual(baseVal, currentVal))

			if changedByExecutor {
				// Apply executor change if user didn't modify it, or if it is a token payload field
				if !changedByUser || IsAuthTokenPayloadKey(k) {
					merged.Metadata[k] = v
				}
			}
		}
		// Deletions by executor: only delete if user didn't modify the field concurrently
		for k, baseVal := range baseMeta {
			if strings.EqualFold(strings.TrimSpace(k), "proxy_url") {
				continue
			}
			if _, inUpdated := updated.Metadata[k]; !inUpdated {
				if currentVal, ok := current.Metadata[k]; ok {
					if reflect.DeepEqual(baseVal, currentVal) {
						delete(merged.Metadata, k)
					}
				}
			}
		}
	}

	// 2. Storage and Runtime
	if updated.Storage != nil {
		merged.Storage = updated.Storage
	}
	if updated.Runtime != nil {
		merged.Runtime = updated.Runtime
	}

	// 3. ProxyURL three-way merge (supporting both struct field and metadata modifications)
	baseStruct := ""
	if base != nil {
		baseStruct = strings.TrimSpace(base.ProxyURL)
	}
	currentStruct := strings.TrimSpace(current.ProxyURL)
	updatedStruct := strings.TrimSpace(updated.ProxyURL)

	baseMetaProxy := ""
	if base != nil && base.Metadata != nil {
		if s, ok := base.Metadata["proxy_url"].(string); ok {
			baseMetaProxy = strings.TrimSpace(s)
		}
	}
	currentMetaProxy := ""
	if current.Metadata != nil {
		if s, ok := current.Metadata["proxy_url"].(string); ok {
			currentMetaProxy = strings.TrimSpace(s)
		}
	}
	updatedMetaProxy := ""
	if updated.Metadata != nil {
		if s, ok := updated.Metadata["proxy_url"].(string); ok {
			updatedMetaProxy = strings.TrimSpace(s)
		}
	}

	userChangedStruct := currentStruct != baseStruct
	userChangedMeta := currentMetaProxy != baseMetaProxy
	execChangedStruct := updatedStruct != baseStruct
	execChangedMeta := updatedMetaProxy != baseMetaProxy

	finalProxy := currentStruct
	if currentMetaProxy != "" && currentStruct == "" && !userChangedStruct {
		finalProxy = currentMetaProxy
	}

	if userChangedStruct || userChangedMeta {
		// User modified proxy concurrently; user takes precedence over executor.
		if userChangedStruct && !userChangedMeta {
			finalProxy = currentStruct
		} else if userChangedMeta && !userChangedStruct {
			finalProxy = currentMetaProxy
		} else if currentStruct != "" {
			finalProxy = currentStruct
		} else {
			finalProxy = currentMetaProxy
		}
	} else if execChangedStruct || execChangedMeta {
		// Executor modified proxy and user did not touch it.
		if execChangedStruct && !execChangedMeta {
			finalProxy = updatedStruct
		} else if execChangedMeta && !execChangedStruct {
			finalProxy = updatedMetaProxy
		} else if updatedStruct != "" {
			finalProxy = updatedStruct
		} else {
			finalProxy = updatedMetaProxy
		}
	}

	if finalProxy != "" {
		merged.ProxyURL = finalProxy
		merged.Metadata["proxy_url"] = finalProxy
	} else {
		merged.ProxyURL = ""
		delete(merged.Metadata, "proxy_url")
	}

	// 4. Prefix (three-way merge, user modification takes precedence)
	basePrefix := ""
	if base != nil {
		basePrefix = strings.TrimSpace(base.Prefix)
	}
	currentPrefix := strings.TrimSpace(current.Prefix)
	updatedPrefix := strings.TrimSpace(updated.Prefix)

	if updatedPrefix != basePrefix && currentPrefix == basePrefix {
		merged.Prefix = updatedPrefix
	} else {
		merged.Prefix = currentPrefix
	}

	// 5. Attributes (three-way merge)
	if updated.Attributes != nil {
		if merged.Attributes == nil {
			merged.Attributes = make(map[string]string)
		}
		var baseAttrs map[string]string
		if base != nil {
			baseAttrs = base.Attributes
		}
		for k, v := range updated.Attributes {
			baseVal, hadInBase := baseAttrs[k]
			currentVal, hadInCurrent := current.Attributes[k]

			changedByExecutor := !hadInBase || baseVal != v
			changedByUser := hadInBase != hadInCurrent || (hadInBase && baseVal != currentVal)

			if changedByExecutor && !changedByUser {
				merged.Attributes[k] = v
			}
		}
		for k, baseVal := range baseAttrs {
			if _, inUpdated := updated.Attributes[k]; !inUpdated {
				if currentVal, ok := current.Attributes[k]; ok {
					if baseVal == currentVal {
						delete(merged.Attributes, k)
					}
				}
			}
		}
	}

	return merged
}

func refreshAuthSourceMatches(base, current *Auth) bool {
	return base != nil && current != nil && base.ID == current.ID &&
		base.registrationGeneration == current.registrationGeneration &&
		!persistentAuthBindingChanged(base, current) && AccessTokenSHA256(base) == AccessTokenSHA256(current)
}
