package oauthcommon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

// ServerConfig holds provider-specific settings for the OAuth callback server.
type ServerConfig struct {
	// CallbackPath is the HTTP path for the OAuth callback endpoint (e.g. "/callback").
	CallbackPath string
	// DefaultPlatformURL is the fallback URL shown on the success page (e.g. "https://console.anthropic.com/").
	DefaultPlatformURL string
	// ProviderName is the display name used in HTML templates (e.g. "Claude", "Codex").
	ProviderName string
}

// OAuthServer handles the local HTTP server for OAuth callbacks.
// It listens for the authorization code response from the OAuth provider
// and captures the necessary parameters to complete the authentication flow.
type OAuthServer struct {
	server     *http.Server
	port       int
	resultChan chan *OAuthResult
	errorChan  chan error
	mu         sync.Mutex
	running    bool
	config     ServerConfig
}

// OAuthResult contains the result of the OAuth callback.
// It holds either the authorization code and state for successful authentication
// or an error message if the authentication failed.
type OAuthResult struct {
	// Code is the authorization code received from the OAuth provider.
	Code string
	// State is the state parameter used to prevent CSRF attacks.
	State string
	// Error contains any error message if the OAuth flow failed.
	Error string
}

// NewOAuthServer creates a new OAuth callback server with the given port and configuration.
func NewOAuthServer(port int, cfg ServerConfig) *OAuthServer {
	return &OAuthServer{
		port:       port,
		resultChan: make(chan *OAuthResult, 1),
		errorChan:  make(chan error, 1),
		config:     cfg,
	}
}

// Start starts the OAuth callback server.
func (s *OAuthServer) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.running {
		return fmt.Errorf("server is already running")
	}

	if !s.isPortAvailable() {
		return fmt.Errorf("port %d is already in use", s.port)
	}

	mux := http.NewServeMux()
	mux.HandleFunc(s.config.CallbackPath, s.handleCallback)
	mux.HandleFunc("/success", s.handleSuccess)

	s.server = &http.Server{
		Addr:         fmt.Sprintf(":%d", s.port),
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	s.running = true

	go func() {
		if err := s.server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.errorChan <- fmt.Errorf("server failed to start: %w", err)
		}
	}()

	// Give server a moment to start
	time.Sleep(100 * time.Millisecond)

	return nil
}

// Stop gracefully stops the OAuth callback server.
func (s *OAuthServer) Stop(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.running || s.server == nil {
		return nil
	}

	log.Debug("Stopping OAuth callback server")

	shutdownCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	err := s.server.Shutdown(shutdownCtx)
	s.running = false
	s.server = nil

	return err
}

// WaitForCallback waits for the OAuth callback with a timeout.
func (s *OAuthServer) WaitForCallback(timeout time.Duration) (*OAuthResult, error) {
	select {
	case result := <-s.resultChan:
		return result, nil
	case err := <-s.errorChan:
		return nil, err
	case <-time.After(timeout):
		return nil, fmt.Errorf("timeout waiting for OAuth callback")
	}
}

// handleCallback handles the OAuth callback endpoint.
func (s *OAuthServer) handleCallback(w http.ResponseWriter, r *http.Request) {
	log.Debug("Received OAuth callback")

	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	query := r.URL.Query()
	code := query.Get("code")
	state := query.Get("state")
	errorParam := query.Get("error")

	if errorParam != "" {
		log.Errorf("OAuth error received: %s", errorParam)
		s.sendResult(&OAuthResult{Error: errorParam})
		http.Error(w, fmt.Sprintf("OAuth error: %s", errorParam), http.StatusBadRequest)
		return
	}

	if code == "" {
		log.Error("No authorization code received")
		s.sendResult(&OAuthResult{Error: "no_code"})
		http.Error(w, "No authorization code received", http.StatusBadRequest)
		return
	}

	if state == "" {
		log.Error("No state parameter received")
		s.sendResult(&OAuthResult{Error: "no_state"})
		http.Error(w, "No state parameter received", http.StatusBadRequest)
		return
	}

	s.sendResult(&OAuthResult{Code: code, State: state})
	http.Redirect(w, r, "/success", http.StatusFound)
}

// handleSuccess handles the success page endpoint.
func (s *OAuthServer) handleSuccess(w http.ResponseWriter, r *http.Request) {
	log.Debug("Serving success page")

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)

	query := r.URL.Query()
	setupRequired := query.Get("setup_required") == "true"
	platformURL := query.Get("platform_url")
	if platformURL != "" {
		u, err := url.Parse(platformURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			platformURL = s.config.DefaultPlatformURL
		}
	}
	if platformURL == "" {
		platformURL = s.config.DefaultPlatformURL
	}

	successHTML := generateSuccessHTML(s.config.ProviderName, setupRequired, platformURL)

	if _, err := w.Write([]byte(successHTML)); err != nil {
		log.Errorf("Failed to write success page: %v", err)
	}
}

// sendResult sends the OAuth result to the waiting channel.
func (s *OAuthServer) sendResult(result *OAuthResult) {
	select {
	case s.resultChan <- result:
		log.Debug("OAuth result sent to channel")
	default:
		log.Warn("OAuth result channel is full, result dropped")
	}
}

// isPortAvailable checks if the specified port is available.
func (s *OAuthServer) isPortAvailable() bool {
	addr := fmt.Sprintf(":%d", s.port)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return false
	}
	defer func() {
		_ = listener.Close()
	}()
	return true
}

// IsRunning returns whether the server is currently running.
func (s *OAuthServer) IsRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}
