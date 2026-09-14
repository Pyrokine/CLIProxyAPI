package pluginhost

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/pluginstore"
	log "github.com/sirupsen/logrus"
	"golang.org/x/sys/cpu"
)

var (
	pluginIDPattern      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	pluginVersionPattern = regexp.MustCompile(`^[0-9][0-9A-Za-z.+-]*$`)
)

type pluginFile struct {
	ID         string
	Path       string
	Version    string
	loadPath   string
	digest     string
	sourceInfo os.FileInfo
}

// pluginFileInfo describes a plugin binary selected by the host discovery rules.
type pluginFileInfo struct {
	ID      string
	Path    string
	Version string
}

// ValidatePluginID reports whether id can be used as a plugin configuration key.
func ValidatePluginID(id string) bool {
	return validPluginID(id)
}

func validPluginID(id string) bool {
	return pluginIDPattern.MatchString(id)
}

func validPluginVersion(version string) bool {
	return version != "" && !strings.HasPrefix(version, "v") && !strings.Contains(version, "-v") &&
		pluginVersionPattern.MatchString(version)
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
			return pluginstore.CanonicalPluginID(base[:len(base)-len(extension)])
		}
	}
	return pluginstore.CanonicalPluginID(base)
}

func pluginFileFromPath(filePath string, requiredExtension string) (pluginFile, bool) {
	base := filepath.Base(filePath)
	lowerBase := strings.ToLower(base)
	extension := strings.TrimSpace(requiredExtension)
	if extension != "" {
		if !strings.HasSuffix(lowerBase, strings.ToLower(extension)) {
			return pluginFile{}, false
		}
	} else {
		for _, candidateExtension := range []string{".so", ".dylib", ".dll"} {
			if strings.HasSuffix(lowerBase, candidateExtension) {
				extension = candidateExtension
				break
			}
		}
		if extension == "" {
			return pluginFile{}, false
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
		return pluginFile{}, false
	}
	return pluginFile{ID: pluginstore.CanonicalPluginID(id), Path: filePath, Version: version}, true
}

func pluginExtension(goos string) string {
	switch goos {
	case "darwin":
		return ".dylib"
	case "windows":
		return ".dll"
	default:
		return ".so"
	}
}

func selectPluginFiles(root string, desiredVersions ...map[string]string) ([]pluginFile, error) {
	selected, _, errSelect := selectPluginFilesWithCandidates(root, desiredVersions...)
	return selected, errSelect
}

func selectPluginFilesWithCandidates(root string, desiredVersions ...map[string]string) (
	[]pluginFile,
	[]pluginFile,
	error,
) {
	root = strings.TrimSpace(root)
	if root == "" {
		root = "plugins"
	}
	resolvedRoot, errResolveRoot := filepath.EvalSymlinks(root)
	if errResolveRoot != nil {
		if errors.Is(errResolveRoot, os.ErrNotExist) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("resolve plugin root: %w", errResolveRoot)
	}
	root = resolvedRoot
	desired := normalizeDesiredPluginVersions(desiredVersions...)
	pendingDeletions, errPending := pluginstore.PendingDeletionPluginIDs(root)
	if errPending != nil {
		return nil, nil, fmt.Errorf("read pending plugin deletions: %w", errPending)
	}
	pendingInstalls, errPending := pluginstore.PendingInstallPluginIDs(root)
	if errPending != nil {
		return nil, nil, fmt.Errorf("read pending plugin installs: %w", errPending)
	}

	candidates := candidateDirs(root, runtime.GOOS, runtime.GOARCH, cpuVariant())
	extension := pluginExtension(runtime.GOOS)
	selectedByID := make(map[string]pluginFile)
	order := make([]string, 0)
	all := make([]pluginFile, 0)
	for _, dir := range candidates {
		entries, exists, errReadDir := securePluginCandidateEntries(root, dir)
		if errReadDir != nil {
			return nil, nil, errReadDir
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
		for _, path := range files {
			file, okFile := pluginFileFromPath(path, extension)
			if !okFile {
				continue
			}
			if _, pendingDelete := pendingDeletions[file.ID]; pendingDelete {
				continue
			}
			if _, pendingInstall := pendingInstalls[file.ID]; pendingInstall {
				continue
			}
			all = append(all, file)
			current, exists := selectedByID[file.ID]
			if !exists {
				selectedByID[file.ID] = file
				order = append(order, file.ID)
				continue
			}
			if pluginFilePreferredForDesired(file, current, desired[file.ID]) {
				selectedByID[file.ID] = file
			}
		}
	}
	selected := make([]pluginFile, 0, len(order))
	for _, id := range order {
		file := selectedByID[id]
		if desiredVersion := desired[id]; desiredVersion != "" && file.Version != desiredVersion {
			continue
		}
		selected = append(selected, file)
	}
	return selected, all, nil
}

func securePluginCandidateEntries(root, dir string) ([]os.DirEntry, bool, error) {
	relative, errRelative := filepath.Rel(root, dir)
	if errRelative != nil || filepath.IsAbs(relative) || relative == ".." ||
		strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return nil, false, fmt.Errorf("plugin candidate directory is outside the plugin root: %s", dir)
	}
	current := root
	if relative != "." {
		for component := range strings.SplitSeq(relative, string(filepath.Separator)) {
			if component == "" || component == "." {
				continue
			}
			current = filepath.Join(current, component)
			info, errInfo := os.Lstat(current)
			if errInfo != nil {
				if errors.Is(errInfo, os.ErrNotExist) {
					return nil, false, nil
				}
				return nil, false, fmt.Errorf("stat plugin candidate directory: %w", errInfo)
			}
			if info.Mode()&os.ModeSymlink != 0 {
				return nil, false, fmt.Errorf("plugin candidate directory is a symlink: %s", current)
			}
			if !info.IsDir() {
				return nil, false, fmt.Errorf("plugin candidate path is not a directory: %s", current)
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

func normalizeDesiredPluginVersions(sources ...map[string]string) map[string]string {
	out := make(map[string]string)
	for _, source := range sources {
		for id, version := range source {
			id = pluginstore.CanonicalPluginID(id)
			version = normalizePluginDesiredVersion(version)
			if id == "" || version == "" {
				continue
			}
			out[id] = version
		}
	}
	return out
}

func pluginFilePreferredForDesired(candidate pluginFile, current pluginFile, desiredVersion string) bool {
	desiredVersion = normalizePluginDesiredVersion(desiredVersion)
	if desiredVersion != "" {
		candidateMatches := candidate.Version == desiredVersion
		currentMatches := current.Version == desiredVersion
		if candidateMatches != currentMatches {
			return candidateMatches
		}
	}
	return pluginFilePreferred(candidate, current)
}

func pluginFilePreferred(candidate pluginFile, current pluginFile) bool {
	if candidate.Version == "" {
		return false
	}
	if current.Version == "" {
		return true
	}
	comparison, comparableVersion := comparePluginVersions(candidate.Version, current.Version)
	if !comparableVersion {
		return candidate.Version > current.Version
	}
	return comparison > 0
}

func comparePluginVersions(a, b string) (int, bool) {
	segmentsA := strings.Split(a, ".")
	segmentsB := strings.Split(b, ".")
	length := max(len(segmentsB), len(segmentsA))
	for index := range length {
		numberA, okA := pluginVersionSegment(segmentsA, index)
		numberB, okB := pluginVersionSegment(segmentsB, index)
		if !okA || !okB {
			return 0, false
		}
		if numberA != numberB {
			if numberA < numberB {
				return -1, true
			}
			return 1, true
		}
	}
	return 0, true
}

func pluginVersionSegment(segments []string, index int) (int64, bool) {
	if index >= len(segments) {
		return 0, true
	}
	number, errParse := strconv.ParseInt(segments[index], 10, 64)
	if errParse != nil || number < 0 {
		return 0, false
	}
	return number, true
}

func cleanupUnselectedPluginFiles(candidates []pluginFile, loaded []pluginFile) error {
	if len(candidates) == 0 || len(loaded) == 0 {
		return nil
	}
	loadedByID := make(map[string]map[string]struct{}, len(loaded))
	for _, file := range loaded {
		if strings.TrimSpace(file.ID) == "" || strings.TrimSpace(file.Path) == "" {
			continue
		}
		paths := loadedByID[file.ID]
		if paths == nil {
			paths = make(map[string]struct{})
			loadedByID[file.ID] = paths
		}
		paths[filepath.Clean(file.Path)] = struct{}{}
	}
	var errs []error
	for _, candidate := range candidates {
		paths := loadedByID[candidate.ID]
		if len(paths) == 0 {
			continue
		}
		if _, selected := paths[filepath.Clean(candidate.Path)]; selected {
			continue
		}
		current, errCurrent := pluginFileSnapshotCurrent(candidate)
		if errors.Is(errCurrent, os.ErrNotExist) {
			continue
		}
		if errCurrent != nil {
			errs = append(errs, errCurrent)
			continue
		}
		if !current {
			continue
		}
		if errRemove := os.Remove(candidate.Path); errRemove != nil && !errors.Is(errRemove, os.ErrNotExist) {
			errs = append(errs, errRemove)
			log.WithError(errRemove).Warnf("pluginhost: failed to remove old plugin file %s", candidate.Path)
			continue
		}
		log.WithFields(
			pluginLogFields(
				candidate.ID, "", candidate.Version, candidate.Path,
			),
		).Info("pluginhost: old plugin file removed")
	}
	return errors.Join(errs...)
}

// DiscoverPluginFiles returns plugin binaries selected by the current host discovery rules.
func DiscoverPluginFiles(root string, desiredVersionsOpt ...map[string]string) ([]pluginFileInfo, error) {
	files, errSelect := selectPluginFiles(root, desiredVersionsOpt...)
	if errSelect != nil {
		return nil, errSelect
	}
	out := make([]pluginFileInfo, 0, len(files))
	for _, file := range files {
		out = append(out, pluginFileInfo{ID: file.ID, Path: file.Path, Version: file.Version})
	}
	return out, nil
}

func candidateDirs(root, goos, goarch, variant string) []string {
	dirs := make([]string, 0, 3)
	if variant != "" {
		dirs = append(dirs, filepath.Join(root, goos, goarch+"-"+variant))
	}
	dirs = append(dirs, filepath.Join(root, goos, goarch))
	dirs = append(dirs, root)
	return dirs
}

func cpuVariant() string {
	if runtime.GOARCH != "amd64" {
		return ""
	}
	if cpu.X86.HasAVX512F && cpu.X86.HasAVX512BW && cpu.X86.HasAVX512CD && cpu.X86.HasAVX512DQ && cpu.X86.HasAVX512VL {
		return "v4"
	}
	if cpu.X86.HasAVX && cpu.X86.HasAVX2 && cpu.X86.HasBMI1 && cpu.X86.HasBMI2 && cpu.X86.HasFMA {
		return "v3"
	}
	if cpu.X86.HasSSE3 && cpu.X86.HasSSSE3 && cpu.X86.HasSSE41 && cpu.X86.HasSSE42 && cpu.X86.HasPOPCNT {
		return "v2"
	}
	return "v1"
}
