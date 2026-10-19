// Command fetch_devin_models fetches and aggregates Devin model catalogs for offline inspection.
//
// Usage:
//
//	go run ./cmd/fetch_devin_models [flags]
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
	"sort"
	"strings"
	"time"

	devinauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/devin"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	executor "github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor/helps"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	sdkauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/auth"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
	"google.golang.org/protobuf/encoding/protowire"
)

const (
	devinGetCliModelConfigsPath = "/exa.api_server_pb.ApiServerService/GetCliModelConfigs"
	devinDefaultUserAgent       = "connect-go/1.19.1 (go1.25.0)"
)

func init() {
	logging.SetupBaseLogger()
	log.SetLevel(log.InfoLevel)
}

type devinCatalogOutput struct {
	Devin []devinModelJSON `json:"devin"`
}

type devinModelJSON struct {
	ID                        string             `json:"id"`
	Object                    string             `json:"object"`
	Type                      string             `json:"type"`
	OwnedBy                   string             `json:"owned_by"`
	DisplayName               string             `json:"display_name"`
	ContextLength             int                `json:"context_length,omitempty"`
	MaxCompletionTokens       int                `json:"max_completion_tokens,omitempty"`
	SupportedInputModalities  []string           `json:"supportedInputModalities,omitempty"`
	SupportedOutputModalities []string           `json:"supportedOutputModalities,omitempty"`
	Thinking                  *devinThinkingJSON `json:"thinking,omitempty"`
}

type devinThinkingJSON struct {
	Levels []string `json:"levels,omitempty"`
}

type rawDevinModel struct {
	UID           string
	Label         string
	ContextLength int
	Multimodal    bool
	VendorID      uint64
}

type devinCredential struct {
	auth       *coreauth.Auth
	authIndex  string
	identity   string
	token      string
	baseURL    string
	deviceSeed string
}

type devinHTTPClientFactory func(*coreauth.Auth) *http.Client

func main() {
	var authsDir string
	var configPath string
	var outputPath string
	var rawOutput bool
	var pretty bool

	flag.StringVar(&authsDir, "auths-dir", "", "Directory containing auth JSON files (overrides config auth-dir)")
	flag.StringVar(&configPath, "config", "", "Configure File Path")
	flag.StringVar(&outputPath, "output", "devin_models.json", "Output JSON file path")
	flag.BoolVar(&rawOutput, "raw", false, "Dump all raw model configurations without aggregation")
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
	authList, err := fileStore.List(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: failed to list auth files: %v\n", err)
		os.Exit(1)
	}
	credentials, err := enabledDevinCredentials(authList)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: failed to prepare Devin credentials: %v\n", err)
		os.Exit(1)
	}
	if len(credentials) == 0 {
		fmt.Fprintf(os.Stderr, "error: no enabled devin auth found in %s\n", authsDir)
		os.Exit(1)
	}
	for _, credential := range credentials {
		fmt.Printf("Using auth: auth_index=%s identity=%s\n", credential.authIndex, credential.identity)
	}
	fmt.Println("Fetching Devin model catalogs from upstream...")

	clientFactory := func(auth *coreauth.Auth) *http.Client {
		return executor.NewProxyAwareHTTPClient(ctx, cfg, auth, 30*time.Second)
	}
	rawModels, err := fetchAllRawDevinModels(ctx, credentials, clientFactory)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: fetch failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Fetched %d raw model configs from %d enabled credentials.\n", len(rawModels), len(credentials))

	output := devinCatalogOutput{}
	if rawOutput {
		output.Devin = formatRawModels(rawModels)
	} else {
		output.Devin = aggregateModels(rawModels)
	}

	var encoded []byte
	if pretty {
		encoded, err = json.MarshalIndent(output, "", "  ")
	} else {
		encoded, err = json.Marshal(output)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: failed to encode JSON: %v\n", err)
		os.Exit(1)
	}
	if containsAnySecret(encoded, credentialSecrets(credentials)) {
		fmt.Fprintln(os.Stderr, "error: encoded model catalog contains credential material")
		os.Exit(1)
	}
	if err = os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "error: failed to create output directory: %v\n", err)
		os.Exit(1)
	}
	if err = os.WriteFile(outputPath, encoded, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "error: failed to write output file: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Catalog written to: %s (%d models)\n", outputPath, len(output.Devin))
}

