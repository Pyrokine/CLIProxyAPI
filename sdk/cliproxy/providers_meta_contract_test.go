package cliproxy

import (
	"context"
	"errors"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
)

func TestAPIKeyClientProviderMetaCountAndCancellation(t *testing.T) {
	cfg := &config.Config{MetaKey: []config.MetaKey{{APIKey: "audit-meta-key"}}}
	provider := NewAPIKeyClientProvider()
	t.Run(
		"meta_count", func(t *testing.T) {
			result, err := provider.Load(context.Background(), cfg)
			if err != nil || result == nil || result.MetaKeyCount != 1 {
				t.Fatalf("Load()=(%#v,%v), want MetaKeyCount=1", result, err)
			}
		},
	)
	t.Run(
		"cancellation", func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			result, err := provider.Load(ctx, cfg)
			if result != nil || !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled Load()=(%#v,%v), want nil/context.Canceled", result, err)
			}
		},
	)
}
