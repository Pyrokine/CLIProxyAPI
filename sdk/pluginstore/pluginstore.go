// Package pluginstore exposes plugin registry and artifact installation helpers
// for embedders such as CLIProxyAPIHome.
package pluginstore

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	internalpluginstore "github.com/Pyrokine/CLIProxyAPI/v6/internal/pluginstore"
)

// CanonicalPluginID returns the case-insensitive identity used for plugin ownership.
func CanonicalPluginID(id string) string {
	return internalpluginstore.CanonicalPluginID(id)
}

// These aliases expose the supported registry protocol values to SDK consumers.
// noinspection GoUnusedConst
const (
	DefaultRegistryURL = internalpluginstore.DefaultRegistryURL
	DefaultSourceID    = internalpluginstore.DefaultSourceID
	DefaultSourceName  = internalpluginstore.DefaultSourceName
	SchemaVersion      = internalpluginstore.SchemaVersion
	SchemaVersionV2    = internalpluginstore.SchemaVersionV2

	InstallTypeGitHubRelease = internalpluginstore.InstallTypeGitHubRelease
	InstallTypeDirect        = internalpluginstore.InstallTypeDirect

	RequestKindRegistry = internalpluginstore.RequestKindRegistry
	RequestKindMetadata = internalpluginstore.RequestKindMetadata
	RequestKindArtifact = internalpluginstore.RequestKindArtifact

	AuthTypeNone        = internalpluginstore.AuthTypeNone
	AuthTypeBearer      = internalpluginstore.AuthTypeBearer
	AuthTypeBasic       = internalpluginstore.AuthTypeBasic
	AuthTypeHeader      = internalpluginstore.AuthTypeHeader
	AuthTypeGitHubToken = internalpluginstore.AuthTypeGitHubToken
)

// Source identifies a plugin registry source.
type Source struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	URL  string `json:"url"`
}

// Registry contains plugins advertised by one registry source.
type Registry struct {
	SchemaVersion int      `json:"schema_version"`
	Plugins       []Plugin `json:"plugins"`
}

// Plugin describes a plugin entry in a registry.
type Plugin struct {
	ID           string      `json:"id"`
	Name         string      `json:"name"`
	Description  string      `json:"description"`
	Author       string      `json:"author"`
	Version      string      `json:"version"`
	Versions     []Version   `json:"versions,omitempty"`
	Repository   string      `json:"repository,omitempty"`
	Logo         string      `json:"logo,omitempty"`
	Homepage     string      `json:"homepage,omitempty"`
	License      string      `json:"license,omitempty"`
	Tags         []string    `json:"tags,omitempty"`
	Install      InstallPlan `json:"install,omitempty"`
	AuthRequired bool        `json:"auth_required,omitempty"`
}

// Version describes one plugin version entry.
type Version struct {
	Version string      `json:"version"`
	Install InstallPlan `json:"install,omitempty"`
}

// Release describes a GitHub release.
type Release struct {
	TagName     string         `json:"tag_name"`
	Name        string         `json:"name"`
	PublishedAt string         `json:"published_at"`
	Prerelease  bool           `json:"prerelease"`
	Draft       bool           `json:"draft"`
	HTMLURL     string         `json:"html_url"`
	Assets      []ReleaseAsset `json:"assets"`
}

// ReleaseAsset describes an asset in a GitHub release.
type ReleaseAsset struct {
	APIURL             string `json:"url"`
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int64  `json:"size"`
}

// InstallOptions controls where and how plugin artifacts are installed.
type InstallOptions struct {
	PluginsDir    string
	GOOS          string
	GOARCH        string
	PluginLoaded  func() bool
	BeforeWrite   func() error
	BeforeReplace func(string) error
	BeforePublish func(path, version string, overwritten bool, sha256 string) error
	RootLock      *RootLock
}

// RootLock serializes plugin artifact mutations for one plugin root.
type RootLock struct {
	inner *internalpluginstore.RootLock
}

// InstallTransaction preserves an artifact until its config commits.
type InstallTransaction struct {
	inner *internalpluginstore.InstallTransaction
}

// DeletionTransaction preserves every plugin version until deletion commits.
type DeletionTransaction struct {
	inner *internalpluginstore.DeletionTransaction
}

