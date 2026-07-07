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
	id = strings.TrimSpace(id)
	if !validPluginFileID(id) {
		return "", false, fmt.Errorf("invalid plugin id %q", id)
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
	deleted := false
	for _, path := range paths {
		if errRemove := os.Remove(path); errRemove != nil {
			if errors.Is(errRemove, os.ErrNotExist) {
				continue
			}
			return paths[0], deleted, errRemove
		}
		deleted = true
	}
	return paths[0], deleted, nil
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
	root = strings.TrimSpace(root)
	if root == "" {
		root = "plugins"
	}
	id = strings.TrimSpace(id)
	platform := CurrentPlatform()
	extension := pluginExtension(platform.GOOS)
	candidates := make([]pluginFileInfo, 0)
	for _, dir := range pluginCandidateDirs(root, platform.GOOS, platform.GOARCH) {
		entries, errReadDir := os.ReadDir(dir)
		if errReadDir != nil {
			if errors.Is(errReadDir, os.ErrNotExist) {
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
			return base[:len(base)-len(extension)]
		}
	}
	return base
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
	return pluginFileInfo{ID: id, Path: filePath, Version: version}, true
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
