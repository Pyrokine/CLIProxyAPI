package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"strings"
)

// AccessTokenSHA256 returns the normalized OAuth access-token fingerprint without exposing the token.
func AccessTokenSHA256(auth *Auth) string {
	accessToken := accessTokenForFingerprint(auth)
	if accessToken == "" {
		return ""
	}
	digest := sha256.Sum256([]byte(accessToken))
	return hex.EncodeToString(digest[:])
}

type accessTokenFingerprintObserverContextKey struct{}

func withAccessTokenFingerprintObserver(ctx context.Context, observer func(*Auth)) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if observer == nil {
		return ctx
	}
	return context.WithValue(ctx, accessTokenFingerprintObserverContextKey{}, observer)
}

// NotifyAccessTokenFingerprint reports the auth snapshot actually used by an executor.
func NotifyAccessTokenFingerprint(ctx context.Context, auth *Auth) {
	if ctx == nil || auth == nil || AccessTokenSHA256(auth) == "" {
		return
	}
	observer, _ := ctx.Value(accessTokenFingerprintObserverContextKey{}).(func(*Auth))
	if observer != nil {
		observer(auth.Clone())
	}
}

func accessTokenForFingerprint(auth *Auth) string {
	if auth == nil || auth.Metadata == nil {
		return ""
	}
	for _, key := range []string{"access_token", "accessToken"} {
		if value, ok := auth.Metadata[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	for _, key := range []string{"token", "Token"} {
		switch token := auth.Metadata[key].(type) {
		case map[string]any:
			for _, tokenKey := range []string{"access_token", "accessToken"} {
				if value, ok := token[tokenKey].(string); ok && strings.TrimSpace(value) != "" {
					return strings.TrimSpace(value)
				}
			}
		case map[string]string:
			for _, tokenKey := range []string{"access_token", "accessToken"} {
				if value := strings.TrimSpace(token[tokenKey]); value != "" {
					return value
				}
			}
		}
	}
	return ""
}

func credentialBindingSHA256(auth *Auth) string {
	if auth == nil {
		return ""
	}
	metadata := credentialBindingMetadata(auth)
	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	material := make(map[string]any)
	if provider != "" {
		material["provider"] = provider
	}

	accountID, planType := "", ""
	if provider == "codex" {
		accountID, planType = codexBindingClaims(metadata)
		if accountID != "" {
			material["account_id"] = accountID
			if planType != "" {
				material["plan_type"] = planType
			}
			return credentialBindingMaterialSHA256(material)
		}
	}

	if refreshToken := credentialStringValue(metadata, "refresh_token", "refreshToken"); refreshToken != "" {
		material["refresh_token"] = refreshToken
		return credentialBindingMaterialSHA256(material)
	}
	if token := credentialMapValue(metadata, "token", "Token"); token != nil {
		if refreshToken := credentialStringValue(token, "refresh_token", "refreshToken"); refreshToken != "" {
			material["refresh_token"] = refreshToken
			return credentialBindingMaterialSHA256(material)
		}
	}

	if accessToken := credentialStringValue(metadata, "access_token", "accessToken"); accessToken != "" {
		material["access_token"] = accessToken
	} else if token := credentialMapValue(metadata, "token", "Token"); token != nil {
		if accessToken := credentialStringValue(token, "access_token", "accessToken"); accessToken != "" {
			material["access_token"] = accessToken
		}
	}
	if auth.Attributes != nil {
		if apiKey := strings.TrimSpace(auth.Attributes[AttributeAPIKey]); apiKey != "" {
			material[AttributeAPIKey] = apiKey
		}
	}
	if len(material) == 0 || len(material) == 1 && provider != "" {
		maps.Copy(material, credentialBindingFields(metadata))
	}
	if len(material) == 0 || len(material) == 1 && provider != "" {
		return ""
	}
	return credentialBindingMaterialSHA256(material)
}

// QuotaCredentialRevisions 返回排除启停状态的配额版本及可证明等价的旧文件摘要
func QuotaCredentialRevisions(auth *Auth) (string, []string, error) {
	if auth == nil {
		return "", nil, fmt.Errorf("quota credential is nil")
	}
	material := make(map[string]any)
	var raw []byte
	if storage, ok := auth.Storage.(interface{ RawJSON() []byte }); ok {
		raw = storage.RawJSON()
	} else if auth.Storage != nil {
		var err error
		raw, err = json.Marshal(auth.Storage)
		if err != nil {
			return "", nil, fmt.Errorf("encode quota credential storage: %w", err)
		}
	}
	if len(raw) > 0 {
		var err error
		material, err = DecodeCredentialMetadata(raw)
		if err != nil {
			return "", nil, fmt.Errorf("decode quota credential storage: %w", err)
		}
	}
	maps.Copy(material, auth.Metadata)
	delete(material, "disabled")
	fingerprint := func(value any) (string, bool, error) {
		encoded, err := json.Marshal(value)
		if err != nil {
			return "", false, fmt.Errorf("encode quota credential material: %w", err)
		}
		precise, err := validateQuotaMaterialNumbers(value)
		if err != nil {
			return "", false, err
		}
		if precise {
			encoded = append([]byte("quota-json-number-v1\x00"), encoded...)
		}
		digest := sha256.Sum256(encoded)
		return hex.EncodeToString(digest[:]), precise, nil
	}
	apiKey := strings.TrimSpace(auth.Attributes[AttributeAPIKey])
	revision, precise, err := fingerprint(map[string]any{
		"provider": strings.ToLower(strings.TrimSpace(auth.Provider)),
		"api_key":  apiKey,
		"metadata": material,
	})
	if err != nil {
		return "", nil, err
	}
	equivalent := []string{revision}
	// 高精度材料使用独立版本，旧摘要无法证明其原始数字
	if precise {
		return revision, equivalent, nil
	}
	persistedKey, _ := material[AttributeAPIKey].(string)
	if apiKey != "" && strings.TrimSpace(persistedKey) != apiKey {
		return revision, equivalent, nil
	}
	if auth.quotaPersistedRevision == revision && auth.quotaPersistedSHA256 != "" {
		equivalent = append(equivalent, auth.quotaPersistedSHA256)
	}
	legacy, _, err := fingerprint(material)
	if err != nil {
		return "", nil, err
	}
	equivalent = append(equivalent, legacy)
	for _, disabled := range []bool{false, true} {
		material["disabled"] = disabled
		legacy, _, err = fingerprint(material)
		if err != nil {
			return "", nil, err
		}
		equivalent = append(equivalent, legacy)
	}
	return revision, equivalent, nil
}

func validateQuotaMaterialNumbers(value any) (bool, error) {
	var precise bool
	switch typed := value.(type) {
	case map[string]any:
		for _, item := range typed {
			contains, err := validateQuotaMaterialNumbers(item)
			if err != nil {
				return false, err
			}
			precise = precise || contains
		}
	case []any:
		for _, item := range typed {
			contains, err := validateQuotaMaterialNumbers(item)
			if err != nil {
				return false, err
			}
			precise = precise || contains
		}
	case json.Number:
		return true, nil
	case float64:
		if typed >= 1<<53 || typed <= -(1<<53) {
			return false, fmt.Errorf("quota credential metadata contains a float64 outside the exact integer range")
		}
	}
	return precise, nil
}

func credentialBindingMetadata(auth *Auth) map[string]any {
	out := make(map[string]any)
	if auth == nil {
		return out
	}
	var raw []byte
	if storage, ok := auth.Storage.(interface{ RawJSON() []byte }); ok {
		raw = storage.RawJSON()
	} else if auth.Storage != nil {
		raw, _ = json.Marshal(auth.Storage)
	}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out)
	}
	maps.Copy(out, auth.Metadata)
	return out
}

