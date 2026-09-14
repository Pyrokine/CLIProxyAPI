package management

import (
	"context"
	"encoding/hex"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/quota"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

const (
	accountInspectionStatusIdle      = "idle"
	accountInspectionStatusRunning   = "running"
	accountInspectionStatusCompleted = "completed"
	accountInspectionStatusStopped   = "stopped"

	accountHealthNormal      = "normal"
	accountHealthUnavailable = "unavailable"
	accountHealthUnknown     = "unknown"
	accountHealthDisabled    = "disabled"

	accountProbePassed       = "passed"
	accountProbeFailed       = "failed"
	accountProbeInconclusive = "inconclusive"
	accountProbeOutdated     = "outdated"

	accountInspectionProbeRevision = "quota-v1"
)

type accountInspectionSchedule struct {
	Enabled         bool     `json:"enabled"`
	IntervalSeconds int      `json:"interval_seconds"`
	Providers       []string `json:"providers"`
	MaxConcurrency  int      `json:"max_concurrency"`
	TimeoutSeconds  int      `json:"timeout_seconds"`
	RetentionRuns   int      `json:"retention_runs"`
}

type accountInspectionSummary struct {
	Total         int `json:"total"`
	Normal        int `json:"normal"`
	Abnormal      int `json:"abnormal"`
	TokenInvalid  int `json:"token_invalid"`
	RefreshFailed int `json:"refresh_failed"`
	Disabled      int `json:"disabled"`
}

type accountInspectionReasonCount struct {
	Reason string `json:"reason"`
	Count  int    `json:"count"`
}

type accountInspectionProviderSummary struct {
	Provider      string                         `json:"provider"`
	Total         int                            `json:"total"`
	Normal        int                            `json:"normal"`
	Abnormal      int                            `json:"abnormal"`
	TokenInvalid  int                            `json:"token_invalid"`
	RefreshFailed int                            `json:"refresh_failed"`
	Disabled      int                            `json:"disabled"`
	Unavailable   int                            `json:"unavailable"`
	Unknown       int                            `json:"unknown"`
	LastCheckedAt *time.Time                     `json:"last_checked_at,omitempty"`
	TopReasons    []accountInspectionReasonCount `json:"top_reasons,omitempty"`
}

type accountInspectionRun struct {
	ID        string                   `json:"id"`
	StartedAt time.Time                `json:"started_at"`
	EndedAt   *time.Time               `json:"ended_at,omitempty"`
	Duration  int64                    `json:"duration_ms"`
	Status    string                   `json:"status"`
	Checked   int                      `json:"checked"`
	Summary   accountInspectionSummary `json:"summary"`
	Error     string                   `json:"error,omitempty"`
}

type accountInspectionResult struct {
	ID               string    `json:"id"`
	Account          string    `json:"account"`
	FileName         string    `json:"file_name"`
	Provider         string    `json:"provider"`
	AuthIndex        string    `json:"auth_index,omitempty"`
	Status           string    `json:"status"`
	Reason           string    `json:"reason"`
	Advice           string    `json:"advice"`
	AutoHandling     string    `json:"auto_handling"`
	ProbeStatus      string    `json:"probe_status"`
	ProbeError       string    `json:"probe_error,omitempty"`
	AuthGeneration   uint64    `json:"auth_generation"`
	CredentialDigest string    `json:"credential_digest,omitempty"`
	ProbeRevision    string    `json:"probe_revision"`
	CheckedAt        time.Time `json:"checked_at"`
}

type accountInspectionCandidate struct {
	auth           *coreauth.Auth
	quotaEntry     *quota.Entry
	probeTarget    quota.RefreshTarget
	hasProbeTarget bool
}

type accountInspectionLogEntry struct {
	Time    time.Time `json:"time"`
	Level   string    `json:"level"`
	Message string    `json:"message"`
}

type accountInspectionRefreshSummary struct {
	Pending     int               `json:"pending"`
	Skipped     int               `json:"skipped"`
	Failed      int               `json:"failed"`
	NextRefresh *time.Time        `json:"next_refresh,omitempty"`
	LatestTask  *quotaRefreshTask `json:"latest_task,omitempty"`
}

type accountInspectionStatusResponse struct {
	Status            string                                      `json:"status"`
	Schedule          accountInspectionSchedule                   `json:"schedule"`
	Summary           accountInspectionSummary                    `json:"summary"`
	SummaryByProvider map[string]accountInspectionProviderSummary `json:"summary_by_provider"`
	RefreshQueue      accountInspectionRefreshSummary             `json:"refresh_queue"`
	CurrentRun        *accountInspectionRun                       `json:"current_run,omitempty"`
	LastRun           *accountInspectionRun                       `json:"last_run,omitempty"`
	UpdatedAt         time.Time                                   `json:"updated_at"`
}

type accountInspectionService struct {
	handler        *Handler
	mu             sync.Mutex
	schedule       accountInspectionSchedule
	status         string
	currentRun     *accountInspectionRun
	lastRun        *accountInspectionRun
	runs           []accountInspectionRun
	results        []accountInspectionResult
	logs           []accountInspectionLogEntry
	cancel         context.CancelFunc
	scheduleCancel context.CancelFunc
	updatedAt      time.Time
}

var accountInspectionSecretPattern = regexp.MustCompile(`(?i)(authorization|cookie|api[_-]?key|access[_-]?token|refresh[_-]?token|secret)(\s*[:=]\s*)("[^"]+"|\S+)`)
var accountInspectionLongKeyPattern = regexp.MustCompile(`\b(sk-[A-Za-z0-9_-]{8,}|[A-Za-z0-9_-]{32,})\b`)

func newAccountInspectionService(handler *Handler) *accountInspectionService {
	now := time.Now().UTC()
	service := &accountInspectionService{
		handler:   handler,
		schedule:  defaultAccountInspectionSchedule(),
		status:    accountInspectionStatusIdle,
		updatedAt: now,
	}
	service.loadPersistedState(now)
	if service.schedule.Enabled {
		ctx, cancel := context.WithCancel(context.Background())
		service.scheduleCancel = cancel
		go service.scheduleLoop(ctx, service.schedule.IntervalSeconds)
	}
	return service
}

func defaultAccountInspectionSchedule() accountInspectionSchedule {
	return accountInspectionSchedule{
		Enabled:         false,
		IntervalSeconds: 3600,
		Providers:       []string{},
		MaxConcurrency:  4,
		TimeoutSeconds:  30,
		RetentionRuns:   20,
	}
}

func (h *Handler) inspectionService() *accountInspectionService {
	if h.accountInspection != nil {
		return h.accountInspection
	}
	h.accountInspection = newAccountInspectionService(h)
	return h.accountInspection
}

func (s *accountInspectionService) loadPersistedState(now time.Time) {
	store := s.stateStore()
	if store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	state, err := store.loadAccountInspection(ctx)
	if err != nil {
		log.WithError(err).Warn("failed to load account inspection state")
		return
	}
	if state == nil {
		return
	}
	s.schedule = normalizePersistedInspectionSchedule(state.Schedule)
	s.status = strings.TrimSpace(state.Status)
	if s.status == "" {
		s.status = accountInspectionStatusIdle
	}
	s.currentRun = cloneInspectionRun(state.CurrentRun)
	s.lastRun = cloneInspectionRun(state.LastRun)
	s.runs = append([]accountInspectionRun(nil), state.Runs...)
	s.results = append([]accountInspectionResult(nil), state.Results...)
	s.logs = append([]accountInspectionLogEntry(nil), state.Logs...)
	s.updatedAt = state.UpdatedAt
	if s.updatedAt.IsZero() {
		s.updatedAt = now
	}
	if s.currentRun != nil || s.status == accountInspectionStatusRunning {
		interrupted := cloneInspectionRun(s.currentRun)
		if interrupted == nil {
			interrupted = &accountInspectionRun{ID: fmt.Sprintf("%d", now.UnixNano()), StartedAt: now}
		}
		interrupted.Status = accountInspectionStatusStopped
		ended := now
		interrupted.EndedAt = &ended
		interrupted.Duration = ended.Sub(interrupted.StartedAt).Milliseconds()
		interrupted.Error = "interrupted by server restart"
		s.currentRun = nil
		s.lastRun = interrupted
		s.runs = append(s.runs, *interrupted)
		s.status = accountInspectionStatusStopped
		s.updatedAt = now
		s.addLogLocked("warn", "account inspection interrupted by server restart")
		s.persist()
	}
}

func (s *accountInspectionService) stateStore() *managementStateStore {
	if s == nil || s.handler == nil {
		return nil
	}
	return s.handler.stateStore
}

func (s *accountInspectionService) close() {
	if s == nil {
		return
	}
	shouldPersist := false
	now := time.Now().UTC()
	s.mu.Lock()
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
	if s.scheduleCancel != nil {
		s.scheduleCancel()
		s.scheduleCancel = nil
	}
	if s.currentRun != nil || s.status == accountInspectionStatusRunning {
		interrupted := cloneInspectionRun(s.currentRun)
		if interrupted == nil {
			interrupted = &accountInspectionRun{ID: fmt.Sprintf("%d", now.UnixNano()), StartedAt: now}
		}
		interrupted.Status = accountInspectionStatusStopped
		interrupted.EndedAt = &now
		interrupted.Duration = now.Sub(interrupted.StartedAt).Milliseconds()
		interrupted.Error = "interrupted by server shutdown"
		s.currentRun = nil
		s.lastRun = interrupted
		s.runs = append(s.runs, *interrupted)
		s.status = accountInspectionStatusStopped
		s.updatedAt = now
		s.addLogLocked("warn", "account inspection interrupted by server shutdown")
		shouldPersist = true
	}
	s.mu.Unlock()
	if shouldPersist {
		s.persist()
	}
}

func (s *accountInspectionService) persist() {
	store := s.stateStore()
	if store == nil {
		return
	}
	state := s.persistenceSnapshot()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := store.saveAccountInspection(ctx, state); err != nil {
		log.WithError(err).Warn("failed to save account inspection state")
	}
}

func (s *accountInspectionService) persistenceSnapshot() accountInspectionPersistedState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return accountInspectionPersistedState{
		Schedule:   cloneInspectionSchedule(s.schedule),
		Status:     s.status,
		CurrentRun: cloneInspectionRun(s.currentRun),
		LastRun:    cloneInspectionRun(s.lastRun),
		Runs:       append([]accountInspectionRun(nil), s.runs...),
		Results:    append([]accountInspectionResult(nil), s.results...),
		Logs:       append([]accountInspectionLogEntry(nil), s.logs...),
		UpdatedAt:  s.updatedAt,
	}
}

