package management

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/usage"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	"github.com/gin-gonic/gin"
)

type usageCredentialMasker struct {
	apiKeys usageAPIKeyPresenter
}

func newUsageCredentialMasker(cfg *config.Config) usageCredentialMasker {
	return usageCredentialMasker{apiKeys: newUsageAPIKeyPresenter(cfg)}
}

func (m usageCredentialMasker) describe(
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

func (m usageCredentialMasker) mask(source string) string {
	display, _ := m.describe(source)
	return display
}

func writeRedactedUsageJSON(
	c *gin.Context,
	status int,
	payload any,
	masker usageCredentialMasker,
) {
	if c == nil {
		return
	}
	ctx := context.Background()
	if c.Request != nil {
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
	if err = presentRedactedUsageAPIKeysContext(ctx, redacted, masker.apiKeys); err != nil {
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

func presentRedactedUsageAPIKeysContext(
	ctx context.Context,
	value any,
	presenter usageAPIKeyPresenter,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(presenter.byRedactedIdentity) == 0 {
		return nil
	}
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if err := ctx.Err(); err != nil {
				return err
			}
			if strings.EqualFold(key, "api_key") {
				if apiKey, ok := child.(string); ok {
					typed[key] = presenter.presentRedacted(apiKey).display
				}
			}
			if strings.EqualFold(key, "apis") {
				if apis, ok := child.(map[string]any); ok {
					presented, err := presentRedactedUsageAPIKeyMap(ctx, apis, presenter)
					if err != nil {
						return err
					}
					typed[key] = presented
					child = presented
				}
			}
			if err := presentRedactedUsageAPIKeysContext(ctx, child, presenter); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range typed {
			if err := presentRedactedUsageAPIKeysContext(ctx, child, presenter); err != nil {
				return err
			}
		}
	}
	return nil
}

func presentRedactedUsageAPIKeyMap(
	ctx context.Context,
	apis map[string]any,
	presenter usageAPIKeyPresenter,
) (map[string]any, error) {
	keys := make([]string, 0, len(apis))
	for key := range apis {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	displays := make(map[string]string, len(keys))
	totals := make(map[string]int, len(keys))
	for _, key := range keys {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		display := presenter.presentRedacted(key).display
		displays[key] = display
		totals[display]++
	}
	counts := make(map[string]int, len(totals))
	out := make(map[string]any, len(apis))
	for _, key := range keys {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		display := displays[key]
		if totals[display] > 1 {
			counts[display]++
			display += "#" + strconv.Itoa(counts[display])
		}
		entry := apis[key]
		if err := presentUsageAPIKeyFieldsContext(ctx, entry, display); err != nil {
			return nil, err
		}
		out[display] = entry
	}
	return out, nil
}

func presentUsageAPIKeyFieldsContext(ctx context.Context, value any, display string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if err := ctx.Err(); err != nil {
				return err
			}
			if strings.EqualFold(key, "api_key") {
				if _, ok := child.(string); ok {
					typed[key] = display
					continue
				}
			}
			if err := presentUsageAPIKeyFieldsContext(ctx, child, display); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range typed {
			if err := presentUsageAPIKeyFieldsContext(ctx, child, display); err != nil {
				return err
			}
		}
	}
	return nil
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
					typed[key] = masker.mask(source)
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
