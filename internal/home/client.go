package home

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

const (
	redisKeyConfig             = "config"
	redisChannelConfig         = "config"
	redisKeyUsage              = "usage"
	redisKeyInFlightSnapshot   = "in-flight-snapshot"
	redisKeyConcurrencyRelease = "concurrency-release"
	redisKeyRequestLog         = "request-log"
	redisKeyAppLog             = "app-log"
	redisKeyPluginStatus       = "plugin-status"
	redisKeyPluginTasks        = "plugin-tasks"

	homeReconnectFailoverThreshold = 3
	homeRedisOperationTimeout      = 3 * time.Second
	homeRefreshOperationTimeout    = 35 * time.Second
	homeSubscriptionReceiveTimeout = 3 * time.Second
	homeRequestLogLookupBatchSize  = 8
	homeRequestLogLookupMaxEntries = 4096
	redisChannelCluster            = "cluster"
)

// dispatchError classifies whether Home may have processed an auth dispatch request.
type dispatchError struct {
	Err       error
	Ambiguous bool
}

func (e *dispatchError) Error() string {
	if e == nil || e.Err == nil {
		return "home auth dispatch failed"
	}
	return e.Err.Error()
}

func (e *dispatchError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// newAmbiguousDispatchError marks a post-send transport failure as requiring a client abort.
func newAmbiguousDispatchError(err error) error {
	if err == nil {
		return nil
	}
	return &dispatchError{Err: err, Ambiguous: true}
}

// IsAmbiguousDispatchError reports whether Home may have processed the dispatch request.
func IsAmbiguousDispatchError(err error) bool {
	var dispatchErr *dispatchError
	return errors.As(err, &dispatchErr) && dispatchErr.Ambiguous
}

var errClusterDiscoveryTransport = errors.New("home cluster discovery transport failed")

var (
	ErrDisabled                  = errors.New("home client disabled")
	ErrNotConnected              = errors.New("home not connected")
	errEmptyResponse             = errors.New("home returned empty response")
	ErrAuthNotFound              = errors.New("home auth not found")
	errConfigNotFound            = errors.New("home config not found")
	errModelsNotFound            = errors.New("home models not found")
	errDispatchFenced            = errors.New("home auth dispatch is fenced")
	ErrCompareAndSwapUnsupported = errors.New("home compare-and-swap is unsupported")
)

func isHomeCommandUnsupported(err error) bool {
	for err != nil {
		message := strings.ToLower(strings.TrimSpace(err.Error()))
		if strings.Contains(message, "unknown command") || strings.Contains(message, "unsupported command") {
			return true
		}
		err = errors.Unwrap(err)
	}
	return false
}

// IsMembershipTakeoverUnavailableError reports whether Home cannot preserve the previous membership state.
func IsMembershipTakeoverUnavailableError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.TrimSpace(strings.ToLower(err.Error()))
	return message == "membership_takeover_unavailable" || message == "err membership_takeover_unavailable"
}

// IsLegacyMembershipProtocolError reports whether Home rejected the secure subscription argument count.
func IsLegacyMembershipProtocolError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.TrimSpace(strings.ToLower(err.Error()))
	return message == "wrong number of arguments for 'subscribe' command" || message == "err wrong number of arguments for 'subscribe' command"
}

type clusterNode struct {
	IP          string    `json:"ip"`
	Port        int       `json:"port"`
	ClientCount int       `json:"client_count"`
	IsMaster    bool      `json:"is_master"`
	LastSeenAt  time.Time `json:"last_seen_at"`
}

type clusterNodesEnvelope struct {
	OK    bool          `json:"ok"`
	Nodes []clusterNode `json:"nodes"`
}