func (h *Handler) GetAccountInspectionStatus(c *gin.Context) {
	c.JSON(http.StatusOK, h.inspectionService().statusSnapshot())
}

func (h *Handler) GetAccountInspectionSchedule(c *gin.Context) {
	c.JSON(http.StatusOK, h.inspectionService().scheduleSnapshot())
}

func (h *Handler) PutAccountInspectionSchedule(c *gin.Context) {
	var req accountInspectionSchedule
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid json"})
		return
	}
	schedule, err := h.inspectionService().updateSchedule(req)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, schedule)
}

func (h *Handler) GetAccountInspectionResults(c *gin.Context) {
	results, total, page, pageSize := h.inspectionService().resultsSnapshot(
		strings.TrimSpace(c.Query("status")),
		strings.EqualFold(strings.TrimSpace(c.Query("issues_only")), "true"),
		strings.EqualFold(strings.TrimSpace(c.Query("include_disabled")), "true"),
		intParam(c, "page", 1),
		intParam(c, "page_size", 50),
	)
	c.JSON(
		http.StatusOK, gin.H{
			"results":     results,
			"total":       total,
			"page":        page,
			"page_size":   pageSize,
			"total_pages": (total + pageSize - 1) / pageSize,
		},
	)
}

func (h *Handler) GetAccountInspectionLogs(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"logs": h.inspectionService().logsSnapshot()})
}