func enabledDevinCredentials(auths []*coreauth.Auth) ([]devinCredential, error) {
	credentials := make([]devinCredential, 0, len(auths))
	var credentialErrors []error
	for _, auth := range auths {
		if auth == nil || auth.Disabled || auth.Status == coreauth.StatusDisabled ||
			!strings.EqualFold(strings.TrimSpace(auth.Provider), "devin") {
			continue
		}
		authIndex, identity := safeAuthIdentity(auth)
		token, baseURL, deviceSeed := devinauth.CredentialsFromAuth(auth)
		if token == "" {
			credentialErrors = append(
				credentialErrors,
				fmt.Errorf("auth_index=%s identity=%s: missing session token", authIndex, identity),
			)
			continue
		}
		credentials = append(credentials, devinCredential{
			auth:       auth,
			authIndex:  authIndex,
			identity:   identity,
			token:      token,
			baseURL:    baseURL,
			deviceSeed: deviceSeed,
		})
	}
	if len(credentialErrors) > 0 {
		return nil, errors.Join(credentialErrors...)
	}
	sort.Slice(credentials, func(i, j int) bool {
		if credentials[i].authIndex == credentials[j].authIndex {
			return credentials[i].identity < credentials[j].identity
		}
		return credentials[i].authIndex < credentials[j].authIndex
	})
	return credentials, nil
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

func fetchAllRawDevinModels(
	ctx context.Context,
	credentials []devinCredential,
	clientFactory devinHTTPClientFactory,
) ([]rawDevinModel, error) {
	allModels := make([]rawDevinModel, 0)
	fetchErrors := make([]error, 0)
	for _, credential := range credentials {
		var client *http.Client
		if clientFactory != nil {
			client = clientFactory(credential.auth)
		}
		models, err := fetchRawDevinModels(ctx, credential, client)
		if err != nil {
			fetchErrors = append(fetchErrors, fmt.Errorf(
				"auth_index=%s identity=%s: %w",
				credential.authIndex,
				credential.identity,
				err,
			))
			continue
		}
		allModels = append(allModels, models...)
	}
	if len(fetchErrors) > 0 {
		return nil, errors.Join(fetchErrors...)
	}
	sort.SliceStable(allModels, func(i, j int) bool {
		if allModels[i].UID == allModels[j].UID {
			return allModels[i].Label < allModels[j].Label
		}
		return allModels[i].UID < allModels[j].UID
	})
	return allModels, nil
}

func fetchRawDevinModels(
	ctx context.Context,
	credential devinCredential,
	client *http.Client,
) ([]rawDevinModel, error) {
	fetchCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	endpoint, err := devinModelsURL(credential.baseURL)
	if err != nil {
		return nil, err
	}
	metadata := helps.BuildDevinClientMetadataBytes(credential.token, credential.deviceSeed, "")
	requestBody := make([]byte, 0, len(metadata)+16)
	requestBody = protowire.AppendTag(requestBody, 1, protowire.BytesType)
	requestBody = protowire.AppendBytes(requestBody, metadata)

	req, err := http.NewRequestWithContext(fetchCtx, http.MethodPost, endpoint, bytes.NewReader(requestBody))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Basic "+credential.token+"-"+credential.token)
	req.Header.Set("Content-Type", "application/proto")
	req.Header.Set("Connect-Protocol-Version", "1")
	req.Header.Set("User-Agent", devinDefaultUserAgent)
	if credential.auth != nil {
		util.ApplyCustomHeadersFromAttrs(req, credential.auth.Attributes)
	}
	if client == nil {
		return nil, errors.New("HTTP client is nil")
	}

	secrets := devinCredentialSecrets(credential)
	resp, err := client.Do(req)
	if err != nil {
		return nil, safeRequestError(err, secrets...)
	}
	body, err := readAndClose(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("upstream returned status %d", resp.StatusCode)
	}
	if containsAnySecret(body, secrets) {
		return nil, errors.New("model response contains credential material")
	}
	return parseRawDevinModelsProto(body)
}

func devinModelsURL(baseURL string) (string, error) {
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		baseURL = devinauth.DefaultServerURL
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("parse Devin base URL: %w", err)
	}
	if u.Scheme == "" || u.Host == "" {
		return "", errors.New("invalid Devin base URL")
	}
	u.Path = strings.TrimRight(u.Path, "/") + devinGetCliModelConfigsPath
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}