func credentialMapValue(metadata map[string]any, keys ...string) map[string]any {
	for _, key := range keys {
		switch value := metadata[key].(type) {
		case map[string]any:
			return value
		case map[string]string:
			out := make(map[string]any, len(value))
			for mapKey, mapValue := range value {
				out[mapKey] = mapValue
			}
			return out
		}
	}
	return nil
}

func credentialStringValue(metadata map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := metadata[key].(string); ok {
			if value = strings.TrimSpace(value); value != "" {
				return value
			}
		}
	}
	return ""
}

func codexBindingClaims(metadata map[string]any) (string, string) {
	accountID := credentialStringValue(metadata, "account_id", "accountId", "chatgpt_account_id")
	planType := credentialStringValue(metadata, "plan_type", "planType", "chatgpt_plan_type")
	idToken := credentialStringValue(metadata, "id_token", "idToken")
	if idToken == "" {
		if token := credentialMapValue(metadata, "token", "Token"); token != nil {
			idToken = credentialStringValue(token, "id_token", "idToken")
		}
	}
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return strings.TrimSpace(accountID), strings.ToLower(strings.TrimSpace(planType))
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return strings.TrimSpace(accountID), strings.ToLower(strings.TrimSpace(planType))
	}
	var claims struct {
		Auth struct {
			AccountID string `json:"chatgpt_account_id"`
			PlanType  string `json:"chatgpt_plan_type"`
		} `json:"https://api.openai.com/auth"`
	}
	if err = json.Unmarshal(payload, &claims); err != nil {
		return strings.TrimSpace(accountID), strings.ToLower(strings.TrimSpace(planType))
	}
	if value := strings.TrimSpace(claims.Auth.AccountID); value != "" {
		accountID = value
	}
	if value := strings.TrimSpace(claims.Auth.PlanType); value != "" {
		planType = value
	}
	return strings.TrimSpace(accountID), strings.ToLower(strings.TrimSpace(planType))
}

func credentialBindingFields(metadata map[string]any) map[string]any {
	out := make(map[string]any)
	for key, value := range metadata {
		normalized := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(key), "-", "_"))
		switch {
		case normalized == "email",
			normalized == "account_id",
			normalized == "account_uuid",
			normalized == "organization_id",
			normalized == "organization_uuid",
			normalized == "project_id",
			normalized == "user_id",
			normalized == "token",
			normalized == "cookie",
			normalized == "credential",
			normalized == "credentials",
			normalized == "secret",
			normalized == "password",
			normalized == "api_key",
			strings.HasSuffix(normalized, "_token"),
			strings.HasSuffix(normalized, "_cookie"),
			strings.HasSuffix(normalized, "_credential"),
			strings.HasSuffix(normalized, "_secret"),
			strings.HasSuffix(normalized, "_password"):
			out[normalized] = value
		}
	}
	return out
}

func credentialBindingMaterialSHA256(material map[string]any) string {
	encoded, err := json.Marshal(material)
	if err != nil || len(encoded) == 0 {
		return ""
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}
