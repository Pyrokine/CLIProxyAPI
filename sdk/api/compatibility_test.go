package api_test

import (
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api"
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

var _ api.ManagementTokenRequester = (*legacyManagementTokenRequester)(nil)
var _ = api.NewManagementTokenRequester

func TestLegacySDKAPISourceCompatibility(t *testing.T) {
	var option api.ServerOption
	// This compile-time compatibility fixture intentionally verifies the zero function value.
	// noinspection GoDfaConstantCondition
	if option != nil {
		t.Fatal("zero ServerOption must remain nil-comparable")
	}
}
