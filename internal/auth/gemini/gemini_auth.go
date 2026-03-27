// Package gemini provides authentication and token management functionality
// for Google's Gemini AI services. It handles OAuth2 authentication flows,
// including obtaining tokens via web-based authorization, storing tokens,
// and refreshing them when they expire.
package gemini

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"maps"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/oauthcommon"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/browser"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/misc"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"golang.org/x/net/proxy"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// OAuth configuration constants for Gemini
const (
	ClientID            = "681255809395-oo8ft2oprdrnp9e3aqf6av3hmdib135j.apps.googleusercontent.com"
	ClientSecret        = "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"
	DefaultCallbackPort = 8085
)

// Scopes defines the OAuth scopes required for Gemini authentication.
var Scopes = []string{
	"https://www.googleapis.com/auth/cloud-platform",
	"https://www.googleapis.com/auth/userinfo.email",
	"https://www.googleapis.com/auth/userinfo.profile",
}

// Auth provides methods for handling the Gemini OAuth2 authentication flow.
// It encapsulates the logic for obtaining, storing, and refreshing authentication tokens
// for Google's Gemini AI services.
type Auth struct {
}

// WebLoginOptions customizes the interactive OAuth flow.
type WebLoginOptions struct {
	NoBrowser    bool
	CallbackPort int
	Prompt       func(string) (string, error)
}

// NewAuth creates a new Auth instance.
func NewAuth() *Auth {
	return &Auth{}
}

// GetAuthenticatedClient configures and returns an HTTP client ready for making authenticated API calls.
// It manages the entire OAuth2 flow, including handling proxies, loading existing tokens,
// initiating a new web-based OAuth flow if necessary, and refreshing tokens.
//
// Parameters:
//   - ctx: The context for the HTTP client
//   - ts: The Gemini token storage containing authentication tokens
//   - cfg: The configuration containing proxy settings
//   - opts: Optional parameters to customize browser and prompt behavior
//
// Returns:
//   - *http.Client: An HTTP client configured with authentication
//   - error: An error if the client configuration fails, nil otherwise
func (g *Auth) GetAuthenticatedClient(
	ctx context.Context,
	ts *TokenStorage,
	cfg *config.Config,
	opts *WebLoginOptions,
) (*http.Client, error) {
	callbackPort := DefaultCallbackPort
	if opts != nil && opts.CallbackPort > 0 {
		callbackPort = opts.CallbackPort
	}
	callbackURL := fmt.Sprintf("http://localhost:%d/oauth2callback", callbackPort)

	// Configure proxy settings for the HTTP client if a proxy URL is provided.
	proxyURL, err := url.Parse(cfg.ProxyURL)
	if err == nil {
		var transport *http.Transport
		if proxyURL.Scheme == "socks5" {
			// Handle SOCKS5 proxy.
			username := proxyURL.User.Username()
			password, _ := proxyURL.User.Password()
			auth := &proxy.Auth{User: username, Password: password}
			dialer, errSOCKS5 := proxy.SOCKS5("tcp", proxyURL.Host, auth, proxy.Direct)
			if errSOCKS5 != nil {
				log.Errorf("create SOCKS5 dialer failed: %v", errSOCKS5)
				return nil, fmt.Errorf("create SOCKS5 dialer failed: %w", errSOCKS5)
			}
			transport = &http.Transport{
				DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
					return dialer.Dial(network, addr)
				},
			}
		} else if proxyURL.Scheme == "http" || proxyURL.Scheme == "https" {
			// Handle HTTP/HTTPS proxy.
			transport = &http.Transport{Proxy: http.ProxyURL(proxyURL)}
		}

		if transport != nil {
			proxyClient := &http.Client{Transport: transport}
			ctx = context.WithValue(ctx, oauth2.HTTPClient, proxyClient)
		}
	}

	// Configure the OAuth2 client.
	conf := &oauth2.Config{
		ClientID:     ClientID,
		ClientSecret: ClientSecret,
		RedirectURL:  callbackURL, // This will be used by the local server.
		Scopes:       Scopes,
		Endpoint:     google.Endpoint,
	}

	var token *oauth2.Token

	// If no token is found in storage, initiate the web-based OAuth flow.
	if ts.Token == nil {
		fmt.Printf("Could not load token from file, starting OAuth flow.\n")
		token, err = g.getTokenFromWeb(ctx, conf, opts)
		if err != nil {
			return nil, fmt.Errorf("failed to get token from web: %w", err)
		}
		// After getting a new token, create a new token storage object with user info.
		newTs, errCreateTokenStorage := g.createTokenStorage(ctx, conf, token, ts.ProjectID)
		if errCreateTokenStorage != nil {
			log.Errorf("Warning: failed to create token storage: %v", errCreateTokenStorage)
			return nil, errCreateTokenStorage
		}
		*ts = *newTs
	}

	// Unmarshal the stored token into an oauth2.Token object.
	tsToken, _ := json.Marshal(ts.Token)
	if err = json.Unmarshal(tsToken, &token); err != nil {
		return nil, fmt.Errorf("failed to unmarshal token: %w", err)
	}

	// Return an HTTP client that automatically handles token refreshing.
	return conf.Client(ctx, token), nil
}