func (h *Handler) PostAccountInspectionRun(c *gin.Context) {
	if run, ok := h.inspectionService().runAsync("manual", ""); ok {
		c.JSON(http.StatusAccepted, run)
		return
	}
	c.JSON(http.StatusConflict, gin.H{"error": "account inspection already running"})
}

func (h *Handler) PostAccountInspectionInspectOne(c *gin.Context) {
	var req struct {
		ID        string `json:"id"`
		FileName  string `json:"file_name"`
		AuthIndex string `json:"auth_index"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid json"})
		return
	}
	target := firstNonEmpty(req.ID, req.FileName, req.AuthIndex)
	if target == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing account identifier"})
		return
	}
	if run, ok := h.inspectionService().runAsync("manual", target); ok {
		c.JSON(http.StatusAccepted, run)
		return
	}
	c.JSON(http.StatusConflict, gin.H{"error": "account inspection already running"})
}

func (h *Handler) PostAccountInspectionStop(c *gin.Context) {
	if h.inspectionService().stop() {
		c.JSON(http.StatusAccepted, gin.H{"status": accountInspectionStatusStopped})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": h.inspectionService().statusSnapshot().Status})
}

func (h *Handler) PostAccountInspectionRefreshToken(c *gin.Context) {
	cfg := h.configSnapshot()
	var req struct {
		ID        string `json:"id"`
		FileName  string `json:"file_name"`
		AuthIndex string `json:"auth_index"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid json"})
		return
	}
	target := firstNonEmpty(req.ID, req.FileName, req.AuthIndex)
	if target == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing account identifier"})
		return
	}
	auth := h.findInspectionAuth(target)
	if auth == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "account not found"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	_, err := h.resolveTokenForAuth(ctx, cfg, auth)
	if err != nil {
		h.inspectionService().addLog(
			"error", "manual token refresh failed for "+inspectionAccountName(auth)+": "+err.Error(),
		)
		c.JSON(http.StatusInternalServerError, gin.H{"error": sanitizeInspectionText(err.Error())})
		return
	}
	h.inspectionService().addLog("info", "manual token refresh completed for "+inspectionAccountName(auth))
	c.JSON(http.StatusOK, gin.H{"status": "refresh_completed"})
}

func (s *accountInspectionService) statusSnapshot() accountInspectionStatusResponse {
	s.mu.Lock()
	defer s.mu.Unlock()
	return accountInspectionStatusResponse{
		Status:            s.status,
		Schedule:          cloneInspectionSchedule(s.schedule),
		Summary:           summarizeInspectionResults(s.results),
		SummaryByProvider: summarizeInspectionResultsByProvider(s.results),
		RefreshQueue:      s.refreshSummaryLocked(),
		CurrentRun:        cloneInspectionRun(s.currentRun),
		LastRun:           cloneInspectionRun(s.lastRun),
		UpdatedAt:         s.updatedAt,
	}
}

func (s *accountInspectionService) scheduleSnapshot() accountInspectionSchedule {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneInspectionSchedule(s.schedule)
}

