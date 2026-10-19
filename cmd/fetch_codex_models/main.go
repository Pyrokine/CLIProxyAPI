// Command fetch_codex_models fetches the official Codex client model catalog for offline inspection.
//
// Usage:
//
//	go run ./cmd/fetch_codex_models [flags]
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
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	codexauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/codex"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	sdkauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/auth"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/proxyutil"
	log "github.com/sirupsen/logrus"
)

const (
	codexModelsBaseURL       = "https://chatgpt.com/backend-api/codex"
	codexModelsPath          = "/models"
	defaultClientVersion     = "0.159.0"
	defaultCodexUserAgent    = "codex_cli_rs/0.159.0 (Mac OS 26.3.1; arm64) iTerm.app/3.6.9"
	defaultCodexOriginator   = "codex_cli_rs"
	accessTokenRefreshLeeway = 30 * time.Second
)

func init() {
	logging.SetupBaseLogger()
	log.SetLevel(log.InfoLevel)
}

func main() {
	var authsDir string
	var configPath string
	var outputPath string
	var clientVersion string
	var pretty bool

	flag.StringVar(&authsDir, "auths-dir", "", "Directory containing auth JSON files (overrides config auth-dir)")
	flag.StringVar(&configPath, "config", "", "Configure File Path")
	flag.StringVar(&outputPath, "output", "codex_client_models.json", "Output JSON file path")
	flag.StringVar(&clientVersion, "client-version", defaultClientVersion, "Codex client_version query value")
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
	chosen := findCodexAuth(auths)
	if chosen == nil {
		fmt.Fprintf(os.Stderr, "error: no enabled codex auth found in %s\n", authsDir)
		os.Exit(1)
	}

	authIndex, identity := safeAuthIdentity(chosen)
	fmt.Printf("Using auth: auth_index=%s identity=%s\n", authIndex, identity)
	accessToken, refreshed, err := ensureAccessToken(ctx, fileStore, chosen)
	if err != nil {
		fmt.Fprintf(
			os.Stderr,
			"error: failed to prepare codex access token: %v\n",
			safeRequestError(err, credentialSecrets(chosen, accessToken)...),
		)
		os.Exit(1)
	}
	if refreshed {
		fmt.Println("Refreshed Codex access token.")
	}

	fmt.Println("Fetching Codex model list from upstream...")
	raw, count, err := fetchModels(ctx, chosen, accessToken, clientVersion)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: failed to fetch codex models: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Fetched %d models.\n", count)

	if pretty {
		raw, err = prettyJSON(raw)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: failed to format JSON: %v\n", err)
			os.Exit(1)
		}
	}
	if err = os.WriteFile(outputPath, raw, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "error: failed to write output file %s: %v\n", outputPath, err)
		os.Exit(1)
	}
	fmt.Printf("Model list saved to: %s\n", outputPath)
}

