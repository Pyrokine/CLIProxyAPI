package usage

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/modelcatalog"
)

const modelPriceRefreshInterval = 5 * time.Minute

// PriceProvider returns an immutable legacy pricing function for one operation.
type PriceProvider interface {
	Snapshot() PriceFunc
}

type detailedPrice struct {
	CanonicalModel string
	Currency       string
	Source         string
	EffectiveFrom  string
	EffectiveUntil string
	Prompt         float64
	Completion     float64
	CacheRead      *float64
	CacheWrite     *float64
	CacheWrite5m   *float64
	CacheWrite1h   *float64
	Status         CostStatus
	Automatic      bool
	Effective      bool
	Reason         string
}

type detailedPriceFunc func(model string, at time.Time) (detailedPrice, bool)

type detailedPriceProvider interface {
	DetailedSnapshot() detailedPriceFunc
}

// detailedPriceRevisionProvider supplies a snapshot identity that survives a
// process restart. Recalculation uses it to decide whether a persisted cursor
// still belongs to the active price table.
type detailedPriceRevisionProvider interface {
	DetailedSnapshotWithRevision() (detailedPriceFunc, string)
}

type modelPriceProvider struct {
	mu              sync.Mutex
	configPath      string
	cached          modelcatalog.File
	loadedAt        time.Time
	priceFn         PriceFunc
	detailedPriceFn detailedPriceFunc
	revision        string
}

// BuildModelPriceProvider creates a provider that refreshes prices between
// operations while keeping each returned snapshot immutable.
func BuildModelPriceProvider(configPath string) PriceProvider {
	if configPath == "" {
		return nil
	}
	provider := &modelPriceProvider{configPath: configPath}
	if provider.DetailedSnapshot() == nil {
		return nil
	}
	return provider
}

// buildModelPriceFunc returns the current immutable model-price snapshot.
func buildModelPriceFunc(configPath string) PriceFunc {
	provider := BuildModelPriceProvider(configPath)
	if provider == nil {
		return nil
	}
	return provider.Snapshot()
}

func (p *modelPriceProvider) Snapshot() PriceFunc {
	priceFn, _, _ := p.snapshots()
	return priceFn
}

func (p *modelPriceProvider) DetailedSnapshot() detailedPriceFunc {
	_, detailed, _ := p.snapshots()
	return detailed
}

func (p *modelPriceProvider) DetailedSnapshotWithRevision() (detailedPriceFunc, string) {
	_, detailed, revision := p.snapshots()
	return detailed, revision
}

func (p *modelPriceProvider) snapshots() (PriceFunc, detailedPriceFunc, string) {
	if p == nil {
		return nil, nil, ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.cached.Prices) == 0 || time.Since(p.loadedAt) > modelPriceRefreshInterval {
		catalog, err := modelcatalog.Load(p.configPath)
		if err == nil && len(catalog.Prices) > 0 && catalog.Validate() == nil {
			p.cached = catalog
			p.loadedAt = time.Now()
			p.priceFn = priceFuncFromCatalog(catalog)
			p.detailedPriceFn = detailedPriceFuncFromCatalog(catalog)
			p.revision = catalogPriceRevision(catalog)
		}
	}
	if p.detailedPriceFn == nil && len(p.cached.Prices) > 0 {
		p.priceFn = priceFuncFromCatalog(p.cached)
		p.detailedPriceFn = detailedPriceFuncFromCatalog(p.cached)
		p.revision = catalogPriceRevision(p.cached)
	}
	return p.priceFn, p.detailedPriceFn, p.revision
}

