package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/home"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/homeplugins"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/misc"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/store"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	sdkAuth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/auth"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// storeResult holds the configuration and token store produced by initStoreAndConfig.
type storeResult struct {
	cfg                  *config.Config
	configFilePath       string
	tokenStore           coreauth.Store
	configLoadedFromHome bool
	homeClient           *home.Client
	homePluginSyncReport homeplugins.SyncReport
}

// initStoreAndConfig initializes the configuration file and token store backend based on
// environment variables. It probes for PostgreSQL, ObjectStore, GitStore, or falls back to
// a local file store.
func initStoreAndConfig(
	wd, configPath string,
	isCloudDeploy bool,
	lookupEnv func(...string) (string, bool),
	homeJWT string,
	homeDisableClusterDiscovery bool,
	pluginRuntime homeplugins.PluginRuntime,
) (*storeResult, error) {
	writableBase := util.WritablePath()

	var (
		pgStoreDSN       string
		pgStoreSchema    string
		pgStoreLocalPath string

		gitStoreRemoteURL string
		gitStoreUser      string
		gitStorePassword  string
		gitStoreBranch    string
		gitStoreLocalPath string

		objectStoreEndpoint  string
		objectStoreAccess    string
		objectStoreSecret    string
		objectStoreBucket    string
		objectStoreLocalPath string

		usePostgresStore bool
		useObjectStore   bool
		useGitStore      bool
	)

	// Postgres store
	if value, ok := lookupEnv("PGSTORE_DSN", "pgstore_dsn"); ok {
		usePostgresStore = true
		pgStoreDSN = value
	}
	if usePostgresStore {
		if value, ok := lookupEnv("PGSTORE_SCHEMA", "pgstore_schema"); ok {
			pgStoreSchema = value
		}
		if value, ok := lookupEnv("PGSTORE_LOCAL_PATH", "pgstore_local_path"); ok {
			pgStoreLocalPath = value
		}
		if pgStoreLocalPath == "" {
			if writableBase != "" {
				pgStoreLocalPath = writableBase
			} else {
				pgStoreLocalPath = wd
			}
		}
		useGitStore = false
	}

	// Git store
	if value, ok := lookupEnv("GITSTORE_GIT_URL", "gitstore_git_url"); ok {
		useGitStore = true
		gitStoreRemoteURL = value
	}
	if value, ok := lookupEnv("GITSTORE_GIT_USERNAME", "gitstore_git_username"); ok {
		gitStoreUser = value
	}
	if value, ok := lookupEnv("GITSTORE_GIT_TOKEN", "gitstore_git_token"); ok {
		gitStorePassword = value
	}
	if value, ok := lookupEnv("GITSTORE_GIT_BRANCH", "gitstore_git_branch"); ok {
		gitStoreBranch = value
	}
	if value, ok := lookupEnv("GITSTORE_LOCAL_PATH", "gitstore_local_path"); ok {
		gitStoreLocalPath = value
	}

	// Object store
	if value, ok := lookupEnv("OBJECTSTORE_ENDPOINT", "objectstore_endpoint"); ok {
		useObjectStore = true
		objectStoreEndpoint = value
	}
	if value, ok := lookupEnv("OBJECTSTORE_ACCESS_KEY", "objectstore_access_key"); ok {
		objectStoreAccess = value
	}
	if value, ok := lookupEnv("OBJECTSTORE_SECRET_KEY", "objectstore_secret_key"); ok {
		objectStoreSecret = value
	}
	if value, ok := lookupEnv("OBJECTSTORE_BUCKET", "objectstore_bucket"); ok {
		objectStoreBucket = value
	}
	if value, ok := lookupEnv("OBJECTSTORE_LOCAL_PATH", "objectstore_local_path"); ok {
		objectStoreLocalPath = value
	}

	var (
		cfg                  *config.Config
		configFilePath       string
		tokenStore           coreauth.Store
		homeClient           *home.Client
		homePluginSyncReport homeplugins.SyncReport
		configLoadedFromHome bool
		err                  error
	)

	if strings.TrimSpace(homeJWT) != "" {
		configLoadedFromHome = true
		cfg, configFilePath, tokenStore, homeClient, homePluginSyncReport, err = initHomeConfig(
			wd, configPath, homeJWT, homeDisableClusterDiscovery, pluginRuntime,
		)
	} else if usePostgresStore {
		cfg, configFilePath, tokenStore, err = initPostgresStore(
			wd, pgStoreDSN, pgStoreSchema, pgStoreLocalPath, isCloudDeploy,
		)
	} else if useObjectStore {
		cfg, configFilePath, tokenStore, err = initObjectStore(
			wd, writableBase, objectStoreEndpoint, objectStoreAccess, objectStoreSecret, objectStoreBucket,
			objectStoreLocalPath, isCloudDeploy,
		)
	} else if useGitStore {
		cfg, configFilePath, tokenStore, err = initGitStore(
			wd, writableBase, gitStoreRemoteURL, gitStoreUser, gitStorePassword, gitStoreBranch, gitStoreLocalPath,
			isCloudDeploy,
		)
	} else if configPath != "" {
		configFilePath = configPath
		if isCloudDeploy {
			cfg, err = config.LoadConfigOptional(configPath, true)
		} else {
			var fallback bool
			cfg, configFilePath, fallback, err = config.LoadWithFallback(configPath)
			if fallback {
				log.Warnf("config.yaml failed to load, using last-good backup (%s)", config.LastGoodPath(configPath))
			}
		}
		tokenStore = sdkAuth.NewFileTokenStore()
	} else {
		configFilePath = filepath.Join(wd, "config.yaml")
		if isCloudDeploy {
			cfg, err = config.LoadConfigOptional(configFilePath, true)
		} else {
			var fallback bool
			cfg, configFilePath, fallback, err = config.LoadWithFallback(configFilePath)
			if fallback {
				log.Warnf(
					"config.yaml failed to load, using last-good backup (%s)", config.LastGoodPath(configFilePath),
				)
			}
		}
		tokenStore = sdkAuth.NewFileTokenStore()
	}
	if err != nil {
		return nil, fmt.Errorf("failed to load config: %w", err)
	}
	if cfg == nil {
		cfg = &config.Config{}
	}

	// 启动时补齐源文件中的默认值
	if !cfg.Home.Enabled && !isCloudDeploy && configFilePath != "" {
		config.EnsureDefaultsInFile(configFilePath, cfg)
	}

	return &storeResult{
		cfg:                  cfg,
		configFilePath:       configFilePath,
		tokenStore:           tokenStore,
		configLoadedFromHome: configLoadedFromHome,
		homeClient:           homeClient,
		homePluginSyncReport: homePluginSyncReport,
	}, nil
}

