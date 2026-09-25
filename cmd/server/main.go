// Package main provides the entry point for the CLI Proxy API server.
// This server acts as a proxy that provides OpenAI/Gemini/Claude compatible API interfaces
// for CLI models, allowing CLI models to be used with tools and libraries designed for standard AI APIs.
package main

import (
	"context"
	cryptorand "crypto/rand"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	configaccess "github.com/Pyrokine/CLIProxyAPI/v6/internal/access/config_access"
	managementHandlers "github.com/Pyrokine/CLIProxyAPI/v6/internal/api/handlers/management"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/buildinfo"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/cmd"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/home"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/homeplugins"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/managementasset"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/misc"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/redisqueue"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/safemode"
	_ "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/usage"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	sdkapi "github.com/Pyrokine/CLIProxyAPI/v6/sdk/api"
	sdkAuth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/joho/godotenv"
	log "github.com/sirupsen/logrus"
	"golang.org/x/term"
)

var (
	version           = "dev"
	commit            = "none"
	buildDate         = "unknown"
	defaultConfigPath = ""
)

func shouldEnableExampleAPIKeySafeMode(
	cfg *config.Config,
	commandMode, tuiMode, standalone, cloudConfigMissing, homeMode bool,
) bool {
	if cfg == nil || commandMode || homeMode || cloudConfigMissing {
		return false
	}
	if tuiMode && !standalone {
		return false
	}
	return safemode.HasExampleAPIKeys(cfg.APIKeys)
}

func shouldCreateExampleAPIKeyLocalManagementPassword(cfg *config.Config, currentPassword string) bool {
	if cfg == nil || strings.TrimSpace(currentPassword) != "" || strings.TrimSpace(os.Getenv("MANAGEMENT_PASSWORD")) != "" {
		return false
	}
	return strings.TrimSpace(cfg.RemoteManagement.SecretKey) == ""
}

func newExampleAPIKeyLocalManagementPassword() string {
	b := make([]byte, 16)
	if _, err := cryptorand.Read(b); err != nil {
		return ""
	}
	return fmt.Sprintf("safe-mode-%x", b)
}

func writeExampleAPIKeyLocalManagementPassword(output *os.File, password string) bool {
	if output == nil || strings.TrimSpace(password) == "" {
		return false
	}
	if !term.IsTerminal(int(output.Fd())) {
		return false
	}
	_, err := fmt.Fprintf(
		output,
		"Temporary local management password for example API key safe mode: %s\n",
		password,
	)
	return err == nil
}

// init initializes the shared logger setup.
func init() {
	logging.SetupBaseLogger()
	buildinfo.Version = version
	buildinfo.Commit = commit
	buildinfo.BuildDate = buildDate
}

