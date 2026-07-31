package pluginstore

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	log "github.com/sirupsen/logrus"
)

type InstallOptions struct {
	PluginsDir string
	GOOS       string
	GOARCH     string
	// PluginLoaded reports whether the plugin's dynamic library is currently
	// loaded by the running host. Windows installs are rejected only when they
	// would overwrite an existing target file while it returns true.
	PluginLoaded func() bool
	// BeforeWrite runs after the archive has been downloaded and verified, but
	// before an existing target plugin file is replaced.
	BeforeWrite func() error
	// BeforeReplace runs after BeforeWrite and the loaded-plugin check. It can
	// stage the existing target for rollback by a later config commit.
	BeforeReplace func(string) error
	// BeforePublish runs immediately before the atomic artifact write.
	BeforePublish func(path, version string, overwritten bool, sha256 string) error
	RootLock      *RootLock
	writeFile     func(string, []byte, os.FileMode) error
}

// ErrLoadedPluginLocked is returned when an install would overwrite a plugin
// library that is loaded by the running process on Windows.
var ErrLoadedPluginLocked = errors.New("loaded plugin library cannot be overwritten while the server is running")

// ErrPendingDeletion is returned when an unfinished deletion owns the plugin ID.
var ErrPendingDeletion = errors.New("plugin has a pending deletion transaction")

// ErrPendingInstall is returned when an unfinished install owns the plugin ID.
var ErrPendingInstall = errors.New("plugin has a pending install transaction")

const maxPluginLibraryBytes int64 = maxPluginStoreArtifactBytes

type InstallResult struct {
	ID          string `json:"id"`
	Version     string `json:"version"`
	ReleaseTag  string `json:"release_tag,omitempty"`
	InstallType string `json:"install_type,omitempty"`
	Path        string `json:"path"`
	Overwritten bool   `json:"overwritten"`
	Skipped     bool   `json:"skipped"`
}

// PreparedInstall contains a downloaded and verified plugin archive that has
// not yet changed the plugin directory.
type PreparedInstall struct {
	archiveData    []byte
	archivePath    string
	archiveTempDir string
	plugin         Plugin
	installType    string
	releaseTag     string
}

// SpoolToTemp moves the verified archive out of memory until installation.
func (p *PreparedInstall) SpoolToTemp() error {
	if p == nil {
		return fmt.Errorf("prepared plugin archive is nil")
	}
	if p.archivePath != "" {
		return nil
	}
	if len(p.archiveData) == 0 {
		return fmt.Errorf("prepared plugin archive is empty")
	}
	tempDir, errTempDir := os.MkdirTemp("", "cliproxy-plugin-archive-*")
	if errTempDir != nil {
		return fmt.Errorf("create prepared plugin archive directory: %w", errTempDir)
	}
	archivePath := filepath.Join(tempDir, "archive.zip")
	if errWrite := os.WriteFile(archivePath, p.archiveData, 0o600); errWrite != nil {
		_ = os.RemoveAll(tempDir)
		return fmt.Errorf("write prepared plugin archive: %w", errWrite)
	}
	archive, errOpen := os.OpenFile(archivePath, os.O_RDWR, 0)
	if errOpen != nil {
		_ = os.RemoveAll(tempDir)
		return fmt.Errorf("open prepared plugin archive: %w", errOpen)
	}
	if errSync := archive.Sync(); errSync != nil {
		_ = archive.Close()
		_ = os.RemoveAll(tempDir)
		return fmt.Errorf("sync prepared plugin archive: %w", errSync)
	}
	if errClose := archive.Close(); errClose != nil {
		_ = os.RemoveAll(tempDir)
		return fmt.Errorf("close prepared plugin archive: %w", errClose)
	}
	p.archiveData = nil
	p.archivePath = archivePath
	p.archiveTempDir = tempDir
	return nil
}

func (p *PreparedInstall) Close() error {
	if p == nil || p.archiveTempDir == "" {
		return nil
	}
	if err := os.RemoveAll(p.archiveTempDir); err != nil {
		return err
	}
	p.archivePath = ""
	p.archiveTempDir = ""
	return nil
}

// Install applies a prepared plugin archive to the target plugin directory.
func (p PreparedInstall) Install(options InstallOptions) (InstallResult, error) {
	archiveData := p.archiveData
	if p.archivePath != "" {
		var errRead error
		archiveData, errRead = os.ReadFile(p.archivePath)
		if errRead != nil {
			return InstallResult{}, fmt.Errorf("read prepared plugin archive: %w", errRead)
		}
	}
	if len(archiveData) == 0 {
		return InstallResult{}, fmt.Errorf("prepared plugin archive is empty")
	}
	result, errInstall := InstallArchive(archiveData, p.plugin, options)
	result.InstallType = p.installType
	result.ReleaseTag = p.releaseTag
	return result, errInstall
}

