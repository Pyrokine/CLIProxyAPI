package auth

import (
	"fmt"
	"strings"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/oauthcommon"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/browser"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/misc"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	log "github.com/sirupsen/logrus"
)

// openBrowserForAuth attempts to open a browser for OAuth authentication.
// Falls back to printing the URL with SSH tunnel instructions.
func openBrowserForAuth(providerName, authURL string, port int, noBrowser bool) {
	if !noBrowser {
		fmt.Printf("Opening browser for %s authentication\n", providerName)
		if !browser.IsAvailable() {
			log.Warn("No browser available; please open the URL manually")
			util.PrintSSHTunnelInstructions(port)
			fmt.Printf("Visit the following URL to continue authentication:\n%s\n", authURL)
		} else if err := browser.OpenURL(authURL); err != nil {
			log.Warnf("Failed to open browser automatically: %v", err)
			util.PrintSSHTunnelInstructions(port)
			fmt.Printf("Visit the following URL to continue authentication:\n%s\n", authURL)
		}
	} else {
		util.PrintSSHTunnelInstructions(port)
		fmt.Printf("Visit the following URL to continue authentication:\n%s\n", authURL)
	}
}

// waitForOAuthCallback waits for an OAuth callback from the server,
// with optional manual URL input fallback after 15 seconds.
// Returns the callback result, any error description from manual input, and an error.
func waitForOAuthCallback(
	oauthServer *oauthcommon.OAuthServer,
	providerName string,
	prompt func(string) (string, error),
) (*oauthcommon.OAuthResult, string, error) {
	callbackCh := make(chan *oauthcommon.OAuthResult, 1)
	callbackErrCh := make(chan error, 1)

	go func() {
		result, err := oauthServer.WaitForCallback(5 * time.Minute)
		if err != nil {
			callbackErrCh <- err
			return
		}
		callbackCh <- result
	}()

	var manualPromptTimer *time.Timer
	var manualPromptC <-chan time.Time
	if prompt != nil {
		manualPromptTimer = time.NewTimer(15 * time.Second)
		manualPromptC = manualPromptTimer.C
		defer manualPromptTimer.Stop()
	}

	handleErr := func(err error) error {
		if strings.Contains(err.Error(), "timeout") {
			return oauthcommon.NewAuthenticationError(oauthcommon.ErrCallbackTimeout, err)
		}
		return err
	}

	for {
		select {
		case result := <-callbackCh:
			return result, "", nil
		case err := <-callbackErrCh:
			return nil, "", handleErr(err)
		case <-manualPromptC:
			manualPromptC = nil
			if manualPromptTimer != nil {
				manualPromptTimer.Stop()
			}
			// Check channels one more time before prompting
			select {
			case result := <-callbackCh:
				return result, "", nil
			case err := <-callbackErrCh:
				return nil, "", handleErr(err)
			default:
			}
			input, errPrompt := prompt(
				fmt.Sprintf("Paste the %s callback URL (or press Enter to keep waiting): ", providerName),
			)
			if errPrompt != nil {
				return nil, "", errPrompt
			}
			parsed, errParse := misc.ParseOAuthCallback(input)
			if errParse != nil {
				return nil, "", errParse
			}
			if parsed == nil {
				continue
			}
			return &oauthcommon.OAuthResult{
				Code:  parsed.Code,
				State: parsed.State,
				Error: parsed.Error,
			}, parsed.ErrorDescription, nil
		}
	}
}
