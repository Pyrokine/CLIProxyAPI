package logging

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/home"
	log "github.com/sirupsen/logrus"
)

const defaultHomeAppLogQueueSize = 1024

type homeAppLogClient interface {
	HeartbeatOK() bool
	RPushAppLog(ctx context.Context, payload []byte) error
}

type homeAppLogPayload struct {
	Line      string `json:"line"`
	Level     string `json:"level,omitempty"`
	Timestamp string `json:"timestamp,omitempty"`
	RequestID string `json:"request_id,omitempty"`
}

var currentHomeAppLogClient = func() homeAppLogClient {
	return home.Current()
}

var activeHomeAppLogForwarders = struct {
	sync.RWMutex
	items map[*HomeAppLogForwarder]struct{}
}{items: make(map[*HomeAppLogForwarder]struct{})}

var activeHomeAppLogScopes atomic.Int64

func BeginHomeAppLogScope() func() {
	activeHomeAppLogScopes.Add(1)
	var once sync.Once
	return func() {
		once.Do(
			func() {
				activeHomeAppLogScopes.Add(-1)
			},
		)
	}
}

type HomeAppLogForwarder struct {
	formatter log.Formatter
	clientMu  sync.RWMutex
	client    homeAppLogClient
	queue     chan homeAppLogPayload
	stop      chan struct{}
	stopOnce  sync.Once
	wg        sync.WaitGroup
	enabled   atomic.Bool
}

func StartHomeAppLogForwarder(queueSize int) *HomeAppLogForwarder {
	return startHomeAppLogForwarder(queueSize, nil)
}

func startHomeAppLogForwarder(queueSize int, client homeAppLogClient) *HomeAppLogForwarder {
	if queueSize <= 0 {
		queueSize = defaultHomeAppLogQueueSize
	}
	forwarder := &HomeAppLogForwarder{
		formatter: &LogFormatter{},
		client:    client,
		queue:     make(chan homeAppLogPayload, queueSize),
		stop:      make(chan struct{}),
	}
	forwarder.enabled.Store(true)
	forwarder.wg.Add(1)
	go forwarder.run()
	registerHomeAppLogForwarder(forwarder)
	log.AddHook(forwarder)
	return forwarder
}

func registerHomeAppLogForwarder(forwarder *HomeAppLogForwarder) {
	if forwarder == nil {
		return
	}
	activeHomeAppLogForwarders.Lock()
	activeHomeAppLogForwarders.items[forwarder] = struct{}{}
	count := len(activeHomeAppLogForwarders.items)
	activeHomeAppLogForwarders.Unlock()
	if count > 1 {
		log.Warn("home app-log forwarding paused while multiple Home services are active")
	}
}

func unregisterHomeAppLogForwarder(forwarder *HomeAppLogForwarder) {
	activeHomeAppLogForwarders.Lock()
	delete(activeHomeAppLogForwarders.items, forwarder)
	activeHomeAppLogForwarders.Unlock()
}

func homeAppLogForwardingAllowed(forwarder *HomeAppLogForwarder) bool {
	if activeHomeAppLogScopes.Load() > 1 {
		return false
	}
	activeHomeAppLogForwarders.RLock()
	defer activeHomeAppLogForwarders.RUnlock()
	if _, registered := activeHomeAppLogForwarders.items[forwarder]; !registered {
		return true
	}
	return len(activeHomeAppLogForwarders.items) == 1
}

func (f *HomeAppLogForwarder) Stop() {
	if f == nil {
		return
	}
	f.stopOnce.Do(
		func() {
			f.enabled.Store(false)
			unregisterHomeAppLogForwarder(f)
			close(f.stop)
			f.wg.Wait()
		},
	)
}

// Bind activates forwarding to client.
func (f *HomeAppLogForwarder) Bind(client *home.Client) {
	f.bind(client)
}