func (c Client) Install(ctx context.Context, plugin Plugin, options InstallOptions) (InstallResult, error) {
	prepared, errPrepare := c.Prepare(ctx, plugin, options)
	if errPrepare != nil {
		return InstallResult{}, errPrepare
	}
	return prepared.Install(options)
}

// Prepare downloads and verifies the selected plugin artifact without changing
// the plugin directory.
func (c Client) Prepare(ctx context.Context, plugin Plugin, options InstallOptions) (PreparedInstall, error) {
	if errValidate := ValidatePlugin(plugin); errValidate != nil {
		return PreparedInstall{}, errValidate
	}
	options = normalizeInstallOptions(options)
	if PluginInstallType(plugin) == InstallTypeDirect {
		plugin.Version = normalizeVersion(plugin.Version)
		return c.PrepareDirect(ctx, plugin, plugin.Install, options)
	}
	release, errRelease := c.FetchLatestRelease(ctx, plugin)
	if errRelease != nil {
		return PreparedInstall{}, errRelease
	}
	latestVersion, errVersion := ReleaseVersion(release)
	if errVersion != nil {
		return PreparedInstall{}, errVersion
	}
	plugin.Version = latestVersion
	return c.prepareRelease(ctx, plugin, release, latestVersion, options)
}

func (c Client) InstallManifest(ctx context.Context, manifest Manifest, options InstallOptions) (InstallResult, error) {
	prepared, errPrepare := c.PrepareManifest(ctx, manifest, options)
	if errPrepare != nil {
		return InstallResult{}, errPrepare
	}
	return prepared.Install(options)
}

// PrepareManifest downloads and verifies a manifest-selected plugin artifact
// without changing the plugin directory.
func (c Client) PrepareManifest(
	ctx context.Context,
	manifest Manifest,
	options InstallOptions,
) (PreparedInstall, error) {
	if errValidate := manifest.Validate(); errValidate != nil {
		return PreparedInstall{}, errValidate
	}
	options = normalizeInstallOptions(options)
	switch manifest.InstallType() {
	case InstallTypeDirect:
		plugin, errPlugin := c.directPluginFromManifest(ctx, manifest)
		if errPlugin != nil {
			return PreparedInstall{}, errPlugin
		}
		return c.PrepareDirect(ctx, plugin, plugin.Install, options)
	case InstallTypeGitHubRelease:
		return c.PrepareVersion(ctx, manifest.Plugin(), manifest.ReleaseTag, manifest.Version, options)
	default:
		return PreparedInstall{}, fmt.Errorf("unsupported install type %q", manifest.Install.Type)
	}
}

// InstallVersion installs a plugin artifact from a fixed release tag/version.
func (c Client) InstallVersion(
	ctx context.Context,
	plugin Plugin,
	releaseTag string,
	version string,
	options InstallOptions,
) (InstallResult, error) {
	prepared, errPrepare := c.PrepareVersion(ctx, plugin, releaseTag, version, options)
	if errPrepare != nil {
		return InstallResult{}, errPrepare
	}
	return prepared.Install(options)
}

// PrepareVersion downloads and verifies a fixed plugin release without
// changing the plugin directory.
func (c Client) PrepareVersion(
	ctx context.Context,
	plugin Plugin,
	releaseTag string,
	version string,
	options InstallOptions,
) (PreparedInstall, error) {
	if errValidate := ValidatePlugin(plugin); errValidate != nil {
		return PreparedInstall{}, errValidate
	}
	options = normalizeInstallOptions(options)
	version = normalizeVersion(version)
	if !validPluginVersion(version) {
		return PreparedInstall{}, fmt.Errorf("invalid plugin version %q", version)
	}
	releaseTag = strings.TrimSpace(releaseTag)
	if releaseTag == "" {
		releaseTag = version
	}
	release, errRelease := c.FetchReleaseByTag(ctx, plugin, releaseTag)
	if errRelease != nil {
		return PreparedInstall{}, errRelease
	}
	releaseVersion, errVersion := ReleaseVersion(release)
	if errVersion != nil {
		return PreparedInstall{}, errVersion
	}
	if releaseVersion != version {
		return PreparedInstall{}, fmt.Errorf(
			"release tag %q resolved version %q, want %q", releaseTag, releaseVersion, version,
		)
	}
	plugin.Version = version
	return c.prepareRelease(ctx, plugin, release, version, options)
}