func catalogPriceRevision(catalog modelcatalog.File) string {
	// Only fields used by indexCatalogPrices can influence a historical cost.
	// encoding/json emits map keys deterministically, and catalog.Sanitize keeps
	// alias collections normalized before the value is hashed.
	catalog.Sanitize()
	payload := struct {
		Prices          map[string]modelcatalog.Price       `json:"prices"`
		OAuthModelAlias map[string][]config.OAuthModelAlias `json:"oauth_model_alias"`
	}{
		Prices:          catalog.Prices,
		OAuthModelAlias: catalog.OAuthModelAlias,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(encoded)
	return fmt.Sprintf("%x", digest[:])
}

type indexedCatalogPrice struct {
	canonicalModel string
	price          modelcatalog.Price
}

func priceFuncFromCatalog(catalog modelcatalog.File) PriceFunc {
	indexed := indexCatalogPrices(catalog)
	if len(indexed) == 0 {
		return nil
	}
	return func(model string) (prompt, completion, cache float64, found bool) {
		entry, ok := indexed[modelPriceKey(model)]
		if !ok {
			return 0, 0, 0, false
		}
		now := time.Now()
		price := entry.price.RatesAt(now)
		currency := strings.ToUpper(strings.TrimSpace(price.Currency))
		if (currency != "" && currency != "USD") ||
			!price.EffectiveAt(now) || !price.HasReferenceTokenRates() {
			return 0, 0, 0, false
		}
		cacheRead, _ := price.CacheReadRate()
		return price.Prompt, price.Completion, cacheRead, true
	}
}

func detailedPriceFuncFromCatalog(catalog modelcatalog.File) detailedPriceFunc {
	indexed := indexCatalogPrices(catalog)
	if len(indexed) == 0 {
		return nil
	}
	return func(model string, at time.Time) (detailedPrice, bool) {
		entry, ok := indexed[modelPriceKey(model)]
		if !ok {
			return detailedPrice{}, false
		}
		price := entry.price.RatesAt(at)
		hasReferenceRates := price.HasReferenceTokenRates()
		cacheRead, hasCacheRead := price.CacheReadRate()
		cacheWrite, hasCacheWrite := price.CacheWriteRate()
		cacheWrite5m, hasCacheWrite5m := price.CacheWrite5mRate()
		cacheWrite1h, hasCacheWrite1h := price.CacheWrite1hRate()
		currency := strings.ToUpper(strings.TrimSpace(price.Currency))
		if currency == "" {
			currency = "USD"
		}
		source := strings.TrimSpace(price.Source)
		if source == "" {
			source = "model-catalog"
		}
		result := detailedPrice{
			CanonicalModel: entry.canonicalModel,
			Currency:       currency,
			Source:         source,
			EffectiveFrom:  strings.TrimSpace(price.EffectiveFrom),
			EffectiveUntil: strings.TrimSpace(price.EffectiveUntil),
			Prompt:         price.Prompt,
			Completion:     price.Completion,
			Status:         CostStatus(price.EffectiveStatus()),
			Automatic:      price.Automatic(),
			Effective:      price.EffectiveAt(at),
			Reason:         strings.Join(price.Limitations, "; "),
		}
		if hasReferenceRates && hasCacheRead {
			result.CacheRead = &cacheRead
		}
		if hasReferenceRates && hasCacheWrite {
			result.CacheWrite = &cacheWrite
		}
		if hasReferenceRates && hasCacheWrite5m {
			result.CacheWrite5m = &cacheWrite5m
		}
		if hasReferenceRates && hasCacheWrite1h {
			result.CacheWrite1h = &cacheWrite1h
		}
		if result.Reason == "" {
			result.Reason = strings.TrimSpace(price.UnpricedReason)
		}
		return result, true
	}
}

func indexCatalogPrices(catalog modelcatalog.File) map[string]indexedCatalogPrice {
	indexed := indexModelPrices(catalog.Prices)
	if len(indexed) == 0 {
		return nil
	}

	aliases := make(map[string]string)
	conflicts := make(map[string]struct{})
	for _, channelAliases := range catalog.OAuthModelAlias {
		for _, alias := range channelAliases {
			aliasKey := modelPriceKey(alias.Alias)
			canonicalKey := modelPriceKey(alias.Name)
			if aliasKey == "" || canonicalKey == "" || aliasKey == canonicalKey {
				continue
			}
			if _, direct := indexed[aliasKey]; direct {
				continue
			}
			if _, priced := indexed[canonicalKey]; !priced {
				continue
			}
			if existing, ok := aliases[aliasKey]; ok && existing != canonicalKey {
				conflicts[aliasKey] = struct{}{}
				continue
			}
			aliases[aliasKey] = canonicalKey
		}
	}
	for aliasKey, canonicalKey := range aliases {
		if _, conflict := conflicts[aliasKey]; conflict {
			continue
		}
		indexed[aliasKey] = indexed[canonicalKey]
	}
	return indexed
}

func indexModelPrices(prices map[string]modelcatalog.Price) map[string]indexedCatalogPrice {
	if len(prices) == 0 {
		return nil
	}
	indexed := make(map[string]indexedCatalogPrice, len(prices))
	for name, price := range prices {
		if key := modelPriceKey(name); key != "" {
			indexed[key] = indexedCatalogPrice{
				canonicalModel: strings.TrimSpace(name),
				price:          price,
			}
		}
	}
	return indexed
}

func modelPriceKey(model string) string {
	model = strings.TrimSpace(model)
	if len(model) >= len("models/") && strings.EqualFold(model[:len("models/")], "models/") {
		model = model[len("models/"):]
	}
	return strings.ToLower(strings.TrimSpace(model))
}
