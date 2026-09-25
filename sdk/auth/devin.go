package auth

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	devinauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/devin"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/misc"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	log "github.com/sirupsen/logrus"
)

const (
	devinLoginTimeout = 5 * time.Minute
	devinHTTPTimeout  = 30 * time.Second
)

// DevinAuthenticator implements Devin OAuth login.
type DevinAuthenticator struct {
	CallbackPort int
}

// NewDevinAuthenticator creates a Devin authenticator using an ephemeral loopback port.
func NewDevinAuthenticator() *DevinAuthenticator {
	return &DevinAuthenticator{}
}

// Provider returns the Devin provider key.
func (*DevinAuthenticator) Provider() string {
	return "devin"
}

// RefreshLead returns nil because Devin session tokens do not expose refresh expiry.
func (*DevinAuthenticator) RefreshLead() *time.Duration {
	return nil
}

// Login performs browser or manual Devin OAuth authentication.
func (authenticator *DevinAuthenticator) Login(
	ctx context.Context,
	cfg *config.Config,
	opts *LoginOptions,
) (*coreauth.Auth, error) {
	if cfg == nil {
		return nil, fmt.Errorf("cliproxy auth: configuration is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if opts == nil {
		opts = &LoginOptions{}
	}

	pkce, err := devinauth.GeneratePKCECodes()
	if err != nil {
		return nil, err
	}
	state, err := misc.GenerateRandomState()
	if err != nil {
		return nil, fmt.Errorf("generate Devin OAuth state: %w", err)
	}

	callbackPort := authenticator.CallbackPort
	if opts.CallbackPort > 0 {
		callbackPort = opts.CallbackPort
	}
	oauthServer := devinauth.NewOAuthServer(callbackPort)
	actualPort, err := oauthServer.Start()
	if err != nil {
		return nil, fmt.Errorf("start Devin OAuth callback server: %w", err)
	}
	defer func() {
		stopContext, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if stopErr := oauthServer.Stop(stopContext); stopErr != nil {
			log.Warnf("Devin OAuth server stop error: %v", stopErr)
		}
	}()

	client := util.SetProxy(&cfg.SDKConfig, &http.Client{Timeout: devinHTTPTimeout})
	service := devinauth.NewDevinAuthService(client)
	redirectURI := fmt.Sprintf("http://127.0.0.1:%d/callback", actualPort)
	authURL := service.BuildAuthorizationURL(redirectURI, pkce.CodeChallenge, state)
	openBrowserForAuth("Devin", authURL, actualPort, opts.NoBrowser)
	fmt.Println("Waiting for Devin authentication callback...")

	loginContext, cancelLogin := context.WithCancel(ctx)
	defer cancelLogin()
	callbackResults := make(chan *devinauth.OAuthResult, 1)
	callbackErrors := make(chan error, 1)
	go func() {
		result, waitErr := oauthServer.WaitForCallbackWithContext(loginContext, devinLoginTimeout)
		if waitErr != nil {
			select {
			case callbackErrors <- waitErr:
			case <-loginContext.Done():
			}
			return
		}
		select {
		case callbackResults <- result:
		case <-loginContext.Done():
		}
	}()

	authCode, pastedToken, err := waitForDevinLoginResult(
		loginContext,
		state,
		opts.Prompt,
		callbackResults,
		callbackErrors,
	)
	if err != nil {
		return nil, err
	}

	var sessionToken string
	if pastedToken != "" {
		sessionToken = devinauth.FormatSessionToken(pastedToken)
	} else {
		sessionToken, err = service.ExchangeCodeForToken(ctx, authCode, pkce.CodeVerifier)
		if err != nil {
			return nil, err
		}
	}
	record, err := service.CreateAuthRecord(ctx, sessionToken)
	if err != nil {
		return nil, err
	}
	fmt.Println("Devin authentication successful")
	return record, nil
}

func parseDevinManualLoginInput(input, expectedState string) (
	authCode,
	pastedToken string,
	accepted bool,
	err error,
) {
	input = strings.TrimSpace(input)
	if input == "" {
		return "", "", false, nil
	}
	if strings.HasPrefix(input, "devin-session-token$") || strings.HasPrefix(input, "eyJ") {
		return "", input, true, nil
	}
	parsed, parseErr := misc.ParseOAuthCallback(input)
	if parseErr == nil && parsed != nil && parsed.Code != "" {
		if parsed.State != expectedState {
			return "", "", false, fmt.Errorf("devin OAuth state mismatch")
		}
		return parsed.Code, "", true, nil
	}
	if !strings.ContainsAny(input, " \t\r\n/?#=") {
		return input, "", true, nil
	}
	return "", "", false, nil
}

func waitForDevinLoginResult(
	ctx context.Context,
	expectedState string,
	prompt func(string) (string, error),
	callbackResults <-chan *devinauth.OAuthResult,
	callbackErrors <-chan error,
) (authCode, pastedToken string, err error) {
	var promptTimer *time.Timer
	var promptTimerChannel <-chan time.Time
	if prompt != nil {
		promptTimer = time.NewTimer(5 * time.Second)
		promptTimerChannel = promptTimer.C
		defer promptTimer.Stop()
	}
	var manualInput <-chan string
	var manualErrors <-chan error

	for {
		select {
		case <-ctx.Done():
			return "", "", ctx.Err()
		case result := <-callbackResults:
			if result == nil {
				return "", "", fmt.Errorf("devin OAuth callback returned no result")
			}
			if result.Error != "" {
				return "", "", fmt.Errorf("devin OAuth error: %s", result.Error)
			}
			if result.State != expectedState {
				return "", "", fmt.Errorf("devin OAuth state mismatch")
			}
			return result.Code, "", nil
		case callbackErr := <-callbackErrors:
			return "", "", fmt.Errorf("devin OAuth callback failed: %w", callbackErr)
		case <-promptTimerChannel:
			promptTimerChannel = nil
			manualInput, manualErrors = misc.AsyncPrompt(
				prompt,
				"Paste the Devin callback URL, authorization code, or session token (or press Enter to keep waiting): ",
			)
		case input := <-manualInput:
			manualInput = nil
			manualErrors = nil
			authCode, pastedToken, accepted, parseErr := parseDevinManualLoginInput(input, expectedState)
			if parseErr != nil {
				return "", "", parseErr
			}
			if accepted {
				return authCode, pastedToken, nil
			}
		case promptErr := <-manualErrors:
			manualInput = nil
			manualErrors = nil
			if promptErr != nil {
				log.Debugf("Devin manual login prompt error: %v", promptErr)
			}
		}
	}
}