func (c Client) prepareRelease(
	ctx context.Context,
	plugin Plugin,
	release Release,
	version string,
	options InstallOptions,
) (PreparedInstall, error) {
	archiveAsset, checksumAsset, errAssets := SelectReleaseAssets(
		release, plugin.ID, plugin.Version, options.GOOS, options.GOARCH,
	)
	if errAssets != nil {
		return PreparedInstall{}, errAssets
	}
	archiveData, errArchive := c.DownloadAsset(ctx, archiveAsset)
	if errArchive != nil {
		return PreparedInstall{}, fmt.Errorf("download %s: %w", archiveAsset.Name, errArchive)
	}
	checksumData, errChecksum := c.DownloadAsset(ctx, checksumAsset)
	if errChecksum != nil {
		return PreparedInstall{}, fmt.Errorf("download checksums.txt: %w", errChecksum)
	}
	checksums, errParse := ParseChecksums(checksumData)
	if errParse != nil {
		return PreparedInstall{}, errParse
	}
	if errVerify := VerifyChecksum(archiveAsset.Name, archiveData, checksums); errVerify != nil {
		return PreparedInstall{}, errVerify
	}
	plugin.Version = version
	return PreparedInstall{
		archiveData: archiveData,
		plugin:      plugin,
		installType: InstallTypeGitHubRelease,
		releaseTag:  strings.TrimSpace(release.TagName),
	}, nil
}

func (c Client) InstallDirect(
	ctx context.Context,
	plugin Plugin,
	plan InstallPlan,
	options InstallOptions,
) (InstallResult, error) {
	prepared, errPrepare := c.PrepareDirect(ctx, plugin, plan, options)
	if errPrepare != nil {
		return InstallResult{}, errPrepare
	}
	return prepared.Install(options)
}

// PrepareDirect downloads and verifies a direct plugin artifact without
// changing the plugin directory.
func (c Client) PrepareDirect(
	ctx context.Context,
	plugin Plugin,
	plan InstallPlan,
	options InstallOptions,
) (PreparedInstall, error) {
	plugin.ID = CanonicalPluginID(plugin.ID)
	plugin.Version = normalizeVersion(plugin.Version)
	if !validPluginID(plugin.ID) {
		return PreparedInstall{}, fmt.Errorf("invalid plugin id %q", plugin.ID)
	}
	if !validPluginVersion(plugin.Version) {
		return PreparedInstall{}, fmt.Errorf("invalid plugin version %q", plugin.Version)
	}
	plan = NormalizeInstallPlan(plan)
	plan.Type = InstallTypeDirect
	if errValidate := ValidateInstallPlan(plan); errValidate != nil {
		return PreparedInstall{}, errValidate
	}
	options = normalizeInstallOptions(options)
	artifact, errSelect := SelectArtifact(plan, options.GOOS, options.GOARCH)
	if errSelect != nil {
		return PreparedInstall{}, errSelect
	}
	archiveData, errDownload := c.DownloadArtifact(ctx, artifact)
	if errDownload != nil {
		return PreparedInstall{}, fmt.Errorf("download artifact: %w", errDownload)
	}
	if errVerify := VerifyArtifactChecksum(artifact, archiveData); errVerify != nil {
		return PreparedInstall{}, errVerify
	}
	return PreparedInstall{
		archiveData: archiveData,
		plugin:      plugin,
		installType: InstallTypeDirect,
	}, nil
}

func (c Client) directPluginFromManifest(ctx context.Context, manifest Manifest) (Plugin, error) {
	plugin := manifest.Plugin()
	plugin.Version = normalizeVersion(manifest.Version)
	plugin.Install = NormalizeInstallPlan(plugin.Install)
	plugin.Install.Type = InstallTypeDirect
	if len(plugin.Install.Artifacts) > 0 {
		return plugin, nil
	}
	sourceURL := strings.TrimSpace(manifest.SourceURL)
	if sourceURL == "" {
		sourceURL = strings.TrimSpace(c.RegistryURL)
	}
	if sourceURL == "" {
		return Plugin{}, fmt.Errorf("direct install manifest missing source-url")
	}
	sourceClient := c
	sourceClient.RegistryURL = sourceURL
	registry, errRegistry := sourceClient.FetchRegistry(ctx)
	if errRegistry != nil {
		return Plugin{}, fmt.Errorf("fetch direct install source: %w", errRegistry)
	}
	resolved, okPlugin := registry.PluginByID(manifest.ID)
	if !okPlugin {
		return Plugin{}, fmt.Errorf("direct install plugin %q not found in source", strings.TrimSpace(manifest.ID))
	}
	if PluginInstallType(resolved) != InstallTypeDirect {
		return Plugin{}, fmt.Errorf(
			"direct install plugin %q resolved as %q", strings.TrimSpace(manifest.ID), PluginInstallType(resolved),
		)
	}
	return directPluginVersion(resolved, manifest.ID, manifest.Version)
}