func (s *accountInspectionService) updateSchedule(req accountInspectionSchedule) (accountInspectionSchedule, error) {
	if req.IntervalSeconds == 0 {
		req.IntervalSeconds = s.schedule.IntervalSeconds
	}
	if req.MaxConcurrency == 0 {
		req.MaxConcurrency = s.schedule.MaxConcurrency
	}
	if req.TimeoutSeconds == 0 {
		req.TimeoutSeconds = s.schedule.TimeoutSeconds
	}
	if req.RetentionRuns == 0 {
		req.RetentionRuns = s.schedule.RetentionRuns
	}
	if req.IntervalSeconds < 60 {
		return accountInspectionSchedule{}, fmt.Errorf("interval_seconds must be at least 60")
	}
	if req.MaxConcurrency < 1 {
		return accountInspectionSchedule{}, fmt.Errorf("max_concurrency must be at least 1")
	}
	if req.TimeoutSeconds < 1 {
		return accountInspectionSchedule{}, fmt.Errorf("timeout_seconds must be at least 1")
	}
	if req.RetentionRuns < 1 {
		return accountInspectionSchedule{}, fmt.Errorf("retention_runs must be at least 1")
	}
	req.Providers = normalizeInspectionProviders(req.Providers)

	s.mu.Lock()
	s.schedule = req
	s.updatedAt = time.Now().UTC()
	if s.scheduleCancel != nil {
		s.scheduleCancel()
		s.scheduleCancel = nil
	}
	if req.Enabled {
		ctx, cancel := context.WithCancel(context.Background())
		s.scheduleCancel = cancel
		go s.scheduleLoop(ctx, req.IntervalSeconds)
	}
	s.addLogLocked(
		"info",
		fmt.Sprintf("account inspection schedule updated enabled=%t interval=%ds", req.Enabled, req.IntervalSeconds),
	)
	out := cloneInspectionSchedule(s.schedule)
	s.mu.Unlock()
	s.persist()
	return out, nil
}

func (s *accountInspectionService) runAsync(trigger string, target string) (*accountInspectionRun, bool) {
	s.mu.Lock()
	if s.status == accountInspectionStatusRunning {
		s.mu.Unlock()
		return nil, false
	}
	ctx, cancel := context.WithCancel(context.Background())
	run := &accountInspectionRun{
		ID:        fmt.Sprintf("%d", time.Now().UnixNano()),
		StartedAt: time.Now().UTC(),
		Status:    accountInspectionStatusRunning,
	}
	s.status = accountInspectionStatusRunning
	s.currentRun = run
	s.cancel = cancel
	s.updatedAt = run.StartedAt
	s.addLogLocked("info", "account inspection started trigger="+trigger)
	s.mu.Unlock()
	s.persist()

	go s.execute(ctx, run, target)
	return cloneInspectionRun(run), true
}

func (s *accountInspectionService) stop() bool {
	s.mu.Lock()
	if s.status != accountInspectionStatusRunning || s.cancel == nil {
		s.mu.Unlock()
		return false
	}
	s.cancel()
	s.addLogLocked("info", "account inspection stop requested")
	s.mu.Unlock()
	s.persist()
	return true
}

func (s *accountInspectionService) scheduleLoop(ctx context.Context, intervalSeconds int) {
	ticker := time.NewTicker(time.Duration(intervalSeconds) * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.runAsync("schedule", "")
		}
	}
}

