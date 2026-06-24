package usage

import (
	"sync"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/modelcatalog"
)

func BuildModelPriceFunc(configPath string) PriceFunc {
	if configPath == "" {
		return nil
	}
	var mu sync.RWMutex
	var cached map[string]modelcatalog.Price
	var loadedAt time.Time
	load := func() bool {
		prices, err := modelcatalog.Prices(configPath)
		if err != nil || len(prices) == 0 {
			return false
		}
		mu.Lock()
		cached = prices
		loadedAt = time.Now()
		mu.Unlock()
		return true
	}
	if !load() {
		return nil
	}
	return func(model string) (prompt, completion, cache float64, found bool) {
		mu.RLock()
		age := time.Since(loadedAt)
		p, ok := cached[model]
		mu.RUnlock()
		if age > 5*time.Minute && load() {
			mu.RLock()
			p, ok = cached[model]
			mu.RUnlock()
		}
		if !ok {
			return 0, 0, 0, false
		}
		return p.Prompt, p.Completion, p.Cache, true
	}
}