// PrepareDeletionTransactionLocked journals a deletion while rootLock is held.
func PrepareDeletionTransactionLocked(
	root string,
	pluginID string,
	targetPaths []string,
	rootLock *RootLock,
) (*DeletionTransaction, error) {
	if rootLock == nil {
		return nil, fmt.Errorf("plugin root lock is required")
	}
	transaction, errPrepare := internalpluginstore.PrepareDeletionTransactionLocked(
		root,
		pluginID,
		targetPaths,
		rootLock.inner,
	)
	if errPrepare != nil {
		return nil, errPrepare
	}
	return &DeletionTransaction{inner: transaction}, nil
}

func (t *DeletionTransaction) Hide() error {
	if t == nil || t.inner == nil {
		return nil
	}
	return t.inner.Hide()
}

func (t *DeletionTransaction) MarkCommitted() error {
	if t == nil || t.inner == nil {
		return nil
	}
	return t.inner.MarkCommitted()
}

func (t *DeletionTransaction) Commit() error {
	if t == nil || t.inner == nil {
		return nil
	}
	return t.inner.Commit()
}

func (t *DeletionTransaction) Rollback() error {
	if t == nil || t.inner == nil {
		return nil
	}
	return t.inner.Rollback()
}

// FindPluginArtifacts returns every installed version owned by pluginID.
func FindPluginArtifacts(root, pluginID string) ([]string, error) {
	return internalpluginstore.FindPluginArtifacts(root, pluginID)
}

// PrepareInstallTransactionLocked journals an install while rootLock is held.
func PrepareInstallTransactionLocked(
	root,
	pluginID,
	targetPath,
	nextVersion,
	nextSHA256 string,
	overwritten bool,
	rootLock *RootLock,
) (*InstallTransaction, error) {
	if rootLock == nil {
		return nil, fmt.Errorf("plugin root lock is required")
	}
	transaction, errPrepare := internalpluginstore.PrepareInstallTransactionLocked(
		root, pluginID, targetPath, nextVersion, nextSHA256, overwritten, rootLock.inner,
	)
	if errPrepare != nil {
		return nil, errPrepare
	}
	return &InstallTransaction{inner: transaction}, nil
}

// SetExpectedConfigFingerprint records the config state that commits this artifact.
func (t *InstallTransaction) SetExpectedConfigFingerprint(fingerprint string) error {
	if t == nil || t.inner == nil {
		return fmt.Errorf("plugin install transaction is not active")
	}
	return t.inner.SetExpectedConfigFingerprint(fingerprint)
}

// Commit removes install rollback state after config persistence.
func (t *InstallTransaction) Commit() error {
	if t == nil || t.inner == nil {
		return nil
	}
	return t.inner.Commit()
}

// Rollback restores the previous artifact or removes a new artifact.
func (t *InstallTransaction) Rollback(result InstallResult) error {
	if t == nil || t.inner == nil {
		return nil
	}
	return t.inner.Rollback(toInternalInstallResult(result))
}

// LockRoot acquires the process-wide artifact mutation lock for root.
func LockRoot(root string) (*RootLock, error) {
	lock, errLock := internalpluginstore.LockRoot(root)
	if errLock != nil {
		return nil, errLock
	}
	return &RootLock{inner: lock}, nil
}

// Unlock releases the plugin root mutation lock.
func (l *RootLock) Unlock() {
	if l != nil && l.inner != nil {
		l.inner.Unlock()
	}
}

// RecoverInstallTransactionsLocked resolves unfinished installs while rootLock is held.
func RecoverInstallTransactionsLocked(
	root string,
	configFingerprints map[string]string,
	rootLock *RootLock,
) error {
	if rootLock == nil || rootLock.inner == nil {
		return fmt.Errorf("plugin root lock is required")
	}
	return internalpluginstore.RecoverInstallTransactions(root, configFingerprints)
}

// RecoverDeletionTransactionsLocked resolves unfinished deletions while rootLock is held.
func RecoverDeletionTransactionsLocked(
	root string,
	configuredPluginIDs map[string]struct{},
	rootLock *RootLock,
) error {
	if rootLock == nil || rootLock.inner == nil {
		return fmt.Errorf("plugin root lock is required")
	}
	return internalpluginstore.RecoverDeletionTransactions(root, configuredPluginIDs)
}