func (s *accountInspectionService) timeout() time.Duration {
	s.mu.Lock()
	seconds := s.schedule.TimeoutSeconds
	s.mu.Unlock()
	if seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

func (s *accountInspectionService) maxConcurrency() int {
	s.mu.Lock()
	limit := s.schedule.MaxConcurrency
	s.mu.Unlock()
	if limit < 1 {
		return 1
	}
	return limit
}

func (s *accountInspectionService) execute(ctx context.Context, run *accountInspectionRun, target string) {
	started := run.StartedAt
	if timeout := s.timeout(); timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	results := s.inspectAccounts(ctx, target)
	ended := time.Now().UTC()
	status := accountInspectionStatusCompleted
	if ctx.Err() != nil {
		status = accountInspectionStatusStopped
	}

	runCopy := *run
	runCopy.Status = status
	runCopy.EndedAt = &ended
	runCopy.Duration = ended.Sub(started).Milliseconds()
	runCopy.Checked = len(results)
	runCopy.Summary = summarizeInspectionResults(results)

	s.mu.Lock()
	if target == "" {
		s.results = results
	} else {
		s.results = mergeInspectionResults(s.results, results)
	}
	s.currentRun = nil
	s.lastRun = &runCopy
	s.runs = append(s.runs, runCopy)
	if retention := s.schedule.RetentionRuns; retention > 0 && len(s.runs) > retention {
		s.runs = append([]accountInspectionRun(nil), s.runs[len(s.runs)-retention:]...)
	}
	s.status = status
	s.cancel = nil
	s.updatedAt = ended
	s.addLogLocked("info", fmt.Sprintf("account inspection %s checked=%d", status, len(results)))
	s.mu.Unlock()
	s.persist()
}

func (s *accountInspectionService) inspectAccounts(ctx context.Context, target string) []accountInspectionResult {
	if s.handler == nil || s.handler.authManager == nil {
		return []accountInspectionResult{}
	}
	auths := s.handler.authManager.List()
	quotaStatus := quota.StatusResponse{Credentials: map[string]*quota.Entry{}}
	probeTargets := make(map[string]quota.RefreshTarget)
	if s.handler.quotaScheduler != nil {
		quotaStatus = s.handler.quotaScheduler.GetStatus()
		targets, _ := s.handler.quotaScheduler.ResolveRefreshTargets(nil)
		for _, probeTarget := range targets {
			probeTargets[inspectionProbeBindingKey(probeTarget.FileName, probeTarget.AuthIndex)] = probeTarget
		}
	}
	providerSet := s.providerSet()
	candidates := make([]accountInspectionCandidate, 0, len(auths))
	for _, auth := range auths {
		if ctx.Err() != nil {
			break
		}
		if auth == nil {
			continue
		}
		auth.EnsureIndex()
		if target != "" && !inspectionAuthMatches(auth, target) {
			continue
		}
		provider := strings.ToLower(strings.TrimSpace(auth.Provider))
		if len(providerSet) > 0 && !providerSet[provider] {
			continue
		}
		probeTarget, hasProbeTarget := probeTargets[inspectionProbeBindingKey(auth.FileName, auth.Index)]
		entry := quotaStatus.Credentials[probeTarget.EntryKey]
		if entry == nil {
			entry = quotaStatus.Credentials[quota.EntryKey(quota.CredentialType(provider), auth.FileName)]
		}
		if entry == nil {
			entry = quotaStatus.Credentials[auth.FileName]
		}
		if target == "" && inspectionAuthRequiresExplicitProbe(auth, entry) {
			continue
		}
		candidates = append(
			candidates, accountInspectionCandidate{
				auth:           auth,
				quotaEntry:     entry,
				probeTarget:    probeTarget,
				hasProbeTarget: hasProbeTarget,
			},
		)
	}
	if len(candidates) == 0 {
		return []accountInspectionResult{}
	}
	results := make([]accountInspectionResult, 0, len(candidates))
	maxConcurrency := min(s.maxConcurrency(), len(candidates))
	sem := make(chan struct{}, maxConcurrency)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, item := range candidates {
		if ctx.Err() != nil {
			break
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(item accountInspectionCandidate) {
			defer wg.Done()
			defer func() { <-sem }()
			if ctx.Err() != nil {
				return
			}
			result := s.inspectAccount(ctx, item)
			s.addLog("info", "checked "+result.Account+" result="+result.Status+" reason="+result.Reason)
			mu.Lock()
			results = append(results, result)
			mu.Unlock()
		}(item)
	}
	wg.Wait()
	sort.Slice(results, func(i, j int) bool { return results[i].Account < results[j].Account })
	return results
}

func (s *accountInspectionService) inspectAccount(
	ctx context.Context,
	candidate accountInspectionCandidate,
) accountInspectionResult {
	result := buildInspectionResult(candidate.auth, candidate.quotaEntry)
	result.ProbeRevision = accountInspectionProbeRevision
	result.AuthGeneration = candidate.auth.Generation
	result.CredentialDigest = inspectionCredentialDigest(candidate.auth)
	if !candidate.hasProbeTarget || s.handler.quotaScheduler == nil {
		applyInspectionProbeInconclusive(&result, "provider-safe probe is not available")
		return result
	}

	probe := s.handler.quotaScheduler.ProbeTarget(ctx, candidate.probeTarget)
	if probe.Outcome == quota.ProbeOutcomeOutdated ||
		!inspectionAuthBindingCurrent(s.handler.authManager, candidate.auth, result.CredentialDigest) {
		applyInspectionProbeOutdated(&result)
		return result
	}
	switch probe.Outcome {
	case quota.ProbeOutcomePassed:
		result.ProbeStatus = accountProbePassed
	case quota.ProbeOutcomeFailed:
		if inspectionProbeAuthRejected(probe.Error) {
			applyInspectionProbeFailed(&result, probe.Error)
		} else {
			applyInspectionProbeInconclusive(&result, probe.Error)
		}
	case quota.ProbeOutcomeInconclusive:
		applyInspectionProbeInconclusive(&result, probe.Error)
	default:
		applyInspectionProbeInconclusive(&result, "provider-safe probe returned an unknown outcome")
	}
	return result
}

func (s *accountInspectionService) providerSet() map[string]bool {
	s.mu.Lock()
	providers := append([]string(nil), s.schedule.Providers...)
	s.mu.Unlock()
	if len(providers) == 0 {
		return nil
	}
	out := make(map[string]bool, len(providers))
	for _, provider := range providers {
		out[strings.ToLower(strings.TrimSpace(provider))] = true
	}
	return out
}

func (s *accountInspectionService) resultsSnapshot(
	statusFilter string,
	issuesOnly bool,
	includeDisabled bool,
	page int,
	pageSize int,
) ([]accountInspectionResult, int, int, int) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 50
	}
	if pageSize > 500 {
		pageSize = 500
	}
	statusFilter = strings.ToLower(strings.TrimSpace(statusFilter))

	s.mu.Lock()
	defer s.mu.Unlock()
	filtered := make([]accountInspectionResult, 0, len(s.results))
	for _, result := range s.results {
		if statusFilter != "" && result.Status != statusFilter && result.Reason != statusFilter {
			continue
		}
		if !includeDisabled && statusFilter == "" && result.Status == accountHealthDisabled {
			continue
		}
		if issuesOnly && result.Status == accountHealthNormal {
			continue
		}
		filtered = append(filtered, result)
	}
	total := len(filtered)
	start := (page - 1) * pageSize
	if start >= total {
		return []accountInspectionResult{}, total, page, pageSize
	}
	end := min(start+pageSize, total)
	return append([]accountInspectionResult(nil), filtered[start:end]...), total, page, pageSize
}

