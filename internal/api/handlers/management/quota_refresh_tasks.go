package management

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/quota"
	"github.com/gin-gonic/gin"
)

const (
	quotaRefreshTaskStatusQueued       = "queued"
	quotaRefreshTaskStatusRunning      = "running"
	quotaRefreshTaskStatusPaused       = "paused"
	quotaRefreshTaskStatusSucceeded    = "succeeded"
	quotaRefreshTaskStatusFailed       = "failed"
	quotaRefreshTaskStatusCancelled    = "cancelled"
	quotaRefreshTaskStatusInconclusive = "inconclusive"
	quotaRefreshTaskStatusInterrupted  = "interrupted"

	quotaRefreshTargetStatusQueued       = "queued"
	quotaRefreshTargetStatusRunning      = "running"
	quotaRefreshTargetStatusSucceeded    = "succeeded"
	quotaRefreshTargetStatusFailed       = "failed"
	quotaRefreshTargetStatusSkipped      = "skipped"
	quotaRefreshTargetStatusCancelled    = "cancelled"
	quotaRefreshTargetStatusInconclusive = "inconclusive"
	quotaRefreshTargetStatusInterrupted  = "interrupted"

	quotaRefreshTaskHistoryLimit        = 100
	quotaRefreshTaskGlobalConcurrency   = 4
	quotaRefreshTaskProviderConcurrency = 2
	maxQuotaRefreshTaskStateBytes       = 16 << 20
)

var (
	errQuotaRefreshTaskNotFound               = errors.New("quota refresh task not found")
	errQuotaRefreshTaskConflict               = errors.New("quota refresh task state does not allow this operation")
	errQuotaRefreshTaskCapacity               = errors.New("too many active quota refresh tasks")
	errQuotaRefreshTaskPersistenceUnavailable = errors.New("quota refresh task persistence is unavailable")
	errQuotaRefreshTaskServiceClosed          = errors.New("quota refresh task service is closed")
	errQuotaRefreshTaskCancelled              = errors.New("quota refresh task cancelled")
	errQuotaRefreshTaskInterrupted            = errors.New("quota refresh task interrupted by server shutdown")
	errQuotaRefreshTaskPersistence            = errors.New("quota refresh task state persistence failed")
)

type quotaRefreshTaskSummary struct {
	Total        int `json:"total"`
	Queued       int `json:"queued"`
	Running      int `json:"running"`
	Succeeded    int `json:"succeeded"`
	Failed       int `json:"failed"`
	Skipped      int `json:"skipped"`
	Cancelled    int `json:"cancelled"`
	Inconclusive int `json:"inconclusive"`
	Interrupted  int `json:"interrupted"`
}

type quotaRefreshTaskTarget struct {
	ID              string     `json:"id"`
	Identifier      string     `json:"identifier,omitempty"`
	EntryKey        string     `json:"entry_key,omitempty"`
	FileName        string     `json:"file_name,omitempty"`
	Provider        string     `json:"provider,omitempty"`
	Status          string     `json:"status"`
	Error           string     `json:"error,omitempty"`
	StartedAt       *time.Time `json:"started_at,omitempty"`
	CompletedAt     *time.Time `json:"completed_at,omitempty"`
	SharedExecution bool       `json:"shared_execution,omitempty"`
}

type quotaRefreshTask struct {
	ID                string                   `json:"id"`
	Status            string                   `json:"status"`
	CreatedAt         time.Time                `json:"created_at"`
	StartedAt         *time.Time               `json:"started_at,omitempty"`
	CompletedAt       *time.Time               `json:"completed_at,omitempty"`
	CancelRequestedAt *time.Time               `json:"cancel_requested_at,omitempty"`
	Summary           quotaRefreshTaskSummary  `json:"summary"`
	Targets           []quotaRefreshTaskTarget `json:"targets"`
	Error             string                   `json:"error,omitempty"`
	PersistenceError  string                   `json:"persistence_error,omitempty"`
}

type quotaRefreshTasksPersistedState struct {
	Tasks     []quotaRefreshTask `json:"tasks"`
	UpdatedAt time.Time          `json:"updated_at"`
}

type quotaRefreshTaskRuntime struct {
	ctx      context.Context
	cancel   context.CancelCauseFunc
	targets  map[string]quota.RefreshTarget
	resumeCh chan struct{}
}

