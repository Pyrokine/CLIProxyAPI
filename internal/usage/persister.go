// Last compiled: 2026-03-10
// Author: pyro

package usage

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	log "github.com/sirupsen/logrus"
)

const (
	defaultSaveInterval = 5 * time.Minute
	defaultFileName     = "usage-statistics.json"
	archivePrefix       = "usage-archive-"
	trimInterval        = 1 * time.Hour
	dataDirName         = "usage-data"

	defaultRetentionDays = 30
	defaultMaxFileSizeMB = 50
	defaultArchiveMonths = 3
)

// PriceFunc returns the pricing for a model. Returns (prompt, completion, cache, found).
type PriceFunc func(model string) (prompt, completion, cache float64, found bool)

// Persister handles recording, periodic persistence, trimming, and archival of usage statistics.
// It manages three data stores:
//   - SummaryData: all-time aggregated metrics (small, always in memory)
//   - TodayStore: current day's request details (in memory, persisted periodically)
//   - DetailStore: historical per-day detail files (on disk, loaded on demand)
type Persister struct {
	mu          sync.RWMutex
	summary     *SummaryData
	today       *TodayStore
	detailStore *DetailStore
	pricesFn    PriceFunc
	baseDir     string
	interval    time.Duration
	retention   config.UsageRetention
	cancel      context.CancelFunc
	done        chan struct{}
	once        sync.Once
}

// NewPersister constructs a new Persister for the given base directory.
// Zero retention values apply defaults; negative values disable the corresponding feature.
func NewPersister(baseDir string, retention config.UsageRetention) *Persister {
	if retention.Days == 0 {
		retention.Days = defaultRetentionDays
	}
	if retention.MaxFileSizeMB == 0 {
		retention.MaxFileSizeMB = defaultMaxFileSizeMB
	}
	if retention.ArchiveMonths == 0 {
		retention.ArchiveMonths = defaultArchiveMonths
	}
	return &Persister{
		baseDir:   baseDir,
		interval:  defaultSaveInterval,
		retention: retention,
	}
}

// SetPriceFunc sets the function used to look up model prices for cost calculation.
func (p *Persister) SetPriceFunc(fn PriceFunc) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pricesFn = fn
}