func (s *accountInspectionService) logsSnapshot() []accountInspectionLogEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]accountInspectionLogEntry(nil), s.logs...)
}

func (s *accountInspectionService) addLog(level string, message string) {
	s.mu.Lock()
	s.addLogLocked(level, message)
	s.mu.Unlock()
	s.persist()
}

func (s *accountInspectionService) addLogLocked(level string, message string) {
	message = sanitizeInspectionText(message)
	s.logs = append(s.logs, accountInspectionLogEntry{Time: time.Now().UTC(), Level: level, Message: message})
	if len(s.logs) > 500 {
		s.logs = append([]accountInspectionLogEntry(nil), s.logs[len(s.logs)-500:]...)
	}
}

func (s *accountInspectionService) refreshSummaryLocked() accountInspectionRefreshSummary {
	if s.handler == nil || s.handler.quotaScheduler == nil {
		return accountInspectionRefreshSummary{}
	}
	status := s.handler.quotaScheduler.GetStatus()
	summary := accountInspectionRefreshSummary{}
	if s.handler.quotaRefreshTasks != nil {
		summary.LatestTask = s.handler.quotaRefreshTasks.latestSnapshot()
		if summary.LatestTask != nil {
			summary.Pending = summary.LatestTask.Summary.Queued + summary.LatestTask.Summary.Running
			summary.Skipped = summary.LatestTask.Summary.Skipped
			summary.Failed = summary.LatestTask.Summary.Failed +
				summary.LatestTask.Summary.Inconclusive + summary.LatestTask.Summary.Interrupted
		}
	}
	if status.Enabled {
		for _, entry := range status.Credentials {
			if entry == nil || entry.NextRefresh == nil {
				continue
			}
			if summary.NextRefresh == nil || entry.NextRefresh.Before(*summary.NextRefresh) {
				next := *entry.NextRefresh
				summary.NextRefresh = &next
			}
		}
	}
	return summary
}

func inspectionAuthDisabled(auth *coreauth.Auth) bool {
	return auth != nil && (auth.Disabled || auth.Status == coreauth.StatusDisabled)
}

func inspectionAuthRequiresExplicitProbe(auth *coreauth.Auth, quotaEntry *quota.Entry) bool {
	if inspectionAuthDisabled(auth) || auth != nil && auth.Quota.Exceeded {
		return true
	}
	return quotaEntry != nil &&
		(quotaEntry.Status == quota.StatusBanned || quotaEntry.Status == quota.StatusQuotaExceeded)
}

func inspectionProbeBindingKey(fileName string, authIndex string) string {
	return strings.TrimSpace(fileName) + "\x00" + strings.TrimSpace(authIndex)
}

func inspectionCredentialDigest(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if digest := strings.TrimSpace(auth.Attributes[coreauth.PersistedSHA256Attribute]); digest != "" {
		return strings.ToLower(digest)
	}
	version, err := coreauth.PersistedVersionForAuth(auth.FileName, auth)
	if err != nil {
		return ""
	}
	return hex.EncodeToString(version.SHA256[:])
}

func inspectionAuthBindingCurrent(
	manager *coreauth.Manager,
	expected *coreauth.Auth,
	expectedDigest string,
) bool {
	if manager == nil || expected == nil {
		return false
	}
	current, ok := manager.GetByID(expected.ID)
	if !ok || current == nil {
		return false
	}
	current.EnsureIndex()
	return current.Generation == expected.Generation &&
		current.Index == expected.Index &&
		strings.TrimSpace(current.FileName) == strings.TrimSpace(expected.FileName) &&
		strings.EqualFold(strings.TrimSpace(current.Provider), strings.TrimSpace(expected.Provider)) &&
		strings.EqualFold(inspectionCredentialDigest(current), expectedDigest)
}

func inspectionProbeAuthRejected(message string) bool {
	message = strings.ToLower(strings.TrimSpace(message))
	return strings.Contains(message, "status 401") || strings.Contains(message, "status 403")
}

func applyInspectionProbeFailed(result *accountInspectionResult, message string) {
	if result == nil {
		return
	}
	result.ProbeStatus = accountProbeFailed
	result.ProbeError = sanitizeInspectionText(message)
	result.Status = accountHealthUnavailable
	result.Reason = "probe_failed"
	result.Advice = "check_login_or_provider_status"
	result.AutoHandling = "manual_inspection_available"
}