type quotaRefreshTaskService struct {
	handler          *Handler
	mu               sync.Mutex
	persistMu        sync.Mutex
	tasks            map[string]*quotaRefreshTask
	order            []string
	runtimes         map[string]*quotaRefreshTaskRuntime
	globalSemaphore        chan struct{}
	providerSemaphores     map[string]chan struct{}
	globalLimit      int
	providerLimit    int
	closed           bool
	loadError        error
	persistenceError string
	wg               sync.WaitGroup
}

func newQuotaRefreshTaskService(handler *Handler) *quotaRefreshTaskService {
	service := &quotaRefreshTaskService{
		handler:       handler,
		tasks:         make(map[string]*quotaRefreshTask),
		runtimes:      make(map[string]*quotaRefreshTaskRuntime),
		globalLimit:   quotaRefreshTaskGlobalConcurrency,
		providerLimit: quotaRefreshTaskProviderConcurrency,
		providerSemaphores:  make(map[string]chan struct{}),
	}
	service.globalSemaphore = make(chan struct{}, service.globalLimit)
	service.loadPersistedState()
	return service
}

func (h *Handler) quotaRefreshService() *quotaRefreshTaskService {
	if h.quotaRefreshTasks == nil {
		h.quotaRefreshTasks = newQuotaRefreshTaskService(h)
	}
	return h.quotaRefreshTasks
}

func (s *quotaRefreshTaskService) stateStore() *managementStateStore {
	if s == nil || s.handler == nil {
		return nil
	}
	return s.handler.stateStore
}

func (s *quotaRefreshTaskService) scheduler() *quota.Scheduler {
	if s == nil || s.handler == nil {
		return nil
	}
	return s.handler.quotaScheduler
}

