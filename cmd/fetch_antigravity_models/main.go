// Command fetch_antigravity_models fetches the Antigravity model catalog for offline inspection.
//
// Usage:
//
//	go run ./cmd/fetch_antigravity_models [flags]
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/misc"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	sdkauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/auth"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/proxyutil"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

const (
	antigravityBaseURLDaily        = "https://daily-cloudcode-pa.googleapis.com"
	antigravityBaseURLProd         = "https://cloudcode-pa.googleapis.com"
	antigravitySandboxBaseURLDaily = "https://daily-cloudcode-pa.sandbox.googleapis.com"
	antigravityModelsPath          = "/v1internal:fetchAvailableModels"
	maxFetchAttemptsPerEndpoint    = 2
)

func init() {
	logging.SetupBaseLogger()
	log.SetLevel(log.InfoLevel)
}

type modelOutput struct {
	Models []modelEntry `json:"models"`
}

type modelEntry struct {
	ID                  string `json:"id"`
	Object              string `json:"object"`
	OwnedBy             string `json:"owned_by"`
	Type                string `json:"type"`
	DisplayName         string `json:"display_name"`
	Name                string `json:"name"`
	Description         string `json:"description"`
	ContextLength       int    `json:"context_length,omitempty"`
	MaxCompletionTokens int    `json:"max_completion_tokens,omitempty"`
}

func main() {
	var authsDir string
	var configPath string
	var outputPath string
	var pretty bool

	flag.StringVar(&authsDir, "auths-dir", "", "Directory containing auth JSON files (overrides config auth-dir)")
	flag.StringVar(&configPath, "config", "", "Configure File Path")
	flag.StringVar(&outputPath, "output", "antigravity_models.json", "Output JSON file path")
	flag.BoolVar(&pretty, "pretty", true, "Pretty-print the output JSON")
	flag.Parse()

	authsDirOverridden := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "auths-dir" {
			authsDirOverridden = true
		}
	})

	wd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: cannot get working directory: %v\n", err)
		os.Exit(1)
	}
	if strings.TrimSpace(configPath) == "" {
		configPath = filepath.Join(wd, "config.yaml")
	}
	cfg, err := config.LoadConfigOptional(configPath, false)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: failed to load config file %s: %v\n", configPath, err)
		os.Exit(1)
	}
	if cfg == nil {
		cfg = &config.Config{}
	}

	if !authsDirOverridden {
		authsDir = cfg.AuthDir
	} else if strings.TrimSpace(authsDir) != "" &&
		!strings.HasPrefix(strings.TrimSpace(authsDir), "~") &&
		!filepath.IsAbs(authsDir) {
		authsDir = filepath.Join(wd, authsDir)
	}
	if authsDir, err = util.ResolveAuthDir(authsDir); err != nil {
		fmt.Fprintf(os.Stderr, "error: failed to resolve auth directory: %v\n", err)
		os.Exit(1)
	}
	if !filepath.IsAbs(outputPath) {
		outputPath = filepath.Join(wd, outputPath)
	}

	fmt.Printf("Scanning auth files in: %s\n", authsDir)
	fileStore := sdkauth.NewFileTokenStore()
	fileStore.SetBaseDir(authsDir)

	ctx := context.Background()
	auths, err := fileStore.List(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: failed to list auth files: %v\n", err)
		os.Exit(1)
	}
	chosen := findAntigravityAuth(auths)
	if chosen == nil {
		fmt.Fprintf(os.Stderr, "error: no enabled antigravity auth found in %s\n", authsDir)
		os.Exit(1)
	}

	authIndex, identity := safeAuthIdentity(chosen)
	fmt.Printf("Using auth: auth_index=%s identity=%s\n", authIndex, identity)
	fmt.Println("Fetching Antigravity model list from upstream...")

	fetchCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	models, err := fetchModels(fetchCtx, chosen)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: failed to fetch Antigravity models: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Fetched %d models.\n", len(models))

	var raw []byte
	if pretty {
		raw, err = json.MarshalIndent(modelOutput{Models: models}, "", "  ")
	} else {
		raw, err = json.Marshal(modelOutput{Models: models})
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: failed to marshal JSON: %v\n", err)
		os.Exit(1)
	}
	if err = os.WriteFile(outputPath, raw, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "error: failed to write output file %s: %v\n", outputPath, err)
		os.Exit(1)
	}
	fmt.Printf("Model list saved to: %s\n", outputPath)
}