// PendingInstallPluginIDs returns plugin IDs owned by unfinished install transactions.
func PendingInstallPluginIDs(root string) (map[string]struct{}, error) {
	return internalpluginstore.PendingInstallPluginIDs(root)
}

// PreparedInstall contains a downloaded and verified plugin artifact that has
// not changed the plugin directory.
type PreparedInstall struct {
	inner internalpluginstore.PreparedInstall
}

// SpoolToTemp moves the verified archive out of memory until installation.
func (p *PreparedInstall) SpoolToTemp() error {
	if p == nil {
		var empty internalpluginstore.PreparedInstall
		return empty.SpoolToTemp()
	}
	return p.inner.SpoolToTemp()
}

// Close removes temporary archive storage created by SpoolToTemp.
func (p *PreparedInstall) Close() error {
	if p == nil {
		return nil
	}
	return p.inner.Close()
}

// Install applies a prepared plugin artifact.

func (p *PreparedInstall) Install(options InstallOptions) (InstallResult, error) {
	if p == nil {
		return InstallResult{}, fmt.Errorf("prepared plugin archive is nil")
	}
	result, err := p.inner.Install(toInternalInstallOptions(options))
	return fromInternalInstallResult(result), err
}

// InstallResult describes an install operation result.
type InstallResult struct {
	ID          string `json:"id"`
	Version     string `json:"version"`
	ReleaseTag  string `json:"release_tag,omitempty"`
	InstallType string `json:"install_type,omitempty"`
	Path        string `json:"path"`
	Overwritten bool   `json:"overwritten"`
	Skipped     bool   `json:"skipped"`
}

// InstallPlan describes plugin artifact delivery.
type InstallPlan struct {
	Type      string     `yaml:"type,omitempty" json:"type,omitempty"`
	Artifacts []Artifact `yaml:"artifacts,omitempty" json:"artifacts,omitempty"`
}

// Artifact describes one installable artifact.
type Artifact struct {
	GOOS   string `yaml:"goos,omitempty" json:"goos,omitempty"`
	GOARCH string `yaml:"goarch,omitempty" json:"goarch,omitempty"`
	URL    string `yaml:"url,omitempty" json:"url,omitempty"`
	SHA256 string `yaml:"sha256,omitempty" json:"sha256,omitempty"`
	Size   int64  `yaml:"size,omitempty" json:"size,omitempty"`
}

// Platform describes an artifact platform.
type Platform struct {
	GOOS   string `json:"goos"`
	GOARCH string `json:"goarch"`
}

// Manifest pins the source, plugin and version selected for installation.
type Manifest struct {
	SchemaVersion int         `yaml:"schema-version,omitempty" json:"schema_version,omitempty"`
	ID            string      `yaml:"id,omitempty" json:"id,omitempty"`
	Name          string      `yaml:"name,omitempty" json:"name,omitempty"`
	Description   string      `yaml:"description,omitempty" json:"description,omitempty"`
	Author        string      `yaml:"author,omitempty" json:"author,omitempty"`
	Version       string      `yaml:"version,omitempty" json:"version,omitempty"`
	ReleaseTag    string      `yaml:"release-tag,omitempty" json:"release_tag,omitempty"`
	Repository    string      `yaml:"repository,omitempty" json:"repository,omitempty"`
	Logo          string      `yaml:"logo,omitempty" json:"logo,omitempty"`
	Homepage      string      `yaml:"homepage,omitempty" json:"homepage,omitempty"`
	License       string      `yaml:"license,omitempty" json:"license,omitempty"`
	Tags          []string    `yaml:"tags,omitempty" json:"tags,omitempty"`
	SourceID      string      `yaml:"source-id,omitempty" json:"source_id,omitempty"`
	SourceName    string      `yaml:"source-name,omitempty" json:"source_name,omitempty"`
	SourceURL     string      `yaml:"source-url,omitempty" json:"source_url,omitempty"`
	Install       InstallPlan `yaml:"install,omitempty" json:"install,omitempty"`
}

