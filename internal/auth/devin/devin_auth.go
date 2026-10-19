package devin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	DefaultAppBaseURL = "https://app.devin.ai"
	DefaultAPIBaseURL = "https://api.devin.ai"
	DefaultServerURL  = "https://server.codeium.com"

	devinTokenPrefix        = "devin-session-token$"
	maxDevinAuthBodySize    = 1 << 20
	defaultDevinHTTPTimeout = 30 * time.Second
)

// DevinTokenBundle contains a completed Devin authentication result.
type DevinTokenBundle struct {
	SessionToken string `json:"session_token"`
	RawToken     string `json:"raw_token"`
	UserName     string `json:"user_name,omitempty"`
	UserID       string `json:"user_id,omitempty"`
	OrgID        string `json:"org_id,omitempty"`
	BaseURL      string `json:"base_url,omitempty"`
}

// DevinAuthService performs Devin OAuth, profile, and status requests.
type DevinAuthService struct {
	client        *http.Client
	appBaseURL    string
	apiBaseURL    string
	serverBaseURL string
}

// NewDevinAuthService creates a Devin authentication service.
func NewDevinAuthService(client *http.Client) *DevinAuthService {
	if client == nil {
		client = &http.Client{Timeout: defaultDevinHTTPTimeout}
	}
	return &DevinAuthService{
		client:        client,
		appBaseURL:    DefaultAppBaseURL,
		apiBaseURL:    DefaultAPIBaseURL,
		serverBaseURL: DefaultServerURL,
	}
}

// SetAppBaseURL overrides the authorization site for tests.
func (service *DevinAuthService) SetAppBaseURL(baseURL string) {
	if service != nil && strings.TrimSpace(baseURL) != "" {
		service.appBaseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	}
}

// SetAPIBaseURL overrides the OAuth and profile API for tests.
func (service *DevinAuthService) SetAPIBaseURL(baseURL string) {
	if service != nil && strings.TrimSpace(baseURL) != "" {
		service.apiBaseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	}
}

// SetServerBaseURL overrides the Connect backend for tests.
func (service *DevinAuthService) SetServerBaseURL(baseURL string) {
	if service != nil && strings.TrimSpace(baseURL) != "" {
		service.serverBaseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	}
}

// BuildAuthorizationURL constructs the Devin PKCE authorization URL.
func (service *DevinAuthService) BuildAuthorizationURL(redirectURI, codeChallenge, state string) string {
	redirectURI = strings.TrimSpace(redirectURI)
	parts := []string{}
	if redirectURI != "" {
		parts = append(parts, "redirect_uri="+url.QueryEscape(redirectURI))
	}
	if state = strings.TrimSpace(state); state != "" {
		parts = append(parts, "state="+url.QueryEscape(state))
	}
	parts = append(parts,
		"prompt=select_account",
		"code_challenge="+url.QueryEscape(strings.TrimSpace(codeChallenge)),
		"code_challenge_method=S256",
	)
	if redirectURI == "" {
		parts = append(parts, "cli_pkce_marker=1")
	}
	return strings.TrimRight(service.appBaseURL, "/") + "/auth/cli/continue?" + strings.Join(parts, "&")
}