func applyInspectionProbeInconclusive(result *accountInspectionResult, message string) {
	if result == nil {
		return
	}
	result.ProbeStatus = accountProbeInconclusive
	result.ProbeError = sanitizeInspectionText(message)
	if result.Status == accountHealthNormal {
		result.Status = accountHealthUnknown
		result.Reason = "probe_inconclusive"
		result.Advice = "wait_or_check_login"
		result.AutoHandling = "manual_inspection_available"
	}
}

func applyInspectionProbeOutdated(result *accountInspectionResult) {
	if result == nil {
		return
	}
	result.ProbeStatus = accountProbeOutdated
	result.ProbeError = "credential binding changed while the probe was running"
	result.Status = accountHealthUnknown
	result.Reason = "probe_outdated"
	result.Advice = "none"
	result.AutoHandling = "none"
}

func buildInspectionResult(auth *coreauth.Auth, quotaEntry *quota.Entry) accountInspectionResult {
	checkedAt := time.Now().UTC()
	result := accountInspectionResult{
		ID:           strings.TrimSpace(auth.ID),
		Account:      inspectionAccountName(auth),
		FileName:     strings.TrimSpace(auth.FileName),
		Provider:     strings.ToLower(strings.TrimSpace(auth.Provider)),
		AuthIndex:    strings.TrimSpace(auth.Index),
		Status:       accountHealthNormal,
		Reason:       "ok",
		Advice:       "none",
		AutoHandling: "auto_refresh_available",
		CheckedAt:    checkedAt,
	}
	if auth.Disabled || auth.Status == coreauth.StatusDisabled {
		result.Status = accountHealthDisabled
		result.Reason = "disabled"
		result.Advice = "enable_when_needed"
		result.AutoHandling = "disabled_skip_auto_refresh"
		return result
	}
	if quotaEntry != nil {
		switch quotaEntry.Status {
		case quota.StatusQuotaExceeded:
			result.Status = accountHealthUnavailable
			result.Reason = "quota_exceeded"
			result.Advice = "wait_quota_recovery"
			result.AutoHandling = "quota_exceeded_skip_auto_refresh"
			return result
		case quota.StatusBanned:
			result.Status = accountHealthUnavailable
			result.Reason = "account_banned"
			result.Advice = "check_provider_account"
			result.AutoHandling = "banned_skip_auto_refresh"
			return result
		case quota.StatusError:
			result.Status = accountHealthUnavailable
			result.Reason = "refresh_failed"
			result.Advice = "manual_refresh_token"
			result.AutoHandling = "auto_refresh_will_retry"
			return result
		}
	}
	if tokenExpired(auth.Metadata) {
		result.Status = accountHealthUnavailable
		result.Reason = "token_expired"
		result.Advice = "manual_refresh_token"
		result.AutoHandling = "refresh_token_available_if_supported"
		return result
	}
	if auth.Quota.Exceeded {
		result.Status = accountHealthUnavailable
		result.Reason = "quota_exceeded"
		result.Advice = "wait_quota_recovery"
		result.AutoHandling = "quota_exceeded_skip_auto_refresh"
		return result
	}
	if auth.LastError != nil || auth.Status == coreauth.StatusError || auth.Unavailable {
		result.Status = accountHealthUnavailable
		result.Reason = "provider_error"
		result.Advice = "check_login_or_provider_status"
		result.AutoHandling = "retry_depends_on_provider"
		return result
	}
	if auth.Status == coreauth.StatusUnknown || auth.Status == coreauth.StatusPending || auth.Status == coreauth.StatusRefreshing {
		result.Status = accountHealthUnknown
		result.Reason = string(auth.Status)
		result.Advice = "wait_or_check_login"
		result.AutoHandling = "unknown"
	}
	return result
}

func mergeInspectionResults(
	existing []accountInspectionResult,
	updates []accountInspectionResult,
) []accountInspectionResult {
	if len(updates) == 0 {
		return append([]accountInspectionResult(nil), existing...)
	}
	merged := append([]accountInspectionResult(nil), existing...)
	indexByKey := make(map[string]int, len(merged))
	for i, result := range merged {
		indexByKey[inspectionResultKey(result)] = i
	}
	for _, update := range updates {
		key := inspectionResultKey(update)
		if idx, ok := indexByKey[key]; ok {
			merged[idx] = update
			continue
		}
		indexByKey[key] = len(merged)
		merged = append(merged, update)
	}
	sort.Slice(merged, func(i, j int) bool { return merged[i].Account < merged[j].Account })
	return merged
}

func inspectionResultKey(result accountInspectionResult) string {
	return strings.Join([]string{result.ID, result.FileName, result.AuthIndex}, "\x00")
}

func summarizeInspectionResults(results []accountInspectionResult) accountInspectionSummary {
	summary := accountInspectionSummary{Total: len(results)}
	for _, result := range results {
		switch result.Status {
		case accountHealthNormal:
			summary.Normal++
		case accountHealthDisabled:
		default:
			summary.Abnormal++
		}
		switch result.Reason {
		case "token_expired":
			summary.TokenInvalid++
		case "refresh_failed", "provider_error", "probe_failed":
			summary.RefreshFailed++
		case "disabled":
			summary.Disabled++
		}
	}
	return summary
}