// AuthConfig describes credentials for registry, metadata, or artifact requests.
type AuthConfig struct {
	Match          string   `yaml:"match,omitempty" json:"match,omitempty"`
	ApplyTo        []string `yaml:"apply-to,omitempty" json:"apply-to,omitempty"`
	Type           string   `yaml:"type,omitempty" json:"type,omitempty"`
	TokenEnv       string   `yaml:"token-env,omitempty" json:"token-env,omitempty"`
	UsernameEnv    string   `yaml:"username-env,omitempty" json:"username-env,omitempty"`
	PasswordEnv    string   `yaml:"password-env,omitempty" json:"password-env,omitempty"`
	HeaderName     string   `yaml:"header-name,omitempty" json:"header-name,omitempty"`
	HeaderValueEnv string   `yaml:"header-value-env,omitempty" json:"header-value-env,omitempty"`
	AllowInsecure  bool     `yaml:"allow-insecure,omitempty" json:"allow-insecure,omitempty"`
}

type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// These aliases expose install transaction errors to SDK consumers.
// noinspection GoUnusedGlobalVariable
var (
	ErrLoadedPluginLocked = internalpluginstore.ErrLoadedPluginLocked
	ErrPendingDeletion    = internalpluginstore.ErrPendingDeletion
	ErrPendingInstall     = internalpluginstore.ErrPendingInstall
)

type Client struct {
	inner internalpluginstore.Client
}

// noinspection GoUnusedExportedFunction
func NewClient(httpClient HTTPDoer, registryURL string) Client {
	return Client{
		inner: internalpluginstore.Client{
			HTTPClient:  httpClient,
			RegistryURL: strings.TrimSpace(registryURL),
		},
	}
}

func NewClientWithAuth(httpClient HTTPDoer, registryURL string, auth []AuthConfig) Client {
	return Client{
		inner: internalpluginstore.Client{
			HTTPClient:  httpClient,
			RegistryURL: strings.TrimSpace(registryURL),
			Auth:        toInternalAuthConfigs(auth),
		},
	}
}

// WithNetworkScope returns a copy with the proxy or egress identity used for
// shared GitHub API cooldowns. It does not configure the HTTP transport.
func (c Client) WithNetworkScope(networkScope string) Client {
	c.inner.NetworkScope = strings.TrimSpace(networkScope)
	return c
}

func DefaultSource() Source {
	return fromInternalSource(internalpluginstore.DefaultSource())
}

// noinspection GoUnusedExportedFunction
func NormalizeSources(registryURLs []string) ([]Source, error) {
	sources, err := internalpluginstore.NormalizeSources(registryURLs)
	return fromInternalSources(sources), err
}

func SourceID(registryURL string) string {
	return internalpluginstore.SourceID(registryURL)
}

// noinspection GoUnusedExportedFunction
func ValidatePlugin(plugin Plugin) error {
	return internalpluginstore.ValidatePlugin(toInternalPlugin(plugin))
}

// noinspection GoUnusedExportedFunction
func PluginInstallType(plugin Plugin) string {
	return internalpluginstore.PluginInstallType(toInternalPlugin(plugin))
}

// noinspection GoUnusedExportedFunction
func PluginPlatforms(plugin Plugin) []Platform {
	return fromInternalPlatforms(internalpluginstore.PluginPlatforms(toInternalPlugin(plugin)))
}

func PluginArtifacts(plugin Plugin) []Artifact {
	return fromInternalArtifacts(internalpluginstore.PluginArtifacts(toInternalPlugin(plugin)))
}

func NormalizeAuthConfigs(auth []AuthConfig) []AuthConfig {
	return fromInternalAuthConfigs(internalpluginstore.NormalizeAuthConfigs(toInternalAuthConfigs(auth)))
}

// noinspection GoUnusedExportedFunction
func AuthConfigured(auth []AuthConfig, requestURL string, kind string) bool {
	return internalpluginstore.AuthConfigured(toInternalAuthConfigs(auth), requestURL, kind)
}

// noinspection GoUnusedExportedFunction
func PluginAuthConfigured(source Source, plugin Plugin, auth []AuthConfig) bool {
	return internalpluginstore.PluginAuthConfigured(
		toInternalSource(source), toInternalPlugin(plugin), toInternalAuthConfigs(auth),
	)
}