// main is the entry point of the application.
// It parses command-line flags, loads configuration, and starts the appropriate
// service based on the provided flags (login, codex-login, or server mode).
func main() {
	if handled, exitCode := managementHandlers.RunUpdateGuardCommand(os.Args[1:]); handled {
		if exitCode != 0 {
			os.Exit(exitCode)
		}
		return
	}

	fmt.Printf(
		"CLIProxyAPI Version: %s, Commit: %s, BuiltAt: %s\n", buildinfo.Version, buildinfo.Commit, buildinfo.BuildDate,
	)

	// Command-line flags
	var (
		lf                          loginFlags
		configPath                  string
		password                    string
		tuiMode                     bool
		standalone                  bool
		localModel                  bool
		testConfig                  bool
		homeJWT                     string
		homeDisableClusterDiscovery bool
	)

	flag.BoolVar(&lf.login, "login", false, "Login Google Account")
	flag.BoolVar(&lf.codexLogin, "codex-login", false, "Login to Codex using OAuth")
	flag.BoolVar(&lf.codexDeviceLogin, "codex-device-login", false, "Login to Codex using device code flow")
	flag.BoolVar(&lf.claudeLogin, "claude-login", false, "Login to Claude using OAuth")
	flag.BoolVar(&lf.qwenLogin, "qwen-login", false, "Login to Qwen using OAuth")
	flag.BoolVar(&lf.iflowLogin, "iflow-login", false, "Login to iFlow using OAuth")
	flag.BoolVar(&lf.iflowCookie, "iflow-cookie", false, "Login to iFlow using Cookie")
	flag.BoolVar(&lf.noBrowser, "no-browser", false, "Don't open browser automatically for OAuth")
	flag.IntVar(
		&lf.oauthCallbackPort, "oauth-callback-port", 0,
		"Override OAuth callback port (defaults to provider-specific port)",
	)
	flag.BoolVar(&lf.antigravityLogin, "antigravity-login", false, "Login to Antigravity using OAuth")
	flag.BoolVar(&lf.kimiLogin, "kimi-login", false, "Login to Kimi using OAuth")
	flag.BoolVar(&lf.xaiLogin, "xai-login", false, "Login to xAI using OAuth device code flow")
	flag.BoolVar(&lf.devinLogin, "devin-login", false, "Login to Devin using OAuth")
	flag.StringVar(&lf.projectID, "project_id", "", "Project ID (Gemini only, not required)")
	flag.StringVar(&configPath, "config", defaultConfigPath, "Configure File Path")
	flag.StringVar(&lf.vertexImport, "vertex-import", "", "Import Vertex service account key JSON file")
	flag.StringVar(&lf.vertexImportPrefix, "vertex-import-prefix", "", "Prefix for imported Vertex model names")
	flag.StringVar(&password, "password", "", "")
	flag.BoolVar(&tuiMode, "tui", false, "Start with terminal management UI")
	flag.BoolVar(&standalone, "standalone", false, "In TUI mode, start an embedded local server")
	flag.BoolVar(&localModel, "local-model", false, "Use embedded model catalog only, skip remote model fetching")
	flag.BoolVar(&testConfig, "t", false, "Test configuration and exit")
	flag.StringVar(&homeJWT, "home-jwt", "", "Home control plane JWT for mTLS certificate bootstrap and connection")
	flag.BoolVar(
		&homeDisableClusterDiscovery, "home-disable-cluster-discovery", false,
		"Disable Home CLUSTER NODES discovery and keep using the configured -home-jwt address",
	)

	flag.CommandLine.Usage = func() {
		out := flag.CommandLine.Output()
		_, _ = fmt.Fprintf(out, "Usage of %s\n", os.Args[0])
		flag.CommandLine.VisitAll(
			func(f *flag.Flag) {
				if f.Name == "password" {
					return
				}
				s := fmt.Sprintf("  -%s", f.Name)
				name, unquoteUsage := flag.UnquoteUsage(f)
				if name != "" {
					s += " " + name
				}
				if len(s) <= 4 {
					s += "	"
				} else {
					s += "\n    "
				}
				if unquoteUsage != "" {
					s += unquoteUsage
				}
				if f.DefValue != "" && f.DefValue != "false" && f.DefValue != "0" {
					s += fmt.Sprintf(" (default %s)", f.DefValue)
				}
				_, _ = fmt.Fprint(out, s+"\n")
			},
		)
	}

	pluginHost := cliproxy.NewPluginHost()
	if bootstrapCfg := loadPluginBootstrapConfig(
		pluginBootstrapConfigPath(
			os.Args[1:], defaultConfigPath,
		),
	); bootstrapCfg != nil {
		if errApply := pluginHost.ApplyConfig(context.Background(), bootstrapCfg); errApply != nil {
			log.Errorf("failed to apply bootstrap plugin config: %v", errApply)
			return
		}
		pluginHost.RegisterCommandLineFlags(context.Background(), flag.CommandLine)
	}

	flag.Parse()

	wd, err := os.Getwd()
	if err != nil {
		log.Errorf("failed to get working directory: %v", err)
		return
	}

	// Load environment variables from .env if present.
	if errLoad := godotenv.Load(filepath.Join(wd, ".env")); errLoad != nil {
		if !errors.Is(errLoad, os.ErrNotExist) {
			log.WithError(errLoad).Warn("failed to load .env file")
		}
	}

	lookupEnv := func(keys ...string) (string, bool) {
		for _, key := range keys {
			if value, ok := os.LookupEnv(key); ok {
				if trimmed := strings.TrimSpace(value); trimmed != "" {
					return trimmed, true
				}
			}
		}
		return "", false
	}

	if strings.TrimSpace(homeJWT) == "" {
		if v, ok := lookupEnv("HOME_JWT", "home_jwt"); ok {
			homeJWT = v
		}
	}

	isCloudDeploy := os.Getenv("DEPLOY") == "cloud"

	result, err := initStoreAndConfig(
		wd, configPath, isCloudDeploy, lookupEnv, homeJWT, homeDisableClusterDiscovery, pluginHost,
	)
	if err != nil {
		log.Errorf("%v", err)
		os.Exit(1)
	}
	cfg := result.cfg
	configFilePath := result.configFilePath
	defer func() {
		if result.homeClient != nil {
			result.homeClient.Close()
		}
	}()

	// -t: test configuration and exit
	if testConfig {
		warnings := config.ValidateConfig(cfg)
		if len(warnings) == 0 {
			fmt.Printf("%s: configuration is valid\n", configFilePath)
		} else {
			fmt.Printf("%s: configuration has %d warning(s):\n", configFilePath, len(warnings))
			for _, w := range warnings {
				fmt.Printf("  - %s\n", w)
			}
			os.Exit(1)
		}
		return
	}

	// Cloud deploy mode: check configuration availability
	var configFileExists bool
	if isCloudDeploy {
		configFileExists = cloudConfigAvailable(configFilePath, cfg)
	}
	usage.SetStatisticsEnabled(cfg.UsageStatisticsEnabled)
	redisqueue.SetUsageStatisticsEnabled(cfg.UsageStatisticsEnabled)
	redisqueue.SetRetentionSeconds(cfg.RedisUsageQueueRetentionSeconds)
	coreauth.SetQuotaCooldownDisabled(cfg.DisableCooling)

	if err = logging.ConfigureLogOutput(cfg); err != nil {
		log.Errorf("failed to configure log output: %v", err)
		return
	}

	log.Infof(
		"CLIProxyAPI Version: %s, Commit: %s, BuiltAt: %s", buildinfo.Version, buildinfo.Commit, buildinfo.BuildDate,
	)
	util.SetLogLevel(cfg)

	if resolvedAuthDir, errResolveAuthDir := util.ResolveAuthDir(cfg.AuthDir); errResolveAuthDir != nil {
		log.Errorf("failed to resolve auth directory: %v", errResolveAuthDir)
		return
	} else {
		cfg.AuthDir = resolvedAuthDir
	}
	managementasset.SetCurrentConfig(cfg)

	// Register the shared token store.
	sdkAuth.RegisterTokenStore(result.tokenStore)

	// Register built-in access providers before constructing services.
	configaccess.Register(&cfg.SDKConfig)
	if errApply := pluginHost.ApplyConfig(context.Background(), cfg); errApply != nil {
		log.Errorf("failed to apply plugin config: %v", errApply)
		return
	}
	if result.configLoadedFromHome {
		errHomePluginLoad := homeplugins.MarkLoadResults(&result.homePluginSyncReport, pluginHost)
		if result.homeClient == nil {
			log.Warn("failed to report home plugin load status: home client is unavailable")
		} else if errReportPlugins := home.ReportPluginStatus(
			context.Background(), result.homeClient, cfg.Home.NodeID, result.homePluginSyncReport,
		); errReportPlugins != nil {
			log.Warnf("failed to report home plugin load status: %v", errReportPlugins)
		}
		if errHomePluginLoad != nil {
			log.Errorf("failed to load home plugins: %v", errHomePluginLoad)
			return
		}
	}
	if result.homeClient != nil {
		result.homeClient.Close()
		result.homeClient = nil
	}
	if pluginHost.HasTriggeredCommandLineFlags() {
		if exitCode, handled := pluginHost.ExecuteCommandLine(
			context.Background(), os.Args[0], os.Args[1:], configFilePath, flag.CommandLine,
		); handled {
			if exitCode != 0 {
				os.Exit(exitCode)
			}
			return
		}
	}

	// Handle login modes.
	if runLoginMode(cfg, &lf) {
		return
	}

	// In cloud deploy mode without config file, just wait for shutdown signals.
	if isCloudDeploy && !configFileExists {
		cmd.WaitForCloudDeploy()
		return
	}

	if localModel {
		log.Info("Local model mode: using embedded model catalog, remote model updates disabled")
	}

	commandMode := lf.vertexImport != "" || lf.login || lf.codexLogin || lf.codexDeviceLogin || lf.claudeLogin || lf.qwenLogin || lf.iflowLogin || lf.iflowCookie || lf.antigravityLogin || lf.kimiLogin || lf.xaiLogin || lf.devinLogin
	homeMode := cfg.Home.Enabled
	serverOptions := []sdkapi.ServerOption(nil)
	if shouldEnableExampleAPIKeySafeMode(cfg, commandMode, tuiMode, standalone, false, homeMode) {
		matches := safemode.ExampleAPIKeys(cfg.APIKeys)
		log.WithField(
			"api_keys", strings.Join(matches, ","),
		).Error("unsafe example API key configured; proxy API endpoints disabled until api-keys is updated")
		if shouldCreateExampleAPIKeyLocalManagementPassword(cfg, password) {
			password = newExampleAPIKeyLocalManagementPassword()
			switch {
			case password == "":
				log.Error("failed to create local management password for example API key safe mode")
			case !writeExampleAPIKeyLocalManagementPassword(os.Stderr, password):
				password = ""
				log.Error(
					"example API key safe mode requires MANAGEMENT_PASSWORD or remote-management secret-key when no interactive terminal is available",
				)
			default:
				log.Warn("temporary local management password written to the interactive terminal")
			}
		}
		serverOptions = append(serverOptions, sdkapi.WithExampleAPIKeySafeMode())
	}

	if tuiMode {
		if standalone {
			managementasset.StartAutoUpdater(context.Background(), configFilePath)
			misc.StartAntigravityVersionUpdater(context.Background())
			startModelCatalogUpdaters(localModel, cfg.Home.Enabled, cfg.ModelRefreshInterval)
		}
		if errRun := runTUIMode(
			cfg, configFilePath, password, standalone, pluginHost, serverOptions...,
		); errRun != nil {
			log.Errorf("TUI exited with error: %v", errRun)
			os.Exit(1)
		}
	} else {
		managementasset.StartAutoUpdater(context.Background(), configFilePath)
		misc.StartAntigravityVersionUpdater(context.Background())
		startModelCatalogUpdaters(localModel, cfg.Home.Enabled, cfg.ModelRefreshInterval)
		if errStart := cmd.StartServiceWithPluginHost(
			cfg, configFilePath, password, pluginHost, serverOptions...,
		); errStart != nil {
			log.Errorf("%v", errStart)
			os.Exit(1)
		}
	}
}