func summarizeInspectionResultsByProvider(
	results []accountInspectionResult,
) map[string]accountInspectionProviderSummary {
	out := make(map[string]accountInspectionProviderSummary)
	reasonsByProvider := make(map[string]map[string]int)
	for _, result := range results {
		provider := strings.ToLower(strings.TrimSpace(result.Provider))
		if provider == "" {
			provider = "unknown"
		}
		summary := out[provider]
		summary.Provider = provider
		summary.Total++
		switch result.Status {
		case accountHealthNormal:
			summary.Normal++
		case accountHealthDisabled:
			summary.Disabled++
		default:
			summary.Abnormal++
		}
		switch result.Status {
		case accountHealthUnavailable:
			summary.Unavailable++
		case accountHealthUnknown:
			summary.Unknown++
		}
		switch result.Reason {
		case "token_expired":
			summary.TokenInvalid++
		case "refresh_failed", "provider_error", "probe_failed":
			summary.RefreshFailed++
		}
		if summary.LastCheckedAt == nil || result.CheckedAt.After(*summary.LastCheckedAt) {
			checkedAt := result.CheckedAt
			summary.LastCheckedAt = &checkedAt
		}
		out[provider] = summary
		reason := strings.TrimSpace(result.Reason)
		if reason == "" {
			continue
		}
		if reasonsByProvider[provider] == nil {
			reasonsByProvider[provider] = make(map[string]int)
		}
		reasonsByProvider[provider][reason]++
	}
	for provider, counts := range reasonsByProvider {
		reasons := make([]accountInspectionReasonCount, 0, len(counts))
		for reason, count := range counts {
			reasons = append(reasons, accountInspectionReasonCount{Reason: reason, Count: count})
		}
		sort.Slice(
			reasons, func(i, j int) bool {
				if reasons[i].Count == reasons[j].Count {
					return reasons[i].Reason < reasons[j].Reason
				}
				return reasons[i].Count > reasons[j].Count
			},
		)
		if len(reasons) > 3 {
			reasons = reasons[:3]
		}
		summary := out[provider]
		summary.TopReasons = reasons
		out[provider] = summary
	}
	return out
}

func (h *Handler) findInspectionAuth(target string) *coreauth.Auth {
	if h == nil || h.authManager == nil {
		return nil
	}
	target = strings.TrimSpace(target)
	for _, auth := range h.authManager.List() {
		if auth != nil && inspectionAuthMatches(auth, target) {
			return auth
		}
	}
	return nil
}

func inspectionAuthMatches(auth *coreauth.Auth, target string) bool {
	target = strings.TrimSpace(target)
	if target == "" || auth == nil {
		return false
	}
	auth.EnsureIndex()
	return target == auth.ID || target == auth.FileName || target == auth.Index
}

func inspectionAccountName(auth *coreauth.Auth) string {
	if auth == nil {
		return "unknown"
	}
	for _, value := range []string{auth.Label, auth.FileName, auth.ID, auth.Index} {
		value = strings.TrimSpace(value)
		if value != "" {
			return sanitizeInspectionText(value)
		}
	}
	return "unknown"
}

func cloneInspectionSchedule(src accountInspectionSchedule) accountInspectionSchedule {
	src.Providers = append([]string(nil), src.Providers...)
	return src
}

func normalizePersistedInspectionSchedule(src accountInspectionSchedule) accountInspectionSchedule {
	defaults := defaultAccountInspectionSchedule()
	if src.IntervalSeconds < 60 {
		src.IntervalSeconds = defaults.IntervalSeconds
	}
	if src.MaxConcurrency < 1 {
		src.MaxConcurrency = defaults.MaxConcurrency
	}
	if src.TimeoutSeconds < 1 {
		src.TimeoutSeconds = defaults.TimeoutSeconds
	}
	if src.RetentionRuns < 1 {
		src.RetentionRuns = defaults.RetentionRuns
	}
	src.Providers = normalizeInspectionProviders(src.Providers)
	return src
}

func cloneInspectionRun(src *accountInspectionRun) *accountInspectionRun {
	if src == nil {
		return nil
	}
	cp := *src
	if src.EndedAt != nil {
		ended := *src.EndedAt
		cp.EndedAt = &ended
	}
	return &cp
}

func normalizeInspectionProviders(providers []string) []string {
	seen := make(map[string]bool, len(providers))
	out := make([]string, 0, len(providers))
	for _, provider := range providers {
		provider = strings.ToLower(strings.TrimSpace(provider))
		if provider == "" || seen[provider] {
			continue
		}
		seen[provider] = true
		out = append(out, provider)
	}
	return out
}

func sanitizeInspectionText(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	value = accountInspectionSecretPattern.ReplaceAllString(value, "$1$2<redacted>")
	value = accountInspectionLongKeyPattern.ReplaceAllStringFunc(value, util.HideAPIKey)
	return value
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			return value
		}
	}
	return ""
}
