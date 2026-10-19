package usage

import (
	"context"
	"errors"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/clienterror"
	internallogging "github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	coresession "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/session"
	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
)

// CredentialSourceKind identifies whether Record.Source is a secret-derived
// value or a non-secret account identity.
type CredentialSourceKind string

const (
	CredentialSourceKindAPIKey   CredentialSourceKind = "api_key"
	CredentialSourceKindIdentity CredentialSourceKind = "identity"
)

// Record contains the usage statistics captured for a single provider request.
type Record struct {
	Provider            string
	BaseURL             string
	ExecutorType        string
	Model               string
	Stream              bool
	Alias               string
	APIKey              string
	SessionID           string
	ParentSessionID     string
	ReservationID       string
	AuthID              string
	AuthIndex           string
	AccessTokenSHA256   string
	AuthType            string
	Source              string
	SourceKind          CredentialSourceKind
	SourceRedacted      bool
	ReasoningEffort     string
	ServiceTier         string
	RequestServiceTier  string
	ResponseServiceTier string
	// ResponseModel is the model reported by the provider, if present.
	ResponseModel string
	// Generate reports whether the client requested actual generation.
	// nil or true means generation is enabled; only an explicit false disables generation.
	Generate    *bool
	RequestedAt time.Time
	Latency     time.Duration
	// RequestID identifies the inbound HTTP request used by persisted request logs.
	RequestID string
	// ExecutionID identifies the model execution shared with plugin lifecycle events.
	ExecutionID       string
	ClientIP          string
	XForwardedFor     string
	UserAgent         string
	TimeToFirstByteMs int64
	TotalDurationMs   int64
	Completed         bool
	Thinking          *Thinking
	Failed            bool
	Fail              Failure
	Detail            Detail
	ResponseHeaders   http.Header
}

const DefaultServiceTier = "default"

type requestedModelAliasContextKey struct{}
type reasoningEffortContextKey struct{}
type serviceTierContextKey struct{}
type generateContextKey struct{}
type streamContextKey struct{}
type deferredRecordContextKey struct{}
type managerContextKey struct{}
type executionIDContextKey struct{}

// WithManager routes usage records published with ctx through manager.
func WithManager(ctx context.Context, manager *Manager) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if manager == nil {
		return ctx
	}
	return context.WithValue(ctx, managerContextKey{}, manager)
}

// ManagerFromContext returns the usage manager assigned to ctx.
func ManagerFromContext(ctx context.Context) *Manager {
	if ctx == nil {
		return nil
	}
	manager, _ := ctx.Value(managerContextKey{}).(*Manager)
	return manager
}

// WithExecutionID associates an execution attempt with its request lifecycle.
func WithExecutionID(ctx context.Context, executionID string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if executionID = strings.TrimSpace(executionID); executionID == "" {
		return ctx
	}
	return context.WithValue(ctx, executionIDContextKey{}, executionID)
}

// ExecutionIDFromContext returns the ID shared by usage and request lifecycle events.
func ExecutionIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(executionIDContextKey{}).(string)
	return id
}

// DeferredRecord delays the main usage record until the response handler knows
// whether downstream validation and response construction succeeded.
type DeferredRecord struct {
	mu        sync.Mutex
	record    *Record
	ctx       context.Context
	decided   bool
	failure   error
	published bool
}

// WithRequestedModelAlias stores the client-requested model name for usage sinks.
func WithRequestedModelAlias(ctx context.Context, alias string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	alias = strings.TrimSpace(alias)
	if alias == "" {
		return ctx
	}
	return context.WithValue(ctx, requestedModelAliasContextKey{}, alias)
}

// RequestedModelAliasFromContext returns the client-requested model name stored in ctx.
func RequestedModelAliasFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	switch value := ctx.Value(requestedModelAliasContextKey{}).(type) {
	case string:
		return strings.TrimSpace(value)
	case []byte:
		return strings.TrimSpace(string(value))
	default:
		return ""
	}
}

// WithReasoningEffort stores the client-requested reasoning effort for usage sinks.
func WithReasoningEffort(ctx context.Context, effort string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	effort = strings.TrimSpace(effort)
	if effort == "" {
		return ctx
	}
	return context.WithValue(ctx, reasoningEffortContextKey{}, effort)
}