type PluginTask struct {
	ID             uint      `json:"id"`
	Operation      string    `json:"operation"`
	PluginID       string    `json:"plugin_id"`
	TargetNodeType string    `json:"target_node_type,omitempty"`
	TargetNodeID   string    `json:"target_node_id,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

type KVSetOptions struct {
	EX time.Duration
	PX time.Duration
	NX bool
	XX bool
}

type homeSubscription interface {
	ReceiveTimeout(context.Context, time.Duration) (any, error)
	Ping(context.Context, ...string) error
}

type subscriptionCloser interface {
	Close() error
}

type recoveryState uint32

const (
	recoveryStateStable recoveryState = iota
	recoveryStateTakeoverEligible
	recoveryStateSwitching
	recoveryStateSwitchingTakeover
)

type Client struct {
	mu sync.Mutex

	homeCfg  config.HomeConfig
	seedHost string
	seedPort int

	cmd         *redis.Client
	cmdOptions  *redis.Options
	sub         *redis.Client
	release     *redis.Client
	connections map[*homeDispatchConn]struct{}
	closing     chan struct{}
	lifecycle   config.CredentialConcurrencyConfig
	limiter     atomic.Pointer[config.CredentialConcurrencyConfig]
	managed     bool

	heartbeatOK          atomic.Bool
	dispatchFenced       atomic.Bool
	ambiguousDispatch    atomic.Bool
	casUnsupported       atomic.Bool
	testOperationTimeout time.Duration
	recoveryState        atomic.Uint32
	instanceID           string
	legacyMembership     bool
	clusterNodes         []clusterNode
	reconnectFailures    int
}

func New(homeCfg config.HomeConfig) *Client {
	return &Client{
		homeCfg:    homeCfg,
		seedHost:   strings.TrimSpace(homeCfg.Host),
		seedPort:   homeCfg.Port,
		instanceID: uuid.NewString(),
	}
}

// NewLifetime creates a fresh client while preserving cluster failover state.
func (c *Client) NewLifetime() *Client {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	next := &Client{
		homeCfg:              c.homeCfg,
		seedHost:             c.seedHost,
		seedPort:             c.seedPort,
		clusterNodes:         append([]clusterNode(nil), c.clusterNodes...),
		reconnectFailures:    c.reconnectFailures,
		testOperationTimeout: c.testOperationTimeout,
		instanceID:           c.instanceID,
		legacyMembership:     c.legacyMembership,
	}
	next.recoveryState.Store(c.recoveryState.Load())
	return next
}

// membershipInstanceID returns the process-scoped Home membership identity.
func (c *Client) membershipInstanceID() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.instanceID
}

// LegacyMembership reports whether this subscriber has downgraded to the legacy protocol.
func (c *Client) LegacyMembership() bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.legacyMembership
}

// EnableLegacyMembership permanently downgrades this subscriber lifetime chain.
func (c *Client) EnableLegacyMembership() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.legacyMembership = true
	c.mu.Unlock()
	c.SuppressTakeover()
}

func (c *Client) Enabled() bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.homeCfg.Enabled
}

func (c *Client) HeartbeatOK() bool {
	if c == nil {
		return false
	}
	if !c.Enabled() {
		return false
	}
	return c.heartbeatOK.Load()
}

// Close permanently ends this client's dispatch lifetime.
func (c *Client) Close() {
	if c == nil {
		return
	}
	c.dispatchFenced.Store(true)
	c.heartbeatOK.Store(false)
	c.mu.Lock()
	commandClient, subscriptionClient, connections := c.detachClientsLocked()
	releaseClient := c.release
	c.release = nil
	closing := c.closing
	c.mu.Unlock()
	closeDetachedClients(commandClient, subscriptionClient, connections)
	if releaseClient != nil {
		_ = releaseClient.Close()
	}
	if closing != nil {
		<-closing
	}
}

// closeBootstrapPools replaces private bootstrap pools without ending the client lifetime.
func (c *Client) closeBootstrapPools() {
	if c == nil {
		return
	}
	c.heartbeatOK.Store(false)
	c.mu.Lock()
	commandClient, subscriptionClient, connections := c.detachClientsLocked()
	c.mu.Unlock()
	closeDetachedClients(commandClient, subscriptionClient, connections)
}

// AbortAmbiguousDispatch fences this client after an auth dispatch response is ambiguous.
func (c *Client) AbortAmbiguousDispatch() {
	if c == nil {
		return
	}
	c.ambiguousDispatch.Store(true)
	c.dispatchFenced.Store(true)
	c.heartbeatOK.Store(false)
	c.mu.Lock()
	commandClient, subscriptionClient, connections := c.detachClientsLocked()
	releaseClient := c.release
	c.release = nil
	c.mu.Unlock()
	for _, conn := range connections {
		_ = closeUnderlyingTransport(conn)
	}
	if commandClient != nil {
		go func() {
			_ = commandClient.Close()
		}()
	}
	if subscriptionClient != nil {
		go func() {
			_ = subscriptionClient.Close()
		}()
	}
	if releaseClient != nil {
		go func() {
			_ = releaseClient.Close()
		}()
	}
}

// AmbiguousDispatch reports whether this lifetime observed an issued dispatch with an unknown delivery result.
func (c *Client) AmbiguousDispatch() bool {
	return c != nil && c.ambiguousDispatch.Load()
}

// SuppressTakeover forces the next subscriber lifetime through normal membership recovery.
func (c *Client) SuppressTakeover() {
	if c == nil {
		return
	}
	if !c.recoveryState.CompareAndSwap(uint32(recoveryStateTakeoverEligible), uint32(recoveryStateStable)) {
		c.recoveryState.CompareAndSwap(uint32(recoveryStateSwitchingTakeover), uint32(recoveryStateSwitching))
	}
}

func (c *Client) detachClientsLocked() (*redis.Client, *redis.Client, []*homeDispatchConn) {
	connections := make([]*homeDispatchConn, 0, len(c.connections))
	for conn := range c.connections {
		connections = append(connections, conn)
	}
	commandClient := c.cmd
	subscriptionClient := c.sub
	c.cmd = nil
	c.cmdOptions = nil
	c.sub = nil
	c.connections = nil
	return commandClient, subscriptionClient, connections
}

func closeDetachedClients(
	commandClient *redis.Client,
	subscriptionClient *redis.Client,
	connections []*homeDispatchConn,
) {
	for _, conn := range connections {
		_ = closeUnderlyingTransport(conn)
	}
	if commandClient != nil {
		_ = commandClient.Close()
	}
	if subscriptionClient != nil {
		_ = subscriptionClient.Close()
	}
}

func (c *Client) closeClientsLocked() {
	commandClient, subscriptionClient, connections := c.detachClientsLocked()
	releaseClient := c.release
	c.release = nil
	previousClosing := c.closing
	done := make(chan struct{})
	c.closing = done
	go func() {
		defer close(done)
		if previousClosing != nil {
			<-previousClosing
		}
		closeDetachedClients(commandClient, subscriptionClient, connections)
		if releaseClient != nil {
			_ = releaseClient.Close()
		}
	}()
}

func (c *Client) waitForClientsClosed() {
	for {
		c.mu.Lock()
		closing := c.closing
		c.mu.Unlock()
		if closing == nil {
			return
		}
		<-closing
		c.mu.Lock()
		if c.closing == closing {
			c.closing = nil
			c.mu.Unlock()
			return
		}
		c.mu.Unlock()
	}
}

// SetManagedLifetime defers client shutdown to the Service lifetime owner.
func (c *Client) SetManagedLifetime(managed bool) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.managed = managed
	c.mu.Unlock()
}

func (c *Client) managedLifetime() bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.managed
}

func (c *Client) addr() (string, bool) {
	if c == nil {
		return "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.addrLocked()
}

func (c *Client) addrLocked() (string, bool) {
	host := strings.TrimSpace(c.homeCfg.Host)
	if host == "" {
		return "", false
	}
	if c.homeCfg.Port <= 0 {
		return "", false
	}
	return net.JoinHostPort(host, strconv.Itoa(c.homeCfg.Port)), true
}

func (c *Client) ensureClients() error {
	if c == nil {
		return ErrDisabled
	}
	if c.dispatchFenced.Load() {
		return errDispatchFenced
	}
	if !c.Enabled() {
		return ErrDisabled
	}
	c.waitForClientsClosed()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dispatchFenced.Load() {
		return errDispatchFenced
	}

	addr, ok := c.addrLocked()
	if !ok {
		return fmt.Errorf("home: invalid address (host=%q port=%d)", c.homeCfg.Host, c.homeCfg.Port)
	}

	if c.cmd == nil {
		options, errOptions := c.redisOptionsLocked(addr)
		if errOptions != nil {
			return errOptions
		}
		c.cmdOptions = cloneRedisOptions(options)
		c.cmd = redis.NewClient(options)
	}
	if c.sub == nil {
		options, errOptions := c.redisOptionsLocked(addr)
		if errOptions != nil {
			return errOptions
		}
		c.sub = redis.NewClient(options)
	}
	return nil
}

func (c *Client) redisOptionsLocked(addr string) (*redis.Options, error) {
	tlsConfig, errTLS := c.homeTLSConfigLocked(addr)
	if errTLS != nil {
		return nil, errTLS
	}
	dialTimeout := homeRedisOperationTimeout
	readTimeout := homeRedisOperationTimeout
	writeTimeout := homeRedisOperationTimeout
	if c.testOperationTimeout > 0 {
		dialTimeout = c.testOperationTimeout
		readTimeout = c.testOperationTimeout
		writeTimeout = c.testOperationTimeout
	} else if c.cmdOptions != nil {
		if c.cmdOptions.DialTimeout > 0 {
			dialTimeout = c.cmdOptions.DialTimeout
		}
		if c.cmdOptions.ReadTimeout > 0 {
			readTimeout = c.cmdOptions.ReadTimeout
		}
		if c.cmdOptions.WriteTimeout > 0 {
			writeTimeout = c.cmdOptions.WriteTimeout
		}
	}
	options := &redis.Options{
		Addr:                  addr,
		TLSConfig:             tlsConfig,
		DialTimeout:           dialTimeout,
		ReadTimeout:           readTimeout,
		WriteTimeout:          writeTimeout,
		MaxRetries:            -1,
		DialerRetries:         1,
		ContextTimeoutEnabled: true,
	}
	options.Dialer = c.trackedRedisDialer(redis.NewDialer(options))
	return options, nil
}

type homeDispatchConn struct {
	net.Conn
	client *Client
	once   sync.Once
}

func (c *Client) trackedRedisDialer(
	dialer func(context.Context, string, string) (
		net.Conn,
		error,
	),
) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network string, address string) (net.Conn, error) {
		conn, errDial := dialer(ctx, network, address)
		if errDial != nil {
			return nil, errDial
		}
		wrapped := &homeDispatchConn{Conn: conn, client: c}
		if c == nil {
			return wrapped, nil
		}
		c.mu.Lock()
		if c.dispatchFenced.Load() {
			c.mu.Unlock()
			_ = wrapped.Close()
			return nil, errDispatchFenced
		}
		if c.connections == nil {
			c.connections = make(map[*homeDispatchConn]struct{})
		}
		c.connections[wrapped] = struct{}{}
		c.mu.Unlock()
		return wrapped, nil
	}
}

func (c *homeDispatchConn) untrack() {
	if c == nil {
		return
	}
	c.once.Do(
		func() {
			if c.client != nil {
				c.client.mu.Lock()
				delete(c.client.connections, c)
				c.client.mu.Unlock()
			}
		},
	)
}

func (c *homeDispatchConn) Close() error {
	if c == nil || c.Conn == nil {
		return net.ErrClosed
	}
	c.untrack()
	return c.Conn.Close()
}

func (c *homeDispatchConn) NetConn() net.Conn {
	if c == nil {
		return nil
	}
	return c.Conn
}

func closeUnderlyingTransport(conn net.Conn) error {
	if conn == nil {
		return net.ErrClosed
	}
	current := conn
	for {
		if dispatchConn, ok := current.(*homeDispatchConn); ok {
			dispatchConn.untrack()
		}
		unwrapper, ok := current.(interface{ NetConn() net.Conn })
		if !ok {
			break
		}
		next := unwrapper.NetConn()
		if next == nil || next == current {
			break
		}
		current = next
	}
	return current.Close()
}

func cloneRedisOptions(options *redis.Options) *redis.Options {
	if options == nil {
		return nil
	}
	cloned := *options
	if options.TLSConfig != nil {
		cloned.TLSConfig = options.TLSConfig.Clone()
	}
	if options.MaintNotificationsConfig != nil {
		maintenanceNotifications := *options.MaintNotificationsConfig
		cloned.MaintNotificationsConfig = &maintenanceNotifications
	}
	return &cloned
}

func (c *Client) homeTLSConfigLocked(addr string) (*tls.Config, error) {
	serverName := strings.TrimSpace(c.homeCfg.TLS.ServerName)
	if serverName == "" {
		if c.homeCfg.TLS.UseTargetServerName {
			serverName = hostFromAddress(addr)
		} else {
			serverName = strings.TrimSpace(c.seedHost)
		}
	}
	if serverName == "" {
		serverName = strings.TrimSpace(c.homeCfg.Host)
	}
	return newHomeTLSConfig(c.homeCfg.TLS, serverName)
}

func hostFromAddress(addr string) string {
	host, _, errSplit := net.SplitHostPort(strings.TrimSpace(addr))
	if errSplit == nil {
		return strings.TrimSpace(host)
	}
	return strings.TrimSpace(addr)
}

func newHomeTLSConfig(cfg config.HomeTLSConfig, fallbackServerName string) (*tls.Config, error) {
	if !cfg.Enable {
		return nil, nil
	}

	serverName := strings.TrimSpace(cfg.ServerName)
	if serverName == "" {
		serverName = strings.TrimSpace(fallbackServerName)
	}

	tlsConfig := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		ServerName:         serverName,
		InsecureSkipVerify: cfg.InsecureSkipVerify,
	}

	clientCertPath := strings.TrimSpace(cfg.ClientCert)
	clientKeyPath := strings.TrimSpace(cfg.ClientKey)
	if clientCertPath != "" || clientKeyPath != "" {
		if clientCertPath == "" || clientKeyPath == "" {
			return nil, fmt.Errorf("home tls: client certificate and key must be set together")
		}
		certPair, errLoad := tls.LoadX509KeyPair(clientCertPath, clientKeyPath)
		if errLoad != nil {
			return nil, fmt.Errorf("home tls: load client certificate: %w", errLoad)
		}
		tlsConfig.Certificates = []tls.Certificate{certPair}
	}

	caCertPath := strings.TrimSpace(cfg.CACert)
	if caCertPath == "" {
		return tlsConfig, nil
	}

	caCertPEM, errRead := os.ReadFile(caCertPath)
	if errRead != nil {
		return nil, fmt.Errorf("home tls: read ca-cert: %w", errRead)
	}

	certPool, errPool := x509.SystemCertPool()
	if errPool != nil || certPool == nil {
		certPool = x509.NewCertPool()
	}
	if !certPool.AppendCertsFromPEM(caCertPEM) {
		return nil, fmt.Errorf("home tls: ca-cert contains no PEM certificates")
	}
	tlsConfig.RootCAs = certPool

	return tlsConfig, nil
}

func (c *Client) commandClient() (*redis.Client, error) {
	if c == nil || c.dispatchFenced.Load() {
		return nil, errDispatchFenced
	}
	if errEnsure := c.ensureClients(); errEnsure != nil {
		return nil, errEnsure
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dispatchFenced.Load() {
		return nil, errDispatchFenced
	}
	if c.cmd == nil {
		return nil, ErrNotConnected
	}
	return c.cmd, nil
}

func (c *Client) subscriptionClient() (*redis.Client, error) {
	if errEnsure := c.ensureClients(); errEnsure != nil {
		return nil, errEnsure
	}
	c.mu.Lock()
	sub := c.sub
	c.mu.Unlock()
	if sub == nil {
		return nil, ErrNotConnected
	}
	return sub, nil
}

func (c *Client) Ping(ctx context.Context) error {
	cmd, errClient := c.commandClient()
	if errClient != nil {
		return errClient
	}
	return cmd.Ping(ctx).Err()
}

func (c *Client) clusterDiscoveryEnabled() bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.clusterDiscoveryEnabledLocked()
}

func (c *Client) clusterDiscoveryEnabledLocked() bool {
	return !c.homeCfg.DisableClusterDiscovery
}

func (c *Client) refreshBestClusterNode(ctx context.Context) error {
	if !c.clusterDiscoveryEnabled() {
		return nil
	}
	switched, errRefresh := c.refreshClusterNodes(ctx)
	if errRefresh != nil {
		log.Debugf("home cluster nodes unavailable: %v", errRefresh)
		return errRefresh
	}
	if switched {
		if addr, ok := c.addr(); ok {
			log.Infof("home cluster target switched to %s", addr)
		}
	}
	return nil
}

func (c *Client) refreshClusterNodes(ctx context.Context) (bool, error) {
	if !c.clusterDiscoveryEnabled() {
		return false, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	cmd, errClient := c.commandClient()
	if errClient != nil {
		return false, fmt.Errorf("%w: %w", errClusterDiscoveryTransport, errClient)
	}
	nodesCommand := cmd.Do(ctx, "CLUSTER", "NODES")
	errDo := nodesCommand.Err()
	if errDo != nil {
		if _, ok := errors.AsType[redis.Error](errDo); !ok {
			return false, fmt.Errorf("%w: %w", errClusterDiscoveryTransport, errDo)
		}
		return false, errDo
	}
	raw, errText := nodesCommand.Text()
	if errText != nil {
		return false, errText
	}

	nodes, errParse := parseClusterNodesPayload([]byte(raw))
	if errParse != nil {
		return false, errParse
	}
	if len(nodes) == 0 {
		return false, nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.clusterNodes = nodes
	c.reconnectFailures = 0
	return c.switchToNodeLocked(nodes[0]), nil
}

func parseClusterNodesPayload(raw []byte) ([]clusterNode, error) {
	var envelope clusterNodesEnvelope
	if errUnmarshal := json.Unmarshal(raw, &envelope); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	return normalizeClusterNodes(envelope.Nodes), nil
}

func (c *Client) updateClusterNodesFromPayload(raw []byte) error {
	if c == nil || !c.clusterDiscoveryEnabled() {
		return nil
	}
	nodes, errParse := parseClusterNodesPayload(raw)
	if errParse != nil {
		return errParse
	}
	c.mu.Lock()
	c.clusterNodes = nodes
	c.mu.Unlock()
	return nil
}

func normalizeClusterNodes(nodes []clusterNode) []clusterNode {
	out := make([]clusterNode, 0, len(nodes))
	for _, node := range nodes {
		node.IP = strings.TrimSpace(node.IP)
		if node.IP == "" || node.Port <= 0 {
			continue
		}
		if node.ClientCount < 0 {
			node.ClientCount = 0
		}
		out = append(out, node)
	}
	sort.SliceStable(
		out, func(i, j int) bool {
			return out[i].ClientCount < out[j].ClientCount
		},
	)
	return out
}

func (c *Client) switchToNodeLocked(node clusterNode) bool {
	host := strings.TrimSpace(node.IP)
	if host == "" || node.Port <= 0 {
		return false
	}
	if strings.TrimSpace(c.homeCfg.Host) == host && c.homeCfg.Port == node.Port {
		return false
	}
	c.homeCfg.Host = host
	c.homeCfg.Port = node.Port
	if !c.recoveryState.CompareAndSwap(uint32(recoveryStateStable), uint32(recoveryStateSwitching)) {
		c.recoveryState.CompareAndSwap(uint32(recoveryStateTakeoverEligible), uint32(recoveryStateSwitchingTakeover))
	}
	c.closeClientsLocked()
	return true
}

func (c *Client) markReconnectFailure(reason string) {
	switched, addr := c.failoverAfterReconnectFailure()
	if switched {
		log.Warnf("home control center unavailable after repeated %s failures; switching to %s", reason, addr)
	}
}

func (c *Client) failoverAfterReconnectFailure() (bool, string) {
	if c == nil {
		return false, ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.clusterDiscoveryEnabledLocked() {
		c.reconnectFailures = 0
		return false, ""
	}
	c.reconnectFailures++
	if c.reconnectFailures < homeReconnectFailoverThreshold {
		return false, ""
	}
	c.reconnectFailures = 0

	return c.switchToNextNodeLocked()
}

func (c *Client) failoverAfterSubscriptionTimeout() (bool, string) {
	if c == nil {
		return false, ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.clusterDiscoveryEnabledLocked() {
		c.reconnectFailures = 0
		return false, ""
	}
	c.reconnectFailures = 0
	return c.switchToNextNodeLocked()
}

func (c *Client) switchToNextNodeLocked() (bool, string) {
	currentHost := strings.TrimSpace(c.homeCfg.Host)
	currentPort := c.homeCfg.Port
	candidates := append([]clusterNode(nil), c.clusterNodes...)
	if strings.TrimSpace(c.seedHost) != "" && c.seedPort > 0 {
		candidates = append(candidates, clusterNode{IP: c.seedHost, Port: c.seedPort})
	}
	for _, node := range candidates {
		host := strings.TrimSpace(node.IP)
		if host == "" || node.Port <= 0 {
			continue
		}
		if host == currentHost && node.Port == currentPort {
			continue
		}
		if c.switchToNodeLocked(clusterNode{IP: host, Port: node.Port}) {
			addr, _ := c.addrLocked()
			return true, addr
		}
	}
	return false, ""
}

func (c *Client) markSubscriptionTimeout() {
	switched, addr := c.failoverAfterSubscriptionTimeout()
	if switched {
		log.Warnf("home subscription heartbeat timeout; switching to %s", addr)
	}
}

func (c *Client) resetReconnectFailures() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.reconnectFailures = 0
	c.mu.Unlock()
}

func (c *Client) GetConfig(ctx context.Context) ([]byte, error) {
	if errRefresh := c.refreshBestClusterNode(ctx); errors.Is(errRefresh, errClusterDiscoveryTransport) {
		return nil, errRefresh
	}
	cmd, errClient := c.commandClient()
	if errClient != nil {
		return nil, errClient
	}
	raw, err := cmd.Get(ctx, redisKeyConfig).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, errConfigNotFound
	}
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, errEmptyResponse
	}
	return raw, nil
}

func (c *Client) GetModels(ctx context.Context) ([]byte, error) {
	return c.GetModelsWithRequest(ctx, nil, nil)
}

func (c *Client) GetModelsWithRequest(ctx context.Context, headers http.Header, query url.Values) ([]byte, error) {
	cmd, errClient := c.commandClient()
	if errClient != nil {
		return nil, errClient
	}
	req := modelsRequest{
		Type:    "models",
		Headers: headersToLowerMap(headers),
		Query:   queryToLowerMap(query),
	}
	keyBytes, err := json.Marshal(&req)
	if err != nil {
		return nil, err
	}
	raw, err := cmd.Get(ctx, string(keyBytes)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, errModelsNotFound
	}
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, errEmptyResponse
	}
	return raw, nil
}

func buildKVSetArgs(key string, value []byte, opts KVSetOptions) ([]any, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, fmt.Errorf("home kv: key is empty")
	}
	if opts.EX > 0 && opts.PX > 0 {
		return nil, fmt.Errorf("home kv: EX and PX are mutually exclusive")
	}
	if opts.EX < 0 || opts.PX < 0 {
		return nil, fmt.Errorf("home kv: ttl must not be negative")
	}
	if opts.NX && opts.XX {
		return nil, fmt.Errorf("home kv: NX and XX are mutually exclusive")
	}

	args := []any{key, append([]byte(nil), value...)}
	if opts.EX > 0 {
		args = append(args, "EX", durationCeil(opts.EX, time.Second))
	}
	if opts.PX > 0 {
		args = append(args, "PX", durationCeil(opts.PX, time.Millisecond))
	}
	if opts.NX {
		args = append(args, "NX")
	}
	if opts.XX {
		args = append(args, "XX")
	}
	return args, nil
}

func durationCeil(value time.Duration, unit time.Duration) int64 {
	if value <= 0 || unit <= 0 {
		return 0
	}
	return int64((value + unit - 1) / unit)
}

func (c *Client) KVGet(ctx context.Context, key string) ([]byte, bool, error) {
	cmd, errClient := c.commandClient()
	if errClient != nil {
		return nil, false, errClient
	}
	raw, errGet := cmd.Get(ctx, key).Bytes()
	if errors.Is(errGet, redis.Nil) {
		return nil, false, nil
	}
	if errGet != nil {
		return nil, false, errGet
	}
	return append([]byte(nil), raw...), true, nil
}

func (c *Client) KVSet(ctx context.Context, key string, value []byte, opts KVSetOptions) (bool, error) {
	cmd, errClient := c.commandClient()
	if errClient != nil {
		return false, errClient
	}
	args, errArgs := buildKVSetArgs(key, value, opts)
	if errArgs != nil {
		return false, errArgs
	}
	result, errSet := cmd.Do(ctx, append([]any{"SET"}, args...)...).Result()
	if errors.Is(errSet, redis.Nil) {
		return false, nil
	}
	if errSet != nil {
		return false, errSet
	}
	if result == nil {
		return false, nil
	}
	return true, nil
}

func (c *Client) KVSetNX(ctx context.Context, key string, value []byte, ttl time.Duration) (bool, error) {
	opts := KVSetOptions{NX: true}
	if ttl > 0 {
		opts.EX = ttl
	}
	return c.KVSet(ctx, key, value, opts)
}

// KVCompareAndSwap atomically replaces a value only when its current state matches the expected state.
//
// Home accepts CAS <key> <expected-exists 0|1> <expected-value> <new-value> [PX <ttl-ms>].
// A Home deployment without CAS support is probed once per client lifetime.
func (c *Client) KVCompareAndSwap(
	ctx context.Context,
	key string,
	expected []byte,
	expectedExists bool,
	value []byte,
	ttl time.Duration,
) (bool, error) {
	if c == nil {
		return false, ErrNotConnected
	}
	if c.casUnsupported.Load() {
		return false, ErrCompareAndSwapUnsupported
	}
	cmd, errClient := c.commandClient()
	if errClient != nil {
		return false, errClient
	}
	expectedFlag := "0"
	if expectedExists {
		expectedFlag = "1"
	}
	args := []any{"CAS", key, expectedFlag, expected, value}
	if milliseconds := durationCeil(ttl, time.Millisecond); milliseconds > 0 {
		args = append(args, "PX", milliseconds)
	}
	result, errCAS := cmd.Do(ctx, args...).Int64()
	if errCAS != nil {
		if isHomeCommandUnsupported(errCAS) {
			if c.casUnsupported.CompareAndSwap(false, true) {
				log.Warn("home kv: this Home does not implement CAS; reasoning replay is disabled until the next Home lifetime")
			}
			return false, ErrCompareAndSwapUnsupported
		}
		return false, errCAS
	}
	return result == 1, nil
}

func (c *Client) KVDel(ctx context.Context, keys ...string) (int64, error) {
	if len(keys) == 0 {
		return 0, nil
	}
	cmd, errClient := c.commandClient()
	if errClient != nil {
		return 0, errClient
	}
	return cmd.Del(ctx, keys...).Result()
}

func (c *Client) KVExpire(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	cmd, errClient := c.commandClient()
	if errClient != nil {
		return false, errClient
	}
	return cmd.Expire(ctx, key, ttl).Result()
}

func headersToLowerMap(headers http.Header) map[string]string {
	if len(headers) == 0 {
		return nil
	}
	out := make(map[string]string, len(headers))
	for key, values := range headers {
		k := strings.ToLower(strings.TrimSpace(key))
		if !includeHomeDispatchHeader(k) {
			continue
		}
		if len(values) == 0 {
			out[k] = ""
			continue
		}
		trimmed := make([]string, 0, len(values))
		for _, v := range values {
			trimmed = append(trimmed, strings.TrimSpace(v))
		}
		out[k] = strings.Join(trimmed, ", ")
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func includeHomeDispatchHeader(key string) bool {
	switch key {
	case "authorization", "x-api-key", "x-goog-api-key", "user-agent", "x-codex-client", "x-codex-client-version", "x-goog-api-client", "anthropic-version", "anthropic-beta":
		return true
	default:
		return false
	}
}

func queryToLowerMap(query url.Values) map[string]string {
	if len(query) == 0 {
		return nil
	}
	out := make(map[string]string, len(query))
	for key, values := range query {
		k := strings.ToLower(strings.TrimSpace(key))
		if k == "" {
			continue
		}
		if len(values) == 0 {
			out[k] = ""
			continue
		}
		trimmed := make([]string, 0, len(values))
		for _, v := range values {
			trimmed = append(trimmed, strings.TrimSpace(v))
		}
		out[k] = strings.Join(trimmed, ", ")
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func newAuthDispatchRequest(
	requestedModel, sessionID string,
	headers http.Header,
	count int,
	constraints ...AuthDispatchConstraints,
) authDispatchRequest {
	if count <= 0 {
		count = 1
	}
	constraint := AuthDispatchConstraints{}
	if len(constraints) > 0 {
		constraint = constraints[0]
	}
	return authDispatchRequest{
		Type:                "auth",
		Model:               requestedModel,
		Count:               count,
		ConcurrencyProtocol: 1,
		SessionID:           strings.TrimSpace(sessionID),
		ParentSessionID:     strings.TrimSpace(constraint.ParentSessionID),
		Headers:             headersToLowerMap(headers),
		CredentialPolicy:    strings.TrimSpace(constraint.CredentialPolicy),
		RetryRound:          max(constraint.RetryRound, 0),
		ExcludedAuthIDs:     append([]string(nil), constraint.ExcludedAuthIDs...),
		PinnedAuthID:        strings.TrimSpace(constraint.PinnedAuthID),
	}
}

func (c *Client) RPopAuth(
	ctx context.Context,
	requestedModel, sessionID string,
	headers http.Header,
	count int,
) ([]byte, error) {
	return c.rPopAuth(ctx, requestedModel, sessionID, headers, count, AuthDispatchConstraints{})
}

// RPopAuthWithPolicy requests a Home credential constrained by the supplied fixed policy.
func (c *Client) RPopAuthWithPolicy(
	ctx context.Context,
	requestedModel, sessionID string,
	headers http.Header,
	count int,
	credentialPolicy string,
) ([]byte, error) {
	return c.rPopAuth(ctx, requestedModel, sessionID, headers, count, AuthDispatchConstraints{CredentialPolicy: credentialPolicy})
}

// RPopAuthWithConstraints requests a credential for one bounded retry round.
func (c *Client) RPopAuthWithConstraints(ctx context.Context, requestedModel, sessionID string, headers http.Header, count int, constraints AuthDispatchConstraints) ([]byte, error) {
	return c.rPopAuth(ctx, requestedModel, sessionID, headers, count, constraints)
}

func (c *Client) rPopAuth(
	ctx context.Context,
	requestedModel, sessionID string,
	headers http.Header,
	count int,
	constraints AuthDispatchConstraints,
) ([]byte, error) {
	if c == nil || c.dispatchFenced.Load() {
		return nil, errDispatchFenced
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if errContext := ctx.Err(); errContext != nil {
		return nil, errContext
	}
	requestedModel = strings.TrimSpace(requestedModel)
	if requestedModel == "" {
		return nil, fmt.Errorf("home: requested model is empty")
	}
	req := newAuthDispatchRequest(requestedModel, sessionID, headers, count, constraints)
	keyBytes, errMarshal := json.Marshal(&req)
	if errMarshal != nil {
		return nil, errMarshal
	}
	if c.dispatchFenced.Load() {
		return nil, errDispatchFenced
	}
	cmd, errClient := c.commandClient()
	if errClient != nil {
		return nil, errClient
	}
	if c.dispatchFenced.Load() {
		return nil, errDispatchFenced
	}
	conn := cmd.Conn()
	defer func() {
		if errClose := conn.Close(); errClose != nil {
			log.WithError(errClose).Debug("Home auth dispatch connection close failed")
		}
	}()
	if errProbe := conn.Ping(ctx).Err(); errProbe != nil {
		return nil, errProbe
	}
	if c.dispatchFenced.Load() {
		return nil, errDispatchFenced
	}
	raw, errRPop := conn.RPop(ctx, string(keyBytes)).Bytes()
	if errors.Is(errRPop, redis.Nil) {
		return nil, ErrAuthNotFound
	}
	if errRPop != nil {
		if isAmbiguousIssuedRPopAuthError(errRPop) {
			return nil, newAmbiguousDispatchError(errRPop)
		}
		return nil, errRPop
	}
	if len(raw) == 0 {
		return nil, errEmptyResponse
	}
	return raw, nil
}

func isAmbiguousIssuedRPopAuthError(err error) bool {
	if err == nil || errors.Is(err, redis.Nil) {
		return false
	}
	var redisErr redis.Error
	return !errors.As(err, &redisErr)
}

func (c *Client) GetRefreshAuth(ctx context.Context, authIndex string, accessTokenSHA256 string) ([]byte, error) {
	cmd, errClient := c.commandClient()
	if errClient != nil {
		return nil, errClient
	}
	authIndex = strings.TrimSpace(authIndex)
	if authIndex == "" {
		return nil, fmt.Errorf("home: auth_index is empty")
	}
	req := refreshRequest{
		Type:                      "refresh",
		AuthIndex:                 authIndex,
		ObservedAccessTokenSHA256: strings.TrimSpace(accessTokenSHA256),
	}
	keyBytes, err := json.Marshal(&req)
	if err != nil {
		return nil, err
	}

	raw, err := cmd.WithTimeout(homeRefreshOperationTimeout).Get(ctx, string(keyBytes)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, ErrAuthNotFound
	}
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, errEmptyResponse
	}
	return raw, nil
}

func (c *Client) LPushUsage(ctx context.Context, payload []byte) error {
	cmd, errClient := c.commandClient()
	if errClient != nil {
		return errClient
	}
	if len(payload) == 0 {
		return nil
	}
	return cmd.LPush(ctx, redisKeyUsage, payload).Err()
}

// LPushInFlightSnapshot publishes a bounded in-flight observation frame.
func (c *Client) LPushInFlightSnapshot(ctx context.Context, payload []byte) error {
	cmd, errClient := c.commandClient()
	if errClient != nil {
		return errClient
	}
	return cmd.LPush(ctx, redisKeyInFlightSnapshot, payload).Err()
}

// PushConcurrencyRelease sends one cumulative concurrency release frame through an independent client.
func (c *Client) PushConcurrencyRelease(ctx context.Context, frame concurrencyReleaseFrame) error {
	if frame.CredentialID == "" || frame.Model == "" || frame.ReleaseSeq <= 0 {
		return fmt.Errorf("invalid concurrency release frame")
	}
	cmd, errClient := c.concurrencyReleaseClient()
	if errClient != nil {
		return errClient
	}
	payload, errMarshal := json.Marshal(frame)
	if errMarshal != nil {
		return fmt.Errorf("marshal concurrency release frame: %w", errMarshal)
	}
	return cmd.Do(ctx, "LPUSH", redisKeyConcurrencyRelease, payload).Err()
}

func (c *Client) concurrencyReleaseClient() (*redis.Client, error) {
	if c == nil || c.dispatchFenced.Load() {
		return nil, errDispatchFenced
	}
	state := recoveryState(c.recoveryState.Load())
	if state == recoveryStateTakeoverEligible || state == recoveryStateSwitching || state == recoveryStateSwitchingTakeover {
		return nil, ErrNotConnected
	}
	if !c.Enabled() {
		return nil, ErrDisabled
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dispatchFenced.Load() {
		return nil, errDispatchFenced
	}
	state = recoveryState(c.recoveryState.Load())
	if state == recoveryStateTakeoverEligible || state == recoveryStateSwitching || state == recoveryStateSwitchingTakeover {
		return nil, ErrNotConnected
	}
	if c.release != nil {
		return c.release, nil
	}
	addr, ok := c.addrLocked()
	if !ok {
		return nil, fmt.Errorf("home: invalid address (host=%q port=%d)", c.homeCfg.Host, c.homeCfg.Port)
	}
	options, errOptions := c.redisOptionsLocked(addr)
	if errOptions != nil {
		return nil, errOptions
	}
	options.Dialer = redis.NewDialer(options)
	c.release = redis.NewClient(options)
	return c.release, nil
}

func (c *Client) RPushRequestLog(ctx context.Context, payload []byte) error {
	cmd, errClient := c.commandClient()
	if errClient != nil {
		return errClient
	}
	if len(payload) == 0 {
		return nil
	}
	return cmd.RPush(ctx, redisKeyRequestLog, payload).Err()
}

// FindRequestLog scans the newest bounded portion of Home's request-log list.
func (c *Client) FindRequestLog(ctx context.Context, requestID string) (string, bool, error) {
	cmd, errClient := c.commandClient()
	if errClient != nil {
		return "", false, errClient
	}
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return "", false, errors.New("home: request log request ID is required")
	}
	length, errLength := cmd.LLen(ctx, redisKeyRequestLog).Result()
	if errLength != nil {
		return "", false, fmt.Errorf("home: read request log list length: %w", errLength)
	}

	remaining := min(length, int64(homeRequestLogLookupMaxEntries))
	for end := length - 1; end >= 0 && remaining > 0; {
		batchSize := min(remaining, int64(homeRequestLogLookupBatchSize))
		start := max(int64(0), end-batchSize+1)
		entries, errRange := cmd.LRange(ctx, redisKeyRequestLog, start, end).Result()
		if errRange != nil {
			return "", false, fmt.Errorf("home: read request log list: %w", errRange)
		}
		for _, entry := range slices.Backward(entries) {

			if strings.TrimSpace(gjson.Get(entry, "request_id").String()) != requestID {
				continue
			}
			requestLog := gjson.Get(entry, "request_log")
			if !requestLog.Exists() {
				return "", false, errors.New("home: matched request log entry has no request_log field")
			}
			return requestLog.String(), true, nil
		}
		remaining -= int64(len(entries))
		if start == 0 || len(entries) == 0 {
			break
		}
		end = start - 1
	}
	return "", false, nil
}

func (c *Client) RPushAppLog(ctx context.Context, payload []byte) error {
	cmd, errClient := c.commandClient()
	if errClient != nil {
		return errClient
	}
	if len(payload) == 0 {
		return nil
	}
	return cmd.RPush(ctx, redisKeyAppLog, payload).Err()
}

func (c *Client) RPushPluginStatus(ctx context.Context, payload []byte) error {
	cmd, errClient := c.commandClient()
	if errClient != nil {
		return errClient
	}
	if len(payload) == 0 {
		return nil
	}
	return cmd.RPush(ctx, redisKeyPluginStatus, payload).Err()
}

func (c *Client) GetPluginTasks(ctx context.Context) ([]PluginTask, error) {
	cmd, errClient := c.commandClient()
	if errClient != nil {
		return nil, errClient
	}
	raw, errGet := cmd.Get(ctx, redisKeyPluginTasks).Bytes()
	if errors.Is(errGet, redis.Nil) {
		return nil, nil
	}
	if errGet != nil {
		return nil, errGet
	}
	if len(raw) == 0 {
		return nil, nil
	}
	var tasks []PluginTask
	if errUnmarshal := json.Unmarshal(raw, &tasks); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	return tasks, nil
}

func (c *Client) SetLifecycleConfig(cfg config.CredentialConcurrencyConfig) error {
	if c == nil {
		return ErrDisabled
	}
	cfg = cfg.WithDefaults()
	if errValidate := config.ValidateCredentialConcurrency(cfg); errValidate != nil {
		return fmt.Errorf("validate credential concurrency lifecycle config: %w", errValidate)
	}
	c.mu.Lock()
	c.lifecycle = cfg
	c.mu.Unlock()
	c.limiter.Store(&cfg)
	return nil
}

// LimiterConfig returns the latest immutable, validated Home limiter configuration.
func (c *Client) LimiterConfig() config.CredentialConcurrencyConfig {
	if c == nil {
		return config.CredentialConcurrencyConfig{}.WithDefaults()
	}
	if cfg := c.limiter.Load(); cfg != nil {
		return *cfg
	}
	return config.CredentialConcurrencyConfig{}.WithDefaults()
}

func (c *Client) subscriptionParameters() ([]string, time.Duration) {
	if c == nil {
		return []string{redisChannelConfig}, config.CredentialConcurrencyConfig{}.WithDefaults().CPAHeartbeatTimeout
	}
	c.mu.Lock()
	cfg := c.lifecycle.WithDefaults()
	instanceID := c.instanceID
	legacyMembership := c.legacyMembership
	testTimeout := c.testOperationTimeout
	c.mu.Unlock()

	timeout := cfg.CPAHeartbeatTimeout
	if testTimeout > 0 && cfg.LifecycleConfigRevision == 0 {
		timeout = testTimeout
	}

	args := []string{redisChannelConfig}
	if cfg.LifecycleConfigRevision > 0 {
		args = append(args, strconv.FormatInt(cfg.LifecycleConfigRevision, 10))
		if legacyMembership {
			return args, timeout
		}
		state := recoveryState(c.recoveryState.Load())
		if state == recoveryStateTakeoverEligible || state == recoveryStateSwitchingTakeover {
			args = append(args, "takeover")
		}
		args = append(args, instanceID)
	}
	return args, timeout
}

func (c *Client) markMembershipTakeoverEligible() {
	if c == nil {
		return
	}
	if !c.recoveryState.CompareAndSwap(uint32(recoveryStateStable), uint32(recoveryStateTakeoverEligible)) {
		c.recoveryState.CompareAndSwap(uint32(recoveryStateSwitching), uint32(recoveryStateSwitchingTakeover))
	}
}

func (c *Client) rebuildCommandPoolAndProbe(ctx context.Context) error {
	c.promoteSubscription()
	if errPing := c.Ping(ctx); errPing != nil {
		return errPing
	}
	c.recoveryState.Store(uint32(recoveryStateStable))
	return nil
}

func (c *Client) promoteSubscription() {
	if c == nil {
		return
	}
	c.mu.Lock()
	commandClient := c.cmd
	c.cmd = nil
	c.cmdOptions = nil
	c.mu.Unlock()
	if commandClient != nil {
		if errClose := commandClient.Close(); errClose != nil {
			log.WithError(errClose).Warn("Home bootstrap command client close failed")
		}
	}
}

func receiveHomeSubscriptionEvent(ctx context.Context, subscription homeSubscription) (any, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	event, errReceive := subscription.ReceiveTimeout(ctx, homeSubscriptionReceiveTimeout)
	if errReceive == nil || !isTimeoutError(errReceive) {
		return event, errReceive
	}
	pingCtx, cancel := context.WithTimeout(ctx, homeRedisOperationTimeout)
	defer cancel()
	if errPing := subscription.Ping(pingCtx); errPing != nil {
		return nil, fmt.Errorf("home subscription ping: %w", errPing)
	}
	event, errReceive = subscription.ReceiveTimeout(pingCtx, homeRedisOperationTimeout)
	if errReceive != nil {
		return nil, fmt.Errorf("home subscription heartbeat: %w", errReceive)
	}
	return event, nil
}

func (c *Client) handleSubscriptionPayload(
	_ context.Context,
	channel string,
	payload string,
	onConfig func([]byte) error,
) error {
	payload = strings.TrimSpace(payload)
	if payload == "" {
		return nil
	}

	switch strings.ToLower(strings.TrimSpace(channel)) {
	case redisChannelConfig:
		if onConfig == nil {
			return nil
		}
		return onConfig([]byte(payload))
	case redisChannelCluster:
		return c.updateClusterNodesFromPayload([]byte(payload))
	default:
		return nil
	}
}

// RunConfigSubscriberLifetime runs one GET, SUBSCRIBE, and receive lifetime.
// Reconnection is owned by the service so each replacement can install a new client lifetime.
func (c *Client) RunConfigSubscriberLifetime(ctx context.Context, onConfig func([]byte) error, onReady func()) error {
	if c == nil || !c.Enabled() {
		return ErrDisabled
	}
	if onConfig == nil {
		return fmt.Errorf("home config subscriber callback is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	c.closeBootstrapPools()
	if errEnsure := c.ensureClients(); errEnsure != nil {
		if ctx.Err() == nil {
			c.markReconnectFailure("connect")
		}
		return c.endConfigSubscriberLifetime(errEnsure)
	}

	raw, errGet := c.GetConfig(ctx)
	if errGet != nil {
		if ctx.Err() == nil {
			c.markReconnectFailure("config fetch")
		}
		return c.endConfigSubscriberLifetime(errGet)
	}
	if errApply := onConfig(raw); errApply != nil {
		return c.endConfigSubscriberLifetime(errApply)
	}

	sub, errSubClient := c.subscriptionClient()
	if errSubClient != nil {
		if ctx.Err() == nil {
			c.markReconnectFailure("subscribe client")
		}
		return c.endConfigSubscriberLifetime(errSubClient)
	}
	args, receiveTimeout := c.subscriptionParameters()
	pubsub := sub.Subscribe(ctx, args...)
	if pubsub == nil {
		if ctx.Err() == nil {
			c.markReconnectFailure("subscribe")
		}
		return c.endConfigSubscriberLifetime(ErrNotConnected)
	}

	if errACK := receiveSubscriptionACKs(ctx, pubsub, receiveTimeout, args[:1]); errACK != nil {
		if ctx.Err() == nil {
			c.markReconnectFailure("subscribe")
		}
		return c.endConfigSubscriberLifetimeWithSubscription(errACK, pubsub, "failed ACK")
	}
	// A protocol-one ACK means Home already committed this membership. Preserve it if the command probe fails.
	if len(args) > 1 {
		c.markMembershipTakeoverEligible()
	}

	if errProbe := c.rebuildCommandPoolAndProbe(ctx); errProbe != nil {
		if ctx.Err() == nil {
			c.markReconnectFailure("command probe")
		}
		return c.endConfigSubscriberLifetimeWithSubscription(errProbe, pubsub, "fresh command probe failure")
	}
	c.resetReconnectFailures()
	c.heartbeatOK.Store(true)
	if onReady != nil {
		onReady()
	}

	for {
		_, receiveTimeout = c.subscriptionParameters()
		event, errReceive := pubsub.ReceiveTimeout(ctx, receiveTimeout)
		if errReceive != nil {
			if ctx.Err() == nil {
				if c.heartbeatOK.Load() {
					c.markMembershipTakeoverEligible()
				}
				if isTimeoutError(errReceive) {
					c.markSubscriptionTimeout()
				} else {
					c.markReconnectFailure("subscription")
				}
			}
			return c.endConfigSubscriberLifetimeWithSubscription(errReceive, pubsub, "heartbeat loss")
		}
		switch msg := event.(type) {
		case *redis.Message:
			if msg == nil {
				continue
			}
			if errApply := c.handleSubscriptionPayload(ctx, msg.Channel, msg.Payload, onConfig); errApply != nil {
				if strings.EqualFold(strings.TrimSpace(msg.Channel), redisChannelCluster) {
					log.Warn("failed to apply cluster update from home control center, ignoring")
				} else {
					log.Warn("failed to apply config update from home control center, ignoring")
				}
			}
		case *redis.Pong:
			c.resetReconnectFailures()
		case *redis.Subscription:
			continue
		default:
			log.Debugf("home subscription returned unsupported message type %T", event)
		}
	}
}

func receiveSubscriptionACKs(
	ctx context.Context,
	pubsub *redis.PubSub,
	receiveTimeout time.Duration,
	channels []string,
) error {
	if pubsub == nil || len(channels) == 0 {
		return fmt.Errorf("home subscription ACK is missing")
	}
	for index, channel := range channels {
		event, errReceive := pubsub.ReceiveTimeout(ctx, receiveTimeout)
		if errReceive != nil {
			return errReceive
		}
		ack, ok := event.(*redis.Subscription)
		if !ok || ack == nil || ack.Kind != "subscribe" || ack.Channel != channel || ack.Count != index+1 {
			return fmt.Errorf("invalid Home subscription ACK")
		}
	}
	return nil
}

func (c *Client) endConfigSubscriberLifetime(err error) error {
	c.heartbeatOK.Store(false)
	if !c.managedLifetime() {
		c.Close()
	}
	return err
}

func (c *Client) endConfigSubscriberLifetimeWithSubscription(
	err error,
	subscription subscriptionCloser,
	reason string,
) error {
	c.heartbeatOK.Store(false)
	if subscription != nil {
		if errClose := subscription.Close(); errClose != nil {
			log.WithError(errClose).Debugf("Home subscription close after %s", reason)
		}
	}
	if !c.managedLifetime() {
		c.Close()
	}
	return err
}

func isTimeoutError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}