// modelCatalogUpdaterPlan decides which remote model catalogs should refresh.
// Codex client templates still refresh under Home mode because the model list
// comes from Home IDs while template metadata stays edge-local.
func modelCatalogUpdaterPlan(localModel, homeEnabled bool) (startModels, startCodexClient bool) {
	if localModel {
		return false, false
	}
	return !homeEnabled, true
}

func startModelCatalogUpdaters(localModel, homeEnabled bool, refreshHours int) {
	startModels, startCodexClient := modelCatalogUpdaterPlan(localModel, homeEnabled)
	if startCodexClient {
		registry.StartCodexClientModelsUpdater(context.Background(), refreshHours)
	}
	if startModels {
		registry.StartModelsUpdater(context.Background(), refreshHours)
	} else if homeEnabled && !localModel {
		log.Info("Home mode: remote models.json updates disabled; Codex client model list follows Home model IDs")
	}
}

// checkCloudConfig validates the configuration file for cloud deploy mode
// and returns true if a usable configuration exists.
func pluginBootstrapConfigPath(args []string, defaultPath string) string {
	for i := range args {
		arg := args[i]
		switch {
		case arg == "--":
			return defaultPluginBootstrapConfigPath(defaultPath)
		case arg == "-config" || arg == "--config":
			if i+1 < len(args) {
				return args[i+1]
			}
			return defaultPluginBootstrapConfigPath(defaultPath)
		case strings.HasPrefix(arg, "-config="):
			return strings.TrimPrefix(arg, "-config=")
		case strings.HasPrefix(arg, "--config="):
			return strings.TrimPrefix(arg, "--config=")
		}
	}
	return defaultPluginBootstrapConfigPath(defaultPath)
}

