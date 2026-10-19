package auth

import (
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
)

const AttributeClaudeFingerprintProfile = "fingerprint_profile"
const legacyClaudeFingerprintProfile = "fingerprint-profile"

// ClaudeFingerprintProfile 按存在性取值，显式空字符串不读取低优先级来源
func ClaudeFingerprintProfile(auth *Auth) (string, bool, error) {
	if auth == nil {
		return "", false, nil
	}
	if raw, exists := auth.Attributes[AttributeClaudeFingerprintProfile]; exists {
		profile, err := config.ParseClaudeFingerprintProfile(raw)
		return profile, true, err
	}
	raw, exists := util.ReadClaudeMetadataValue(&auth.Metadata, AttributeClaudeFingerprintProfile)
	if !exists {
		raw, exists = util.ReadClaudeMetadataValue(&auth.Metadata, legacyClaudeFingerprintProfile)
	}
	if !exists {
		return "", false, nil
	}
	profile, err := config.ParseClaudeFingerprintProfile(raw)
	return profile, true, err
}

func claudeFingerprintProfileMetadata(metadata map[string]any) (string, bool, error) {
	raw, exists := metadata[AttributeClaudeFingerprintProfile]
	if !exists {
		raw, exists = metadata[legacyClaudeFingerprintProfile]
	}
	if !exists {
		return "", false, nil
	}
	profile, err := config.ParseClaudeFingerprintProfile(raw)
	return profile, true, err
}

// ApplyClaudeFingerprintProfileMetadata 在载入或编辑时同步源 metadata 和运行属性
func ApplyClaudeFingerprintProfileMetadata(auth *Auth, metadata map[string]any) error {
	profile, exists, err := claudeFingerprintProfileMetadata(metadata)
	if err != nil || auth == nil {
		return err
	}
	if !exists {
		_, _, err = ClaudeFingerprintProfile(auth)
		return err
	}
	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	metadata[AttributeClaudeFingerprintProfile] = profile
	delete(metadata, legacyClaudeFingerprintProfile)
	auth.Metadata[AttributeClaudeFingerprintProfile] = profile
	delete(auth.Metadata, legacyClaudeFingerprintProfile)
	auth.Attributes[AttributeClaudeFingerprintProfile] = profile
	delete(auth.Attributes, legacyClaudeFingerprintProfile)
	return nil
}
