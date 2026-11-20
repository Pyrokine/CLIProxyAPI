// Package api exposes helpers for embedding CLIProxyAPI.
//
// It wraps internal management handler types so external projects can integrate
// management endpoints without importing internal packages.
package api

import (
	"context"

	internalmanagement "github.com/Pyrokine/CLIProxyAPI/v6/internal/api/handlers/management"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	"github.com/gin-gonic/gin"
)

// Handler 保留嵌入应用使用的管理处理器类型
type Handler = internalmanagement.Handler

// NewHandler 创建使用指定配置文件的管理处理器
func NewHandler(cfg *config.Config, configFilePath string, manager *coreauth.Manager) *Handler {
	return internalmanagement.NewHandler(cfg, configFilePath, manager)
}

// NewHandlerWithoutConfigFilePath 创建不写入配置文件的管理处理器
func NewHandlerWithoutConfigFilePath(cfg *config.Config, manager *coreauth.Manager) *Handler {
	return internalmanagement.NewHandlerWithoutConfigFilePath(cfg, manager)
}

// ManagementTokenRequester exposes a limited subset of management endpoints for requesting tokens.
type ManagementTokenRequester interface {
	RequestAnthropicToken(*gin.Context)
	RequestGeminiCLIToken(*gin.Context)
	RequestCodexToken(*gin.Context)
	RequestAntigravityToken(*gin.Context)
	RequestQwenToken(*gin.Context)
	RequestKimiToken(*gin.Context)
	RequestIFlowToken(*gin.Context)
	RequestIFlowCookieToken(*gin.Context)
	GetAuthStatus(c *gin.Context)
	PostOAuthCallback(c *gin.Context)
}

// ManagementAuthSessionRequester adds cancellation for in-flight authentication sessions.
type ManagementAuthSessionRequester interface {
	ManagementTokenRequester
	CancelAuthSession(c *gin.Context)
}

type managementTokenRequester struct {
	handler *internalmanagement.Handler
}

// NewManagementTokenRequester creates a limited management handler exposing only token request endpoints.
// noinspection GoUnusedExportedFunction
func NewManagementTokenRequester(cfg *config.Config, manager *coreauth.Manager) ManagementTokenRequester {
	return NewManagementAuthSessionRequester(cfg, manager)
}

// NewManagementAuthSessionRequester creates a management handler with authentication session cancellation.
//
//goland:noinspection GoUnnecessarilyExportedIdentifiers,GoUnusedExportedFunction
func NewManagementAuthSessionRequester(
	cfg *config.Config,
	manager *coreauth.Manager,
) ManagementAuthSessionRequester {
	return &managementTokenRequester{
		handler: internalmanagement.NewHandlerWithoutConfigFilePath(cfg, manager),
	}
}

func (m *managementTokenRequester) RequestAnthropicToken(c *gin.Context) {
	m.handler.RequestAnthropicToken(c)
}

func (m *managementTokenRequester) RequestGeminiCLIToken(c *gin.Context) {
	m.handler.RequestGeminiCLIToken(c)
}

func (m *managementTokenRequester) RequestCodexToken(c *gin.Context) {
	m.handler.RequestCodexToken(c)
}

func (m *managementTokenRequester) RequestAntigravityToken(c *gin.Context) {
	m.handler.RequestAntigravityToken(c)
}

func (m *managementTokenRequester) RequestQwenToken(c *gin.Context) {
	m.handler.RequestQwenToken(c)
}

func (m *managementTokenRequester) RequestKimiToken(c *gin.Context) {
	m.handler.RequestKimiToken(c)
}

func (m *managementTokenRequester) RequestIFlowToken(c *gin.Context) {
	m.handler.RequestIFlowToken(c)
}

func (m *managementTokenRequester) RequestIFlowCookieToken(c *gin.Context) {
	m.handler.RequestIFlowCookieToken(c)
}

func (m *managementTokenRequester) GetAuthStatus(c *gin.Context) {
	m.handler.GetAuthStatus(c)
}

func (m *managementTokenRequester) CancelAuthSession(c *gin.Context) {
	m.handler.CancelAuthSession(c)
}

func (m *managementTokenRequester) PostOAuthCallback(c *gin.Context) {
	m.handler.PostOAuthCallback(c)
}

// WriteConfig 校验 YAML 并保存管理配置
func WriteConfig(path string, data []byte) error {
	return internalmanagement.WriteConfig(path, data)
}

// RegisterOAuthSession 注册内建 Provider 的待处理会话
func RegisterOAuthSession(state, provider string) {
	internalmanagement.RegisterOAuthSession(state, provider)
}

// SetOAuthSessionError 记录会话错误并停止等待
func SetOAuthSessionError(state, message string) {
	internalmanagement.SetOAuthSessionError(state, message)
}

// CompleteOAuthSession 完成指定会话
func CompleteOAuthSession(state string) {
	internalmanagement.CompleteOAuthSession(state)
}

// CompleteOAuthSessionsByProvider 完成内建 Provider 的会话并返回数量
func CompleteOAuthSessionsByProvider(provider string) int {
	return internalmanagement.CompleteOAuthSessionsByProvider(provider)
}

// GetOAuthSession 返回未完成会话的 Provider 和错误状态
func GetOAuthSession(state string) (provider string, status string, ok bool) {
	return internalmanagement.GetOAuthSession(state)
}

// IsOAuthSessionPending 查询指定 Provider 的会话是否仍可处理
func IsOAuthSessionPending(state, provider string) bool {
	return internalmanagement.IsOAuthSessionPending(state, provider)
}

// ValidateOAuthState 检查 OAuth 状态值
func ValidateOAuthState(state string) error {
	return internalmanagement.ValidateOAuthState(state)
}

// NormalizeOAuthProvider 返回内建 Provider 的规范名称
func NormalizeOAuthProvider(provider string) (string, error) {
	return internalmanagement.NormalizeOAuthProvider(provider)
}

// WriteOAuthCallbackFile 将回调写入指定凭据目录
func WriteOAuthCallbackFile(authDir, provider, state, code, errorMessage string) (string, error) {
	return internalmanagement.WriteOAuthCallbackFile(authDir, provider, state, code, errorMessage)
}

// WriteOAuthCallbackFileForPendingSession 发布指定待处理会话的回调
func WriteOAuthCallbackFileForPendingSession(authDir, provider, state, code, errorMessage string) (string, error) {
	return internalmanagement.WriteOAuthCallbackFileForPendingSession(provider, state, code, errorMessage, authDir)
}

// PopulateAuthContext 将请求信息写入认证上下文
func PopulateAuthContext(ctx context.Context, c *gin.Context) context.Context {
	return internalmanagement.PopulateAuthContext(ctx, c)
}