// HasPricing reports whether a price lookup function has been configured.
func (p *Persister) HasPricing() bool {
	if p == nil {
		return false
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.pricesFn != nil
}

// Start loads existing data from disk and begins periodic saving and trimming.
func (p *Persister) Start(ctx context.Context) {
	if p == nil || p.baseDir == "" {
		return
	}

	if err := os.MkdirAll(p.baseDir, 0o700); err != nil {
		log.Errorf("usage: failed to create data directory %s: %v", p.baseDir, err)
		return
	}

	p.load()

	childCtx, cancel := context.WithCancel(ctx)
	p.cancel = cancel
	p.done = make(chan struct{})
	go p.run(childCtx)
}

// Stop saves a final snapshot and stops the periodic saver.
func (p *Persister) Stop() {
	if p == nil {
		return
	}
	p.once.Do(
		func() {
			if p.cancel != nil {
				p.cancel()
			}
			if p.done != nil {
				<-p.done
			}
			p.save()
		},
	)
}

// Record ingests a new request detail, updates summary, and appends to today's store.
func (p *Persister) Record(detail FlatDetail) {
	if p == nil {
		return
	}
	p.mu.RLock()
	defer p.mu.RUnlock()

	if p.summary == nil || p.today == nil {
		return
	}

	// Check for date rotation
	detailDate := detail.Timestamp.UTC().Format("2006-01-02")
	if p.today.Date() != detailDate && detailDate > p.today.Date() {
		// Need write lock for rotation
		p.mu.RUnlock()
		p.rotateDay(detailDate)
		p.mu.RLock()
	}

	cost := p.calculateCost(detail)
	p.summary.Record(detail, cost)
	p.today.Append(detail)
}

// Summary returns the in-memory SummaryData.
func (p *Persister) Summary() *SummaryData {
	if p == nil {
		return nil
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.summary
}

// TodayStore returns the today store for direct queries.
func (p *Persister) TodayStore() *TodayStore {
	if p == nil {
		return nil
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.today
}

// DetailStore returns the historical detail store.
func (p *Persister) DetailStore() *DetailStore {
	if p == nil {
		return nil
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.detailStore
}

// BaseDir returns the usage data directory.
func (p *Persister) BaseDir() string {
	if p == nil {
		return ""
	}
	return p.baseDir
}

// Trim performs an immediate trim of old details and saves the result.
func (p *Persister) Trim() {
	if p == nil {
		return
	}
	p.trim()
}

// TrimPreviewResult returns a preview of what would be cleaned by Trim.
type TrimPreviewResult struct {
	FilesCount     int                `json:"files_count"`
	TotalSizeBytes int64              `json:"total_size_bytes"`
	DateRange      *trimDateRange     `json:"date_range,omitempty"`
	Details        []cleanPreviewFile `json:"details"`
}

// trimDateRange describes the date range of files to be cleaned.
type trimDateRange struct {
	Oldest string `json:"oldest"`
	Newest string `json:"newest"`
}

// TrimPreview returns info about what Trim would clean, without actually cleaning.
func (p *Persister) TrimPreview() TrimPreviewResult {
	if p == nil {
		return TrimPreviewResult{}
	}

	p.mu.RLock()
	retentionDays := p.retention.Days
	p.mu.RUnlock()

	if retentionDays <= 0 || p.detailStore == nil {
		return TrimPreviewResult{}
	}

	cutoff := time.Now().Add(-time.Duration(retentionDays) * 24 * time.Hour)
	files := p.detailStore.cleanPreview(cutoff)

	result := TrimPreviewResult{
		FilesCount: len(files),
		Details:    files,
	}
	for _, f := range files {
		result.TotalSizeBytes += f.SizeBytes
	}
	if len(files) > 0 {
		result.DateRange = &trimDateRange{
			Oldest: files[0].Date,
			Newest: files[len(files)-1].Date,
		}
	}
	return result
}

// Retention returns the current retention configuration.
func (p *Persister) Retention() config.UsageRetention {
	if p == nil {
		return config.UsageRetention{}
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.retention
}

// SetRetention updates the retention configuration.
func (p *Persister) SetRetention(r config.UsageRetention) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.retention = r
}

// ListArchives returns info about archived day files.
func (p *Persister) ListArchives() []archiveInfo {
	if p == nil || p.detailStore == nil {
		return nil
	}

	days := p.detailStore.listDays()
	var archives []archiveInfo
	for _, day := range days {
		t, err := time.Parse("2006-01-02", day)
		if err != nil {
			continue
		}
		monthDir := filepath.Join(p.baseDir, t.Format("2006-01"))
		filePath := filepath.Join(monthDir, day+".json")
		info, err := os.Stat(filePath)
		if err != nil {
			continue
		}
		archives = append(
			archives, archiveInfo{
				Month:     t.Format("2006-01"),
				FileName:  day + ".json",
				SizeBytes: info.Size(),
			},
		)
	}
	return archives
}

// archiveInfo describes an archived usage detail file.
type archiveInfo struct {
	Month     string `json:"month"`
	FileName  string `json:"file_name"`
	SizeBytes int64  `json:"size_bytes"`
}

// Snapshot builds a legacy StatisticsSnapshot from the current state for export compatibility.
func (p *Persister) Snapshot() StatisticsSnapshot {
	if p == nil {
		return StatisticsSnapshot{}
	}

	p.mu.RLock()
	defer p.mu.RUnlock()

	snapshot := StatisticsSnapshot{
		APIs:           make(map[string]aPISnapshot),
		RequestsByDay:  make(map[string]int64),
		RequestsByHour: make(map[string]int64),
		TokensByDay:    make(map[string]int64),
		TokensByHour:   make(map[string]int64),
	}

	if p.summary == nil {
		return snapshot
	}

	snapshot.TotalRequests = p.summary.Totals.Requests
	snapshot.SuccessCount = p.summary.Totals.Success
	snapshot.FailureCount = p.summary.Totals.Failure
	snapshot.TotalTokens = p.summary.Totals.Tokens.TotalTokens

	// Build APIs map from today's details
	if p.today != nil {
		details := p.today.Details()
		for _, d := range details {
			apiKey := d.Source
			if apiKey == "" {
				apiKey = d.AuthIndex
			}
			if apiKey == "" {
				apiKey = "unknown"
			}
			api, ok := snapshot.APIs[apiKey]
			if !ok {
				api = aPISnapshot{Models: make(map[string]modelSnapshot)}
			}
			model := api.Models[d.Model]
			model.Details = append(
				model.Details, requestDetail{
					Timestamp: d.Timestamp,
					Source:    d.Source,
					AuthIndex: d.AuthIndex,
					Tokens:    d.Tokens,
					Failed:    d.Failed,
				},
			)
			model.TotalRequests++
			model.TotalTokens += d.Tokens.TotalTokens
			api.Models[d.Model] = model
			api.TotalRequests++
			api.TotalTokens += d.Tokens.TotalTokens
			snapshot.APIs[apiKey] = api
		}
	}

	return snapshot
}

func (p *Persister) run(ctx context.Context) {
	defer close(p.done)
	saveTicker := time.NewTicker(p.interval)
	trimTicker := time.NewTicker(trimInterval)
	defer saveTicker.Stop()
	defer trimTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-saveTicker.C:
			p.save()
		case <-trimTicker.C:
			p.trim()
		}
	}
}

func (p *Persister) load() {
	summaryPath := filepath.Join(p.baseDir, "summary.json")
	todayPath := filepath.Join(p.baseDir, "today.json")
	todayDate := time.Now().UTC().Format("2006-01-02")

	// Load summary
	summary, err := loadSummary(summaryPath)
	if err != nil {
		log.Warnf("usage: failed to load summary: %v", err)
		summary = newSummaryData()
	}

	// Load today store
	today, staleDate, staleDetails, err := loadTodayStore(todayPath, todayDate)
	if err != nil {
		log.Warnf("usage: failed to load today store: %v", err)
		today = newTodayStore(todayDate, todayPath)
	}

	detailStore := newDetailStore(p.baseDir)

	// Archive stale details from a previous day
	if staleDate != "" && len(staleDetails) > 0 {
		if err := detailStore.Archive(staleDate, staleDetails); err != nil {
			log.Warnf("usage: failed to archive stale day %s: %v", staleDate, err)
		} else {
			log.Infof("usage: archived %d stale details from %s", len(staleDetails), staleDate)
		}
	}

	p.mu.Lock()
	p.summary = summary
	p.today = today
	p.detailStore = detailStore
	p.mu.Unlock()

	log.Infof(
		"usage: loaded data from %s (summary: %d requests, today: %d details)",
		p.baseDir, summary.Totals.Requests, today.Len(),
	)
}

func (p *Persister) save() {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if p.summary == nil {
		return
	}

	summaryPath := filepath.Join(p.baseDir, "summary.json")
	if err := saveSummary(summaryPath, p.summary); err != nil {
		log.Errorf("usage: failed to save summary: %v", err)
	}

	if p.today != nil {
		if err := p.today.Save(); err != nil {
			log.Errorf("usage: failed to save today store: %v", err)
		}
	}

	log.Debugf("usage: saved data to %s (%d requests)", p.baseDir, p.summary.Totals.Requests)
}

func (p *Persister) trim() {
	p.mu.RLock()
	retentionDays := p.retention.Days
	p.mu.RUnlock()

	if retentionDays <= 0 {
		return
	}
	cutoff := time.Now().Add(-time.Duration(retentionDays) * 24 * time.Hour)

	// Clean old detail files
	if p.detailStore != nil {
		if err := p.detailStore.cleanBefore(cutoff); err != nil {
			log.Warnf("usage: failed to clean old details: %v", err)
		}
	}

	// Clean old daily entries from summary
	if p.summary != nil {
		p.summary.cleanBefore(cutoff)
	}

	p.save()
}

func (p *Persister) rotateDay(newDate string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.today == nil {
		return
	}

	prevDate, prevDetails := p.today.rotate(newDate)
	if len(prevDetails) > 0 && p.detailStore != nil {
		if err := p.detailStore.Archive(prevDate, prevDetails); err != nil {
			log.Errorf("usage: failed to archive day %s on rotation: %v", prevDate, err)
		} else {
			log.Infof("usage: rotated day %s → %s (archived %d details)", prevDate, newDate, len(prevDetails))
		}
	}
}

func (p *Persister) calculateCost(detail FlatDetail) float64 {
	if p.pricesFn == nil {
		return 0
	}
	prompt, completion, cache, ok := p.pricesFn(detail.Model)
	if !ok {
		return 0
	}
	return float64(detail.Tokens.InputTokens)*prompt/tokenPriceUnit +
		float64(detail.Tokens.OutputTokens)*completion/tokenPriceUnit +
		float64(detail.Tokens.CachedTokens)*cache/tokenPriceUnit
}

// persistedPayload is the legacy on-disk format (v1), kept for migration compatibility.
type persistedPayload struct {
	Version int                `json:"version"`
	SavedAt time.Time          `json:"saved_at"`
	Usage   StatisticsSnapshot `json:"usage"`
}

// ResolveDataDir determines the base directory for usage data storage.
// Priority: configured > WRITABLE_PATH/usage-data > config dir/usage-data
func ResolveDataDir(configured string, configFilePath string) string {
	if configured != "" {
		return configured
	}
	if wp := util.WritablePath(); wp != "" {
		return filepath.Join(wp, dataDirName)
	}
	if configFilePath != "" {
		return filepath.Join(filepath.Dir(configFilePath), dataDirName)
	}
	return ""
}

func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("failed to create directory %s: %w", dir, err)
	}
	tmpFile, err := os.CreateTemp(dir, filepath.Base(path)+".tmp.*")
	if err != nil {
		return fmt.Errorf("failed to create temp file in %s: %w", dir, err)
	}
	tmpPath := tmpFile.Name()
	if _, err := tmpFile.Write(data); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("failed to write %s: %w", tmpPath, err)
	}
	if err := tmpFile.Chmod(0o600); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("failed to chmod %s: %w", tmpPath, err)
	}
	tmpFile.Close()
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("failed to rename %s -> %s: %w", tmpPath, path, err)
	}
	return nil
}

// NeedsMigration checks if a legacy usage-statistics.json file exists.
func NeedsMigration(configFilePath string, configured string) (oldPath string, needed bool) {
	path := configured
	if path == "" {
		if configFilePath == "" {
			return "", false
		}
		path = filepath.Join(filepath.Dir(configFilePath), defaultFileName)
	}
	if _, err := os.Stat(path); err == nil {
		return path, true
	}
	return "", false
}