func UpdateAvailable(installed, latest string) bool {
	return internalpluginstore.UpdateAvailable(installed, latest)
}

// noinspection GoUnusedExportedFunction
func ReleaseVersion(release Release) (string, error) {
	return internalpluginstore.ReleaseVersion(toInternalRelease(release))
}

func ManifestFromRelease(source Source, plugin Plugin, release Release) (Manifest, error) {
	manifest, err := internalpluginstore.ManifestFromRelease(
		toInternalSource(source), toInternalPlugin(plugin), toInternalRelease(release),
	)
	return fromInternalManifest(manifest), err
}

func ManifestFromPlugin(source Source, plugin Plugin) (Manifest, error) {
	manifest, err := internalpluginstore.ManifestFromPlugin(toInternalSource(source), toInternalPlugin(plugin))
	return fromInternalManifest(manifest), err
}

func (r Registry) PluginByID(id string) (Plugin, bool) {
	plugin, ok := toInternalRegistry(r).PluginByID(id)
	return fromInternalPlugin(plugin), ok
}

func (m Manifest) Plugin() Plugin {
	return fromInternalPlugin(toInternalManifest(m).Plugin())
}

func (m Manifest) InstallType() string {
	return toInternalManifest(m).InstallType()
}

func (m Manifest) Validate() error {
	return toInternalManifest(m).Validate()
}

func (c Client) FetchRegistry(ctx context.Context) (Registry, error) {
	registry, err := c.inner.FetchRegistry(ctx)
	return fromInternalRegistry(registry), err
}

func (c Client) FetchLatestRelease(ctx context.Context, plugin Plugin) (Release, error) {
	release, err := c.inner.FetchLatestRelease(ctx, toInternalPlugin(plugin))
	return fromInternalRelease(release), err
}

func (c Client) FetchReleases(ctx context.Context, plugin Plugin, pageSize int) ([]Release, error) {
	releases, err := c.inner.FetchReleases(ctx, toInternalPlugin(plugin), pageSize)
	return fromInternalReleases(releases), err
}

func (c Client) FetchReleaseByTag(ctx context.Context, plugin Plugin, tag string) (Release, error) {
	release, err := c.inner.FetchReleaseByTag(ctx, toInternalPlugin(plugin), tag)
	return fromInternalRelease(release), err
}

func (c Client) Install(ctx context.Context, plugin Plugin, options InstallOptions) (InstallResult, error) {
	result, err := c.inner.Install(ctx, toInternalPlugin(plugin), toInternalInstallOptions(options))
	return fromInternalInstallResult(result), err
}

func (c Client) InstallVersion(
	ctx context.Context,
	plugin Plugin,
	releaseTag string,
	version string,
	options InstallOptions,
) (InstallResult, error) {
	result, err := c.inner.InstallVersion(
		ctx, toInternalPlugin(plugin), releaseTag, version, toInternalInstallOptions(options),
	)
	return fromInternalInstallResult(result), err
}

func (c Client) PrepareManifest(
	ctx context.Context,
	manifest Manifest,
	options InstallOptions,
) (PreparedInstall, error) {
	prepared, err := c.inner.PrepareManifest(
		ctx,
		toInternalManifest(manifest),
		toInternalInstallOptions(options),
	)
	return PreparedInstall{inner: prepared}, err
}

func (c Client) InstallManifest(ctx context.Context, manifest Manifest, options InstallOptions) (InstallResult, error) {
	prepared, err := c.PrepareManifest(ctx, manifest, options)
	if err != nil {
		return InstallResult{}, err
	}
	return prepared.Install(options)
}

func toInternalSource(source Source) internalpluginstore.Source {
	return internalpluginstore.Source{ID: source.ID, Name: source.Name, URL: source.URL}
}

func fromInternalSource(source internalpluginstore.Source) Source {
	return Source{ID: source.ID, Name: source.Name, URL: source.URL}
}

func fromInternalSources(sources []internalpluginstore.Source) []Source {
	if len(sources) == 0 {
		return nil
	}
	out := make([]Source, len(sources))
	for i := range sources {
		out[i] = fromInternalSource(sources[i])
	}
	return out
}