func directPluginVersion(plugin Plugin, id string, version string) (Plugin, error) {
	id = CanonicalPluginID(id)
	plugin.ID = CanonicalPluginID(plugin.ID)
	version = normalizeVersion(version)
	if normalizeVersion(plugin.Version) == version {
		plugin.Version = version
		plugin.Install = NormalizeInstallPlan(plugin.Install)
		plugin.Install.Type = InstallTypeDirect
		if errPlan := ValidateInstallPlan(plugin.Install); errPlan != nil {
			return Plugin{}, fmt.Errorf("direct install plugin %q version %q: %w", id, version, errPlan)
		}
		return plugin, nil
	}
	for _, candidate := range plugin.Versions {
		if normalizeVersion(candidate.Version) != version {
			continue
		}
		plugin.Version = version
		plugin.Install = NormalizeInstallPlan(candidate.Install)
		if plugin.Install.Type == "" {
			plugin.Install.Type = InstallTypeDirect
		}
		if plugin.Install.Type != InstallTypeDirect {
			return Plugin{}, fmt.Errorf(
				"direct install plugin %q version %q resolved as %q", id, version, plugin.Install.Type,
			)
		}
		if errPlan := ValidateInstallPlan(plugin.Install); errPlan != nil {
			return Plugin{}, fmt.Errorf("direct install plugin %q version %q: %w", id, version, errPlan)
		}
		return plugin, nil
	}
	return Plugin{}, fmt.Errorf("direct install plugin %q version %q not found in source", id, version)
}