func (s *quotaRefreshTaskService) loadPersistedState() {
	store := s.stateStore()
	if store == nil {
		s.loadError = errQuotaRefreshTaskPersistenceUnavailable
		s.persistenceError = errQuotaRefreshTaskPersistenceUnavailable.Error()
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	state, err := store.loadQuotaRefreshTasks(ctx)
	if err != nil {
		s.loadError = err
		s.persistenceError = err.Error()
		return
	}
	if state == nil {
		return
	}
	now := time.Now().UTC()
	changed := false
	for index := range state.Tasks {
		task := cloneQuotaRefreshTask(&state.Tasks[index])
		if task == nil || strings.TrimSpace(task.ID) == "" {
			continue
		}
		if _, exists := s.tasks[task.ID]; exists {
			continue
		}
		if !quotaRefreshTaskTerminal(task.Status) {
			changed = true
			interruptPersistedQuotaRefreshTask(task, now)
		}
		s.tasks[task.ID] = task
		s.order = append(s.order, task.ID)
	}
	s.trimHistoryLocked()
	if changed {
		if err = s.persistState(); err != nil {
			s.persistenceError = err.Error()
		}
	}
}

func interruptPersistedQuotaRefreshTask(task *quotaRefreshTask, now time.Time) {
	if task == nil {
		return
	}
	cancelRequested := task.CancelRequestedAt != nil
	for index := range task.Targets {
		target := &task.Targets[index]
		if quotaRefreshTargetTerminal(target.Status) {
			continue
		}
		if cancelRequested {
			target.Status = quotaRefreshTargetStatusCancelled
			target.Error = errQuotaRefreshTaskCancelled.Error()
		} else {
			target.Status = quotaRefreshTargetStatusInterrupted
			target.Error = "quota refresh target interrupted by server restart"
		}
		target.CompletedAt = cloneQuotaRefreshTaskTime(&now)
	}
	task.Summary = summarizeQuotaRefreshTaskTargets(task.Targets)
	task.CompletedAt = cloneQuotaRefreshTaskTime(&now)
	if cancelRequested {
		task.Status = quotaRefreshTaskStatusCancelled
		task.Error = errQuotaRefreshTaskCancelled.Error()
	} else {
		task.Status = quotaRefreshTaskStatusInterrupted
		task.Error = "quota refresh task interrupted by server restart"
	}
}

func (s *quotaRefreshTaskService) createTask(identifiers []string) (*quotaRefreshTask, error) {
	scheduler := s.scheduler()
	if scheduler == nil {
		return nil, fmt.Errorf("quota scheduler not configured")
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, errQuotaRefreshTaskServiceClosed
	}
	if s.loadError != nil {
		err := s.loadError
		s.mu.Unlock()
		return nil, fmt.Errorf("%w: %v", errQuotaRefreshTaskPersistenceUnavailable, err)
	}
	s.mu.Unlock()

	targets, unresolved := scheduler.ResolveRefreshTargets(identifiers)
	explicit := len(identifiers) > 0
	taskTargets := make([]quotaRefreshTaskTarget, 0, len(targets)+len(unresolved))
	runtimeTargets := make(map[string]quota.RefreshTarget, len(targets))
	now := time.Now().UTC()
	for _, target := range targets {
		taskTarget := quotaRefreshTaskTarget{
			ID:         target.EntryKey,
			Identifier: target.EntryKey,
			EntryKey:   target.EntryKey,
			FileName:   target.FileName,
			Provider:   string(target.Type),
			Status:     quotaRefreshTargetStatusQueued,
		}
		if !explicit && quotaRefreshTargetSkippedByBulk(target) {
			taskTarget.Status = quotaRefreshTargetStatusSkipped
			taskTarget.Error = "credential requires explicit refresh"
			taskTarget.CompletedAt = cloneQuotaRefreshTaskTime(&now)
		} else {
			runtimeTargets[target.EntryKey] = target
		}
		taskTargets = append(taskTargets, taskTarget)
	}
	sort.Strings(unresolved)
	seenUnresolved := make(map[string]struct{}, len(unresolved))
	for _, identifier := range unresolved {
		if _, exists := seenUnresolved[identifier]; exists {
			continue
		}
		seenUnresolved[identifier] = struct{}{}
		taskTargets = append(
			taskTargets, quotaRefreshTaskTarget{
				ID:          "unresolved::" + identifier,
				Identifier:  identifier,
				Status:      quotaRefreshTargetStatusSkipped,
				Error:       "credential not found",
				CompletedAt: cloneQuotaRefreshTaskTime(&now),
			},
		)
	}

	taskID, err := generateQuotaRefreshTaskID()
	if err != nil {
		return nil, err
	}
	task := &quotaRefreshTask{
		ID:        taskID,
		Status:    quotaRefreshTaskStatusQueued,
		CreatedAt: now,
		Targets:   taskTargets,
	}
	task.Summary = summarizeQuotaRefreshTaskTargets(task.Targets)
	if task.Summary.Queued == 0 {
		task.Status = quotaRefreshTaskStatusInconclusive
		task.Error = "no quota refresh target was started"
		task.CompletedAt = cloneQuotaRefreshTaskTime(&now)
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, errQuotaRefreshTaskServiceClosed
	}
	if err = s.makeTaskCapacityLocked(); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	runtime := &quotaRefreshTaskRuntime{ctx: ctx, cancel: cancel, targets: runtimeTargets}
	s.tasks[task.ID] = task
	s.order = append(s.order, task.ID)
	if task.Status == quotaRefreshTaskStatusQueued {
		s.runtimes[task.ID] = runtime
	}
	s.mu.Unlock()

	if err = s.persistState(); err != nil {
		s.mu.Lock()
		delete(s.tasks, task.ID)
		delete(s.runtimes, task.ID)
		s.removeTaskOrderLocked(task.ID)
		s.mu.Unlock()
		cancel(errQuotaRefreshTaskPersistence)
		return nil, err
	}
	if task.Status == quotaRefreshTaskStatusQueued {
		s.wg.Add(1)
		go s.runTask(task.ID)
	} else {
		cancel(context.Canceled)
	}
	return s.taskSnapshot(task.ID)
}

func quotaRefreshTargetSkippedByBulk(target quota.RefreshTarget) bool {
	return target.Disabled || target.Status == quota.StatusBanned || target.Status == quota.StatusQuotaExceeded
}

func generateQuotaRefreshTaskID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate quota refresh task id: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}

func (s *quotaRefreshTaskService) makeTaskCapacityLocked() error {
	for len(s.order) >= quotaRefreshTaskHistoryLimit {
		removed := false
		for _, taskID := range s.order {
			task := s.tasks[taskID]
			if task == nil || quotaRefreshTaskTerminal(task.Status) {
				delete(s.tasks, taskID)
				s.removeTaskOrderLocked(taskID)
				removed = true
				break
			}
		}
		if !removed {
			return errQuotaRefreshTaskCapacity
		}
	}
	return nil
}

func (s *quotaRefreshTaskService) trimHistoryLocked() {
	for len(s.order) > quotaRefreshTaskHistoryLimit {
		taskID := s.order[0]
		delete(s.tasks, taskID)
		s.order = s.order[1:]
	}
}

func (s *quotaRefreshTaskService) removeTaskOrderLocked(taskID string) {
	for index, current := range s.order {
		if current != taskID {
			continue
		}
		s.order = append(s.order[:index], s.order[index+1:]...)
		return
	}
}