// ExchangeCodeForToken exchanges an OAuth authorization code for a session token.
func (service *DevinAuthService) ExchangeCodeForToken(ctx context.Context, code, codeVerifier string) (string, error) {
	if service == nil || service.client == nil {
		return "", errors.New("devin auth service is not initialized")
	}
	code = strings.TrimSpace(code)
	codeVerifier = strings.TrimSpace(codeVerifier)
	if code == "" || codeVerifier == "" {
		return "", errors.New("devin authorization code and PKCE verifier are required")
	}
	body, err := json.Marshal(map[string]string{"code": code, "code_verifier": codeVerifier})
	if err != nil {
		return "", fmt.Errorf("encode Devin token exchange: %w", err)
	}
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		strings.TrimRight(service.apiBaseURL, "/")+"/auth/cli/token",
		bytes.NewReader(body),
	)
	if err != nil {
		return "", fmt.Errorf("create Devin token exchange request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")

	response, err := service.client.Do(request)
	if err != nil {
		return "", fmt.Errorf("exchange Devin authorization code: %w", err)
	}
	defer response.Body.Close()
	responseBody, err := readBoundedDevinBody(response.Body, maxDevinAuthBodySize)
	if err != nil {
		return "", fmt.Errorf("read Devin token exchange response: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", fmt.Errorf("devin token exchange returned status %d", response.StatusCode)
	}
	var payload struct {
		Token string `json:"token"`
	}
	if err = json.Unmarshal(responseBody, &payload); err != nil {
		return "", fmt.Errorf("decode Devin token exchange response: %w", err)
	}
	token := FormatSessionToken(payload.Token)
	if token == "" {
		return "", errors.New("devin token exchange response is missing token")
	}
	return token, nil
}

// FetchSelfProfile retrieves the authenticated Devin profile.
func (service *DevinAuthService) FetchSelfProfile(ctx context.Context, sessionToken string) (userName, userID, orgID string, err error) {
	if service == nil || service.client == nil {
		return "", "", "", errors.New("devin auth service is not initialized")
	}
	sessionToken = FormatSessionToken(sessionToken)
	if sessionToken == "" {
		return "", "", "", errors.New("devin session token is required")
	}
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		strings.TrimRight(service.apiBaseURL, "/")+"/v3/self",
		nil,
	)
	if err != nil {
		return "", "", "", fmt.Errorf("create Devin profile request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+sessionToken)
	request.Header.Set("Accept", "application/json")

	response, err := service.client.Do(request)
	if err != nil {
		return "", "", "", fmt.Errorf("fetch Devin profile: %w", err)
	}
	defer response.Body.Close()
	responseBody, err := readBoundedDevinBody(response.Body, maxDevinAuthBodySize)
	if err != nil {
		return "", "", "", fmt.Errorf("read Devin profile response: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		return "", "", "", fmt.Errorf("devin profile returned status %d", response.StatusCode)
	}
	var profile struct {
		UserName string `json:"user_name"`
		UserID   string `json:"user_id"`
		OrgID    string `json:"org_id"`
	}
	if err = json.Unmarshal(responseBody, &profile); err != nil {
		return "", "", "", fmt.Errorf("decode Devin profile response: %w", err)
	}
	return strings.TrimSpace(profile.UserName), strings.TrimSpace(profile.UserID), strings.TrimSpace(profile.OrgID), nil
}

// FormatSessionToken applies the mandatory Devin session-token prefix.
func FormatSessionToken(rawToken string) string {
	token := strings.TrimSpace(rawToken)
	if token == "" || strings.HasPrefix(token, devinTokenPrefix) {
		return token
	}
	return devinTokenPrefix + token
}

func readBoundedDevinBody(reader io.Reader, limit int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("response exceeds %d bytes", limit)
	}
	return body, nil
}

// OAuthResult contains one local OAuth callback result.
type OAuthResult struct {
	Code  string
	State string
	Error string
}

// OAuthServer receives the browser callback on loopback only.
type OAuthServer struct {
	server     *http.Server
	listener   net.Listener
	port       int
	resultChan chan *OAuthResult
	errorChan  chan error
	mu         sync.Mutex
	running    bool
}

// NewOAuthServer creates a loopback OAuth callback server.
func NewOAuthServer(port int) *OAuthServer {
	return &OAuthServer{
		port:       port,
		resultChan: make(chan *OAuthResult, 1),
		errorChan:  make(chan error, 1),
	}
}

// Start binds the callback server and returns its actual port.
func (server *OAuthServer) Start() (int, error) {
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.running {
		return server.port, nil
	}
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", server.port))
	if err != nil {
		return 0, fmt.Errorf("bind Devin OAuth callback: %w", err)
	}
	server.listener = listener
	server.port = listener.Addr().(*net.TCPAddr).Port
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", server.handleCallback)
	server.server = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       15 * time.Second,
	}
	server.running = true
	go func() {
		if serveErr := server.server.Serve(listener); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			select {
			case server.errorChan <- serveErr:
			default:
				log.Debugf("Devin OAuth server error dropped: %v", serveErr)
			}
		}
	}()
	return server.port, nil
}

// Stop shuts down the callback server.
func (server *OAuthServer) Stop(ctx context.Context) error {
	server.mu.Lock()
	if !server.running || server.server == nil {
		server.mu.Unlock()
		return nil
	}
	server.running = false
	httpServer := server.server
	server.mu.Unlock()
	return httpServer.Shutdown(ctx)
}

// WaitForCallbackWithContext waits for a callback, cancellation, or timeout.
func (server *OAuthServer) WaitForCallbackWithContext(ctx context.Context, timeout time.Duration) (*OAuthResult, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case result := <-server.resultChan:
		return result, nil
	case err := <-server.errorChan:
		return nil, err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return nil, errors.New("devin authentication timed out")
	}
}

func (server *OAuthServer) handleCallback(response http.ResponseWriter, request *http.Request) {
	response.Header().Set("Content-Type", "text/html; charset=utf-8")
	if request.Method != http.MethodGet {
		response.WriteHeader(http.StatusMethodNotAllowed)
		_, _ = response.Write([]byte("method not allowed"))
		return
	}
	query := request.URL.Query()
	result := &OAuthResult{
		Code:  strings.TrimSpace(query.Get("code")),
		State: strings.TrimSpace(query.Get("state")),
		Error: strings.TrimSpace(query.Get("error")),
	}
	if description := strings.TrimSpace(query.Get("error_description")); description != "" {
		if result.Error == "" {
			result.Error = description
		} else {
			result.Error += ": " + description
		}
	}
	if result.Error == "" && result.Code == "" {
		result.Error = "missing authorization code"
	}
	if result.Error != "" {
		response.WriteHeader(http.StatusBadRequest)
		_, _ = response.Write([]byte("<!doctype html><title>Devin authentication failed</title><p>" + html.EscapeString(result.Error) + "</p>"))
	} else {
		response.WriteHeader(http.StatusOK)
		_, _ = response.Write([]byte("<!doctype html><title>Devin authentication complete</title><p>You may close this window.</p>"))
	}
	select {
	case server.resultChan <- result:
	default:
	}
}