func normalizeHomePort(port int) int {
	if port <= 0 {
		return 8317
	}
	return port
}

func initHomeConfig(
	wd string,
	configPath string,
	homeJWT string,
	disableClusterDiscovery bool,
	pluginRuntime homeplugins.PluginRuntime,
) (
	*config.Config,
	string,
	coreauth.Store,
	*home.Client,
	homeplugins.SyncReport,
	error,
) {
	var emptyReport homeplugins.SyncReport
	ctxHome, cancelHome := context.WithTimeout(context.Background(), 30*time.Second)
	homeCfg, errHomeCfg := home.ConfigFromJWT(ctxHome, homeJWT)
	cancelHome()
	if errHomeCfg != nil {
		return nil, "", nil, nil, emptyReport, fmt.Errorf("invalid -home-jwt: %w", errHomeCfg)
	}
	if disableClusterDiscovery {
		homeCfg.DisableClusterDiscovery = true
	}

	homeClient := home.New(homeCfg)
	ctxHomeConfig, cancelHomeConfig := context.WithTimeout(context.Background(), 30*time.Second)
	raw, errGetConfig := homeClient.GetConfig(ctxHomeConfig)
	cancelHomeConfig()
	if errGetConfig != nil {
		homeClient.Close()
		return nil, "", nil, nil, emptyReport, fmt.Errorf("failed to fetch config from home: %w", errGetConfig)
	}

	parsed, errParseConfig := config.ParseConfigBytes(raw)
	if errParseConfig != nil {
		homeClient.Close()
		return nil, "", nil, nil, emptyReport, fmt.Errorf(
			"failed to parse config payload from home: %w", errParseConfig,
		)
	}
	if parsed == nil {
		parsed = &config.Config{}
	}
	parsed.Home = homeCfg
	parsed.Port = normalizeHomePort(parsed.Port)
	config.ApplyHomeRuntimeConfig(parsed)

	ctxHomePlugins, cancelHomePlugins := context.WithTimeout(context.Background(), 30*time.Second)
	homePluginSyncReport, errHomePlugins := homeplugins.SyncWithReport(ctxHomePlugins, parsed, pluginRuntime)
	cancelHomePlugins()
	errReportPlugins := home.ReportPluginStatus(context.Background(), homeClient, homeCfg.NodeID, homePluginSyncReport)
	if errReportPlugins != nil {
		log.Warnf("failed to report home plugin sync status: %v", errReportPlugins)
	}
	if errHomePlugins != nil {
		homeClient.Close()
		return nil, "", nil, nil, homePluginSyncReport, fmt.Errorf(
			"failed to fetch plugins from home: %w", errHomePlugins,
		)
	}

	configFilePath := strings.TrimSpace(configPath)
	if configFilePath == "" {
		configFilePath = filepath.Join(wd, "config.yaml")
	}

	return parsed, configFilePath, sdkAuth.NewFileTokenStore(), homeClient, homePluginSyncReport, nil
}