func (f *HomeAppLogForwarder) bind(client homeAppLogClient) {
	if f == nil {
		return
	}
	f.clientMu.Lock()
	f.client = client
	f.enabled.Store(client != nil)
	f.clientMu.Unlock()
}

// Deactivate stops forwarding only when client owns the forwarder.
func (f *HomeAppLogForwarder) Deactivate(client *home.Client) {
	f.deactivate(client)
}

func (f *HomeAppLogForwarder) deactivate(client homeAppLogClient) {
	if f == nil || client == nil {
		return
	}
	f.clientMu.Lock()
	if f.client == client {
		f.client = nil
		f.enabled.Store(false)
	}
	f.clientMu.Unlock()
}

func (f *HomeAppLogForwarder) Levels() []log.Level {
	return log.AllLevels
}

func (f *HomeAppLogForwarder) homeClient() homeAppLogClient {
	if f != nil {
		f.clientMu.RLock()
		client := f.client
		f.clientMu.RUnlock()
		if client != nil {
			return client
		}
	}
	return currentHomeAppLogClient()
}

func (f *HomeAppLogForwarder) Fire(entry *log.Entry) error {
	if f == nil || entry == nil || !f.enabled.Load() || !homeAppLogForwardingAllowed(f) {
		return nil
	}
	client := f.homeClient()
	if client == nil || !client.HeartbeatOK() {
		return nil
	}
	line, errFormat := f.formatEntry(entry)
	if errFormat != nil || strings.TrimSpace(line) == "" {
		return nil
	}

	payload := homeAppLogPayload{
		Line:      line,
		Level:     entry.Level.String(),
		Timestamp: entry.Time.Format(time.RFC3339Nano),
		RequestID: appLogRequestID(entry),
	}
	select {
	case f.queue <- payload:
	default:
	}
	return nil
}

func appLogRequestID(entry *log.Entry) string {
	if entry == nil {
		return ""
	}
	requestID, _ := entry.Data["request_id"].(string)
	requestID = strings.TrimSpace(requestID)
	if requestID == "--------" {
		return ""
	}
	return requestID
}

func (f *HomeAppLogForwarder) formatEntry(entry *log.Entry) (string, error) {
	formatter := f.formatter
	if formatter == nil {
		formatter = &LogFormatter{}
	}
	raw, errFormat := formatter.Format(entry)
	if errFormat != nil {
		return "", errFormat
	}
	return string(raw), nil
}

func (f *HomeAppLogForwarder) run() {
	defer f.wg.Done()
	for {
		select {
		case <-f.stop:
			return
		case payload := <-f.queue:
			f.forward(payload)
		}
	}
}

func (f *HomeAppLogForwarder) forward(payload homeAppLogPayload) {
	if !f.enabled.Load() {
		return
	}
	client := f.homeClient()
	if client == nil || !client.HeartbeatOK() {
		return
	}
	raw, errMarshal := json.Marshal(&payload)
	if errMarshal != nil {
		return
	}
	if errPush := client.RPushAppLog(context.Background(), raw); errPush != nil && isHomeAppLogUnsupported(errPush) {
		f.disableIfCurrentOwner(client)
	}
}

func (f *HomeAppLogForwarder) disableIfCurrentOwner(client homeAppLogClient) {
	if f == nil || client == nil {
		return
	}
	f.clientMu.Lock()
	defer f.clientMu.Unlock()
	if f.client == client || (f.client == nil && currentHomeAppLogClient() == client) {
		f.enabled.Store(false)
	}
}

func isHomeAppLogUnsupported(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(strings.TrimSpace(err.Error()))
	if msg == "" {
		return false
	}
	for {
		switch {
		case strings.Contains(msg, "unsupported key"):
			return true
		case strings.Contains(msg, "unknown command"):
			return true
		case strings.Contains(msg, "unsupported command"):
			return true
		}
		err = errors.Unwrap(err)
		if err == nil {
			return false
		}
		msg = strings.ToLower(strings.TrimSpace(err.Error()))
	}
}
