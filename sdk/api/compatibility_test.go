package api_test

import (
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	"github.com/gin-gonic/gin"
)

type legacyManagementTokenRequester struct{}

func (*legacyManagementTokenRequester) RequestAnthropicToken(*gin.Context)   {}
func (*legacyManagementTokenRequester) RequestGeminiCLIToken(*gin.Context)   {}
func (*legacyManagementTokenRequester) RequestCodexToken(*gin.Context)       {}
func (*legacyManagementTokenRequester) RequestAntigravityToken(*gin.Context) {}
func (*legacyManagementTokenRequester) RequestQwenToken(*gin.Context)        {}
func (*legacyManagementTokenRequester) RequestKimiToken(*gin.Context)        {}
func (*legacyManagementTokenRequester) RequestIFlowToken(*gin.Context)       {}
func (*legacyManagementTokenRequester) RequestIFlowCookieToken(*gin.Context) {}
func (*legacyManagementTokenRequester) GetAuthStatus(*gin.Context)           {}
func (*legacyManagementTokenRequester) PostOAuthCallback(*gin.Context)       {}

func TestLegacySDKAPISourceCompatibility(t *testing.T) {
	var option api.ServerOption
	if option != nil {
		t.Fatal("zero ServerOption must remain nil-comparable")
	}
	var requester api.ManagementTokenRequester = &legacyManagementTokenRequester{}
	if requester == nil {
		t.Fatal("legacy ManagementTokenRequester implementation is nil")
	}
	var factory func(*config.Config, *coreauth.Manager) api.ManagementTokenRequester = api.NewManagementTokenRequester
	if factory == nil {
		t.Fatal("legacy NewManagementTokenRequester function type is nil")
	}
}
