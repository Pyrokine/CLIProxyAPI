package usage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// BuildModelPriceFunc creates a cached PriceFunc that reads model prices from model-prices.json.
// When the file is missing, empty, or invalid we return nil so callers do not
// pretend pricing is available and accidentally emit flat-zero cost charts.
func BuildModelPriceFunc(configPath string) PriceFunc {
	if configPath == "" {
		return nil
	}

	filePath := filepath.Join(filepath.Dir(configPath), "model-prices.json")

	type priceInfo struct {
		Prompt     float64 `json:"prompt"`
		Completion float64 `json:"completion"`
		Cache      float64 `json:"cache"`
	}

	var mu sync.RWMutex
	var cached map[string]priceInfo
	var loadedAt time.Time

	load := func() bool {
		data, err := os.ReadFile(filePath)
		if err != nil {
			return false
		}
		var m map[string]priceInfo
		if err := json.Unmarshal(data, &m); err != nil {
			return false
		}
		if len(m) == 0 {
			return false
		}
		mu.Lock()
		cached = m
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