func parseRawDevinModelsProto(data []byte) ([]rawDevinModel, error) {
	models := make([]rawDevinModel, 0)
	for len(data) > 0 {
		number, wireType, consumed := protowire.ConsumeTag(data)
		if consumed < 0 {
			return nil, errors.New("malformed protobuf tag")
		}
		data = data[consumed:]
		if number == 1 && wireType == protowire.BytesType {
			submessage, size := protowire.ConsumeBytes(data)
			if size < 0 {
				return nil, errors.New("malformed protobuf model submessage")
			}
			data = data[size:]
			model, err := parseSingleModelConfig(submessage)
			if err != nil {
				return nil, err
			}
			if model.UID != "" {
				models = append(models, model)
			}
			continue
		}
		skipped := protowire.ConsumeFieldValue(number, wireType, data)
		if skipped < 0 {
			return nil, errors.New("failed to skip protobuf field")
		}
		data = data[skipped:]
	}
	return models, nil
}

func parseSingleModelConfig(data []byte) (rawDevinModel, error) {
	var model rawDevinModel
	for len(data) > 0 {
		number, wireType, consumed := protowire.ConsumeTag(data)
		if consumed < 0 {
			return rawDevinModel{}, errors.New("malformed model protobuf tag")
		}
		data = data[consumed:]

		switch number {
		case 1:
			if wireType == protowire.BytesType {
				value, size := protowire.ConsumeString(data)
				if size < 0 {
					return rawDevinModel{}, errors.New("malformed model label")
				}
				model.Label = value
				data = data[size:]
				continue
			}
		case 5:
			if wireType == protowire.VarintType {
				value, size := protowire.ConsumeVarint(data)
				if size < 0 {
					return rawDevinModel{}, errors.New("malformed model multimodal flag")
				}
				model.Multimodal = value == 1
				data = data[size:]
				continue
			}
		case 10:
			if wireType == protowire.VarintType {
				value, size := protowire.ConsumeVarint(data)
				if size < 0 {
					return rawDevinModel{}, errors.New("malformed model vendor")
				}
				model.VendorID = value
				data = data[size:]
				continue
			}
		case 18:
			if wireType == protowire.VarintType {
				value, size := protowire.ConsumeVarint(data)
				if size < 0 {
					return rawDevinModel{}, errors.New("malformed model context length")
				}
				model.ContextLength = int(value)
				data = data[size:]
				continue
			}
		case 22:
			if wireType == protowire.BytesType {
				value, size := protowire.ConsumeString(data)
				if size < 0 {
					return rawDevinModel{}, errors.New("malformed model UID")
				}
				model.UID = value
				data = data[size:]
				continue
			}
		}

		skipped := protowire.ConsumeFieldValue(number, wireType, data)
		if skipped < 0 {
			return rawDevinModel{}, errors.New("failed to skip model protobuf field")
		}
		data = data[skipped:]
	}
	return model, nil
}

func vendorName(id uint64, uid string) string {
	switch id {
	case 1:
		return "cognition"
	case 2:
		return "openai"
	case 3:
		return "anthropic"
	case 4:
		return "google"
	case 6:
		return "deepseek"
	case 7:
		return "moonshot"
	case 9:
		return "zhipu"
	case 11:
		return "nvidia"
	default:
		if strings.Contains(strings.ToLower(uid), "grok") {
			return "xai"
		}
		return "devin"
	}
}