func findAntigravityAuth(auths []*coreauth.Auth) *coreauth.Auth {
	for _, auth := range auths {
		if auth == nil || auth.Disabled || auth.Status == coreauth.StatusDisabled {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(auth.Provider), "antigravity") {
			return auth
		}
	}
	return nil
}

func safeAuthIdentity(auth *coreauth.Auth) (string, string) {
	if auth == nil {
		return "", ""
	}
	identity := strings.TrimSpace(auth.FileName)
	if identity == "" {
		identity = strings.TrimSpace(auth.ID)
	}
	if identity != "" {
		identity = filepath.Base(filepath.Clean(identity))
	}
	if identity == "" || identity == "." || identity == string(filepath.Separator) {
		identity = strings.ToLower(strings.TrimSpace(auth.Provider))
	}
	return auth.EnsureIndex(), identity
}

func defaultAntigravityFetchBaseURLs() []string {
	return []string{antigravityBaseURLDaily, antigravityBaseURLProd, antigravitySandboxBaseURLDaily}
}

func fetchModels(ctx context.Context, auth *coreauth.Auth) ([]modelEntry, error) {
	return fetchModelsFromBaseURLs(ctx, auth, defaultAntigravityFetchBaseURLs(), nil)
}

func fetchModelsFromBaseURLs(
	ctx context.Context,
	auth *coreauth.Auth,
	baseURLs []string,
	client *http.Client,
) ([]modelEntry, error) {
	accessToken := ""
	if auth != nil {
		accessToken = metaStringValue(auth.Metadata, "access_token")
	}
	if accessToken == "" {
		return nil, errors.New("no access token found in auth")
	}

	if client == nil {
		var errClient error
		client, errClient = antigravityHTTPClient(auth)
		if errClient != nil {
			return nil, errClient
		}
	}

	payload := []byte(`{}`)
	if auth != nil {
		if projectID := metaStringValue(auth.Metadata, "project_id"); projectID != "" {
			var errPayload error
			payload, errPayload = json.Marshal(map[string]string{"project": projectID})
			if errPayload != nil {
				return nil, fmt.Errorf("encode project request: %w", errPayload)
			}
		}
	}

	secrets := authStringValues(auth, accessToken)
	attemptErrors := make([]error, 0, len(baseURLs)*maxFetchAttemptsPerEndpoint)
	for _, rawBaseURL := range baseURLs {
		baseURL := strings.TrimRight(strings.TrimSpace(rawBaseURL), "/")
		modelsURL := baseURL + antigravityModelsPath
		for attempt := 1; attempt <= maxFetchAttemptsPerEndpoint; attempt++ {
			models, errFetch := fetchAntigravityModelsAttempt(ctx, client, modelsURL, payload, accessToken, secrets)
			if errFetch == nil {
				return models, nil
			}
			attemptErrors = append(attemptErrors, fmt.Errorf("%s attempt %d: %w", baseURL, attempt, errFetch))
			if ctx.Err() != nil {
				return nil, errors.Join(attemptErrors...)
			}
		}
	}
	if len(attemptErrors) == 0 {
		return nil, errors.New("no Antigravity endpoints configured")
	}
	return nil, errors.Join(attemptErrors...)
}

func antigravityHTTPClient(auth *coreauth.Auth) (*http.Client, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	if auth == nil || strings.TrimSpace(auth.ProxyURL) == "" {
		return client, nil
	}
	transport, _, err := proxyutil.BuildHTTPTransport(auth.ProxyURL)
	if err != nil {
		return nil, fmt.Errorf("build auth proxy transport: %w", err)
	}
	client.Transport = transport
	return client, nil
}

