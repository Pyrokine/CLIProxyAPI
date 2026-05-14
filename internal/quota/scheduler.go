// Last compiled: 2026-05-07
// Author: pyro

package quota

import (
	"container/heap"
	"context"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

// FetchFunc is called to refresh quota data for a credential.
// It receives the entry with all credential info, returns raw JSON data or an error.
type FetchFunc func(entry *Entry) ([]byte, error)

// Scheduler manages periodic quota refresh for all registered credentials.
// It uses a min-heap priority queue sorted by next refresh time, with a single
// dispatcher goroutine that serializes all fetch operations.
type Scheduler struct {
	mu              sync.RWMutex
	entries         map[string]*Entry
	cfg             Config
	fetchFn         FetchFunc
	cancel          context.CancelFunc
	wg              sync.WaitGroup
	wake            chan struct{} // signal dispatcher to re-check heap
	updatedAt       time.Time
	manualRefreshMu sync.Mutex // serializes manual RefreshNow calls so later requests wait instead of being dropped
}

// NewScheduler creates a new quota scheduler.
func NewScheduler(cfg Config, fetchFn FetchFunc) *Scheduler {
	return &Scheduler{
		entries: make(map[string]*Entry),
		cfg:     cfg,
		fetchFn: fetchFn,
		wake:    make(chan struct{}, 1),
	}
}

// Start begins the scheduler's dispatch loop. No-op if not enabled.
func (s *Scheduler) Start() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.cfg.Enabled {
		return
	}
	if s.cancel != nil {
		return // already running
	}

	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.wg.Add(1)
	go s.dispatchLoop(ctx)
}

// Stop gracefully shuts down the scheduler.
func (s *Scheduler) Stop() {
	s.mu.Lock()
	cancel := s.cancel
	s.cancel = nil
	s.mu.Unlock()

	if cancel != nil {
		cancel()
		s.wg.Wait()
	}
}

// Register adds or updates a credential in the scheduler.
func (s *Scheduler) Register(fileName string, credType CredentialType, authIndex string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if existing, exists := s.entries[fileName]; exists {
		existing.mu.Lock()
		existing.Type = credType
		existing.AuthIndex = authIndex
		existing.mu.Unlock()
		s.poke()
		return
	}

	now := time.Now()
	entry := &Entry{
		FileName:  fileName,
		Type:      credType,
		AuthIndex: authIndex,
		Status:    StatusIdle,
	}
	next := now.Add(time.Duration(len(s.entries)) * time.Second * 2) // stagger
	entry.NextRefresh = &next
	s.entries[fileName] = entry

	s.poke()
}

// Unregister removes a credential from the scheduler.
func (s *Scheduler) Unregister(fileName string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.entries, fileName)
}

// UpdateAuthIndex sets the auth_index for a registered entry.
func (s *Scheduler) UpdateAuthIndex(fileName, authIndex string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.entries[fileName]; ok {
		e.mu.Lock()
		e.AuthIndex = authIndex
		e.mu.Unlock()
	}
}

// SetDisabled updates the disabled state of a registered entry and pokes the
// dispatcher to re-evaluate the heap. Disabled entries are skipped by the auto
// refresh loop; manual RefreshNow(fileNames) still executes them so users can
// verify whether a banned/disabled account has recovered.
func (s *Scheduler) SetDisabled(fileName string, disabled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.entries[fileName]; ok {
		e.mu.Lock()
		e.Disabled = disabled
		e.mu.Unlock()
		s.poke()
	}
}

// RefreshNow triggers an immediate refresh for specific credentials (or all if empty).
// Works even when the scheduler is disabled (manual refresh).
// Manual calls are serialized so later requests wait for the current refresh instead
// of being silently dropped.
//
// When fileNames is non-empty the call is treated as a user-initiated manual refresh
// and executes every named entry regardless of disabled/banned/quota-exceeded state
// so the operator can verify whether an account has recovered. When fileNames is
// empty the call is a bulk auto-refresh and skips entries that shouldSkipAutoRefresh
// reports as ineligible.
func (s *Scheduler) RefreshNow(fileNames []string) {
	s.manualRefreshMu.Lock()
	defer s.manualRefreshMu.Unlock()

	isManual := len(fileNames) > 0

	s.mu.RLock()
	targets := fileNames
	if !isManual {
		targets = make([]string, 0, len(s.entries))
		for name := range s.entries {
			targets = append(targets, name)
		}
	}
	toRefresh := make([]*Entry, 0, len(targets))
	for _, name := range targets {
		e, ok := s.entries[name]
		if !ok {
			continue
		}
		if !isManual && e.shouldSkipAutoRefresh() {
			continue
		}
		toRefresh = append(toRefresh, e)
	}
	s.mu.RUnlock()

	// Execute refreshes directly (serialized)
	for _, entry := range toRefresh {
		s.executeRefresh(entry)
	}
}

