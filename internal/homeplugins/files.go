package homeplugins

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	sdkpluginstore "github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginstore"
)

func deletePluginArtifact(root string, id string, pluginRuntime PluginRuntime) (string, bool, error) {
	rootLock, errLock := sdkpluginstore.LockRoot(root)
	if errLock != nil {
		return "", false, errLock
	}
	defer rootLock.Unlock()

	id = sdkpluginstore.CanonicalPluginID(id)
	if !validPluginFileID(id) {
		return "", false, fmt.Errorf("invalid plugin id %q", id)
	}
	pendingInstalls, errPending := sdkpluginstore.PendingInstallPluginIDs(root)
	if errPending != nil {
		return "", false, fmt.Errorf("inspect pending plugin installs: %w", errPending)
	}
	if _, pending := pendingInstalls[id]; pending {
		return "", false, fmt.Errorf("%w: %s", sdkpluginstore.ErrPendingInstall, id)
	}
	paths, errPaths := pluginFilePaths(root, id)
	if errPaths != nil {
		return "", false, errPaths
	}
	if len(paths) == 0 {
		return "", false, nil
	}
	if pluginRuntime != nil && pluginRuntime.PluginBusy(id) {
		if !pluginRuntime.UnloadPlugin(id) && pluginRuntime.PluginBusy(id) {
			return paths[0], false, sdkpluginstore.ErrLoadedPluginLocked
		}
	}
	transaction, errPrepare := sdkpluginstore.PrepareDeletionTransactionLocked(root, id, paths, rootLock)
	if errPrepare != nil {
		return paths[0], false, errPrepare
	}
	rollback := func(cause error) error {
		return errors.Join(cause, transaction.Rollback())
	}
	if errHide := transaction.Hide(); errHide != nil {
		return paths[0], false, rollback(errHide)
	}
	remaining, errRemaining := sdkpluginstore.FindPluginArtifacts(root, id)
	if errRemaining != nil {
		return paths[0], false, rollback(errRemaining)
	}
	if len(remaining) > 0 {
		return paths[0], false, rollback(fmt.Errorf("plugin artifact appeared during deletion: %s", remaining[0]))
	}
	if errMark := transaction.MarkCommitted(); errMark != nil {
		return paths[0], false, rollback(errMark)
	}
	if errCommit := transaction.Commit(); errCommit != nil {
		return paths[0], false, errCommit
	}
	return paths[0], true, nil
}

func currentPluginFilePath(root string, id string) (string, error) {
	paths, errPaths := pluginFilePaths(root, id)
	if errPaths != nil {
		return "", errPaths
	}
	if len(paths) == 0 {
		return "", nil
	}
	return paths[0], nil
}

func pluginFilePaths(root string, id string) ([]string, error) {
	files, errFiles := pluginFileInfos(root, id)
	if errFiles != nil {
		return nil, errFiles
	}
	out := make([]string, 0, len(files))
	for _, file := range files {
		out = append(out, file.Path)
	}
	return out, nil
}

func pluginFileInfos(root string, id string) ([]pluginFileInfo, error) {
	resolvedRoot, errResolveRoot := resolvePluginRoot(root)
	if errors.Is(errResolveRoot, os.ErrNotExist) {
		return nil, nil
	}
	if errResolveRoot != nil {
		return nil, errResolveRoot
	}
	id = sdkpluginstore.CanonicalPluginID(id)
	platform := CurrentPlatform()
	extension := pluginExtension(platform.GOOS)
	candidates := make([]pluginFileInfo, 0)
	for _, dir := range pluginCandidateDirs(resolvedRoot, platform.GOOS, platform.GOARCH) {
		entries, exists, errReadDir := securePluginCandidateEntries(resolvedRoot, dir)
		if errReadDir != nil {
			return nil, errReadDir
		}
		if !exists {
			continue
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
		for _, filePath := range files {
			file, okFile := pluginFileFromPath(filePath, extension)
			if !okFile || file.ID != id {
				continue
			}
			candidates = append(candidates, file)
		}
	}
	if len(candidates) <= 1 {
		return candidates, nil
	}
	bestIndex := 0
	for index := 1; index < len(candidates); index++ {
		if pluginFilePreferred(candidates[index], candidates[bestIndex]) {
			bestIndex = index
		}
	}
	if bestIndex == 0 {
		return candidates, nil
	}
	out := make([]pluginFileInfo, 0, len(candidates))
	out = append(out, candidates[bestIndex])
	for index, candidate := range candidates {
		if index == bestIndex {
			continue
		}
		out = append(out, candidate)
	}
	return out, nil
}

func resolvePluginRoot(root string) (string, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		root = "plugins"
	}
	absoluteRoot, errAbsolute := filepath.Abs(root)
	if errAbsolute != nil {
		return "", fmt.Errorf("resolve plugin root: %w", errAbsolute)
	}
	resolvedRoot, errResolve := filepath.EvalSymlinks(absoluteRoot)
	if errResolve != nil {
		return "", fmt.Errorf("resolve plugin root links: %w", errResolve)
	}
	return resolvedRoot, nil
}