func toInternalRegistry(registry Registry) internalpluginstore.Registry {
	return internalpluginstore.Registry{
		SchemaVersion: registry.SchemaVersion, Plugins: toInternalPlugins(registry.Plugins),
	}
}

func fromInternalRegistry(registry internalpluginstore.Registry) Registry {
	return Registry{SchemaVersion: registry.SchemaVersion, Plugins: fromInternalPlugins(registry.Plugins)}
}

func toInternalPlugin(plugin Plugin) internalpluginstore.Plugin {
	return internalpluginstore.Plugin{
		ID:           plugin.ID,
		Name:         plugin.Name,
		Description:  plugin.Description,
		Author:       plugin.Author,
		Version:      plugin.Version,
		Versions:     toInternalVersions(plugin.Versions),
		Repository:   plugin.Repository,
		Logo:         plugin.Logo,
		Homepage:     plugin.Homepage,
		License:      plugin.License,
		Tags:         append([]string(nil), plugin.Tags...),
		Install:      toInternalInstallPlan(plugin.Install),
		AuthRequired: plugin.AuthRequired,
	}
}

func fromInternalPlugin(plugin internalpluginstore.Plugin) Plugin {
	return Plugin{
		ID:           plugin.ID,
		Name:         plugin.Name,
		Description:  plugin.Description,
		Author:       plugin.Author,
		Version:      plugin.Version,
		Versions:     fromInternalVersions(plugin.Versions),
		Repository:   plugin.Repository,
		Logo:         plugin.Logo,
		Homepage:     plugin.Homepage,
		License:      plugin.License,
		Tags:         append([]string(nil), plugin.Tags...),
		Install:      fromInternalInstallPlan(plugin.Install),
		AuthRequired: plugin.AuthRequired,
	}
}

func toInternalPlugins(plugins []Plugin) []internalpluginstore.Plugin {
	if len(plugins) == 0 {
		return nil
	}
	out := make([]internalpluginstore.Plugin, len(plugins))
	for i := range plugins {
		out[i] = toInternalPlugin(plugins[i])
	}
	return out
}

func fromInternalPlugins(plugins []internalpluginstore.Plugin) []Plugin {
	if len(plugins) == 0 {
		return nil
	}
	out := make([]Plugin, len(plugins))
	for i := range plugins {
		out[i] = fromInternalPlugin(plugins[i])
	}
	return out
}

func toInternalVersion(version Version) internalpluginstore.Version {
	return internalpluginstore.Version{Version: version.Version, Install: toInternalInstallPlan(version.Install)}
}

func fromInternalVersion(version internalpluginstore.Version) Version {
	return Version{Version: version.Version, Install: fromInternalInstallPlan(version.Install)}
}

func toInternalVersions(versions []Version) []internalpluginstore.Version {
	if len(versions) == 0 {
		return nil
	}
	out := make([]internalpluginstore.Version, len(versions))
	for i := range versions {
		out[i] = toInternalVersion(versions[i])
	}
	return out
}

func fromInternalVersions(versions []internalpluginstore.Version) []Version {
	if len(versions) == 0 {
		return nil
	}
	out := make([]Version, len(versions))
	for i := range versions {
		out[i] = fromInternalVersion(versions[i])
	}
	return out
}

func toInternalRelease(release Release) internalpluginstore.Release {
	return internalpluginstore.Release{
		TagName:     release.TagName,
		Name:        release.Name,
		PublishedAt: release.PublishedAt,
		Prerelease:  release.Prerelease,
		Draft:       release.Draft,
		HTMLURL:     release.HTMLURL,
		Assets:      toInternalReleaseAssets(release.Assets),
	}
}

func fromInternalRelease(release internalpluginstore.Release) Release {
	return Release{
		TagName:     release.TagName,
		Name:        release.Name,
		PublishedAt: release.PublishedAt,
		Prerelease:  release.Prerelease,
		Draft:       release.Draft,
		HTMLURL:     release.HTMLURL,
		Assets:      fromInternalReleaseAssets(release.Assets),
	}
}

func fromInternalReleases(releases []internalpluginstore.Release) []Release {
	if len(releases) == 0 {
		return nil
	}
	out := make([]Release, len(releases))
	for i := range releases {
		out[i] = fromInternalRelease(releases[i])
	}
	return out
}