func formatRawModels(raw []rawDevinModel) []devinModelJSON {
	models := make([]devinModelJSON, 0, len(raw))
	for _, source := range raw {
		modalities := []string{"text"}
		if source.Multimodal {
			modalities = append(modalities, "image")
		}
		models = append(models, devinModelJSON{
			ID:                        source.UID,
			Object:                    "model",
			Type:                      "devin",
			OwnedBy:                   vendorName(source.VendorID, source.UID),
			DisplayName:               source.Label,
			ContextLength:             source.ContextLength,
			MaxCompletionTokens:       helps.DevinDefaultMaxTokens,
			SupportedInputModalities:  modalities,
			SupportedOutputModalities: []string{"text"},
		})
	}
	sort.SliceStable(models, func(i, j int) bool {
		if models[i].ID == models[j].ID {
			return models[i].DisplayName < models[j].DisplayName
		}
		return models[i].ID < models[j].ID
	})
	return models
}

func splitDevinUID(uid string) (string, string) {
	if uid == "swe-1-6-slow" {
		return uid, ""
	}
	if uid == "swe-1-6-fast" {
		return "swe-1-6", ""
	}
	upper := strings.ToUpper(uid)
	for _, suffix := range []struct {
		name   string
		effort string
	}{
		{"_NONE", "none"}, {"_MINIMAL", "minimal"}, {"_LOW", "low"},
		{"_MEDIUM", "medium"}, {"_HIGH", "high"}, {"_XHIGH", "xhigh"},
		{"_MAX", "max"}, {"_THINKING", "high"},
	} {
		if strings.HasSuffix(upper, suffix.name) {
			return uid[:len(uid)-len(suffix.name)], suffix.effort
		}
	}
	for _, suffix := range []struct {
		name   string
		effort string
		readd  string
	}{
		{"-low-fast", "low", ""}, {"-medium-fast", "medium", ""},
		{"-high-fast", "high", ""}, {"-xhigh-fast", "xhigh", ""},
		{"-max-fast", "max", ""}, {"-none-fast", "none", ""},
		{"-low-priority", "low", ""}, {"-medium-priority", "medium", ""},
		{"-high-priority", "high", ""}, {"-xhigh-priority", "xhigh", ""},
		{"-max-priority", "max", ""}, {"-none-priority", "none", ""},
		{"-thinking-1m", "", "-1m"}, {"-thinking", "", ""},
		{"-max-1m", "max", "-1m"}, {"-none-1m", "none", "-1m"},
	} {
		if strings.HasSuffix(uid, suffix.name) {
			return strings.TrimSuffix(uid, suffix.name) + suffix.readd, suffix.effort
		}
	}
	for _, effort := range []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"} {
		if strings.HasSuffix(uid, "-"+effort) {
			return strings.TrimSuffix(uid, "-"+effort), effort
		}
	}
	return uid, ""
}

