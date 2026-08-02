package management

import (
	"net/http"
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/usage"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	"github.com/gin-gonic/gin"
)

type usageCredentialMasker struct{}

func newUsageCredentialMasker(*config.Config) usageCredentialMasker {
	return usageCredentialMasker{}
}

func (m usageCredentialMasker) Describe(
	source string,
) (string, coreusage.CredentialSourceKind) {
	trimmed := strings.TrimSpace(source)
	if trimmed == "" {
		return "", ""
	}
	display, kind, tagged := usage.DecodeCredentialSource(trimmed)
	if tagged {
		return display, kind
	}
	return util.HideAPIKey(trimmed), coreusage.CredentialSourceKindAPIKey
}

func (m usageCredentialMasker) Mask(source string) string {
	display, _ := m.Describe(source)
	return display
}

func writeRedactedUsageJSON(
	c *gin.Context,
	status int,
	payload any,
	masker usageCredentialMasker,
) {
	redacted, err := redactedManagementPayload(payload)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to redact management response"})
		return
	}
	maskUsageSourceFields(redacted, masker)
	c.JSON(status, redacted)
}

func maskUsageSourceFields(value any, masker usageCredentialMasker) {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if strings.EqualFold(key, "source") {
				if source, ok := child.(string); ok {
					typed[key] = masker.Mask(source)
				}
			}
			maskUsageSourceFields(typed[key], masker)
		}
	case []any:
		for _, child := range typed {
			maskUsageSourceFields(child, masker)
		}
	}
}