func initPostgresStore(wd, dsn, schema, localPath string, isCloudDeploy bool) (
	*config.Config,
	string,
	coreauth.Store,
	error,
) {
	if localPath == "" {
		localPath = wd
	}
	localPath = filepath.Join(localPath, "pgstore")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pgStoreInst, err := store.NewPostgresStore(
		ctx, store.PostgresStoreConfig{
			DSN:      dsn,
			Schema:   schema,
			SpoolDir: localPath,
		},
	)
	if err != nil {
		return nil, "", nil, fmt.Errorf("failed to initialize postgres token store: %w", err)
	}

	ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	examplePath := filepath.Join(wd, "config.example.yaml")
	if errBootstrap := pgStoreInst.Bootstrap(ctx, examplePath); errBootstrap != nil {
		_ = pgStoreInst.Close()
		return nil, "", nil, fmt.Errorf("failed to bootstrap postgres-backed config: %w", errBootstrap)
	}

	configFilePath := pgStoreInst.ConfigPath()
	cfg, err := config.LoadConfigOptional(configFilePath, isCloudDeploy)
	if err != nil {
		_ = pgStoreInst.Close()
		return nil, "", nil, err
	}
	if cfg == nil {
		cfg = &config.Config{}
	}
	cfg.AuthDir = pgStoreInst.AuthDir()
	log.Infof("postgres-backed token store enabled, workspace path: %s", pgStoreInst.WorkDir())

	return cfg, configFilePath, pgStoreInst, nil
}

func initObjectStore(
	wd, writableBase, endpoint, accessKey, secretKey, bucket, localPath string,
	isCloudDeploy bool,
) (*config.Config, string, coreauth.Store, error) {
	if localPath == "" {
		if writableBase != "" {
			localPath = writableBase
		} else {
			localPath = wd
		}
	}
	objectStoreRoot := filepath.Join(localPath, "objectstore")

	resolvedEndpoint, useSSL, err := parseObjectStoreEndpoint(endpoint)
	if err != nil {
		return nil, "", nil, err
	}

	objCfg := store.ObjectStoreConfig{
		Endpoint:  resolvedEndpoint,
		Bucket:    bucket,
		AccessKey: accessKey,
		SecretKey: secretKey,
		LocalRoot: objectStoreRoot,
		UseSSL:    useSSL,
		PathStyle: true,
	}
	objectStoreInst, err := store.NewObjectTokenStore(objCfg)
	if err != nil {
		return nil, "", nil, fmt.Errorf("failed to initialize object token store: %w", err)
	}

	examplePath := filepath.Join(wd, "config.example.yaml")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if errBootstrap := objectStoreInst.Bootstrap(ctx, examplePath); errBootstrap != nil {
		return nil, "", nil, fmt.Errorf("failed to bootstrap object-backed config: %w", errBootstrap)
	}

	configFilePath := objectStoreInst.ConfigPath()
	cfg, err := config.LoadConfigOptional(configFilePath, isCloudDeploy)
	if err != nil {
		return nil, "", nil, err
	}
	if cfg == nil {
		cfg = &config.Config{}
	}
	cfg.AuthDir = objectStoreInst.AuthDir()
	log.Infof("object-backed token store enabled, bucket: %s", bucket)

	return cfg, configFilePath, objectStoreInst, nil
}