// ReasoningEffortFromContext returns the client-requested reasoning effort stored in ctx.
func ReasoningEffortFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	switch value := ctx.Value(reasoningEffortContextKey{}).(type) {
	case string:
		return strings.TrimSpace(value)
	case []byte:
		return strings.TrimSpace(string(value))
	default:
		return ""
	}
}

// WithServiceTier stores the client-requested service tier for usage sinks.
func WithServiceTier(ctx context.Context, tier string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	tier = strings.TrimSpace(tier)
	if tier == "" {
		tier = DefaultServiceTier
	}
	return context.WithValue(ctx, serviceTierContextKey{}, tier)
}

// ServiceTierFromContext returns the client-requested service tier stored in ctx.
func ServiceTierFromContext(ctx context.Context) string {
	if ctx == nil {
		return DefaultServiceTier
	}
	switch value := ctx.Value(serviceTierContextKey{}).(type) {
	case string:
		tier := strings.TrimSpace(value)
		if tier == "" {
			return DefaultServiceTier
		}
		return tier
	case []byte:
		tier := strings.TrimSpace(string(value))
		if tier == "" {
			return DefaultServiceTier
		}
		return tier
	default:
		return DefaultServiceTier
	}
}

// WithGenerate stores whether the client requested actual generation for usage sinks.
func WithGenerate(ctx context.Context, generate bool) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, generateContextKey{}, generate)
}

// GenerateFromContext returns whether the client requested actual generation.
// Missing values default to true.
func GenerateFromContext(ctx context.Context) bool {
	if ctx == nil {
		return true
	}
	value, ok := ctx.Value(generateContextKey{}).(bool)
	if !ok {
		return true
	}
	return value
}

// GenerateFlag returns a pointer suitable for Record.Generate.
//
//go:fix inline
func GenerateFlag(generate bool) *bool {
	return new(generate)
}

// GenerateEnabled applies the backward-compatible default for omitted values.
func GenerateEnabled(generate *bool) bool {
	return generate == nil || *generate
}

// WithStream stores whether the current execution uses streaming delivery.
func WithStream(ctx context.Context, stream bool) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, streamContextKey{}, stream)
}

// StreamFromContext returns whether the current execution uses streaming delivery.
func StreamFromContext(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	stream, _ := ctx.Value(streamContextKey{}).(bool)
	return stream
}

// WithDeferredRecord stores a deferred main usage record in ctx.
func WithDeferredRecord(ctx context.Context, deferred *DeferredRecord) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if deferred == nil {
		return ctx
	}
	return context.WithValue(ctx, deferredRecordContextKey{}, deferred)
}

// WithoutDeferredRecord prevents nested executions from claiming a parent request's deferred record.
func WithoutDeferredRecord(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, deferredRecordContextKey{}, (*DeferredRecord)(nil))
}

// NewDeferredRecord creates a usage record holder whose first completion result wins.
func NewDeferredRecord() *DeferredRecord {
	return &DeferredRecord{}
}

// DeferRecord stores the main usage record when ctx has a deferred holder.
func DeferRecord(ctx context.Context, record Record) bool {
	if ctx == nil {
		return false
	}
	deferred, _ := ctx.Value(deferredRecordContextKey{}).(*DeferredRecord)
	if deferred == nil {
		return false
	}
	return deferred.store(ctx, record)
}

// Complete publishes the deferred record after applying the final handler outcome.
func (d *DeferredRecord) Complete(ctx context.Context, err error) {
	if d == nil {
		return
	}
	d.mu.Lock()
	if d.decided {
		d.mu.Unlock()
		return
	}
	d.decided = true
	d.failure = err
	if d.record == nil || d.published {
		d.mu.Unlock()
		return
	}
	record := *d.record
	publishCtx := d.ctx
	if publishCtx == nil {
		publishCtx = ctx
	}
	d.published = true
	d.mu.Unlock()
	_ = PublishRecord(publishCtx, deferredRecordWithOutcome(record, err))
}