func securePluginCandidateEntries(root, dir string) ([]os.DirEntry, bool, error) {
	relative, errRelative := filepath.Rel(root, dir)
	if errRelative != nil || filepath.IsAbs(relative) || relative == ".." ||
		strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return nil, false, fmt.Errorf("plugin candidate directory is outside the plugin root: %s", dir)
	}
	current := root
	if relative != "." {
		for _, component := range strings.Split(relative, string(filepath.Separator)) {
			if component == "" || component == "." {
				continue
			}
			current = filepath.Join(current, component)
			info, errInfo := os.Lstat(current)
			if errInfo != nil {
				if errors.Is(errInfo, os.ErrNotExist) {
					return nil, false, nil
				}
				return nil, false, fmt.Errorf("inspect plugin candidate directory: %w", errInfo)
			}
			if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				return nil, false, fmt.Errorf("plugin candidate path is not a regular directory: %s", current)
			}
		}
	}
	entries, errReadDir := os.ReadDir(dir)
	if errReadDir != nil {
		if errors.Is(errReadDir, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, errReadDir
	}
	return entries, true, nil
}

type pluginFileInfo struct {
	ID      string
	Path    string
	Version string
}

func pluginCandidateDirs(root string, goos string, goarch string) []string {
	dirs := make([]string, 0, 2)
	dirs = append(dirs, filepath.Join(root, goos, goarch))
	dirs = append(dirs, root)
	return dirs
}

func pluginIDFromPath(path string) string {
	file, ok := pluginFileFromPath(path, "")
	if ok {
		return file.ID
	}
	base := filepath.Base(path)
	lowerBase := strings.ToLower(base)
	for _, extension := range []string{".so", ".dylib", ".dll"} {
		if strings.HasSuffix(lowerBase, extension) {
			return sdkpluginstore.CanonicalPluginID(base[:len(base)-len(extension)])
		}
	}
	return sdkpluginstore.CanonicalPluginID(base)
}

func pluginFileFromPath(filePath string, requiredExtension string) (pluginFileInfo, bool) {
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
		if validPluginFileID(candidateID) && validPluginFileVersion(candidateVersion) {
			id = candidateID
			version = candidateVersion
		}
	}
	if !validPluginFileID(id) {
		return pluginFileInfo{}, false
	}
	return pluginFileInfo{ID: sdkpluginstore.CanonicalPluginID(id), Path: filePath, Version: version}, true
}

func pluginFilePreferred(candidate pluginFileInfo, current pluginFileInfo) bool {
	if strings.TrimSpace(current.Path) == "" {
		return true
	}
	if candidate.Version == "" {
		return false
	}
	if current.Version == "" {
		return true
	}
	return sdkpluginstore.UpdateAvailable(current.Version, candidate.Version)
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

func validPluginFileID(id string) bool {
	id = strings.TrimSpace(id)
	if id == "" || id == "." || id == ".." || strings.ContainsAny(id, `/\`) {
		return false
	}
	for _, char := range id {
		switch {
		case char >= 'a' && char <= 'z':
		case char >= 'A' && char <= 'Z':
		case char >= '0' && char <= '9':
		case char == '-', char == '_', char == '.':
		default:
			return false
		}
	}
	return true
}

func validPluginFileVersion(version string) bool {
	version = strings.TrimSpace(version)
	if version == "" || strings.HasPrefix(version, "v") {
		return false
	}
	first := version[0]
	return first >= '0' && first <= '9'
}