func initGitStore(wd, writableBase, remoteURL, user, password, branch, localPath string, isCloudDeploy bool) (
	*config.Config,
	string,
	coreauth.Store,
	error,
) {
	if localPath == "" {
		if writableBase != "" {
			localPath = writableBase
		} else {
			localPath = wd
		}
	}
	gitStoreRoot := filepath.Join(localPath, "gitstore")
	authDir := filepath.Join(gitStoreRoot, "auths")

	gitStoreInst := store.NewGitTokenStore(remoteURL, user, password, branch)
	gitStoreInst.SetBaseDir(authDir)
	if errRepo := gitStoreInst.EnsureRepository(); errRepo != nil {
		return nil, "", nil, fmt.Errorf("failed to prepare git token store: %w", errRepo)
	}

	configFilePath := gitStoreInst.ConfigPath()
	if configFilePath == "" {
		configFilePath = filepath.Join(gitStoreRoot, "config", "config.yaml")
	}

	if _, statErr := os.Stat(configFilePath); errors.Is(statErr, fs.ErrNotExist) {
		examplePath := filepath.Join(wd, "config.example.yaml")
		if _, errExample := os.Stat(examplePath); errExample != nil {
			return nil, "", nil, fmt.Errorf("failed to find template config file: %w", errExample)
		}
		if errCopy := misc.CopyConfigTemplate(examplePath, configFilePath); errCopy != nil {
			return nil, "", nil, fmt.Errorf("failed to bootstrap git-backed config: %w", errCopy)
		}
		if errCommit := gitStoreInst.PersistConfig(context.Background()); errCommit != nil {
			return nil, "", nil, fmt.Errorf("failed to commit initial git-backed config: %w", errCommit)
		}
		log.Infof("git-backed config initialized from template: %s", configFilePath)
	} else if statErr != nil {
		return nil, "", nil, fmt.Errorf("failed to inspect git-backed config: %w", statErr)
	}

	cfg, err := config.LoadConfigOptional(configFilePath, isCloudDeploy)
	if err != nil {
		return nil, "", nil, err
	}
	if cfg == nil {
		cfg = &config.Config{}
	}
	cfg.AuthDir = gitStoreInst.AuthDir()
	log.Infof("git-backed token store enabled, repository path: %s", gitStoreRoot)

	return cfg, configFilePath, gitStoreInst, nil
}

// parseObjectStoreEndpoint parses a raw endpoint string, stripping the scheme to produce
// a host (with optional path) and a useSSL flag.
func parseObjectStoreEndpoint(raw string) (endpoint string, useSSL bool, err error) {
	endpoint = strings.TrimSpace(raw)
	useSSL = true

	if strings.Contains(endpoint, "://") {
		parsed, errParse := url.Parse(endpoint)
		if errParse != nil {
			return "", false, fmt.Errorf("failed to parse object store endpoint %q: %w", raw, errParse)
		}
		switch strings.ToLower(parsed.Scheme) {
		case "http":
			useSSL = false
		case "https":
			useSSL = true
		default:
			return "", false, fmt.Errorf(
				"unsupported object store scheme %q (only http and https are allowed)", parsed.Scheme,
			)
		}
		if parsed.Host == "" {
			return "", false, fmt.Errorf("object store endpoint %q is missing host information", raw)
		}
		endpoint = parsed.Host
		if parsed.Path != "" && parsed.Path != "/" {
			endpoint = strings.TrimSuffix(parsed.Host+parsed.Path, "/")
		}
	}

	endpoint = strings.TrimRight(endpoint, "/")
	return endpoint, useSSL, nil
}