// GetStatus returns the current status of all credentials.
func (s *Scheduler) GetStatus() StatusResponse {
	s.mu.RLock()
	defer s.mu.RUnlock()

	creds := make(map[string]*Entry, len(s.entries))
	for name, e := range s.entries {
		e.mu.RLock()
		cp := Entry{
			FileName:     e.FileName,
			Type:         e.Type,
			AuthIndex:    e.AuthIndex,
			Status:       e.Status,
			LastRefresh:  e.LastRefresh,
			NextRefresh:  e.NextRefresh,
			Error:        e.Error,
			FailureCount: e.FailureCount,
			Data:         e.Data,
			Disabled:     e.Disabled,
		}
		e.mu.RUnlock()
		creds[name] = &cp
	}
	return StatusResponse{
		Enabled:         s.cfg.Enabled,
		IntervalSeconds: s.cfg.Interval,
		Credentials:     creds,
		UpdatedAt:       s.updatedAt,
	}
}

// GetConfig returns the current configuration.
func (s *Scheduler) GetConfig() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

// UpdateConfig updates the scheduler configuration.
func (s *Scheduler) UpdateConfig(cfg Config) {
	s.mu.Lock()
	wasEnabled := s.cfg.Enabled
	s.cfg = cfg

	if cfg.Enabled && !wasEnabled {
		s.mu.Unlock()
		s.Start()
	} else if !cfg.Enabled && wasEnabled {
		s.mu.Unlock()
		s.Stop()
	} else {
		s.mu.Unlock()
	}
}

// poke signals the dispatcher to re-check the heap. Must be called with s.mu held.
func (s *Scheduler) poke() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// dispatchLoop is the main scheduler goroutine.
func (s *Scheduler) dispatchLoop(ctx context.Context) {
	defer s.wg.Done()

	for {
		s.mu.RLock()
		h := s.buildHeap()
		s.mu.RUnlock()

		if h.Len() == 0 {
			select {
			case <-ctx.Done():
				return
			case <-s.wake:
				continue
			}
		}

		next := h[0]
		delay := time.Until(*next.NextRefresh)
		if delay <= 0 {
			// Execute immediately
			s.executeRefresh(next)
			continue
		}

		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-s.wake:
			timer.Stop()
			continue
		case <-timer.C:
			s.executeRefresh(next)
		}
	}
}

func (s *Scheduler) executeRefresh(entry *Entry) {
	entry.mu.Lock()
	entry.Status = StatusLoading
	entry.mu.Unlock()

	data, err := s.fetchFn(entry)

	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	entry.mu.Lock()
	defer entry.mu.Unlock()

	entry.LastRefresh = &now
	s.updatedAt = now

	if err != nil {
		errMsg := err.Error()
		status, backoffMultiplier := classifyError(errMsg)

		entry.Status = status
		entry.Error = errMsg
		entry.FailureCount++

		backoff := time.Duration(s.cfg.Interval) * time.Second *
			time.Duration(backoffMultiplier) * (1 << min(entry.FailureCount, 5))
		maxInterval := time.Duration(s.cfg.MaxInterval) * time.Second
		if backoff > maxInterval {
			backoff = maxInterval
		}
		next := now.Add(backoff)
		entry.NextRefresh = &next
		log.Warnf(
			"quota: refresh failed for %s [%s]: %v (next retry in %v)",
			entry.FileName, status, err, backoff,
		)
	} else {
		entry.Status = StatusSuccess
		entry.Error = ""
		entry.FailureCount = 0
		entry.Data = data
		next := now.Add(time.Duration(s.cfg.Interval) * time.Second)
		entry.NextRefresh = &next
	}
}

// classifyError determines the error status and backoff multiplier from the error message.
// Returns (status, backoffMultiplier) where multiplier scales the base exponential backoff.
func classifyError(errMsg string) (QuotaStatus, int) {
	if strings.Contains(errMsg, "status 429") || strings.Contains(errMsg, "status 403") {
		return StatusBanned, 4
	}
	if strings.Contains(errMsg, "status 402") {
		return StatusQuotaExceeded, 1
	}
	return StatusError, 1
}

// buildHeap constructs a min-heap of entries sorted by NextRefresh. Must be called with s.mu held.
// Entries that shouldSkipAutoRefresh reports as ineligible (disabled, banned, quota-exceeded)
// are excluded so the dispatcher never wakes for them; manual RefreshNow still reaches them directly.
func (s *Scheduler) buildHeap() entryHeap {
	h := make(entryHeap, 0, len(s.entries))
	for _, e := range s.entries {
		if e.NextRefresh == nil {
			continue
		}
		if e.shouldSkipAutoRefresh() {
			continue
		}
		h = append(h, e)
	}
	heap.Init(&h)
	return h
}

// entryHeap implements heap.Interface for Entry pointers, ordered by NextRefresh.
type entryHeap []*Entry

func (h entryHeap) Len() int { return len(h) }
func (h entryHeap) Less(i, j int) bool {
	if h[i].NextRefresh == nil {
		return false
	}
	if h[j].NextRefresh == nil {
		return true
	}
	return h[i].NextRefresh.Before(*h[j].NextRefresh)
}
func (h entryHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }

func (h *entryHeap) Push(x interface{}) {
	*h = append(*h, x.(*Entry))
}

func (h *entryHeap) Pop() interface{} {
	old := *h
	n := len(old)
	item := old[n-1]
	*h = old[:n-1]
	return item
}
