package auth

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	log "github.com/sirupsen/logrus"
)

const (
	schedulerCheckpointFileName      = ".scheduler-state"
	schedulerCheckpointLegacyVersion = 1
	schedulerCheckpointFileVersion   = 2
	schedulerCheckpointMaxBytes      = 4 << 20
	schedulerCheckpointTempAttempts  = 10
)

// SchedulerCheckpointStore persists opaque scheduler progress without owning auth or routing facts.
type SchedulerCheckpointStore interface {
	Load(context.Context) ([]byte, error)
	Save(context.Context, []byte) error
}

// FileSchedulerCheckpointStore stores scheduler progress in the auth directory.
type FileSchedulerCheckpointStore struct {
	mu   sync.Mutex
	path string
}

// NewFileSchedulerCheckpointStore creates a checkpoint store in authDir.
func NewFileSchedulerCheckpointStore(authDir string) *FileSchedulerCheckpointStore {
	authDir = strings.TrimSpace(authDir)
	if authDir == "" {
		return &FileSchedulerCheckpointStore{}
	}
	return &FileSchedulerCheckpointStore{
		path: filepath.Join(authDir, schedulerCheckpointFileName),
	}
}

func (s *FileSchedulerCheckpointStore) Load(ctx context.Context) (data []byte, err error) {
	if s == nil || strings.TrimSpace(s.path) == "" {
		return nil, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if errCtx := ctx.Err(); errCtx != nil {
		return nil, errCtx
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	root, errRoot := util.OpenOrCreateDirectoryRoot(filepath.Dir(s.path), 0o700)
	if errRoot != nil {
		return nil, fmt.Errorf("open scheduler checkpoint directory: %w", errRoot)
	}
	defer func() {
		if errClose := root.Close(); errClose != nil {
			err = errors.Join(err, fmt.Errorf("close scheduler checkpoint directory: %w", errClose))
		}
	}()
	name := filepath.Base(s.path)
	info, errInfo := root.Lstat(name)
	if errors.Is(errInfo, os.ErrNotExist) {
		return nil, nil
	}
	if errInfo != nil {
		return nil, fmt.Errorf("inspect scheduler checkpoint: %w", errInfo)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("scheduler checkpoint is not a regular file")
	}
	if info.Size() > schedulerCheckpointMaxBytes {
		return nil, fmt.Errorf("scheduler checkpoint exceeds %d bytes", schedulerCheckpointMaxBytes)
	}
	file, errOpen := root.Open(name)
	if errOpen != nil {
		return nil, fmt.Errorf("open scheduler checkpoint: %w", errOpen)
	}
	defer func() {
		if errClose := file.Close(); errClose != nil {
			err = errors.Join(err, fmt.Errorf("close scheduler checkpoint: %w", errClose))
		}
	}()
	openedInfo, errOpenedInfo := file.Stat()
	verifiedInfo, errVerifiedInfo := root.Lstat(name)
	if errOpenedInfo != nil {
		return nil, fmt.Errorf("inspect opened scheduler checkpoint: %w", errOpenedInfo)
	}
	if errVerifiedInfo != nil {
		return nil, fmt.Errorf("verify scheduler checkpoint: %w", errVerifiedInfo)
	}
	if !openedInfo.Mode().IsRegular() || verifiedInfo.Mode()&os.ModeSymlink != 0 ||
		!os.SameFile(info, openedInfo) || !os.SameFile(openedInfo, verifiedInfo) {
		return nil, fmt.Errorf("scheduler checkpoint changed during validation")
	}
	if errSecure := util.SecurePrivateFile(file, false); errSecure != nil {
		return nil, fmt.Errorf("secure scheduler checkpoint: %w", errSecure)
	}
	data, errRead := io.ReadAll(io.LimitReader(file, schedulerCheckpointMaxBytes+1))
	if errRead != nil {
		return nil, fmt.Errorf("read scheduler checkpoint: %w", errRead)
	}
	if len(data) > schedulerCheckpointMaxBytes {
		return nil, fmt.Errorf("scheduler checkpoint exceeds %d bytes", schedulerCheckpointMaxBytes)
	}
	return data, nil
}

func (s *FileSchedulerCheckpointStore) Save(ctx context.Context, data []byte) (err error) {
	if s == nil || strings.TrimSpace(s.path) == "" {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if errCtx := ctx.Err(); errCtx != nil {
		return errCtx
	}
	if len(data) > schedulerCheckpointMaxBytes {
		return fmt.Errorf("scheduler checkpoint exceeds %d bytes", schedulerCheckpointMaxBytes)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	root, errRoot := util.OpenOrCreateDirectoryRoot(filepath.Dir(s.path), 0o700)
	if errRoot != nil {
		return fmt.Errorf("open scheduler checkpoint directory: %w", errRoot)
	}
	defer func() {
		if errClose := root.Close(); errClose != nil {
			err = errors.Join(err, fmt.Errorf("close scheduler checkpoint directory: %w", errClose))
		}
	}()
	temp, tempName, errCreate := createSchedulerCheckpointTemp(root)
	if errCreate != nil {
		return errCreate
	}
	cleanup := func() {
		_ = temp.Close()
		_ = root.Remove(tempName)
	}
	if errSecure := util.SecurePrivateFile(temp, false); errSecure != nil {
		cleanup()
		return fmt.Errorf("secure scheduler checkpoint temp file: %w", errSecure)
	}
	written, errWrite := temp.Write(data)
	if errWrite != nil {
		cleanup()
		return fmt.Errorf("write scheduler checkpoint: %w", errWrite)
	}
	if written != len(data) {
		cleanup()
		return io.ErrShortWrite
	}
	if errSync := temp.Sync(); errSync != nil {
		cleanup()
		return fmt.Errorf("sync scheduler checkpoint: %w", errSync)
	}
	if errClose := temp.Close(); errClose != nil {
		_ = root.Remove(tempName)
		return fmt.Errorf("close scheduler checkpoint: %w", errClose)
	}
	if errCtx := ctx.Err(); errCtx != nil {
		_ = root.Remove(tempName)
		return errCtx
	}
	if errRename := root.Rename(tempName, filepath.Base(s.path)); errRename != nil {
		_ = root.Remove(tempName)
		return fmt.Errorf("replace scheduler checkpoint: %w", errRename)
	}
	directory, errOpenDirectory := root.Open(".")
	if errOpenDirectory != nil {
		return fmt.Errorf("open scheduler checkpoint directory for sync: %w", errOpenDirectory)
	}
	if errSync := directory.Sync(); errSync != nil {
		_ = directory.Close()
		return fmt.Errorf("sync scheduler checkpoint directory: %w", errSync)
	}
	if errClose := directory.Close(); errClose != nil {
		return fmt.Errorf("close scheduler checkpoint directory after sync: %w", errClose)
	}
	return nil
}

func createSchedulerCheckpointTemp(root *os.Root) (*os.File, string, error) {
	if root == nil {
		return nil, "", fmt.Errorf("scheduler checkpoint directory is nil")
	}
	var random [8]byte
	for range schedulerCheckpointTempAttempts {
		if _, errRandom := rand.Read(random[:]); errRandom != nil {
			return nil, "", fmt.Errorf("generate scheduler checkpoint temp name: %w", errRandom)
		}
		name := schedulerCheckpointFileName + "." + hex.EncodeToString(random[:]) + ".tmp"
		file, errOpen := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(errOpen, os.ErrExist) {
			continue
		}
		if errOpen != nil {
			return nil, "", fmt.Errorf("create scheduler checkpoint temp file: %w", errOpen)
		}
		return file, name, nil
	}
	return nil, "", fmt.Errorf("create scheduler checkpoint temp file: name collision limit reached")
}

type schedulerCheckpointRuntime struct {
	store      SchedulerCheckpointStore
	wake       chan struct{}
	stop       chan struct{}
	done       chan struct{}
	stopOnce   sync.Once
	finalizeMu sync.Mutex
	revision   uint64
}

type schedulerState struct {
	strategy            schedulerStrategy
	views               map[schedulerViewKey]readyBucketCursorState
	mixedCursors        map[string]string
	mixedWeightedStates map[string]smoothWeightedState
	mixedQuotaStates    map[string]quotaAwareViewState
}

type schedulerViewKey struct {
	provider string
	model    string
	priority int
}

type schedulerCheckpointFile struct {
	Version             int                                       `json:"version"`
	Strategy            string                                    `json:"strategy"`
	Views               []schedulerCheckpointView                 `json:"views,omitempty"`
	MixedCursors        map[string]string                         `json:"mixed_cursors,omitempty"`
	MixedWeightedStates map[string]schedulerCheckpointWeightState `json:"mixed_weighted_states,omitempty"`
	MixedQuotaStates    map[string]schedulerCheckpointQuotaState  `json:"mixed_quota_states,omitempty"`
}

type schedulerCheckpointView struct {
	Provider string                        `json:"provider"`
	Model    string                        `json:"model"`
	Priority int                           `json:"priority"`
	All      schedulerCheckpointViewCursor `json:"all"`
	WS       schedulerCheckpointViewCursor `json:"ws"`
}

type schedulerCheckpointViewCursor struct {
	LastPickedAuth   string                         `json:"last_picked_auth,omitempty"`
	LastPickedParent string                         `json:"last_picked_parent,omitempty"`
	ChildLastPicked  map[string]string              `json:"child_last_picked,omitempty"`
	Weighted         schedulerCheckpointWeightState `json:"weighted,omitempty"`
	Quota            schedulerCheckpointQuotaState  `json:"quota,omitempty"`
}

type schedulerCheckpointQuotaState struct {
	LastPickedPool string                                    `json:"last_picked_pool,omitempty"`
	PoolStates     map[string]schedulerCheckpointWeightState `json:"pool_states,omitempty"`
}

type schedulerCheckpointWeightState struct {
	Current    map[string]int64 `json:"current,omitempty"`
	Weights    map[string]int64 `json:"weights,omitempty"`
	LastPicked string           `json:"last_picked,omitempty"`
}

func (s *authScheduler) snapshotStateLocked() schedulerState {
	state := schedulerState{
		strategy:            s.strategy,
		views:               make(map[schedulerViewKey]readyBucketCursorState),
		mixedCursors:        make(map[string]string, len(s.mixedCursors)),
		mixedWeightedStates: make(map[string]smoothWeightedState, len(s.mixedWeightedStates)),
		mixedQuotaStates:    make(map[string]quotaAwareViewState, len(s.mixedQuotaStates)),
	}
	maps.Copy(state.mixedCursors, s.mixedCursors)
	for key, weighted := range s.mixedWeightedStates {
		if weighted != nil {
			state.mixedWeightedStates[key] = cloneSmoothWeightedState(*weighted)
		}
	}
	for key, quota := range s.mixedQuotaStates {
		if quota != nil {
			state.mixedQuotaStates[key] = cloneQuotaAwareViewState(*quota)
		}
	}
	for providerKey, provider := range s.providers {
		if provider == nil {
			continue
		}
		for modelKey, shard := range provider.modelShards {
			if shard == nil {
				continue
			}
			for priority, cursors := range shard.snapshotCursorStatesLocked() {
				state.views[schedulerViewKey{
					provider: providerKey,
					model:    modelKey,
					priority: priority,
				}] = cursors
			}
		}
	}
	return state
}

func (s *authScheduler) restoreStateLocked(state schedulerState, now time.Time) {
	if state.strategy != s.strategy {
		return
	}
	maps.Copy(s.mixedCursors, state.mixedCursors)
	for key, weighted := range state.mixedWeightedStates {
		cloned := cloneSmoothWeightedState(weighted)
		s.mixedWeightedStates[key] = &cloned
	}
	for key, quota := range state.mixedQuotaStates {
		cloned := cloneQuotaAwareViewState(quota)
		s.mixedQuotaStates[key] = &cloned
	}
	for key, cursors := range state.views {
		provider := s.providers[key.provider]
		if provider == nil {
			continue
		}
		shard := provider.ensureModelLocked(key.model, now)
		if shard == nil || !shard.hasPriorityLocked(key.priority) {
			continue
		}
		if shard.cursorStatesByPriority == nil {
			shard.cursorStatesByPriority = make(map[int]readyBucketCursorState)
		}
		cursors = cloneReadyBucketCursorState(cursors)
		shard.cursorStatesByPriority[key.priority] = cursors
		bucket := shard.readyByPriority[key.priority]
		if bucket == nil {
			continue
		}
		restoreReadyViewCursors(&bucket.all, cursors.all)
		restoreReadyViewCursors(&bucket.ws, cursors.ws)
	}
}

func (s *authScheduler) startCheckpoint(ctx context.Context, store SchedulerCheckpointStore) error {
	if s == nil || store == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	data, errLoad := store.Load(ctx)
	var state schedulerState
	if errLoad == nil && len(strings.TrimSpace(string(data))) > 0 {
		state, errLoad = decodeSchedulerCheckpoint(data)
	}

	runtime := &schedulerCheckpointRuntime{
		store: store,
		wake:  make(chan struct{}, 1),
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
	}
	s.mu.Lock()
	if s.checkpoint != nil {
		s.mu.Unlock()
		return fmt.Errorf("scheduler checkpoint is already running")
	}
	if errLoad == nil && state.strategy == s.strategy {
		s.restoreStateLocked(state, time.Now())
	}
	s.checkpoint = runtime
	s.mu.Unlock()
	go s.runCheckpoint(runtime)
	return errLoad
}

func (s *authScheduler) runCheckpoint(runtime *schedulerCheckpointRuntime) {
	defer close(runtime.done)
	for {
		select {
		case <-runtime.wake:
			if _, errSave := s.saveCheckpoint(context.Background(), runtime); errSave != nil {
				log.WithError(errSave).Warn("failed to persist scheduler checkpoint")
			}
		case <-runtime.stop:
			return
		}
	}
}

func (s *authScheduler) markCheckpointDirtyLocked() {
	if s == nil || s.checkpoint == nil {
		return
	}
	runtime := s.checkpoint
	runtime.revision++
	select {
	case runtime.wake <- struct{}{}:
	default:
	}
}

func (s *authScheduler) saveCheckpoint(
	ctx context.Context,
	runtime *schedulerCheckpointRuntime,
) (uint64, error) {
	if s == nil || runtime == nil || runtime.store == nil {
		return 0, nil
	}
	s.mu.Lock()
	revision := runtime.revision
	state := s.snapshotStateLocked()
	s.mu.Unlock()
	data, errEncode := encodeSchedulerCheckpoint(state)
	if errEncode != nil {
		return revision, errEncode
	}
	return revision, runtime.store.Save(ctx, data)
}

func (s *authScheduler) stopCheckpoint(ctx context.Context) error {
	if s == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Lock()
	runtime := s.checkpoint
	if runtime != nil {
		runtime.stopOnce.Do(
			func() {
				close(runtime.stop)
			},
		)
	}
	s.mu.Unlock()
	if runtime == nil {
		return nil
	}
	select {
	case <-runtime.done:
	case <-ctx.Done():
		return ctx.Err()
	}

	runtime.finalizeMu.Lock()
	defer runtime.finalizeMu.Unlock()
	for {
		s.mu.Lock()
		if s.checkpoint != runtime {
			s.mu.Unlock()
			return nil
		}
		s.mu.Unlock()

		revision, errSave := s.saveCheckpoint(ctx, runtime)
		if errSave != nil {
			return errSave
		}
		s.mu.Lock()
		if s.checkpoint == runtime && runtime.revision == revision {
			s.checkpoint = nil
			s.mu.Unlock()
			return nil
		}
		s.mu.Unlock()
		if errCtx := ctx.Err(); errCtx != nil {
			return errCtx
		}
	}
}

func encodeSchedulerCheckpoint(state schedulerState) ([]byte, error) {
	file := schedulerCheckpointFile{
		Version:             schedulerCheckpointFileVersion,
		Strategy:            schedulerStrategyName(state.strategy),
		MixedCursors:        state.mixedCursors,
		MixedWeightedStates: make(map[string]schedulerCheckpointWeightState, len(state.mixedWeightedStates)),
		MixedQuotaStates:    make(map[string]schedulerCheckpointQuotaState, len(state.mixedQuotaStates)),
	}
	keys := make([]schedulerViewKey, 0, len(state.views))
	for key := range state.views {
		keys = append(keys, key)
	}
	sort.Slice(
		keys, func(i, j int) bool {
			if keys[i].provider != keys[j].provider {
				return keys[i].provider < keys[j].provider
			}
			if keys[i].model != keys[j].model {
				return keys[i].model < keys[j].model
			}
			return keys[i].priority > keys[j].priority
		},
	)
	for _, key := range keys {
		cursors := state.views[key]
		file.Views = append(
			file.Views, schedulerCheckpointView{
				Provider: key.provider,
				Model:    key.model,
				Priority: key.priority,
				All:      encodeCheckpointViewCursor(cursors.all),
				WS:       encodeCheckpointViewCursor(cursors.ws),
			},
		)
	}
	for key, weighted := range state.mixedWeightedStates {
		file.MixedWeightedStates[key] = encodeCheckpointWeightState(weighted)
	}
	for key, quota := range state.mixedQuotaStates {
		file.MixedQuotaStates[key] = encodeCheckpointQuotaState(quota)
	}
	data, errMarshal := json.Marshal(file)
	if errMarshal != nil {
		return nil, fmt.Errorf("marshal scheduler checkpoint: %w", errMarshal)
	}
	return append(data, '\n'), nil
}

func decodeSchedulerCheckpoint(data []byte) (schedulerState, error) {
	var file schedulerCheckpointFile
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if errDecode := decoder.Decode(&file); errDecode != nil {
		return schedulerState{}, fmt.Errorf("parse scheduler checkpoint: %w", errDecode)
	}
	var trailing any
	if errTrailing := decoder.Decode(&trailing); !errors.Is(errTrailing, io.EOF) {
		if errTrailing == nil {
			return schedulerState{}, fmt.Errorf("parse scheduler checkpoint: multiple JSON values")
		}
		return schedulerState{}, fmt.Errorf("parse scheduler checkpoint: %w", errTrailing)
	}
	if file.Version != schedulerCheckpointLegacyVersion && file.Version != schedulerCheckpointFileVersion {
		return schedulerState{}, fmt.Errorf("parse scheduler checkpoint: unsupported version %d", file.Version)
	}
	strategy, ok := parseSchedulerStrategyName(file.Strategy)
	if !ok {
		return schedulerState{}, fmt.Errorf("parse scheduler checkpoint: unsupported strategy %q", file.Strategy)
	}
	state := schedulerState{
		strategy:            strategy,
		views:               make(map[schedulerViewKey]readyBucketCursorState, len(file.Views)),
		mixedCursors:        file.MixedCursors,
		mixedWeightedStates: make(map[string]smoothWeightedState, len(file.MixedWeightedStates)),
		mixedQuotaStates:    make(map[string]quotaAwareViewState, len(file.MixedQuotaStates)),
	}
	for _, view := range file.Views {
		key := schedulerViewKey{
			provider: strings.TrimSpace(view.Provider),
			model:    canonicalModelKey(view.Model),
			priority: view.Priority,
		}
		if key.provider == "" {
			continue
		}
		state.views[key] = readyBucketCursorState{
			all: decodeCheckpointViewCursor(view.All),
			ws:  decodeCheckpointViewCursor(view.WS),
		}
	}
	for key, weighted := range file.MixedWeightedStates {
		state.mixedWeightedStates[key] = decodeCheckpointWeightState(weighted)
	}
	for key, quota := range file.MixedQuotaStates {
		state.mixedQuotaStates[key] = decodeCheckpointQuotaState(quota)
	}
	return state, nil
}

func encodeCheckpointViewCursor(state readyViewCursorState) schedulerCheckpointViewCursor {
	return schedulerCheckpointViewCursor{
		LastPickedAuth:   state.lastPickedAuth,
		LastPickedParent: state.lastPickedParent,
		ChildLastPicked:  state.childLastPicked,
		Weighted:         encodeCheckpointWeightState(state.weightedState),
		Quota:            encodeCheckpointQuotaState(state.quotaState),
	}
}

func decodeCheckpointViewCursor(state schedulerCheckpointViewCursor) readyViewCursorState {
	return readyViewCursorState{
		lastPickedAuth:   state.LastPickedAuth,
		lastPickedParent: state.LastPickedParent,
		childLastPicked:  state.ChildLastPicked,
		weightedState:    decodeCheckpointWeightState(state.Weighted),
		quotaState:       decodeCheckpointQuotaState(state.Quota),
	}
}

func encodeCheckpointQuotaState(state quotaAwareViewState) schedulerCheckpointQuotaState {
	encoded := schedulerCheckpointQuotaState{
		LastPickedPool: state.lastPickedPool,
		PoolStates:     make(map[string]schedulerCheckpointWeightState, len(state.poolStates)),
	}
	for poolKey, poolState := range state.poolStates {
		encoded.PoolStates[poolKey] = encodeCheckpointWeightState(poolState)
	}
	return encoded
}

func decodeCheckpointQuotaState(state schedulerCheckpointQuotaState) quotaAwareViewState {
	decoded := quotaAwareViewState{
		lastPickedPool: state.LastPickedPool,
		poolStates:     make(map[string]smoothWeightedState, len(state.PoolStates)),
	}
	for poolKey, poolState := range state.PoolStates {
		decoded.poolStates[poolKey] = decodeCheckpointWeightState(poolState)
	}
	return decoded
}

func encodeCheckpointWeightState(state smoothWeightedState) schedulerCheckpointWeightState {
	return schedulerCheckpointWeightState{
		Current:    state.current,
		Weights:    state.weights,
		LastPicked: state.lastPicked,
	}
}

func decodeCheckpointWeightState(state schedulerCheckpointWeightState) smoothWeightedState {
	return smoothWeightedState{
		current:    state.Current,
		weights:    state.Weights,
		lastPicked: state.LastPicked,
	}
}

func schedulerStrategyName(strategy schedulerStrategy) string {
	switch strategy {
	case schedulerStrategyRoundRobin:
		return "round-robin"
	case schedulerStrategyFillFirst:
		return "fill-first"
	case schedulerStrategyWeightedRoundRobin:
		return "weighted-round-robin"
	case schedulerStrategyQuotaAware:
		return "quota-aware"
	default:
		return "custom"
	}
}

func parseSchedulerStrategyName(value string) (schedulerStrategy, bool) {
	switch strings.TrimSpace(value) {
	case "round-robin":
		return schedulerStrategyRoundRobin, true
	case "fill-first":
		return schedulerStrategyFillFirst, true
	case "weighted-round-robin":
		return schedulerStrategyWeightedRoundRobin, true
	case "quota-aware":
		return schedulerStrategyQuotaAware, true
	case "custom":
		return schedulerStrategyCustom, true
	default:
		return schedulerStrategyCustom, false
	}
}
