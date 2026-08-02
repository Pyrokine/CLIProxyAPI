package management

import (
	"context"
	"encoding/json"
	"errors"
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
	ctx := context.Background()
	if c != nil && c.Request != nil {
		ctx = c.Request.Context()
	}
	redacted, err := redactedManagementPayloadContext(ctx, payload)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to redact management response"})
		return
	}
	if err = maskUsageSourceFieldsContext(ctx, redacted, masker); err != nil {
		return
	}
	if err = ctx.Err(); err != nil {
		return
	}
	data, err := json.Marshal(redacted)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to encode management response"})
		return
	}
	if err = ctx.Err(); err != nil {
		return
	}
	c.Data(status, "application/json; charset=utf-8", data)
}

func maskUsageSourceFields(value any, masker usageCredentialMasker) {
	_ = maskUsageSourceFieldsContext(context.Background(), value, masker)
}

func maskUsageSourceFieldsContext(
	ctx context.Context,
	value any,
	masker usageCredentialMasker,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if err := ctx.Err(); err != nil {
				return err
			}
			if strings.EqualFold(key, "source") {
				if source, ok := child.(string); ok {
					typed[key] = masker.Mask(source)
				}
			}
			if err := maskUsageSourceFieldsContext(ctx, typed[key], masker); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range typed {
			if err := maskUsageSourceFieldsContext(ctx, child, masker); err != nil {
				return err
			}
		}
	}
	return nil
}