func findCodexAuth(auths []*coreauth.Auth) *coreauth.Auth {
	for _, auth := range auths {
		if auth == nil || auth.Disabled || auth.Status == coreauth.StatusDisabled {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
			continue
		}
		if metaStringValue(auth.Metadata, "access_token") == "" && metaStringValue(auth.Metadata, "refresh_token") == "" {
			continue
		}
		return auth
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

func ensureAccessToken(
	ctx context.Context,
	store *sdkauth.FileTokenStore,
	auth *coreauth.Auth,
) (string, bool, error) {
	accessToken := metaStringValue(auth.Metadata, "access_token")
	if accessToken != "" {
		if expiresAt, ok := auth.ExpirationTime(); !ok || time.Now().Add(accessTokenRefreshLeeway).Before(expiresAt) {
			return accessToken, false, nil
		}
	}

	refreshToken := metaStringValue(auth.Metadata, "refresh_token")
	if refreshToken == "" {
		if accessToken != "" {
			return accessToken, false, nil
		}
		return "", false, errors.New("missing access_token and refresh_token")
	}

	refreshConfig := &config.Config{}
	refreshConfig.ProxyURL = auth.ProxyURL
	service := codexauth.NewAuth(refreshConfig)
	tokenData, err := service.RefreshTokensWithRetry(ctx, refreshToken, 3)
	if err != nil {
		return "", false, err
	}
	if strings.TrimSpace(tokenData.AccessToken) == "" {
		return "", false, errors.New("refresh response did not include access_token")
	}

	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["id_token"] = tokenData.IDToken
	auth.Metadata["access_token"] = tokenData.AccessToken
	if tokenData.RefreshToken != "" {
		auth.Metadata["refresh_token"] = tokenData.RefreshToken
	}
	if tokenData.AccountID != "" {
		auth.Metadata["account_id"] = tokenData.AccountID
	}
	if tokenData.Email != "" {
		auth.Metadata["email"] = tokenData.Email
	}
	auth.Metadata["expired"] = tokenData.Expire
	auth.Metadata["type"] = "codex"
	auth.Metadata["last_refresh"] = time.Now().Format(time.RFC3339)
	applyRefreshedCodexPlan(auth, tokenData)

	if _, errSave := store.Save(ctx, auth); errSave != nil {
		return "", false, fmt.Errorf("save refreshed auth: %w", errSave)
	}
	return tokenData.AccessToken, true, nil
}

func applyRefreshedCodexPlan(auth *coreauth.Auth, tokenData *codexauth.TokenData) {
	planType := strings.TrimSpace(tokenData.PlanType)
	if planType == "" && tokenData.IDToken != "" {
		if claims, err := codexauth.ParseJWTToken(tokenData.IDToken); err == nil && claims != nil {
			planType = claims.GetPlanType()
		}
	}
	if planType == "" {
		planType = codexauth.DefaultPlanType
	}
	auth.Metadata["plan_type"] = planType
	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	auth.Attributes["plan_type"] = planType
}

func fetchModels(
	ctx context.Context,
	auth *coreauth.Auth,
	accessToken string,
	clientVersion string,
) ([]byte, int, error) {
	client, err := codexHTTPClient(auth)
	if err != nil {
		return nil, 0, err
	}
	return fetchModelsWithClient(ctx, auth, accessToken, clientVersion, client)
}

func codexHTTPClient(auth *coreauth.Auth) (*http.Client, error) {
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

func fetchModelsWithClient(
	ctx context.Context,
	auth *coreauth.Auth,
	accessToken string,
	clientVersion string,
	client *http.Client,
) ([]byte, int, error) {
	modelsURL, err := codexModelsURL(clientVersion)
	if err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, modelsURL, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Close = true
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Originator", defaultCodexOriginator)
	req.Header.Set("User-Agent", defaultCodexUserAgent)
	if auth != nil {
		if accountID := metaStringValue(auth.Metadata, "account_id"); accountID != "" {
			req.Header.Set("Chatgpt-Account-Id", accountID)
		}
		util.ApplyCustomHeadersFromAttrs(req, auth.Attributes)
	}

	if client == nil {
		return nil, 0, errors.New("HTTP client is nil")
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, safeRequestError(err, credentialSecrets(auth, accessToken)...)
	}
	body, err := readAndClose(resp.Body)
	if err != nil {
		return nil, 0, err
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, 0, fmt.Errorf("models request failed with status %d", resp.StatusCode)
	}
	if containsCredentialMaterial(body, credentialSecrets(auth, accessToken)) {
		return nil, 0, errors.New("models response contains credential material")
	}
	count, err := countModels(body)
	if err != nil {
		return nil, 0, err
	}
	return body, count, nil
}

func codexModelsURL(clientVersion string) (string, error) {
	u, err := url.Parse(codexModelsBaseURL + codexModelsPath)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(clientVersion) != "" {
		query := u.Query()
		query.Set("client_version", strings.TrimSpace(clientVersion))
		u.RawQuery = query.Encode()
	}
	return u.String(), nil
}

func countModels(raw []byte) (int, error) {
	var payload struct {
		Models []json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return 0, fmt.Errorf("parse response JSON: %w", err)
	}
	if payload.Models == nil {
		return 0, errors.New("response JSON does not contain models array")
	}
	return len(payload.Models), nil
}

func prettyJSON(raw []byte) ([]byte, error) {
	var buffer bytes.Buffer
	if err := json.Indent(&buffer, raw, "", "  "); err != nil {
		return nil, err
	}
	buffer.WriteByte('\n')
	return buffer.Bytes(), nil
}

func credentialSecrets(auth *coreauth.Auth, additional ...string) []string {
	secrets := append([]string(nil), additional...)
	if auth == nil {
		return secrets
	}
	for _, value := range auth.Attributes {
		secrets = append(secrets, strings.TrimSpace(value))
	}
	for _, raw := range auth.Metadata {
		if value, ok := raw.(string); ok {
			secrets = append(secrets, strings.TrimSpace(value))
		}
	}
	return secrets
}

func containsCredentialMaterial(raw []byte, secrets []string) bool {
	for _, secret := range secrets {
		secret = strings.TrimSpace(secret)
		if len(secret) >= 8 && bytes.Contains(raw, []byte(secret)) {
			return true
		}
	}
	return false
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