func InstallArchive(archiveData []byte, plugin Plugin, options InstallOptions) (InstallResult, error) {
	options = normalizeInstallOptions(options)
	rootLock := options.RootLock
	if rootLock == nil {
		var errLock error
		rootLock, errLock = LockRoot(options.PluginsDir)
		if errLock != nil {
			return InstallResult{}, errLock
		}
		defer rootLock.Unlock()
	} else if !rootLock.matches(options.PluginsDir) {
		return InstallResult{}, fmt.Errorf("plugin root lock does not match install directory")
	}
	id := CanonicalPluginID(plugin.ID)
	if !validPluginID(id) {
		return InstallResult{}, fmt.Errorf("invalid plugin id %q", plugin.ID)
	}
	pendingDeletions, errPending := PendingDeletionPluginIDs(options.PluginsDir)
	if errPending != nil {
		return InstallResult{}, fmt.Errorf("inspect pending plugin deletions: %w", errPending)
	}
	if _, pending := pendingDeletions[id]; pending {
		return InstallResult{}, fmt.Errorf("%w: %s", ErrPendingDeletion, id)
	}
	pendingInstalls, errPending := PendingInstallPluginIDs(options.PluginsDir)
	if errPending != nil {
		return InstallResult{}, fmt.Errorf("inspect pending plugin installs: %w", errPending)
	}
	if _, pending := pendingInstalls[id]; pending {
		return InstallResult{}, fmt.Errorf("%w: %s", ErrPendingInstall, id)
	}
	plugin.ID = id
	version := normalizeVersion(plugin.Version)
	if !validPluginVersion(version) {
		return InstallResult{}, fmt.Errorf("invalid plugin version %q", plugin.Version)
	}
	plugin.Version = version
	reader, errZip := zip.NewReader(bytes.NewReader(archiveData), int64(len(archiveData)))
	if errZip != nil {
		return InstallResult{}, fmt.Errorf("open zip: %w", errZip)
	}

	libraryData, mode, errLibrary := readTargetLibrary(reader, id, version, options.GOOS)
	if errLibrary != nil {
		return InstallResult{}, errLibrary
	}

	targetRoot, targetName, targetPath, errTarget := secureInstallTarget(options, id, version)
	if errTarget != nil {
		return InstallResult{}, errTarget
	}
	defer targetRoot.Close()
	overwritten := false
	var existingInfo os.FileInfo
	if info, errStat := targetRoot.Lstat(targetName); errStat == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return InstallResult{}, fmt.Errorf("target plugin is not a regular file")
		}
		overwritten = true
		existingInfo = info
	} else if !errors.Is(errStat, os.ErrNotExist) {
		return InstallResult{}, fmt.Errorf("stat target plugin: %w", errStat)
	}
	if overwritten {
		existing, errOpenExisting := targetRoot.Open(targetName)
		if errOpenExisting != nil {
			return InstallResult{}, fmt.Errorf("open target plugin: %w", errOpenExisting)
		}
		existingData, errReadExisting := io.ReadAll(existing)
		openedInfo, errOpenedInfo := existing.Stat()
		errCloseExisting := existing.Close()
		verifiedInfo, errVerifiedInfo := targetRoot.Lstat(targetName)
		if errReadExisting != nil {
			return InstallResult{}, fmt.Errorf("read target plugin: %w", errReadExisting)
		}
		if errOpenedInfo != nil {
			return InstallResult{}, fmt.Errorf("stat opened target plugin: %w", errOpenedInfo)
		}
		if errCloseExisting != nil {
			return InstallResult{}, fmt.Errorf("close target plugin: %w", errCloseExisting)
		}
		if errVerifiedInfo != nil {
			return InstallResult{}, fmt.Errorf("verify target plugin: %w", errVerifiedInfo)
		}
		if verifiedInfo.Mode()&os.ModeSymlink != 0 || !verifiedInfo.Mode().IsRegular() ||
			!os.SameFile(existingInfo, openedInfo) || !os.SameFile(openedInfo, verifiedInfo) {
			return InstallResult{}, fmt.Errorf("target plugin changed during inspection")
		}
		if bytes.Equal(existingData, libraryData) {
			return InstallResult{
				ID:          id,
				Version:     strings.TrimSpace(plugin.Version),
				Path:        targetPath,
				Overwritten: true,
				Skipped:     true,
			}, nil
		}
	}
	if overwritten && options.BeforeWrite != nil {
		if errBeforeWrite := options.BeforeWrite(); errBeforeWrite != nil {
			return InstallResult{}, fmt.Errorf("prepare plugin write: %w", errBeforeWrite)
		}
	}
	// Re-check immediately before replacing an existing file: the same version
	// may have been loaded while the archive was being downloaded and verified.
	if overwritten && loadedPluginInstallBlocked(options) {
		return InstallResult{}, ErrLoadedPluginLocked
	}
	if overwritten && options.BeforeReplace != nil {
		if errBeforeReplace := options.BeforeReplace(targetPath); errBeforeReplace != nil {
			return InstallResult{}, fmt.Errorf("preserve plugin before write: %w", errBeforeReplace)
		}
	}
	if options.BeforePublish != nil {
		digest := sha256.Sum256(libraryData)
		if errBeforePublish := options.BeforePublish(
			targetPath, version, overwritten, hex.EncodeToString(digest[:]),
		); errBeforePublish != nil {
			return InstallResult{}, fmt.Errorf("prepare plugin publication: %w", errBeforePublish)
		}
	}
	result := InstallResult{
		ID:          id,
		Version:     strings.TrimSpace(plugin.Version),
		Path:        targetPath,
		Overwritten: overwritten,
	}
	var errWrite error
	if options.writeFile != nil {
		errWrite = options.writeFile(targetPath, libraryData, mode)
	} else {
		errWrite = writeFileAtomicRooted(targetRoot, targetName, libraryData, mode)
	}
	if errWrite != nil {
		return result, errWrite
	}
	return result, nil
}

func secureInstallTarget(
	options InstallOptions,
	id string,
	version string,
) (*os.Root, string, string, error) {
	version = normalizeVersion(version)
	if !validPluginVersion(version) {
		return nil, "", "", fmt.Errorf("invalid plugin version %q", version)
	}
	absoluteRoot, errRoot := filepath.Abs(options.PluginsDir)
	if errRoot != nil {
		return nil, "", "", fmt.Errorf("resolve plugin root: %w", errRoot)
	}
	resolvedRoot, errRoot := filepath.EvalSymlinks(absoluteRoot)
	if errRoot != nil {
		return nil, "", "", fmt.Errorf("resolve plugin root links: %w", errRoot)
	}
	targetRoot, errTargetRoot := openOrCreatePluginDirectoryRoot(
		resolvedRoot, []string{options.GOOS, options.GOARCH}, 0o755,
	)
	if errTargetRoot != nil {
		return nil, "", "", fmt.Errorf("open plugin install directory: %w", errTargetRoot)
	}
	targetName := versionedPluginFileName(id, version, options.GOOS)
	return targetRoot, targetName, filepath.Join(targetRoot.Name(), targetName), nil
}