func fetchAntigravityModelsAttempt(
	ctx context.Context,
	client *http.Client,
	modelsURL string,
	payload []byte,
	accessToken string,
	secrets []string,
) ([]modelEntry, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, modelsURL, strings.NewReader(string(payload)))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Close = true
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("User-Agent", misc.AntigravityUserAgent())

	resp, err := client.Do(req)
	if err != nil {
		return nil, safeRequestError(err, secrets...)
	}
	body, err := readAndClose(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("upstream returned status %d", resp.StatusCode)
	}
	if containsAnySecret(body, secrets) {
		return nil, errors.New("models response contains credential material")
	}
	return parseAntigravityModels(body)
}

func parseAntigravityModels(body []byte) ([]modelEntry, error) {
	result := gjson.GetBytes(body, "models")
	if !result.Exists() || !result.IsObject() {
		return nil, errors.New("response JSON does not contain models object")
	}

	models := make([]modelEntry, 0, len(result.Map()))
	for originalName, modelData := range result.Map() {
		modelID := strings.TrimSpace(originalName)
		if modelID == "" {
			continue
		}
		switch modelID {
		case "chat_20706", "chat_23310", "tab_flash_lite_preview", "tab_jump_flash_lite_preview",
			"gemini-2.5-flash-thinking", "gemini-2.5-pro":
			continue
		}

		displayName := strings.TrimSpace(modelData.Get("displayName").String())
		if displayName == "" {
			displayName = modelID
		}
		entry := modelEntry{
			ID:          modelID,
			Object:      "model",
			OwnedBy:     "antigravity",
			Type:        "antigravity",
			DisplayName: displayName,
			Name:        modelID,
			Description: displayName,
		}
		if maxTokens := modelData.Get("maxTokens").Int(); maxTokens > 0 {
			entry.ContextLength = int(maxTokens)
		}
		if maxOutputTokens := modelData.Get("maxOutputTokens").Int(); maxOutputTokens > 0 {
			entry.MaxCompletionTokens = int(maxOutputTokens)
		}
		models = append(models, entry)
	}
	sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })
	return models, nil
}

func readAndClose(body io.ReadCloser) ([]byte, error) {
	if body == nil {
		return nil, errors.New("response body is nil")
	}
	raw, errRead := io.ReadAll(body)
	errClose := body.Close()
	var responseErrors []error
	if errRead != nil {
		responseErrors = append(responseErrors, fmt.Errorf("read response body: %w", errRead))
	}
	if errClose != nil {
		responseErrors = append(responseErrors, fmt.Errorf("close response body: %w", errClose))
	}
	if len(responseErrors) > 0 {
		return nil, errors.Join(responseErrors...)
	}
	return raw, nil
}

func authStringValues(auth *coreauth.Auth, additional ...string) []string {
	values := append([]string(nil), additional...)
	if auth == nil {
		return values
	}
	for _, value := range auth.Attributes {
		values = append(values, strings.TrimSpace(value))
	}
	for _, raw := range auth.Metadata {
		if value, ok := raw.(string); ok {
			values = append(values, strings.TrimSpace(value))
		}
	}
	return values
}

func containsAnySecret(raw []byte, secrets []string) bool {
	for _, secret := range secrets {
		secret = strings.TrimSpace(secret)
		if len(secret) >= 8 && bytes.Contains(raw, []byte(secret)) {
			return true
		}
	}
	return false
}

func safeRequestError(err error, secrets ...string) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	message := err.Error()
	for _, secret := range secrets {
		secret = strings.TrimSpace(secret)
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "[REDACTED]")
		}
	}
	return errors.New(message)
}

func metaStringValue(metadata map[string]any, key string) string {
	if metadata == nil {
		return ""
	}
	value, _ := metadata[key].(string)
	return strings.TrimSpace(value)
}