// createTokenStorage creates a new TokenStorage object. It fetches the user's email
// using the provided token and populates the storage structure.
//
// Parameters:
//   - ctx: The context for the HTTP request
//   - config: The OAuth2 configuration
//   - token: The OAuth2 token to use for authentication
//   - projectID: The Google Cloud Project ID to associate with this token
//
// Returns:
//   - *TokenStorage: A new token storage object with user information
//   - error: An error if the token storage creation fails, nil otherwise
func (g *Auth) createTokenStorage(
	ctx context.Context,
	config *oauth2.Config,
	token *oauth2.Token,
	projectID string,
) (*TokenStorage, error) {
	httpClient := config.Client(ctx, token)
	req, err := http.NewRequestWithContext(ctx, "GET", "https://www.googleapis.com/oauth2/v1/userinfo?alt=json", nil)
	if err != nil {
		return nil, fmt.Errorf("could not get user info: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token.AccessToken))

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to execute request: %w", err)
	}
	defer func() {
		if err = resp.Body.Close(); err != nil {
			log.Printf("warn: failed to close response body: %v", err)
		}
	}()

	bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("get user info request failed with status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	emailResult := gjson.GetBytes(bodyBytes, "email")
	if emailResult.Exists() && emailResult.Type == gjson.String {
		fmt.Printf("Authenticated user email: %s\n", emailResult.String())
	} else {
		fmt.Println("Failed to get user email from token")
	}

	var ifToken map[string]any
	jsonData, _ := json.Marshal(token)
	err = json.Unmarshal(jsonData, &ifToken)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal token: %w", err)
	}

	ifToken["token_uri"] = "https://oauth2.googleapis.com/token"
	ifToken["client_id"] = ClientID
	ifToken["client_secret"] = ClientSecret
	ifToken["scopes"] = Scopes
	ifToken["universe_domain"] = "googleapis.com"

	ts := TokenStorage{
		Token:     ifToken,
		ProjectID: projectID,
		Email:     emailResult.String(),
	}

	return &ts, nil
}

// getTokenFromWeb initiates the web-based OAuth2 authorization flow.
// It starts a local HTTP server to listen for the callback from Google's auth server,
// opens the user's browser to the authorization URL, and exchanges the received
// authorization code for an access token.
//
// Parameters:
//   - ctx: The context for the HTTP client
//   - config: The OAuth2 configuration
//   - opts: Optional parameters to customize browser and prompt behavior
//
// Returns:
//   - *oauth2.Token: The OAuth2 token obtained from the authorization flow
//   - error: An error if the token acquisition fails, nil otherwise
func (g *Auth) getTokenFromWeb(ctx context.Context, config *oauth2.Config, opts *WebLoginOptions) (
	*oauth2.Token,
	error,
) {
	callbackPort := DefaultCallbackPort
	if opts != nil && opts.CallbackPort > 0 {
		callbackPort = opts.CallbackPort
	}
	callbackURL := fmt.Sprintf("http://localhost:%d/oauth2callback", callbackPort)

	// Generate random state for CSRF protection.
	state, errState := misc.GenerateRandomState()
	if errState != nil {
		return nil, fmt.Errorf("failed to generate state: %w", errState)
	}

	// Generate PKCE verifier for code exchange protection.
	verifier := oauth2.GenerateVerifier()

	// Use channels to pass the authorization code and state from the HTTP handler.
	type callbackResult struct {
		code  string
		state string
	}
	codeChan := make(chan callbackResult, 1)
	errChan := make(chan error, 1)

	// Create a new HTTP server with its own multiplexer.
	mux := http.NewServeMux()
	server := &http.Server{Addr: fmt.Sprintf(":%d", callbackPort), Handler: mux}
	config.RedirectURL = callbackURL

	mux.HandleFunc(
		"/oauth2callback", func(w http.ResponseWriter, r *http.Request) {
			if errParam := r.URL.Query().Get("error"); errParam != "" {
				http.Error(w, "Authentication failed: "+html.EscapeString(errParam), http.StatusBadRequest)
				select {
				case errChan <- fmt.Errorf("authentication failed via callback: %s", errParam):
				default:
				}
				return
			}
			code := r.URL.Query().Get("code")
			if code == "" {
				_, _ = fmt.Fprint(w, "Authentication failed: code not found.")
				select {
				case errChan <- fmt.Errorf("code not found in callback"):
				default:
				}
				return
			}
			_, _ = fmt.Fprint(
				w, "<html><body><h1>Authentication successful!</h1><p>You can close this window.</p></body></html>",
			)
			select {
			case codeChan <- callbackResult{code: code, state: r.URL.Query().Get("state")}:
			default:
			}
		},
	)

	// Start the server in a goroutine.
	go func() {
		if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			log.Errorf("ListenAndServe(): %v", err)
			select {
			case errChan <- err:
			default:
			}
		}
	}()
	defer func() {
		if err := server.Shutdown(ctx); err != nil {
			log.Errorf("Failed to shut down server: %v", err)
		}
	}()

	// Open the authorization URL in the user's browser.
	authURL := config.AuthCodeURL(
		state, oauth2.AccessTypeOffline,
		oauth2.SetAuthURLParam("prompt", "consent"),
		oauth2.S256ChallengeOption(verifier),
	)

	noBrowser := false
	if opts != nil {
		noBrowser = opts.NoBrowser
	}

	if !noBrowser {
		fmt.Println("Opening browser for authentication...")

		// Check if browser is available
		if !browser.IsAvailable() {
			log.Warn("No browser available on this system")
			util.PrintSSHTunnelInstructions(callbackPort)
			fmt.Printf("Please manually open this URL in your browser:\n\n%s\n", authURL)
		} else {
			if err := browser.OpenURL(authURL); err != nil {
				authErr := oauthcommon.NewAuthenticationError(oauthcommon.ErrBrowserOpenFailed, err)
				log.Warn(oauthcommon.GetUserFriendlyMessage(authErr))
				util.PrintSSHTunnelInstructions(callbackPort)
				fmt.Printf("Please manually open this URL in your browser:\n\n%s\n", authURL)

				// Log platform info for debugging
				platformInfo := browser.GetPlatformInfo()
				log.Debugf("Browser platform info: %+v", platformInfo)
			} else {
				log.Debug("Browser opened successfully")
			}
		}
	} else {
		util.PrintSSHTunnelInstructions(callbackPort)
		fmt.Printf("Please open this URL in your browser:\n\n%s\n", authURL)
	}

	fmt.Println("Waiting for authentication callback...")

	// Wait for the authorization code or an error.
	var cbResult callbackResult
	timeoutTimer := time.NewTimer(5 * time.Minute)
	defer timeoutTimer.Stop()

	var manualPromptTimer *time.Timer
	var manualPromptC <-chan time.Time
	if opts != nil && opts.Prompt != nil {
		manualPromptTimer = time.NewTimer(15 * time.Second)
		manualPromptC = manualPromptTimer.C
		defer manualPromptTimer.Stop()
	}

waitForCallback:
	for {
		select {
		case res := <-codeChan:
			cbResult = res
			break waitForCallback
		case err := <-errChan:
			return nil, err
		case <-manualPromptC:
			manualPromptC = nil
			if manualPromptTimer != nil {
				manualPromptTimer.Stop()
			}
			select {
			case res := <-codeChan:
				cbResult = res
				break waitForCallback
			case err := <-errChan:
				return nil, err
			default:
			}
			input, err := opts.Prompt("Paste the Gemini callback URL (or press Enter to keep waiting): ")
			if err != nil {
				return nil, err
			}
			parsed, err := misc.ParseOAuthCallback(input)
			if err != nil {
				return nil, err
			}
			if parsed == nil {
				continue
			}
			if parsed.Error != "" {
				return nil, fmt.Errorf("authentication failed via callback: %s", parsed.Error)
			}
			if parsed.Code == "" {
				return nil, fmt.Errorf("code not found in callback")
			}
			cbResult = callbackResult{code: parsed.Code, state: parsed.State}
			break waitForCallback
		case <-timeoutTimer.C:
			return nil, fmt.Errorf("oauth flow timed out")
		}
	}

	// Validate state to prevent CSRF attacks.
	if cbResult.state != state {
		return nil, fmt.Errorf("invalid state: expected %s, got %s", state, cbResult.state)
	}

	// Exchange the authorization code for a token with PKCE verifier.
	token, err := config.Exchange(ctx, cbResult.code, oauth2.VerifierOption(verifier))
	if err != nil {
		return nil, fmt.Errorf("failed to exchange token: %w", err)
	}

	fmt.Println("Authentication successful.")
	return token, nil
}