func defaultPluginBootstrapConfigPath(defaultPath string) string {
	if strings.TrimSpace(defaultPath) != "" {
		return defaultPath
	}
	wd, errGetwd := os.Getwd()
	if errGetwd != nil {
		return "config.yaml"
	}
	return filepath.Join(wd, "config.yaml")
}

func loadPluginBootstrapConfig(path string) *config.Config {
	raw, errReadFile := os.ReadFile(path)
	if errReadFile != nil {
		if !errors.Is(errReadFile, os.ErrNotExist) {
			log.Warnf("failed to read plugin bootstrap config: %v", errReadFile)
		}
		cfg := &config.Config{}
		cfg.NormalizePluginsConfig()
		return cfg
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		cfg := &config.Config{}
		cfg.NormalizePluginsConfig()
		return cfg
	}
	cfg, errParseConfig := config.ParseConfigBytes(raw)
	if errParseConfig != nil {
		log.Warnf("failed to parse plugin bootstrap config: %v", errParseConfig)
		cfg = &config.Config{}
		cfg.NormalizePluginsConfig()
		return cfg
	}
	return cfg
}

func cloudConfigAvailable(configFilePath string, cfg *config.Config) bool {
	if cfg != nil && cfg.Home.Enabled && cfg.Port != 0 {
		return true
	}
	return checkCloudConfig(configFilePath, cfg)
}

func checkCloudConfig(configFilePath string, cfg *config.Config) bool {
	info, errStat := os.Stat(configFilePath)
	if errStat != nil {
		log.Info("Cloud deploy mode: No configuration file detected; standing by for configuration")
		return false
	}
	if info.IsDir() {
		log.Info("Cloud deploy mode: Config path is a directory; standing by for configuration")
		return false
	}
	if cfg == nil || cfg.Port == 0 {
		log.Info("Cloud deploy mode: Configuration file is empty or invalid; standing by for valid configuration")
		return false
	}
	log.Info("Cloud deploy mode: Configuration file detected; starting service")
	return true
}