func (s *quotaRefreshTaskService) runTask(taskID string) {
	defer s.wg.Done()

	s.mu.Lock()
	task := s.tasks[taskID]
	runtime := s.runtimes[taskID]
	if task == nil || runtime == nil {
		s.mu.Unlock()
		return
	}
	now := time.Now().UTC()
	task.StartedAt = cloneQuotaRefreshTaskTime(&now)
	if task.Status == quotaRefreshTaskStatusQueued {
		task.Status = quotaRefreshTaskStatusRunning
	}
	targetIDs := make([]string, 0, len(runtime.targets))
	for _, target := range task.Targets {
		if target.Status == quotaRefreshTargetStatusQueued {
			targetIDs = append(targetIDs, target.ID)
		}
	}
	s.mu.Unlock()
	if !s.persistOrFailTask(taskID) {
		s.finishTask(taskID)
		return
	}

	var workers sync.WaitGroup
	for _, targetID := range targetIDs {
		workers.Add(1)
		go func(targetID string) {
			defer workers.Done()
			s.runTarget(taskID, targetID)
		}(targetID)
	}
	workers.Wait()
	s.finishTask(taskID)
}

func (s *quotaRefreshTaskService) runTarget(taskID string, targetID string) {
	for {
		if !s.waitUntilTaskRunnable(taskID, targetID) {
			s.finishTargetFromRuntimeCause(taskID, targetID)
			return
		}
		runtime, target, ok := s.runtimeTarget(taskID, targetID)
		if !ok {
			return
		}
		providerSem := s.providerSemaphore(string(target.Type))
		if !acquireQuotaRefreshSemaphore(runtime.ctx, providerSem) {
			s.finishTargetFromRuntimeCause(taskID, targetID)
			return
		}
		if !acquireQuotaRefreshSemaphore(runtime.ctx, s.globalSemaphore) {
			<-providerSem
			s.finishTargetFromRuntimeCause(taskID, targetID)
			return
		}
		if !s.markTargetRunning(taskID, targetID) {
			<-s.globalSemaphore
			<-providerSem
			continue
		}
		if !s.persistOrFailTask(taskID) {
			<-s.globalSemaphore
			<-providerSem
			return
		}

		result := s.scheduler().RefreshTarget(runtime.ctx, target)
		<-s.globalSemaphore
		<-providerSem
		s.finishTarget(taskID, targetID, result)
		if !s.persistOrFailTask(taskID) {
			return
		}
		return
	}
}