func (d *DeferredRecord) store(ctx context.Context, record Record) bool {
	d.mu.Lock()
	if d.record != nil || d.published {
		d.mu.Unlock()
		return false
	}
	stored := cloneRecord(record)
	d.record = &stored
	if ctx != nil {
		d.ctx = context.WithoutCancel(ctx)
	}
	if !d.decided {
		d.mu.Unlock()
		return true
	}
	err := d.failure
	publishCtx := d.ctx
	if publishCtx == nil {
		publishCtx = ctx
	}
	d.published = true
	d.mu.Unlock()
	_ = PublishRecord(publishCtx, deferredRecordWithOutcome(stored, err))
	return true
}

func deferredRecordWithOutcome(record Record, err error) Record {
	if err == nil {
		return record
	}
	record.Failed = true
	record.Completed = false
	if strings.TrimSpace(record.Fail.Body) == "" {
		record.Fail.Body = strings.TrimSpace(err.Error())
	}
	if record.Fail.StatusCode == 0 {
		record.Fail.StatusCode = clienterror.HTTPStatusFromError(err)
	}
	return record
}

func prepareRecordForDelivery(ctx context.Context, record Record) Record {
	record = cloneRecord(record)
	if record.ExecutionID = strings.TrimSpace(record.ExecutionID); record.ExecutionID == "" {
		record.ExecutionID = ExecutionIDFromContext(ctx)
	}
	if record.ExecutionID == "" {
		record.ExecutionID = uuid.NewString()
	}
	if record.RequestID = strings.TrimSpace(record.RequestID); record.RequestID == "" {
		record.RequestID = strings.TrimSpace(internallogging.GetRequestID(ctx))
	}
	record.BaseURL = strings.TrimSpace(record.BaseURL)
	if !record.Stream {
		record.Stream = StreamFromContext(ctx)
	}

	metadata := internallogging.GetClientRequestMetadata(ctx)
	sessionID := coresession.NormalizeToCanonicalUUID(strings.TrimSpace(record.SessionID))
	parentSessionID := coresession.NormalizeToCanonicalUUID(strings.TrimSpace(record.ParentSessionID))
	fallbackSessionID := coresession.NormalizeToCanonicalUUID(strings.TrimSpace(metadata.SessionID))
	fallbackParentSessionID := coresession.NormalizeToCanonicalUUID(strings.TrimSpace(metadata.ParentSessionID))
	if sessionID == "" {
		sessionID = fallbackSessionID
		parentSessionID = fallbackParentSessionID
	} else if parentSessionID == "" && sessionID == fallbackSessionID {
		parentSessionID = fallbackParentSessionID
	}
	if sessionID == "" || sessionID == parentSessionID {
		parentSessionID = ""
	}
	record.SessionID = sessionID
	record.ParentSessionID = parentSessionID
	return record
}

func cloneRecord(record Record) Record {
	record.ResponseHeaders = record.ResponseHeaders.Clone()
	if record.Generate != nil {
		generate := *record.Generate
		record.Generate = &generate
	}
	if record.Thinking != nil {
		thinking := *record.Thinking
		if record.Thinking.Budget != nil {
			budget := *record.Thinking.Budget
			thinking.Budget = &budget
		}
		record.Thinking = &thinking
	}
	return record
}

// AddNonNegativeInt64 adds non-negative values without integer wraparound.
func AddNonNegativeInt64(values ...int64) (int64, bool) {
	var total int64
	for _, value := range values {
		if value < 0 || total > math.MaxInt64-value {
			return 0, false
		}
		total += value
	}
	return total, true
}

// Failure holds HTTP failure metadata for an upstream request attempt.
type Failure struct {
	StatusCode int
	Body       string
}

// Detail holds the token usage breakdown.
type Detail struct {
	InputTokens                int64
	OutputTokens               int64
	ReasoningTokens            int64
	CachedTokens               int64
	CacheReadTokens            int64
	CacheCreationTokens        int64
	CacheCreation5mInputTokens int64
	CacheCreation1hInputTokens int64
	TotalTokens                int64
	TokenBreakdown             TokenBreakdown
	ResponseServiceTier        string
}

// Thinking describes the requested thinking intensity for a usage record.
type Thinking struct {
	Intensity string `json:"intensity,omitempty"`
	Mode      string `json:"mode,omitempty"`
	Level     string `json:"level,omitempty"`
	Budget    *int   `json:"budget,omitempty"`
}