// TokenAndConfigFromMetadata parses an oauth2.Token from auth metadata
// and returns the raw token base map, parsed token, and Gemini OAuth2 config.
func TokenAndConfigFromMetadata(metadata map[string]any) (map[string]any, *oauth2.Token, *oauth2.Config) {
	base := make(map[string]any)
	if tokenRaw, ok := metadata["token"].(map[string]any); ok && tokenRaw != nil {
		maps.Copy(base, tokenRaw)
	}

	var token oauth2.Token
	if len(base) > 0 {
		if raw, err := json.Marshal(base); err == nil {
			_ = json.Unmarshal(raw, &token)
		}
	}

	if token.AccessToken == "" {
		token.AccessToken = metadataString(metadata, "access_token")
	}
	if token.RefreshToken == "" {
		token.RefreshToken = metadataString(metadata, "refresh_token")
	}
	if token.TokenType == "" {
		token.TokenType = metadataString(metadata, "token_type")
	}
	if token.Expiry.IsZero() {
		if expiry := metadataString(metadata, "expiry"); expiry != "" {
			if ts, err := time.Parse(time.RFC3339, expiry); err == nil {
				token.Expiry = ts
			}
		}
	}

	conf := &oauth2.Config{
		ClientID:     ClientID,
		ClientSecret: ClientSecret,
		Scopes:       Scopes,
		Endpoint:     google.Endpoint,
	}

	return base, &token, conf
}

func metadataString(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}