func openOrCreatePluginDirectoryRoot(
	rootPath string,
	components []string,
	perm os.FileMode,
) (*os.Root, error) {
	root, errRoot := os.OpenRoot(rootPath)
	if errRoot != nil {
		return nil, errRoot
	}
	for _, component := range components {
		component = strings.TrimSpace(component)
		if component == "" || component == "." || component == ".." || filepath.Base(component) != component {
			_ = root.Close()
			return nil, fmt.Errorf("invalid plugin directory component %q", component)
		}
		created := false
		info, errInfo := root.Lstat(component)
		if errors.Is(errInfo, os.ErrNotExist) {
			errMkdir := root.Mkdir(component, perm)
			if errMkdir != nil && !errors.Is(errMkdir, os.ErrExist) {
				_ = root.Close()
				return nil, errMkdir
			}
			created = errMkdir == nil
			info, errInfo = root.Lstat(component)
		}
		if errInfo != nil {
			_ = root.Close()
			return nil, errInfo
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			_ = root.Close()
			return nil, fmt.Errorf("plugin directory component %q is not a regular directory", component)
		}
		next, errOpen := root.OpenRoot(component)
		if errOpen != nil {
			_ = root.Close()
			return nil, errOpen
		}
		openedInfo, errOpened := next.Stat(".")
		verifiedInfo, errVerified := root.Lstat(component)
		if errOpened != nil || errVerified != nil || !os.SameFile(info, openedInfo) || !os.SameFile(openedInfo, verifiedInfo) {
			_ = next.Close()
			_ = root.Close()
			if errOpened != nil {
				return nil, errOpened
			}
			if errVerified != nil {
				return nil, errVerified
			}
			return nil, fmt.Errorf("plugin directory component %q changed during validation", component)
		}
		if created {
			directory, errDirectory := next.Open(".")
			if errDirectory != nil {
				_ = next.Close()
				_ = root.Close()
				return nil, errDirectory
			}
			if errChmod := directory.Chmod(perm); errChmod != nil {
				_ = directory.Close()
				_ = next.Close()
				_ = root.Close()
				return nil, errChmod
			}
			if errClose := directory.Close(); errClose != nil {
				_ = next.Close()
				_ = root.Close()
				return nil, errClose
			}
		}
		if errClose := root.Close(); errClose != nil {
			_ = next.Close()
			return nil, errClose
		}
		root = next
	}
	return root, nil
}