func aggregateModels(raw []rawDevinModel) []devinModelJSON {
	type aggregateEntry struct {
		baseID        string
		displayName   string
		vendorID      uint64
		contextLength int
		multimodal    bool
		levels        map[string]struct{}
	}

	grouped := make(map[string]*aggregateEntry)
	for _, source := range raw {
		baseID, level := splitDevinUID(source.UID)
		if baseID == "" {
			baseID = source.UID
		}
		entry := grouped[baseID]
		if entry == nil {
			levels := make(map[string]struct{})
			if known := registry.LookupDevinModel(baseID); known != nil && known.Thinking != nil {
				for _, knownLevel := range known.Thinking.Levels {
					if knownLevel != "" && knownLevel != "priority" {
						levels[knownLevel] = struct{}{}
					}
				}
			}
			entry = &aggregateEntry{
				baseID:        baseID,
				displayName:   cleanDisplayName(source.Label),
				vendorID:      source.VendorID,
				contextLength: source.ContextLength,
				multimodal:    source.Multimodal,
				levels:        levels,
			}
			grouped[baseID] = entry
		}
		if baseID == source.UID {
			if source.Label != "" {
				entry.displayName = cleanDisplayName(source.Label)
			}
			if source.VendorID != 0 {
				entry.vendorID = source.VendorID
			}
		}
		if entry.displayName == "" {
			entry.displayName = cleanDisplayName(source.Label)
		}
		if entry.vendorID == 0 && source.VendorID != 0 {
			entry.vendorID = source.VendorID
		}
		entry.multimodal = entry.multimodal || source.Multimodal
		if source.ContextLength > entry.contextLength {
			entry.contextLength = source.ContextLength
		}
		if level != "" && level != "priority" {
			entry.levels[level] = struct{}{}
		}
	}

	ids := make([]string, 0, len(grouped))
	for id := range grouped {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	models := make([]devinModelJSON, 0, len(ids))
	for _, id := range ids {
		entry := grouped[id]
		modalities := []string{"text"}
		if entry.multimodal {
			modalities = append(modalities, "image")
		}
		var thinking *devinThinkingJSON
		if len(entry.levels) > 0 {
			levels := make([]string, 0, len(entry.levels))
			for level := range entry.levels {
				levels = append(levels, level)
			}
			sortLevels(levels)
			thinking = &devinThinkingJSON{Levels: levels}
		}
		models = append(models, devinModelJSON{
			ID:                        entry.baseID,
			Object:                    "model",
			Type:                      "devin",
			OwnedBy:                   vendorName(entry.vendorID, entry.baseID),
			DisplayName:               entry.displayName,
			ContextLength:             entry.contextLength,
			MaxCompletionTokens:       helps.DevinDefaultMaxTokens,
			SupportedInputModalities:  modalities,
			SupportedOutputModalities: []string{"text"},
			Thinking:                  thinking,
		})
	}
	return models
}

func cleanDisplayName(label string) string {
	name := strings.TrimSpace(label)
	for {
		changed := false
		for _, suffix := range []string{
			" Low Thinking Fast", " Medium Thinking Fast", " High Thinking Fast",
			" XHigh Thinking Fast", " Max Thinking Fast", " No Thinking Fast",
			" Low Fast", " Medium Fast", " High Fast", " XHigh Fast", " Max Fast",
			" Low Thinking", " Medium Thinking", " High Thinking",
			" XHigh Thinking", " Max Thinking", " No Thinking",
			" Low", " Medium", " High", " XHigh", " Max", " None", " Minimal",
			" Thinking", " Fast", " Priority",
		} {
			if strings.HasSuffix(strings.ToLower(name), strings.ToLower(suffix)) {
				name = strings.TrimSpace(name[:len(name)-len(suffix)])
				changed = true
				break
			}
		}
		if !changed {
			return name
		}
	}
}

func sortLevels(levels []string) {
	rank := map[string]int{
		"none":    0,
		"minimal": 1,
		"low":     2,
		"medium":  3,
		"high":    4,
		"xhigh":   5,
		"max":     6,
		"fast":    7,
	}
	sort.SliceStable(levels, func(i, j int) bool {
		left, leftKnown := rank[levels[i]]
		if !leftKnown {
			left = 99
		}
		right, rightKnown := rank[levels[j]]
		if !rightKnown {
			right = 99
		}
		if left == right {
			return levels[i] < levels[j]
		}
		return left < right
	})
}

func credentialSecrets(credentials []devinCredential) []string {
	var secrets []string
	for _, credential := range credentials {
		secrets = append(secrets, devinCredentialSecrets(credential)...)
	}
	return secrets
}

func devinCredentialSecrets(credential devinCredential) []string {
	secrets := []string{credential.token}
	if credential.auth == nil {
		return secrets
	}
	for _, value := range credential.auth.Attributes {
		secrets = append(secrets, strings.TrimSpace(value))
	}
	for _, raw := range credential.auth.Metadata {
		if value, ok := raw.(string); ok {
			secrets = append(secrets, strings.TrimSpace(value))
		}
	}
	return secrets
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
