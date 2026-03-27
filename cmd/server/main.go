// Package main provides the entry point for the CLI Proxy API server.
// This server acts as a proxy that provides OpenAI/Gemini/Claude compatible API interfaces
// for CLI models, allowing CLI models to be used with tools and libraries designed for standard AI APIs.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	configaccess "github.com/Pyrokine/CLIProxyAPI/v6/internal/access/config_access"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/buildinfo"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/cmd"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/managementasset"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	_ "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/usage"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	sdkAuth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/auth"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/joho/godotenv"
	log "github.com/sirupsen/logrus"
)

var (
	version           = "dev"
	commit            = "none"
	buildDate         = "unknown"
	defaultConfigPath = ""
)

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
	fmt.Printf(
		"CLIProxyAPI Version: %s, Commit: %s, BuiltAt: %s\n", buildinfo.Version, buildinfo.Commit, buildinfo.BuildDate,
	)

	// Command-line flags
	var (
		lf         loginFlags
		configPath string
		password   string
		tuiMode    bool
		standalone bool
		localModel bool
		testConfig bool
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
	flag.StringVar(&lf.projectID, "project_id", "", "Project ID (Gemini only, not required)")
	flag.StringVar(&configPath, "config", defaultConfigPath, "Configure File Path")
	flag.StringVar(&lf.vertexImport, "vertex-import", "", "Import Vertex service account key JSON file")
	flag.StringVar(&password, "password", "", "")
	flag.BoolVar(&tuiMode, "tui", false, "Start with terminal management UI")
	flag.BoolVar(&standalone, "standalone", false, "In TUI mode, start an embedded local server")
	flag.BoolVar(&localModel, "local-model", false, "Use embedded model catalog only, skip remote model fetching")
	flag.BoolVar(&testConfig, "t", false, "Test configuration and exit")

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

	isCloudDeploy := os.Getenv("DEPLOY") == "cloud"

	result, err := initStoreAndConfig(wd, configPath, isCloudDeploy, lookupEnv)
	if err != nil {
		log.Errorf("%v", err)
		return
	}
	cfg := result.cfg
	configFilePath := result.configFilePath

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
		configFileExists = checkCloudConfig(configFilePath, cfg)
	}
	usage.SetStatisticsEnabled(cfg.UsageStatisticsEnabled)
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

	if tuiMode {
		if standalone {
			managementasset.StartAutoUpdater(context.Background(), configFilePath)
		}
		if !localModel {
			registry.StartModelsUpdater(context.Background())
		}
		runTUIMode(cfg, configFilePath, password, standalone)
	} else {
		managementasset.StartAutoUpdater(context.Background(), configFilePath)
		if !localModel {
			registry.StartModelsUpdater(context.Background())
		}
		cmd.StartService(cfg, configFilePath, password)
	}
}

// checkCloudConfig validates the configuration file for cloud deploy mode
// and returns true if a usable configuration exists.
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
	if cfg.Port == 0 {
		log.Info("Cloud deploy mode: Configuration file is empty or invalid; standing by for valid configuration")
		return false
	}
	log.Info("Cloud deploy mode: Configuration file detected; starting service")
	return true
}