// Plugin consumes usage records emitted by the proxy runtime.
type Plugin interface {
	HandleUsage(ctx context.Context, record Record)
}

type pluginGeneration struct {
	plugin Plugin

	mu      sync.Mutex
	pending int
	retired bool
	done    chan struct{}
}

// Retirement represents a detached named plugin generation. Wait blocks until
// every record that captured the generation at publication time has finished.
type Retirement struct {
	generation *pluginGeneration
}

// Wait waits for the retired generation to drain. It must not be called from
// a callback that captured the same generation.
func (r *Retirement) Wait() {
	_ = r.WaitContext(context.Background())
}

// WaitContext waits for the retired generation to drain or for ctx to end.
func (r *Retirement) WaitContext(ctx context.Context) error {
	if r == nil || r.generation == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-r.generation.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Drained reports whether the retired generation has no captured callbacks left.
func (r *Retirement) Drained() bool {
	if r == nil || r.generation == nil {
		return true
	}
	select {
	case <-r.generation.done:
		return true
	default:
		return false
	}
}

func newPluginGeneration(plugin Plugin) *pluginGeneration {
	return &pluginGeneration{plugin: plugin, done: make(chan struct{})}
}

func (g *pluginGeneration) acquire() bool {
	if g == nil || g.plugin == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.retired {
		return false
	}
	g.pending++
	return true
}

func (g *pluginGeneration) release() {
	if g == nil {
		return
	}
	g.mu.Lock()
	g.pending--
	if g.retired && g.pending == 0 {
		close(g.done)
	}
	g.mu.Unlock()
}

func (g *pluginGeneration) retire() *Retirement {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	if !g.retired {
		g.retired = true
		if g.pending == 0 {
			close(g.done)
		}
	}
	g.mu.Unlock()
	return &Retirement{generation: g}
}

type pluginInvocation struct {
	plugin  Plugin
	release func()
}

type queueItem struct {
	ctx     context.Context
	record  Record
	plugins []pluginInvocation
}

type managerState uint8

const (
	managerStateNew managerState = iota
	managerStateRunning
	managerStateStopping
	managerStateStopped
)

const defaultQueueCapacity = 512

var ErrManagerStopped = errors.New("usage: manager stopped")

// Manager maintains a bounded queue of usage records and delivers them to registered plugins.
type Manager struct {
	capacity int

	mu       sync.Mutex
	cond     *sync.Cond
	queue    []queueItem
	state    managerState
	done     chan struct{}
	stopping chan struct{}
	slots    chan struct{}

	pluginsMu    sync.RWMutex
	plugins      []Plugin
	namedPlugins map[string]*pluginGeneration
}

// NewManager constructs a manager with a bounded queue.
func NewManager(capacity int) *Manager {
	if capacity <= 0 {
		capacity = defaultQueueCapacity
	}
	m := &Manager{capacity: capacity}
	m.cond = sync.NewCond(&m.mu)
	return m
}

// Start launches or restarts the background dispatcher. Calling Start while it
// is already running is safe.
func (m *Manager) Start(_ context.Context) {
	if m == nil {
		return
	}
	m.mu.Lock()
	if m.state == managerStateRunning || m.state == managerStateStopping {
		m.mu.Unlock()
		return
	}
	m.queue = nil
	m.done = make(chan struct{})
	m.stopping = make(chan struct{})
	m.slots = make(chan struct{}, m.capacity)
	m.state = managerStateRunning
	done := m.done
	m.mu.Unlock()
	go m.run(done)
}

// Stop stops the dispatcher and drains the queue.
func (m *Manager) Stop() {
	_ = m.StopContext(context.Background())
}

// StopContext stops the dispatcher and waits up to ctx for active callbacks.
func (m *Manager) StopContext(ctx context.Context) error {
	if m == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	m.mu.Lock()
	switch m.state {
	case managerStateNew, managerStateStopped:
		m.state = managerStateStopped
		m.mu.Unlock()
		return nil
	case managerStateRunning:
		m.state = managerStateStopping
		close(m.stopping)
		m.cond.Broadcast()
	case managerStateStopping:
	}
	done := m.done
	m.mu.Unlock()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Register appends a plugin to the delivery list.
func (m *Manager) Register(plugin Plugin) {
	if m == nil || plugin == nil {
		return
	}
	m.pluginsMu.Lock()
	m.plugins = append(m.plugins, plugin)
	m.pluginsMu.Unlock()
}

// RegisterNamed adds or replaces a plugin identified by name.
func (m *Manager) RegisterNamed(name string, plugin Plugin) {
	_ = m.RegisterNamedWithRetirement(name, plugin)
}

// RegisterNamedWithRetirement adds or replaces a named plugin and returns the
// retirement for the replaced generation. Wait outside a plugin callback stack
// before releasing generation-owned resources.
func (m *Manager) RegisterNamedWithRetirement(name string, plugin Plugin) *Retirement {
	if m == nil || name == "" || plugin == nil {
		return nil
	}
	m.pluginsMu.Lock()
	if m.namedPlugins == nil {
		m.namedPlugins = make(map[string]*pluginGeneration)
	}
	previous := m.namedPlugins[name]
	m.namedPlugins[name] = newPluginGeneration(plugin)
	var retirement *Retirement
	if previous != nil {
		retirement = previous.retire()
	}
	m.pluginsMu.Unlock()
	return retirement
}

// UnregisterNamed detaches a named plugin without affecting records that
// already captured its generation. Wait for the returned retirement outside
// a plugin callback stack before releasing generation-owned resources.
func (m *Manager) UnregisterNamed(name string) *Retirement {
	if m == nil || name == "" {
		return nil
	}
	m.pluginsMu.Lock()
	generation := m.namedPlugins[name]
	delete(m.namedPlugins, name)
	var retirement *Retirement
	if generation != nil {
		retirement = generation.retire()
	}
	m.pluginsMu.Unlock()
	return retirement
}

// Publish enqueues a usage record for processing. It applies backpressure when
// the configured queue capacity is reached and returns when ctx is canceled.
func (m *Manager) Publish(ctx context.Context, record Record) error {
	if m == nil {
		return ErrManagerStopped
	}
	if ctx == nil {
		ctx = context.Background()
	}

	m.mu.Lock()
	if m.state == managerStateNew {
		m.mu.Unlock()
		m.Start(context.Background())
		m.mu.Lock()
	}
	if m.state != managerStateRunning {
		m.mu.Unlock()
		return ErrManagerStopped
	}
	slots := m.slots
	stopping := m.stopping
	m.mu.Unlock()

	item := queueItem{
		ctx:     context.WithoutCancel(ctx),
		record:  prepareRecordForDelivery(ctx, record),
		plugins: m.registeredPlugins(),
	}
	select {
	case slots <- struct{}{}:
	case <-stopping:
		releaseInvocations(item.plugins)
		return ErrManagerStopped
	case <-ctx.Done():
		releaseInvocations(item.plugins)
		return ctx.Err()
	}

	m.mu.Lock()
	if m.state != managerStateRunning || m.slots != slots {
		m.mu.Unlock()
		<-slots
		releaseInvocations(item.plugins)
		return ErrManagerStopped
	}
	m.queue = append(m.queue, item)
	m.cond.Signal()
	m.mu.Unlock()
	return nil
}

func (m *Manager) run(done chan struct{}) {
	defer close(done)
	for {
		m.mu.Lock()
		for m.state == managerStateRunning && len(m.queue) == 0 {
			m.cond.Wait()
		}
		if len(m.queue) == 0 && m.state == managerStateStopping {
			m.state = managerStateStopped
			m.mu.Unlock()
			return
		}
		if len(m.queue) == 0 {
			m.mu.Unlock()
			continue
		}
		item := m.queue[0]
		m.queue[0] = queueItem{}
		m.queue = m.queue[1:]
		slots := m.slots
		m.mu.Unlock()
		<-slots
		m.dispatch(item)
	}
}

func (m *Manager) registeredPlugins() []pluginInvocation {
	m.pluginsMu.RLock()
	plugins := make([]pluginInvocation, 0, len(m.plugins)+len(m.namedPlugins))
	for _, plugin := range m.plugins {
		if plugin != nil {
			plugins = append(plugins, pluginInvocation{plugin: plugin})
		}
	}
	for _, generation := range m.namedPlugins {
		if generation == nil || !generation.acquire() {
			continue
		}
		captured := generation
		plugins = append(
			plugins,
			pluginInvocation{plugin: captured.plugin, release: captured.release},
		)
	}
	m.pluginsMu.RUnlock()
	return plugins
}

func (m *Manager) dispatch(item queueItem) {
	for _, invocation := range item.plugins {
		invokePlugin(invocation, item.ctx, item.record)
	}
}

func invokePlugin(invocation pluginInvocation, ctx context.Context, record Record) {
	if invocation.release != nil {
		defer invocation.release()
	}
	if invocation.plugin != nil {
		safeInvoke(invocation.plugin, ctx, cloneRecord(record))
	}
}

func releaseInvocations(invocations []pluginInvocation) {
	for _, invocation := range invocations {
		if invocation.release != nil {
			invocation.release()
		}
	}
}

func safeInvoke(plugin Plugin, ctx context.Context, record Record) {
	defer func() {
		if r := recover(); r != nil {
			log.Errorf("usage: plugin panic recovered: %v", r)
		}
	}()
	plugin.HandleUsage(ctx, record)
}

var defaultManager = NewManager(512)

// DefaultManager returns the global usage manager instance.
func DefaultManager() *Manager { return defaultManager }

// RegisterPlugin registers a plugin on the default manager.
func RegisterPlugin(plugin Plugin) { DefaultManager().Register(plugin) }

// RegisterNamedPlugin registers or replaces a named plugin on the default manager.
func RegisterNamedPlugin(name string, plugin Plugin) {
	DefaultManager().RegisterNamed(name, plugin)
}

// RegisterNamedPluginWithRetirement registers or replaces a named plugin and
// returns the retirement for the replaced generation.
// noinspection GoUnusedExportedFunction
func RegisterNamedPluginWithRetirement(name string, plugin Plugin) *Retirement {
	return DefaultManager().RegisterNamedWithRetirement(name, plugin)
}

// UnregisterNamedPlugin removes a named plugin from the default manager.
func UnregisterNamedPlugin(name string) *Retirement {
	return DefaultManager().UnregisterNamed(name)
}

func markRequestOutcomeFromRecord(ctx context.Context, record Record) {
	if !record.Failed {
		return
	}
	kind := internallogging.RequestOutcomeUpstreamError
	reason := strings.TrimSpace(record.Fail.Body)
	switch {
	case errors.Is(context.Cause(ctx), context.DeadlineExceeded) ||
		strings.Contains(strings.ToLower(reason), context.DeadlineExceeded.Error()):
		kind = internallogging.RequestOutcomeDeadline
	case errors.Is(context.Cause(ctx), context.Canceled) ||
		record.Fail.StatusCode == clienterror.StatusClientClosedRequest ||
		strings.EqualFold(reason, context.Canceled.Error()):
		kind = internallogging.RequestOutcomeCanceled
	}
	internallogging.MarkRequestOutcomeFailure(ctx, kind, "usage", reason)
}

// PublishRecord publishes a record using the manager assigned to ctx, or the
// default manager when no instance manager is present.
func PublishRecord(ctx context.Context, record Record) error {
	markRequestOutcomeFromRecord(ctx, record)
	manager := ManagerFromContext(ctx)
	if manager == nil {
		manager = DefaultManager()
	}
	if err := manager.Publish(ctx, record); err != nil {
		log.WithError(err).Warn("usage: record was not queued")
		return err
	}
	return nil
}

// StartDefault starts the default manager's dispatcher.
// noinspection GoUnusedExportedFunction
func StartDefault(ctx context.Context) { DefaultManager().Start(ctx) }

// StopDefault stops the default manager's dispatcher.
// noinspection GoUnusedExportedFunction
func StopDefault() { DefaultManager().Stop() }

// StopDefaultContext stops the default manager and waits up to ctx.
// noinspection GoUnusedExportedFunction
func StopDefaultContext(ctx context.Context) error { return DefaultManager().StopContext(ctx) }