func toInternalReleaseAsset(asset ReleaseAsset) internalpluginstore.ReleaseAsset {
	return internalpluginstore.ReleaseAsset{
		APIURL:             asset.APIURL,
		Name:               asset.Name,
		BrowserDownloadURL: asset.BrowserDownloadURL,
		Size:               asset.Size,
	}
}

func fromInternalReleaseAsset(asset internalpluginstore.ReleaseAsset) ReleaseAsset {
	return ReleaseAsset{
		APIURL:             asset.APIURL,
		Name:               asset.Name,
		BrowserDownloadURL: asset.BrowserDownloadURL,
		Size:               asset.Size,
	}
}

func toInternalReleaseAssets(assets []ReleaseAsset) []internalpluginstore.ReleaseAsset {
	if len(assets) == 0 {
		return nil
	}
	out := make([]internalpluginstore.ReleaseAsset, len(assets))
	for i := range assets {
		out[i] = toInternalReleaseAsset(assets[i])
	}
	return out
}

func fromInternalReleaseAssets(assets []internalpluginstore.ReleaseAsset) []ReleaseAsset {
	if len(assets) == 0 {
		return nil
	}
	out := make([]ReleaseAsset, len(assets))
	for i := range assets {
		out[i] = fromInternalReleaseAsset(assets[i])
	}
	return out
}

func toInternalInstallOptions(options InstallOptions) internalpluginstore.InstallOptions {
	return internalpluginstore.InstallOptions{
		PluginsDir:    options.PluginsDir,
		GOOS:          options.GOOS,
		GOARCH:        options.GOARCH,
		PluginLoaded:  options.PluginLoaded,
		BeforeWrite:   options.BeforeWrite,
		BeforeReplace: options.BeforeReplace,
		BeforePublish: options.BeforePublish,
		RootLock: func() *internalpluginstore.RootLock {
			if options.RootLock == nil {
				return nil
			}
			return options.RootLock.inner
		}(),
	}
}

func toInternalInstallResult(result InstallResult) internalpluginstore.InstallResult {
	return internalpluginstore.InstallResult{
		ID:          result.ID,
		Version:     result.Version,
		ReleaseTag:  result.ReleaseTag,
		InstallType: result.InstallType,
		Path:        result.Path,
		Overwritten: result.Overwritten,
		Skipped:     result.Skipped,
	}
}

func fromInternalInstallResult(result internalpluginstore.InstallResult) InstallResult {
	return InstallResult{
		ID:          result.ID,
		Version:     result.Version,
		ReleaseTag:  result.ReleaseTag,
		InstallType: result.InstallType,
		Path:        result.Path,
		Overwritten: result.Overwritten,
		Skipped:     result.Skipped,
	}
}

func toInternalInstallPlan(plan InstallPlan) internalpluginstore.InstallPlan {
	return internalpluginstore.InstallPlan{Type: plan.Type, Artifacts: toInternalArtifacts(plan.Artifacts)}
}

func fromInternalInstallPlan(plan internalpluginstore.InstallPlan) InstallPlan {
	return InstallPlan{Type: plan.Type, Artifacts: fromInternalArtifacts(plan.Artifacts)}
}

func toInternalArtifact(artifact Artifact) internalpluginstore.Artifact {
	return internalpluginstore.Artifact{
		GOOS:   artifact.GOOS,
		GOARCH: artifact.GOARCH,
		URL:    artifact.URL,
		SHA256: artifact.SHA256,
		Size:   artifact.Size,
	}
}

func fromInternalArtifact(artifact internalpluginstore.Artifact) Artifact {
	return Artifact{
		GOOS:   artifact.GOOS,
		GOARCH: artifact.GOARCH,
		URL:    artifact.URL,
		SHA256: artifact.SHA256,
		Size:   artifact.Size,
	}
}

func toInternalArtifacts(artifacts []Artifact) []internalpluginstore.Artifact {
	if len(artifacts) == 0 {
		return nil
	}
	out := make([]internalpluginstore.Artifact, len(artifacts))
	for i := range artifacts {
		out[i] = toInternalArtifact(artifacts[i])
	}
	return out
}

