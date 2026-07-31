package usage

import (
	"sync"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/modelcatalog"
)

const modelPriceRefreshInterval = 5 * time.Minute

// PriceProvider returns an immutable pricing function for one operation.
type PriceProvider interface {
	Snapshot() PriceFunc
}

type modelPriceProvider struct {
	mu         sync.Mutex
	configPath string
	cached     map[string]modelcatalog.Price
	loadedAt   time.Time
}

// BuildModelPriceProvider creates a provider that refreshes prices between
// operations while keeping each returned snapshot immutable.
func BuildModelPriceProvider(configPath string) PriceProvider {
	if configPath == "" {
		return nil
	}
	provider := &modelPriceProvider{configPath: configPath}
	if provider.Snapshot() == nil {
		return nil
	}
	return provider
}

// BuildModelPriceFunc returns the current immutable model-price snapshot.
func BuildModelPriceFunc(configPath string) PriceFunc {
	provider := BuildModelPriceProvider(configPath)
	if provider == nil {
		return nil
	}
	return provider.Snapshot()
}

func (p *modelPriceProvider) Snapshot() PriceFunc {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.cached) == 0 || time.Since(p.loadedAt) > modelPriceRefreshInterval {
		prices, err := modelcatalog.Prices(p.configPath)
		if err == nil && len(prices) > 0 {
			p.cached = prices
			p.loadedAt = time.Now()
		}
	}
	return priceFuncFromMap(p.cached)
}

func priceFuncFromMap(prices map[string]modelcatalog.Price) PriceFunc {
	if len(prices) == 0 {
		return nil
	}
	return func(model string) (prompt, completion, cache float64, found bool) {
		price, ok := prices[model]
		if !ok {
			return 0, 0, 0, false
		}
		return price.Prompt, price.Completion, price.Cache, true
	}
}
