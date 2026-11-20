package management

import (
	"context"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/gin-gonic/gin"
	"gopkg.in/yaml.v3"
)

// WriteConfig 校验 YAML，保留注释格式并以私有权限原子保存
func WriteConfig(path string, data []byte) error {
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return err
	}
	return writeConfigFileAtomically(path, config.NormalizeCommentIndentation(data))
}

// RegisterOAuthSession 注册不含网页 nonce 的 SDK 内建会话
func RegisterOAuthSession(state, provider string) {
	registerOAuthSession(state, provider, "")
}

// SetOAuthSessionError 更新现有会话的错误
func SetOAuthSessionError(state, message string) {
	setOAuthSessionError(state, message)
}

// CompleteOAuthSession 完成现有会话并停止其等待
func CompleteOAuthSession(state string) {
	completeOAuthSession(state)
}

// CompleteOAuthSessionsByProvider 完成指定 Provider 的内建会话
func CompleteOAuthSessionsByProvider(provider string) int {
	return oauthSessions.completeProvider(provider)
}

// GetOAuthSession 返回现有未完成会话
func GetOAuthSession(state string) (provider string, status string, ok bool) {
	return getOAuthSession(state)
}

// IsOAuthSessionPending 检查现有会话的状态及 Provider
func IsOAuthSessionPending(state, provider string) bool {
	return isOAuthSessionPending(state, provider)
}

// ValidateOAuthState 检查状态值及路径分隔符
func ValidateOAuthState(state string) error {
	return validateOAuthState(state)
}

// NormalizeOAuthProvider 返回内建 Provider 的规范名称
func NormalizeOAuthProvider(provider string) (string, error) {
	return normalizeOAuthProvider(provider)
}

// normalizeOAuthCallbackProvider 规范内建名称或保留合法插件 Provider 名称
func normalizeOAuthCallbackProvider(provider string) (string, error) {
	if normalized, err := normalizeOAuthProvider(provider); err == nil {
		return normalized, nil
	}
	return normalizePluginOAuthProvider(provider)
}

// normalizePluginOAuthCallbackProvider 校验插件 Provider 名称并保留其归属
func normalizePluginOAuthCallbackProvider(provider string) (string, error) {
	return normalizePluginOAuthProvider(provider)
}

// WriteOAuthCallbackFile 校验 Provider 与状态值后原子写入回调
func WriteOAuthCallbackFile(authDir, provider, state, code, errorMessage string) (string, error) {
	canonicalProvider, err := normalizeOAuthCallbackProvider(provider)
	if err != nil {
		return "", err
	}
	return writeOAuthCallbackFile(authDir, canonicalProvider, state, code, errorMessage)
}

// PopulateAuthContext 通过同一管理请求入口保存认证信息
func PopulateAuthContext(ctx context.Context, c *gin.Context) context.Context {
	return populateAuthContext(ctx, c)
}