func acquireQuotaRefreshSemaphore(ctx context.Context, semaphore chan struct{}) bool {
	select {
	case semaphore <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

func (s *quotaRefreshTaskService) providerSemaphore(provider string) chan struct{} {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" {
		provider = "unknown"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	semaphore := s.providerSemaphores[provider]
	if semaphore == nil {
		semaphore = make(chan struct{}, s.providerLimit)
		s.providerSemaphores[provider] = semaphore
	}
	return semaphore
}

func (s *quotaRefreshTaskService) waitUntilTaskRunnable(taskID string, targetID string) bool {
	for {
		s.mu.Lock()
		task := s.tasks[taskID]
		runtime := s.runtimes[taskID]
		target := findQuotaRefreshTaskTarget(task, targetID)
		if task == nil || runtime == nil || target == nil || target.Status != quotaRefreshTargetStatusQueued ||
			quotaRefreshTaskTerminal(task.Status) || runtime.ctx.Err() != nil {
			s.mu.Unlock()
			return false
		}
		if task.Status != quotaRefreshTaskStatusPaused {
			s.mu.Unlock()
			return true
		}
		resumeCh := runtime.resumeCh
		s.mu.Unlock()
		select {
		case <-runtime.ctx.Done():
			return false
		case <-resumeCh:
		}
	}
}

func (s *quotaRefreshTaskService) runtimeTarget(
	taskID string,
	targetID string,
) (*quotaRefreshTaskRuntime, quota.RefreshTarget, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	runtime := s.runtimes[taskID]
	if runtime == nil {
		return nil, quota.RefreshTarget{}, false
	}
	target, ok := runtime.targets[targetID]
	return runtime, target, ok
}

func (s *quotaRefreshTaskService) markTargetRunning(taskID string, targetID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	task := s.tasks[taskID]
	runtime := s.runtimes[taskID]
	target := findQuotaRefreshTaskTarget(task, targetID)
	if task == nil || runtime == nil || target == nil || target.Status != quotaRefreshTargetStatusQueued ||
		quotaRefreshTaskTerminal(task.Status) || runtime.ctx.Err() != nil || task.Status == quotaRefreshTaskStatusPaused {
		return false
	}
	now := time.Now().UTC()
	target.Status = quotaRefreshTargetStatusRunning
	target.StartedAt = cloneQuotaRefreshTaskTime(&now)
	task.Summary = summarizeQuotaRefreshTaskTargets(task.Targets)
	return true
}

func (s *quotaRefreshTaskService) finishTarget(
	taskID string,
	targetID string,
	result quota.RefreshResult,
) {
	s.mu.Lock()
	defer s.mu.Unlock()
	task := s.tasks[taskID]
	target := findQuotaRefreshTaskTarget(task, targetID)
	if task == nil || target == nil || quotaRefreshTargetTerminal(target.Status) {
		return
	}
	completedAt := result.CompletedAt
	if completedAt.IsZero() {
		completedAt = time.Now().UTC()
	}
	target.CompletedAt = cloneQuotaRefreshTaskTime(&completedAt)
	target.SharedExecution = result.Shared
	target.Error = result.Error
	switch result.Outcome {
	case quota.RefreshOutcomeSucceeded:
		target.Status = quotaRefreshTargetStatusSucceeded
	case quota.RefreshOutcomeFailed:
		target.Status = quotaRefreshTargetStatusFailed
	case quota.RefreshOutcomeCancelled:
		target.Status = quotaRefreshTargetStatusCancelled
	case quota.RefreshOutcomeInconclusive:
		target.Status = quotaRefreshTargetStatusInconclusive
	default:
		target.Status = quotaRefreshTargetStatusInconclusive
		if target.Error == "" {
			target.Error = "quota refresh returned an unknown outcome"
		}
	}
	task.Summary = summarizeQuotaRefreshTaskTargets(task.Targets)
}

func (s *quotaRefreshTaskService) finishTargetFromRuntimeCause(taskID string, targetID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	task := s.tasks[taskID]
	runtime := s.runtimes[taskID]
	target := findQuotaRefreshTaskTarget(task, targetID)
	if task == nil || runtime == nil || target == nil || quotaRefreshTargetTerminal(target.Status) {
		return
	}
	cause := context.Cause(runtime.ctx)
	if cause == nil {
		return
	}
	now := time.Now().UTC()
	target.CompletedAt = cloneQuotaRefreshTaskTime(&now)
	target.Error = cause.Error()
	switch {
	case errors.Is(cause, errQuotaRefreshTaskInterrupted):
		target.Status = quotaRefreshTargetStatusInterrupted
	case errors.Is(cause, errQuotaRefreshTaskPersistence):
		target.Status = quotaRefreshTargetStatusInconclusive
	default:
		target.Status = quotaRefreshTargetStatusCancelled
	}
	task.Summary = summarizeQuotaRefreshTaskTargets(task.Targets)
}

func (s *quotaRefreshTaskService) finishTask(taskID string) {
	s.mu.Lock()
	task := s.tasks[taskID]
	runtime := s.runtimes[taskID]
	if task == nil || runtime == nil {
		s.mu.Unlock()
		return
	}
	cause := context.Cause(runtime.ctx)
	now := time.Now().UTC()
	task.Summary = summarizeQuotaRefreshTaskTargets(task.Targets)
	if task.PersistenceError != "" || errors.Is(cause, errQuotaRefreshTaskPersistence) {
		task.Status = quotaRefreshTaskStatusInconclusive
		if task.Error == "" {
			task.Error = errQuotaRefreshTaskPersistence.Error()
		}
	} else if cause != nil && errors.Is(cause, errQuotaRefreshTaskInterrupted) {
		task.Status = quotaRefreshTaskStatusInterrupted
		task.Error = cause.Error()
	} else if task.CancelRequestedAt != nil || errors.Is(cause, errQuotaRefreshTaskCancelled) {
		task.Status = quotaRefreshTaskStatusCancelled
		task.Error = errQuotaRefreshTaskCancelled.Error()
	} else {
		task.Status, task.Error = terminalQuotaRefreshTaskStatus(task.Summary)
	}
	task.CompletedAt = cloneQuotaRefreshTaskTime(&now)
	if runtime.resumeCh != nil {
		close(runtime.resumeCh)
		runtime.resumeCh = nil
	}
	delete(s.runtimes, taskID)
	s.mu.Unlock()
	runtime.cancel(context.Canceled)
	if err := s.persistState(); err != nil {
		s.failTaskPersistence(taskID, err)
	}
}

func terminalQuotaRefreshTaskStatus(summary quotaRefreshTaskSummary) (string, string) {
	switch {
	case summary.Interrupted > 0:
		return quotaRefreshTaskStatusInterrupted, "one or more quota refresh targets were interrupted"
	case summary.Failed > 0:
		return quotaRefreshTaskStatusFailed, "one or more quota refresh targets failed"
	case summary.Inconclusive > 0:
		return quotaRefreshTaskStatusInconclusive, "one or more quota refresh targets were inconclusive"
	case summary.Cancelled > 0:
		return quotaRefreshTaskStatusCancelled, "one or more quota refresh targets were cancelled"
	case summary.Succeeded > 0:
		return quotaRefreshTaskStatusSucceeded, ""
	default:
		return quotaRefreshTaskStatusInconclusive, "no quota refresh target succeeded"
	}
}

func (s *quotaRefreshTaskService) persistOrFailTask(taskID string) bool {
	if err := s.persistState(); err != nil {
		s.failTaskPersistence(taskID, err)
		return false
	}
	return true
}

func (s *quotaRefreshTaskService) failTaskPersistence(taskID string, persistenceErr error) {
	if persistenceErr == nil {
		return
	}
	s.mu.Lock()
	task := s.tasks[taskID]
	runtime := s.runtimes[taskID]
	if task == nil {
		s.mu.Unlock()
		return
	}
	now := time.Now().UTC()
	task.PersistenceError = persistenceErr.Error()
	task.Error = errQuotaRefreshTaskPersistence.Error()
	task.Status = quotaRefreshTaskStatusInconclusive
	task.CompletedAt = cloneQuotaRefreshTaskTime(&now)
	for index := range task.Targets {
		target := &task.Targets[index]
		if quotaRefreshTargetTerminal(target.Status) {
			continue
		}
		target.Status = quotaRefreshTargetStatusInconclusive
		target.Error = errQuotaRefreshTaskPersistence.Error()
		target.CompletedAt = cloneQuotaRefreshTaskTime(&now)
	}
	task.Summary = summarizeQuotaRefreshTaskTargets(task.Targets)
	if runtime != nil {
		runtime.cancel(errQuotaRefreshTaskPersistence)
		if runtime.resumeCh != nil {
			close(runtime.resumeCh)
			runtime.resumeCh = nil
		}
	}
	s.mu.Unlock()
	_ = s.persistState()
}

func (s *quotaRefreshTaskService) persistState() error {
	store := s.stateStore()
	if store == nil {
		return errQuotaRefreshTaskPersistenceUnavailable
	}
	s.persistMu.Lock()
	defer s.persistMu.Unlock()
	state := s.persistenceSnapshot()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := store.saveQuotaRefreshTasks(ctx, state)
	s.mu.Lock()
	if err != nil {
		s.persistenceError = err.Error()
	} else {
		s.persistenceError = ""
	}
	s.mu.Unlock()
	return err
}

func (s *quotaRefreshTaskService) persistenceSnapshot() quotaRefreshTasksPersistedState {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := quotaRefreshTasksPersistedState{
		Tasks:     make([]quotaRefreshTask, 0, len(s.order)),
		UpdatedAt: time.Now().UTC(),
	}
	for _, taskID := range s.order {
		task := cloneQuotaRefreshTask(s.tasks[taskID])
		if task != nil {
			state.Tasks = append(state.Tasks, *task)
		}
	}
	return state
}

func (s *quotaRefreshTaskService) taskSnapshot(taskID string) (*quotaRefreshTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	task := cloneQuotaRefreshTask(s.tasks[strings.TrimSpace(taskID)])
	if task == nil {
		return nil, errQuotaRefreshTaskNotFound
	}
	return task, nil
}

func (s *quotaRefreshTaskService) listSnapshot() ([]quotaRefreshTask, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tasks := make([]quotaRefreshTask, 0, len(s.order))
	for _, v := range slices.Backward(s.order) {
		task := cloneQuotaRefreshTask(s.tasks[v])
		if task != nil {
			tasks = append(tasks, *task)
		}
	}
	return tasks, s.persistenceError
}

func (s *quotaRefreshTaskService) latestSnapshot() *quotaRefreshTask {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, v := range slices.Backward(s.order) {
		if task := cloneQuotaRefreshTask(s.tasks[v]); task != nil {
			return task
		}
	}
	return nil
}

func (s *quotaRefreshTaskService) pauseTask(taskID string) (*quotaRefreshTask, error) {
	s.mu.Lock()
	task := s.tasks[strings.TrimSpace(taskID)]
	runtime := s.runtimes[strings.TrimSpace(taskID)]
	if task == nil {
		s.mu.Unlock()
		return nil, errQuotaRefreshTaskNotFound
	}
	if runtime == nil || task.Status != quotaRefreshTaskStatusQueued && task.Status != quotaRefreshTaskStatusRunning {
		s.mu.Unlock()
		return nil, errQuotaRefreshTaskConflict
	}
	if runtime.resumeCh == nil {
		runtime.resumeCh = make(chan struct{})
	}
	task.Status = quotaRefreshTaskStatusPaused
	s.mu.Unlock()
	if err := s.persistState(); err != nil {
		s.failTaskPersistence(taskID, err)
		return s.taskSnapshotWithError(taskID, err)
	}
	return s.taskSnapshot(taskID)
}

func (s *quotaRefreshTaskService) resumeTask(taskID string) (*quotaRefreshTask, error) {
	s.mu.Lock()
	task := s.tasks[strings.TrimSpace(taskID)]
	runtime := s.runtimes[strings.TrimSpace(taskID)]
	if task == nil {
		s.mu.Unlock()
		return nil, errQuotaRefreshTaskNotFound
	}
	if runtime == nil || task.Status != quotaRefreshTaskStatusPaused {
		s.mu.Unlock()
		return nil, errQuotaRefreshTaskConflict
	}
	if runtime.resumeCh != nil {
		close(runtime.resumeCh)
		runtime.resumeCh = nil
	}
	if task.StartedAt == nil {
		task.Status = quotaRefreshTaskStatusQueued
	} else {
		task.Status = quotaRefreshTaskStatusRunning
	}
	s.mu.Unlock()
	if err := s.persistState(); err != nil {
		s.failTaskPersistence(taskID, err)
		return s.taskSnapshotWithError(taskID, err)
	}
	return s.taskSnapshot(taskID)
}

func (s *quotaRefreshTaskService) cancelTask(taskID string) (*quotaRefreshTask, error) {
	s.mu.Lock()
	task := s.tasks[strings.TrimSpace(taskID)]
	runtime := s.runtimes[strings.TrimSpace(taskID)]
	if task == nil {
		s.mu.Unlock()
		return nil, errQuotaRefreshTaskNotFound
	}
	if runtime == nil || quotaRefreshTaskTerminal(task.Status) {
		s.mu.Unlock()
		return nil, errQuotaRefreshTaskConflict
	}
	now := time.Now().UTC()
	task.CancelRequestedAt = cloneQuotaRefreshTaskTime(&now)
	runtime.cancel(errQuotaRefreshTaskCancelled)
	if runtime.resumeCh != nil {
		close(runtime.resumeCh)
		runtime.resumeCh = nil
	}
	s.mu.Unlock()
	if err := s.persistState(); err != nil {
		s.failTaskPersistence(taskID, err)
		return s.taskSnapshotWithError(taskID, err)
	}
	return s.taskSnapshot(taskID)
}

func (s *quotaRefreshTaskService) taskSnapshotWithError(taskID string, err error) (*quotaRefreshTask, error) {
	task, _ := s.taskSnapshot(taskID)
	return task, err
}

func (s *quotaRefreshTaskService) close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	hadRuntimes := len(s.runtimes) > 0
	for _, runtime := range s.runtimes {
		runtime.cancel(errQuotaRefreshTaskInterrupted)
		if runtime.resumeCh != nil {
			close(runtime.resumeCh)
			runtime.resumeCh = nil
		}
	}
	s.mu.Unlock()
	s.wg.Wait()
	if !hadRuntimes {
		return nil
	}
	return s.persistState()
}

func findQuotaRefreshTaskTarget(task *quotaRefreshTask, targetID string) *quotaRefreshTaskTarget {
	if task == nil {
		return nil
	}
	for index := range task.Targets {
		if task.Targets[index].ID == targetID {
			return &task.Targets[index]
		}
	}
	return nil
}

func summarizeQuotaRefreshTaskTargets(targets []quotaRefreshTaskTarget) quotaRefreshTaskSummary {
	summary := quotaRefreshTaskSummary{Total: len(targets)}
	for _, target := range targets {
		switch target.Status {
		case quotaRefreshTargetStatusQueued:
			summary.Queued++
		case quotaRefreshTargetStatusRunning:
			summary.Running++
		case quotaRefreshTargetStatusSucceeded:
			summary.Succeeded++
		case quotaRefreshTargetStatusFailed:
			summary.Failed++
		case quotaRefreshTargetStatusSkipped:
			summary.Skipped++
		case quotaRefreshTargetStatusCancelled:
			summary.Cancelled++
		case quotaRefreshTargetStatusInconclusive:
			summary.Inconclusive++
		case quotaRefreshTargetStatusInterrupted:
			summary.Interrupted++
		}
	}
	return summary
}

func quotaRefreshTaskTerminal(status string) bool {
	switch status {
	case quotaRefreshTaskStatusSucceeded,
		quotaRefreshTaskStatusFailed,
		quotaRefreshTaskStatusCancelled,
		quotaRefreshTaskStatusInconclusive,
		quotaRefreshTaskStatusInterrupted:
		return true
	default:
		return false
	}
}

func quotaRefreshTargetTerminal(status string) bool {
	switch status {
	case quotaRefreshTargetStatusSucceeded,
		quotaRefreshTargetStatusFailed,
		quotaRefreshTargetStatusSkipped,
		quotaRefreshTargetStatusCancelled,
		quotaRefreshTargetStatusInconclusive,
		quotaRefreshTargetStatusInterrupted:
		return true
	default:
		return false
	}
}

func cloneQuotaRefreshTask(task *quotaRefreshTask) *quotaRefreshTask {
	if task == nil {
		return nil
	}
	cloned := *task
	cloned.StartedAt = cloneQuotaRefreshTaskTime(task.StartedAt)
	cloned.CompletedAt = cloneQuotaRefreshTaskTime(task.CompletedAt)
	cloned.CancelRequestedAt = cloneQuotaRefreshTaskTime(task.CancelRequestedAt)
	cloned.Targets = append([]quotaRefreshTaskTarget(nil), task.Targets...)
	for index := range cloned.Targets {
		cloned.Targets[index].StartedAt = cloneQuotaRefreshTaskTime(cloned.Targets[index].StartedAt)
		cloned.Targets[index].CompletedAt = cloneQuotaRefreshTaskTime(cloned.Targets[index].CompletedAt)
	}
	return &cloned
}

func cloneQuotaRefreshTaskTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func (h *Handler) GetQuotaRefreshTasks(c *gin.Context) {
	tasks, persistenceError := h.quotaRefreshService().listSnapshot()
	c.JSON(
		http.StatusOK, gin.H{
			"tasks":             tasks,
			"total":             len(tasks),
			"persistence_error": persistenceError,
		},
	)
}

func (h *Handler) GetQuotaRefreshTask(c *gin.Context) {
	task, err := h.quotaRefreshService().taskSnapshot(c.Param("task_id"))
	if err != nil {
		writeQuotaRefreshTaskError(c, err)
		return
	}
	c.JSON(http.StatusOK, task)
}

func (h *Handler) PostQuotaRefreshTaskPause(c *gin.Context) {
	task, err := h.quotaRefreshService().pauseTask(c.Param("task_id"))
	if err != nil {
		writeQuotaRefreshTaskError(c, err)
		return
	}
	c.JSON(http.StatusOK, task)
}

func (h *Handler) PostQuotaRefreshTaskResume(c *gin.Context) {
	task, err := h.quotaRefreshService().resumeTask(c.Param("task_id"))
	if err != nil {
		writeQuotaRefreshTaskError(c, err)
		return
	}
	c.JSON(http.StatusOK, task)
}

func (h *Handler) PostQuotaRefreshTaskCancel(c *gin.Context) {
	task, err := h.quotaRefreshService().cancelTask(c.Param("task_id"))
	if err != nil {
		writeQuotaRefreshTaskError(c, err)
		return
	}
	c.JSON(http.StatusAccepted, task)
}

func writeQuotaRefreshTaskError(c *gin.Context, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, errQuotaRefreshTaskNotFound):
		status = http.StatusNotFound
	case errors.Is(err, errQuotaRefreshTaskConflict):
		status = http.StatusConflict
	case errors.Is(err, errQuotaRefreshTaskCapacity):
		status = http.StatusTooManyRequests
	case errors.Is(err, errQuotaRefreshTaskPersistenceUnavailable),
		errors.Is(err, errQuotaRefreshTaskPersistence):
		status = http.StatusServiceUnavailable
	case errors.Is(err, errQuotaRefreshTaskServiceClosed):
		status = http.StatusServiceUnavailable
	}
	c.JSON(status, gin.H{"error": err.Error()})
}