func writeFileAtomicRooted(root *os.Root, targetName string, data []byte, mode os.FileMode) error {
	if root == nil {
		return fmt.Errorf("plugin install directory is unavailable")
	}
	tempName := "." + targetName + ".tmp-" + rand.Text()
	temp, errTemp := root.OpenFile(tempName, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if errTemp != nil {
		return fmt.Errorf("create temp plugin file: %w", errTemp)
	}
	removeTemp := true
	closed := false
	defer func() {
		if !closed {
			if errClose := temp.Close(); errClose != nil {
				log.WithError(errClose).Debug("failed to close temp plugin file")
			}
		}
		if removeTemp {
			if errRemove := root.Remove(tempName); errRemove != nil && !errors.Is(errRemove, os.ErrNotExist) {
				log.WithError(errRemove).Debug("failed to remove temp plugin file")
			}
		}
	}()

	if errChmod := temp.Chmod(mode); errChmod != nil {
		return fmt.Errorf("chmod temp plugin file: %w", errChmod)
	}
	if _, errWrite := temp.Write(data); errWrite != nil {
		return fmt.Errorf("write temp plugin file: %w", errWrite)
	}
	if errSync := temp.Sync(); errSync != nil {
		return fmt.Errorf("sync temp plugin file: %w", errSync)
	}
	if errClose := temp.Close(); errClose != nil {
		return fmt.Errorf("close temp plugin file: %w", errClose)
	}
	closed = true
	if errRename := root.Rename(tempName, targetName); errRename != nil {
		return fmt.Errorf("install plugin file: %w", errRename)
	}
	removeTemp = false
	if runtime.GOOS == "windows" {
		return nil
	}
	directory, errDirectory := root.Open(".")
	if errDirectory != nil {
		return fmt.Errorf("open plugin directory: %w", errDirectory)
	}
	defer directory.Close()
	if errSync := directory.Sync(); errSync != nil {
		return fmt.Errorf("sync plugin directory: %w", errSync)
	}
	return nil
}

func readTargetLibrary(reader *zip.Reader, id string, version string, goos string) ([]byte, os.FileMode, error) {
	targetName := CanonicalPluginID(id) + pluginExtension(goos)
	versionedTargetName := versionedPluginFileName(id, version, goos)
	var target *zip.File
	for _, file := range reader.File {
		cleanedName, errClean := cleanZipName(file.Name)
		if errClean != nil {
			return nil, 0, errClean
		}
		if file.FileInfo().IsDir() {
			continue
		}
		if !regularZipFile(file) {
			return nil, 0, fmt.Errorf("zip entry %s is not a regular file", file.Name)
		}
		if !hasDynamicLibraryExtension(cleanedName) {
			continue
		}
		if cleanedName != targetName && cleanedName != versionedTargetName {
			if path.Base(cleanedName) == targetName || path.Base(cleanedName) == versionedTargetName {
				return nil, 0, fmt.Errorf("target dynamic library must be at zip root")
			}
			return nil, 0, fmt.Errorf("dynamic library filename must be %s or %s", targetName, versionedTargetName)
		}
		if target != nil {
			return nil, 0, fmt.Errorf("zip contains multiple target dynamic libraries")
		}
		target = file
	}
	if target == nil {
		return nil, 0, fmt.Errorf("zip does not contain %s", targetName)
	}
	if target.UncompressedSize64 > uint64(maxPluginLibraryBytes) {
		return nil, 0, fmt.Errorf("plugin library exceeds %d bytes", maxPluginLibraryBytes)
	}

	handle, errOpen := target.Open()
	if errOpen != nil {
		return nil, 0, fmt.Errorf("open %s: %w", targetName, errOpen)
	}
	defer func() {
		if errClose := handle.Close(); errClose != nil {
			log.WithError(errClose).Debug("failed to close plugin archive entry")
		}
	}()
	data, errRead := io.ReadAll(io.LimitReader(handle, maxPluginLibraryBytes+1))
	if errRead != nil {
		return nil, 0, fmt.Errorf("read %s: %w", targetName, errRead)
	}
	if int64(len(data)) > maxPluginLibraryBytes {
		return nil, 0, fmt.Errorf("plugin library exceeds %d bytes", maxPluginLibraryBytes)
	}
	mode := target.FileInfo().Mode().Perm()
	if mode == 0 {
		mode = 0o755
	}
	return data, mode, nil
}

func versionedPluginFileName(id string, version string, goos string) string {
	return CanonicalPluginID(id) + "-v" + normalizeVersion(version) + pluginExtension(goos)
}

func cleanZipName(name string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", fmt.Errorf("zip entry has empty name")
	}
	if strings.Contains(name, `\`) {
		return "", fmt.Errorf("zip entry %s uses backslash path separators", name)
	}
	if path.IsAbs(name) {
		return "", fmt.Errorf("zip entry %s is absolute", name)
	}
	cleaned := path.Clean(name)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("zip entry %s escapes archive root", name)
	}
	return cleaned, nil
}

func regularZipFile(file *zip.File) bool {
	mode := file.FileInfo().Mode()
	return mode.IsRegular() || mode.Type() == 0
}

func hasDynamicLibraryExtension(name string) bool {
	lowerName := strings.ToLower(name)
	return strings.HasSuffix(lowerName, ".dylib") || strings.HasSuffix(lowerName, ".so") || strings.HasSuffix(
		lowerName, ".dll",
	)
}

type pluginFileInfo struct {
	ID      string
	Path    string
	Version string
}

func discoverCurrentPluginFiles(root string) ([]pluginFileInfo, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		root = "plugins"
	}
	candidates := pluginCandidateDirs(root, runtime.GOOS, runtime.GOARCH)
	extension := pluginExtension(runtime.GOOS)
	selected := make([]pluginFileInfo, 0)
	seen := make(map[string]struct{})
	for _, dir := range candidates {
		entries, errReadDir := os.ReadDir(dir)
		if errReadDir != nil {
			if os.IsNotExist(errReadDir) {
				continue
			}
			return nil, errReadDir
		}
		files := make([]string, 0, len(entries))
		for _, entry := range entries {
			if entry == nil || !entry.Type().IsRegular() {
				continue
			}
			if strings.HasSuffix(strings.ToLower(entry.Name()), extension) {
				files = append(files, filepath.Join(dir, entry.Name()))
			}
		}
		sort.Strings(files)
		for _, candidatePath := range files {
			file, okFile := pluginFileInfoFromPath(candidatePath, extension)
			if !okFile {
				continue
			}
			if _, exists := seen[file.ID]; exists {
				continue
			}
			seen[file.ID] = struct{}{}
			selected = append(selected, file)
		}
	}
	return selected, nil
}

func pluginCandidateDirs(root string, goos string, goarch string) []string {
	dirs := make([]string, 0, 2)
	dirs = append(dirs, filepath.Join(root, goos, goarch))
	dirs = append(dirs, root)
	return dirs
}

func pluginIDFromPath(path string) string {
	file, ok := pluginFileInfoFromPath(path, "")
	if ok {
		return file.ID
	}
	base := filepath.Base(path)
	lowerBase := strings.ToLower(base)
	for _, extension := range []string{".so", ".dylib", ".dll"} {
		if strings.HasSuffix(lowerBase, extension) {
			return CanonicalPluginID(base[:len(base)-len(extension)])
		}
	}
	return CanonicalPluginID(base)
}

func pluginFileInfoFromPath(filePath string, requiredExtension string) (pluginFileInfo, bool) {
	base := filepath.Base(filePath)
	lowerBase := strings.ToLower(base)
	extension := strings.TrimSpace(requiredExtension)
	if extension != "" {
		if !strings.HasSuffix(lowerBase, strings.ToLower(extension)) {
			return pluginFileInfo{}, false
		}
	} else {
		for _, candidateExtension := range []string{".so", ".dylib", ".dll"} {
			if strings.HasSuffix(lowerBase, candidateExtension) {
				extension = candidateExtension
				break
			}
		}
		if extension == "" {
			return pluginFileInfo{}, false
		}
	}
	name := base[:len(base)-len(extension)]
	id := name
	version := ""
	if versionIndex := strings.LastIndex(name, "-v"); versionIndex > 0 {
		candidateID := name[:versionIndex]
		candidateVersion := name[versionIndex+2:]
		if validPluginID(candidateID) && validPluginVersion(candidateVersion) {
			id = candidateID
			version = candidateVersion
		}
	}
	if !validPluginID(id) {
		return pluginFileInfo{}, false
	}
	return pluginFileInfo{ID: CanonicalPluginID(id), Path: filePath, Version: version}, true
}

func pluginExtension(goos string) string {
	switch strings.ToLower(strings.TrimSpace(goos)) {
	case "darwin", "mac", "macos", "osx":
		return ".dylib"
	case "windows":
		return ".dll"
	default:
		return ".so"
	}
}

func writeFileAtomic(targetPath string, data []byte, mode os.FileMode) error {
	targetDir := filepath.Dir(targetPath)
	if errMkdir := os.MkdirAll(targetDir, 0o755); errMkdir != nil {
		return fmt.Errorf("create plugin directory: %w", errMkdir)
	}

	temp, errTemp := os.CreateTemp(targetDir, "."+filepath.Base(targetPath)+".tmp-*")
	if errTemp != nil {
		return fmt.Errorf("create temp plugin file: %w", errTemp)
	}
	tempPath := temp.Name()
	removeTemp := true
	closed := false
	defer func() {
		if !closed {
			if errClose := temp.Close(); errClose != nil {
				log.WithError(errClose).Debug("failed to close temp plugin file")
			}
		}
		if removeTemp {
			if errRemove := os.Remove(tempPath); errRemove != nil && !errors.Is(errRemove, os.ErrNotExist) {
				log.WithError(errRemove).Debug("failed to remove temp plugin file")
			}
		}
	}()

	if errChmod := temp.Chmod(mode); errChmod != nil {
		return fmt.Errorf("chmod temp plugin file: %w", errChmod)
	}
	if _, errWrite := temp.Write(data); errWrite != nil {
		return fmt.Errorf("write temp plugin file: %w", errWrite)
	}
	if errSync := temp.Sync(); errSync != nil {
		return fmt.Errorf("sync temp plugin file: %w", errSync)
	}
	if errClose := temp.Close(); errClose != nil {
		return fmt.Errorf("close temp plugin file: %w", errClose)
	}
	closed = true
	if errReplace := replaceFile(tempPath, targetPath); errReplace != nil {
		return fmt.Errorf("install plugin file: %w", errReplace)
	}
	removeTemp = false
	return syncDirectory(targetDir)
}

func loadedPluginInstallBlocked(options InstallOptions) bool {
	return options.PluginLoaded != nil && strings.EqualFold(options.GOOS, "windows") && options.PluginLoaded()
}

func normalizeInstallOptions(options InstallOptions) InstallOptions {
	options.PluginsDir = strings.TrimSpace(options.PluginsDir)
	if options.PluginsDir == "" {
		options.PluginsDir = "plugins"
	}
	options.GOOS = strings.TrimSpace(options.GOOS)
	if options.GOOS == "" {
		options.GOOS = runtime.GOOS
	}
	options.GOARCH = strings.TrimSpace(options.GOARCH)
	if options.GOARCH == "" {
		options.GOARCH = runtime.GOARCH
	}
	options.GOOS = normalizeGOOS(options.GOOS)
	options.GOARCH = normalizeGOARCH(options.GOARCH)
	return options
}