func fromInternalArtifacts(artifacts []internalpluginstore.Artifact) []Artifact {
	if len(artifacts) == 0 {
		return nil
	}
	out := make([]Artifact, len(artifacts))
	for i := range artifacts {
		out[i] = fromInternalArtifact(artifacts[i])
	}
	return out
}

func fromInternalPlatform(platform internalpluginstore.Platform) Platform {
	return Platform{GOOS: platform.GOOS, GOARCH: platform.GOARCH}
}

func fromInternalPlatforms(platforms []internalpluginstore.Platform) []Platform {
	if len(platforms) == 0 {
		return nil
	}
	out := make([]Platform, len(platforms))
	for i := range platforms {
		out[i] = fromInternalPlatform(platforms[i])
	}
	return out
}

func toInternalManifest(manifest Manifest) internalpluginstore.Manifest {
	return internalpluginstore.Manifest{
		SchemaVersion: manifest.SchemaVersion,
		ID:            manifest.ID,
		Name:          manifest.Name,
		Description:   manifest.Description,
		Author:        manifest.Author,
		Version:       manifest.Version,
		ReleaseTag:    manifest.ReleaseTag,
		Repository:    manifest.Repository,
		Logo:          manifest.Logo,
		Homepage:      manifest.Homepage,
		License:       manifest.License,
		Tags:          append([]string(nil), manifest.Tags...),
		SourceID:      manifest.SourceID,
		SourceName:    manifest.SourceName,
		SourceURL:     manifest.SourceURL,
		Install:       toInternalInstallPlan(manifest.Install),
	}
}

func fromInternalManifest(manifest internalpluginstore.Manifest) Manifest {
	return Manifest{
		SchemaVersion: manifest.SchemaVersion,
		ID:            manifest.ID,
		Name:          manifest.Name,
		Description:   manifest.Description,
		Author:        manifest.Author,
		Version:       manifest.Version,
		ReleaseTag:    manifest.ReleaseTag,
		Repository:    manifest.Repository,
		Logo:          manifest.Logo,
		Homepage:      manifest.Homepage,
		License:       manifest.License,
		Tags:          append([]string(nil), manifest.Tags...),
		SourceID:      manifest.SourceID,
		SourceName:    manifest.SourceName,
		SourceURL:     manifest.SourceURL,
		Install:       fromInternalInstallPlan(manifest.Install),
	}
}

func toInternalAuthConfig(auth AuthConfig) internalpluginstore.AuthConfig {
	return internalpluginstore.AuthConfig{
		Match:          auth.Match,
		ApplyTo:        append([]string(nil), auth.ApplyTo...),
		Type:           auth.Type,
		TokenEnv:       auth.TokenEnv,
		UsernameEnv:    auth.UsernameEnv,
		PasswordEnv:    auth.PasswordEnv,
		HeaderName:     auth.HeaderName,
		HeaderValueEnv: auth.HeaderValueEnv,
		AllowInsecure:  auth.AllowInsecure,
	}
}

func fromInternalAuthConfig(auth internalpluginstore.AuthConfig) AuthConfig {
	return AuthConfig{
		Match:          auth.Match,
		ApplyTo:        append([]string(nil), auth.ApplyTo...),
		Type:           auth.Type,
		TokenEnv:       auth.TokenEnv,
		UsernameEnv:    auth.UsernameEnv,
		PasswordEnv:    auth.PasswordEnv,
		HeaderName:     auth.HeaderName,
		HeaderValueEnv: auth.HeaderValueEnv,
		AllowInsecure:  auth.AllowInsecure,
	}
}

func toInternalAuthConfigs(auth []AuthConfig) []internalpluginstore.AuthConfig {
	if len(auth) == 0 {
		return nil
	}
	out := make([]internalpluginstore.AuthConfig, len(auth))
	for i := range auth {
		out[i] = toInternalAuthConfig(auth[i])
	}
	return out
}

func fromInternalAuthConfigs(auth []internalpluginstore.AuthConfig) []AuthConfig {
	if len(auth) == 0 {
		return nil
	}
	out := make([]AuthConfig, len(auth))
	for i := range auth {
		out[i] = fromInternalAuthConfig(auth[i])
	}
	return out
}
