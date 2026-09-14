// Package artifact stores generated image artifacts for authenticated API retrieval.
package artifact

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/proxyutil"
	log "github.com/sirupsen/logrus"
)

const (
	defaultMaxImageBytes            = 50 << 20
	defaultCacheCleanupInterval     = time.Hour
	defaultTempFileRetention        = 24 * time.Hour
	imageIDPrefix                   = "img_"
	remoteImageUserAgent            = "CLIProxyAPI-image-artifact"
	tempFilePrefix                  = ".tmp-"
	imageArtifactCacheMegabyteBytes = 1024 * 1024
)

var (
	errInvalidID       = errors.New("invalid artifact id")
	errIntegrity       = errors.New("artifact content does not match its id")
	errUnsupportedMIME = errors.New("unsupported artifact MIME type")
	errTooLarge        = errors.New("artifact exceeds maximum size")

	artifactRootMutexes sync.Map
)

type integrityError struct {
	info os.FileInfo
}

func (*integrityError) Error() string {
	return errIntegrity.Error()
}

func (*integrityError) Unwrap() error {
	return errIntegrity
}

type Record struct {
	ID       string
	SHA256   string
	MIMEType string
	Size     int64
	ModTime  time.Time
}

type CacheConfig struct {
	Retention       time.Duration
	MaxTotalBytes   int64
	CleanupInterval time.Duration
}

type StoreStats struct {
	TotalBytes int64
	FileCount  int
}

type CleanupResult struct {
	DeletedFiles     int
	DeletedBytes     int64
	DeletedTempFiles int
	Stats            StoreStats
}

type Store struct {
	root     string
	maxBytes int64

	proxyMu  sync.RWMutex
	proxyURL string

	cacheMu         sync.RWMutex
	retention       time.Duration
	maxTotalBytes   int64
	cleanupInterval time.Duration

	filesMu       sync.Mutex
	sharedFilesMu *sync.Mutex

	cleanerMu     sync.Mutex
	cleanerStop   chan struct{}
	cleanerDone   chan struct{}
	cleanerClosed bool
}

type Option func(*Store)

func WithProxyURL(proxyURL string) Option {
	return func(s *Store) {
		s.SetProxyURL(proxyURL)
	}
}

// SetProxyURL updates the immutable proxy snapshot used by subsequent downloads
func (s *Store) SetProxyURL(proxyURL string) {
	if s == nil {
		return
	}
	s.proxyMu.Lock()
	s.proxyURL = strings.TrimSpace(proxyURL)
	s.proxyMu.Unlock()
}

func (s *Store) proxyURLSnapshot() string {
	if s == nil {
		return ""
	}
	s.proxyMu.RLock()
	defer s.proxyMu.RUnlock()
	return s.proxyURL
}

func withMaxBytes(maxBytes int64) Option {
	return func(s *Store) {
		if s != nil && maxBytes > 0 {
			s.maxBytes = maxBytes
		}
	}
}

func Root(configFilePath string) string {
	configFilePath = strings.TrimSpace(configFilePath)
	if configFilePath == "" {
		wd, err := os.Getwd()
		if err != nil {
			return ""
		}
		configFilePath = wd
	}
	namespace := InstanceNamespace(configFilePath)
	if writable := util.WritablePath(); writable != "" {
		return filepath.Join(writable, "artifacts", "instances", namespace, "images")
	}

	base := filepath.Dir(configFilePath)
	if info, err := os.Stat(configFilePath); err == nil && info.IsDir() {
		base = configFilePath
	}
	return filepath.Join(base, "artifacts", "instances", namespace, "images")
}

func InstanceNamespace(configFilePath string) string {
	return artifactInstanceNamespace(configFilePath)
}

func artifactInstanceNamespace(configFilePath string) string {
	identity := filepath.Clean(strings.TrimSpace(configFilePath))
	if absolute, err := filepath.Abs(identity); err == nil {
		identity = absolute
	}
	if runtime.GOOS == "windows" {
		identity = strings.ToLower(identity)
	}
	digest := sha256.Sum256([]byte(identity))
	return hex.EncodeToString(digest[:])
}

func NewStore(root string, opts ...Option) (*Store, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return nil, fmt.Errorf("artifact root is empty")
	}
	root = filepath.Clean(root)
	store := &Store{
		root:          root,
		maxBytes:      defaultMaxImageBytes,
		sharedFilesMu: artifactRootMutex(root),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(store)
		}
	}
	return store, nil
}

func artifactRootMutex(root string) *sync.Mutex {
	key := canonicalArtifactRoot(root)
	mutex, _ := artifactRootMutexes.LoadOrStore(key, &sync.Mutex{})
	return mutex.(*sync.Mutex)
}

func canonicalArtifactRoot(root string) string {
	key := filepath.Clean(root)
	if absolute, err := filepath.Abs(key); err == nil {
		key = absolute
	}
	resolved, err := evalArtifactRootSymlinks(key, 0)
	if err != nil {
		return key
	}
	return resolved
}

func evalArtifactRootSymlinks(path string, depth int) (string, error) {
	if depth > 255 {
		return "", fmt.Errorf("artifact root contains too many symbolic links")
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return filepath.Clean(resolved), nil
	}

	volume := filepath.VolumeName(path)
	rest := strings.TrimPrefix(path, volume)
	current := volume
	if filepath.IsAbs(path) {
		current += string(os.PathSeparator)
		for len(rest) > 0 && os.IsPathSeparator(rest[0]) {
			rest = rest[1:]
		}
	} else if current == "" {
		current = "."
	}
	parts := strings.FieldsFunc(
		rest, func(r rune) bool {
			return r <= 255 && os.IsPathSeparator(uint8(r))
		},
	)
	for i, part := range parts {
		candidate := filepath.Join(current, part)
		info, err := os.Lstat(candidate)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return filepath.Clean(filepath.Join(append([]string{candidate}, parts[i+1:]...)...)), nil
			}
			return "", err
		}
		if info.Mode()&os.ModeSymlink == 0 {
			current = candidate
			continue
		}
		target, err := os.Readlink(candidate)
		if err != nil {
			return "", err
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(current, target)
		}
		if i+1 < len(parts) {
			target = filepath.Join(append([]string{target}, parts[i+1:]...)...)
		}
		return evalArtifactRootSymlinks(filepath.Clean(target), depth+1)
	}
	return filepath.Clean(current), nil
}

func (s *Store) lockFiles() {
	if s.sharedFilesMu != nil {
		s.sharedFilesMu.Lock()
	}
	s.filesMu.Lock()
}

func (s *Store) unlockFiles() {
	s.filesMu.Unlock()
	if s.sharedFilesMu != nil {
		s.sharedFilesMu.Unlock()
	}
}

func CacheConfigFromLimits(retentionDays int, maxTotalSizeMB int) CacheConfig {
	if retentionDays < 0 {
		retentionDays = 0
	}
	if maxTotalSizeMB < 0 {
		maxTotalSizeMB = 0
	}
	return CacheConfig{
		Retention:       time.Duration(retentionDays) * 24 * time.Hour,
		MaxTotalBytes:   int64(maxTotalSizeMB) * imageArtifactCacheMegabyteBytes,
		CleanupInterval: defaultCacheCleanupInterval,
	}
}

func (s *Store) Root() string {
	if s == nil {
		return ""
	}
	return s.root
}

func (s *Store) ApplyCacheConfig(cfg CacheConfig) {
	if s == nil {
		return
	}
	if cfg.Retention < 0 {
		cfg.Retention = 0
	}
	if cfg.MaxTotalBytes < 0 {
		cfg.MaxTotalBytes = 0
	}
	if cfg.CleanupInterval < 0 {
		cfg.CleanupInterval = 0
	}
	s.cacheMu.Lock()
	s.retention = cfg.Retention
	s.maxTotalBytes = cfg.MaxTotalBytes
	s.cleanupInterval = cfg.CleanupInterval
	s.cacheMu.Unlock()
}

func (s *Store) CacheConfig() CacheConfig {
	if s == nil {
		return CacheConfig{}
	}
	s.cacheMu.RLock()
	defer s.cacheMu.RUnlock()
	return CacheConfig{
		Retention:       s.retention,
		MaxTotalBytes:   s.maxTotalBytes,
		CleanupInterval: s.cleanupInterval,
	}
}

func (s *Store) StartCleaner() {
	if s == nil {
		return
	}
	s.cleanerMu.Lock()
	defer s.cleanerMu.Unlock()
	if s.cleanerClosed {
		return
	}
	s.stopCleanerLocked()
	interval := s.CacheConfig().CleanupInterval
	if interval <= 0 {
		return
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	s.cleanerStop = stop
	s.cleanerDone = done
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if _, err := s.Cleanup(context.Background()); err != nil {
					log.WithError(err).Warn("image artifact cache cleanup failed")
				}
			case <-stop:
				return
			}
		}
	}()
}

func (s *Store) StopCleaner() {
	if s == nil {
		return
	}
	s.cleanerMu.Lock()
	defer s.cleanerMu.Unlock()
	s.stopCleanerLocked()
}

func (s *Store) CloseCleaner() {
	if s == nil {
		return
	}
	s.cleanerMu.Lock()
	defer s.cleanerMu.Unlock()
	s.cleanerClosed = true
	s.stopCleanerLocked()
}

func (s *Store) stopCleanerLocked() {
	if s.cleanerStop == nil {
		return
	}
	close(s.cleanerStop)
	if s.cleanerDone != nil {
		<-s.cleanerDone
	}
	s.cleanerStop = nil
	s.cleanerDone = nil
}

func (s *Store) PutDataURL(ctx context.Context, raw string) (Record, error) {
	if s == nil {
		return Record{}, fmt.Errorf("artifact store is unavailable")
	}
	maxBytes := s.maxBytes
	if maxTotalBytes := s.CacheConfig().MaxTotalBytes; maxTotalBytes > 0 && maxTotalBytes < maxBytes {
		maxBytes = maxTotalBytes
	}
	mediaType, data, err := decodeDataURL(raw, maxBytes)
	if err != nil {
		return Record{}, err
	}
	return s.PutBytes(ctx, data, mediaType)
}

func (s *Store) PutRemoteImage(ctx context.Context, rawURL string) (Record, []byte, error) {
	if s == nil {
		return Record{}, nil, fmt.Errorf("artifact store is unavailable")
	}
	if err := proxyutil.ValidatePublicHTTPURL(rawURL); err != nil {
		return Record{}, nil, err
	}

	parsedURL, _ := url.Parse(strings.TrimSpace(rawURL))
	host := parsedURL.Hostname()
	client, errClient := s.remoteHTTPClient()
	if errClient != nil {
		return Record{}, nil, errClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSpace(rawURL), nil)
	if err != nil {
		return Record{}, nil, fmt.Errorf("create image artifact request failed")
	}
	req.Header.Set("Accept", "image/png,image/jpeg,image/webp,image/gif;q=0.9")
	req.Header.Set("User-Agent", remoteImageUserAgent)

	resp, err := client.Do(req)
	if err != nil {
		return Record{}, nil, fmt.Errorf("download image from %s failed", host)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return Record{}, nil, fmt.Errorf("download image from %s failed: status %d", host, resp.StatusCode)
	}
	if resp.ContentLength > s.maxBytes {
		return Record{}, nil, errTooLarge
	}

	declaredMIME, err := declaredImageMIME(resp.Header.Get("Content-Type"))
	if err != nil {
		return Record{}, nil, err
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, s.maxBytes+1))
	if err != nil {
		return Record{}, nil, fmt.Errorf("read remote image failed")
	}
	if int64(len(data)) > s.maxBytes {
		return Record{}, nil, errTooLarge
	}

	record, err := s.PutBytes(ctx, data, declaredMIME)
	if err != nil {
		return Record{}, nil, err
	}
	return record, data, nil
}

func (s *Store) PutBytes(ctx context.Context, data []byte, mediaType string) (Record, error) {
	if s == nil {
		return Record{}, fmt.Errorf("artifact store is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return Record{}, ctx.Err()
	default:
	}
	if len(data) == 0 {
		return Record{}, fmt.Errorf("artifact data is empty")
	}
	if int64(len(data)) > s.maxBytes {
		return Record{}, errTooLarge
	}
	if maxTotalBytes := s.CacheConfig().MaxTotalBytes; maxTotalBytes > 0 && int64(len(data)) > maxTotalBytes {
		return Record{}, errTooLarge
	}

	mediaType, ext, err := normalizeImageMIME(mediaType, data)
	if err != nil {
		return Record{}, err
	}

	hashBytes := sha256.Sum256(data)
	hash := hex.EncodeToString(hashBytes[:])
	id := imageIDPrefix + hash + ext

	s.lockFiles()
	defer s.unlockFiles()

	if err := s.ensureRoot(); err != nil {
		return Record{}, err
	}

	if record, err := s.recordForID(id); err == nil {
		touched := s.touchRecord(record)
		if err = s.enforceMaxTotalBytes(ctx, touched.ID); err != nil {
			return Record{}, err
		}
		return touched, nil
	} else if errors.Is(err, errIntegrity) {
		if errRemove := os.Remove(s.pathForID(id)); errRemove != nil && !errors.Is(errRemove, os.ErrNotExist) {
			return Record{}, errors.Join(err, errRemove)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return Record{}, err
	}

	tmp, err := os.CreateTemp(s.root, ".tmp-"+id+"-")
	if err != nil {
		return Record{}, err
	}
	tmpName := tmp.Name()
	removeTmp := true
	defer func() {
		if removeTmp {
			_ = os.Remove(tmpName)
		}
	}()
	if err = tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return Record{}, err
	}
	if _, err = tmp.Write(data); err != nil {
		_ = tmp.Close()
		return Record{}, err
	}
	if err = tmp.Sync(); err != nil {
		_ = tmp.Close()
		return Record{}, err
	}
	if err = tmp.Close(); err != nil {
		return Record{}, err
	}
	targetPath := s.pathForID(id)
	if err = os.Rename(tmpName, targetPath); err != nil {
		return Record{}, err
	}
	removeTmp = false
	if err = syncArtifactDirectory(s.root); err != nil {
		errRemove := os.Remove(targetPath)
		if errRemove == nil {
			errRemove = syncArtifactDirectory(s.root)
		}
		return Record{}, errors.Join(err, errRemove)
	}

	record, err := s.recordForID(id)
	if err != nil {
		return Record{}, err
	}
	record.MIMEType = mediaType
	if err = s.enforceMaxTotalBytes(ctx, record.ID); err != nil {
		if errRemove := os.Remove(s.pathForID(record.ID)); errRemove != nil && !errors.Is(errRemove, os.ErrNotExist) {
			return Record{}, errors.Join(err, errRemove)
		}
		return Record{}, err
	}
	return record, nil
}

func (s *Store) Open(id string) (*os.File, Record, error) {
	if s == nil {
		return nil, Record{}, fmt.Errorf("artifact store is unavailable")
	}
	id = strings.TrimSpace(id)
	if err := ValidateID(id); err != nil {
		return nil, Record{}, err
	}

	s.lockFiles()
	defer s.unlockFiles()

	file, record, err := s.openExistingRecord(id)
	if err != nil {
		return nil, Record{}, err
	}
	return file, s.touchRecord(record), nil
}

func (s *Store) Stats() (StoreStats, error) {
	if s == nil {
		return StoreStats{}, fmt.Errorf("artifact store is unavailable")
	}

	s.lockFiles()
	defer s.unlockFiles()

	return s.stats()
}

func (s *Store) stats() (StoreStats, error) {
	records, _, err := s.scanRecords()
	if err != nil {
		return StoreStats{}, err
	}
	return statsFromRecords(records), nil
}

func (s *Store) Cleanup(ctx context.Context) (CleanupResult, error) {
	if s == nil {
		return CleanupResult{}, fmt.Errorf("artifact store is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	s.lockFiles()
	defer s.unlockFiles()

	cacheCfg := s.CacheConfig()
	now := time.Now()
	records, tempFiles, err := s.scanRecords()
	if err != nil {
		return CleanupResult{}, err
	}
	result := CleanupResult{Stats: statsFromRecords(records)}

	for _, tempFile := range tempFiles {
		select {
		case <-ctx.Done():
			return result, ctx.Err()
		default:
		}
		if now.Sub(tempFile.ModTime) < defaultTempFileRetention {
			continue
		}
		if err = os.Remove(s.pathForID(tempFile.ID)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return result, err
		}
		result.DeletedTempFiles++
	}

	if cacheCfg.Retention > 0 {
		kept := records[:0]
		for _, record := range records {
			select {
			case <-ctx.Done():
				return result, ctx.Err()
			default:
			}
			if now.Sub(record.ModTime) < cacheCfg.Retention {
				kept = append(kept, record)
				continue
			}
			deleted, errDelete := s.deleteRecord(record)
			if errDelete != nil {
				return result, errDelete
			}
			if deleted {
				result.DeletedFiles++
				result.DeletedBytes += record.Size
				result.Stats.TotalBytes -= record.Size
				result.Stats.FileCount--
			}
		}
		records = kept
	}

	if cacheCfg.MaxTotalBytes > 0 && result.Stats.TotalBytes > cacheCfg.MaxTotalBytes {
		sort.Slice(
			records, func(i int, j int) bool {
				if records[i].ModTime.Equal(records[j].ModTime) {
					return records[i].ID < records[j].ID
				}
				return records[i].ModTime.Before(records[j].ModTime)
			},
		)
		for _, record := range records {
			select {
			case <-ctx.Done():
				return result, ctx.Err()
			default:
			}
			if result.Stats.TotalBytes <= cacheCfg.MaxTotalBytes {
				break
			}
			deleted, errDelete := s.deleteRecord(record)
			if errDelete != nil {
				return result, errDelete
			}
			if deleted {
				result.DeletedFiles++
				result.DeletedBytes += record.Size
				result.Stats.TotalBytes -= record.Size
				result.Stats.FileCount--
			}
		}
	}

	stats, err := s.stats()
	if err != nil {
		return result, err
	}
	result.Stats = stats
	return result, nil
}

func (s *Store) enforceMaxTotalBytes(ctx context.Context, protectedID string) error {
	if s == nil {
		return fmt.Errorf("artifact store is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	maxTotalBytes := s.CacheConfig().MaxTotalBytes
	if maxTotalBytes <= 0 {
		return nil
	}
	records, _, err := s.scanRecords()
	if err != nil {
		return err
	}
	stats := statsFromRecords(records)
	if stats.TotalBytes <= maxTotalBytes {
		return nil
	}
	sort.Slice(
		records, func(i int, j int) bool {
			if records[i].ModTime.Equal(records[j].ModTime) {
				return records[i].ID < records[j].ID
			}
			return records[i].ModTime.Before(records[j].ModTime)
		},
	)
	for _, record := range records {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if record.ID == protectedID {
			continue
		}
		if stats.TotalBytes <= maxTotalBytes {
			break
		}
		deleted, errDelete := s.deleteRecord(record)
		if errDelete != nil {
			return errDelete
		}
		if deleted {
			stats.TotalBytes -= record.Size
			stats.FileCount--
		}
	}
	if stats.TotalBytes > maxTotalBytes {
		return errTooLarge
	}
	return nil
}

func ValidateID(id string) error {
	id = strings.TrimSpace(id)
	if id == "" || filepath.Base(id) != id || strings.ContainsAny(id, `/\\`) || strings.Contains(id, "..") {
		return errInvalidID
	}
	ext := strings.ToLower(filepath.Ext(id))
	mediaType, ok := mimeByExtension(ext)
	if !ok || mediaType == "" {
		return errInvalidID
	}
	hash := strings.TrimSuffix(strings.TrimPrefix(id, imageIDPrefix), ext)
	if !strings.HasPrefix(id, imageIDPrefix) || len(hash) != sha256.Size*2 {
		return errInvalidID
	}
	for _, r := range hash {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return errInvalidID
		}
	}
	return nil
}

func (s *Store) ensureRoot() error {
	if strings.TrimSpace(s.root) == "" {
		return fmt.Errorf("artifact root is empty")
	}
	if err := os.MkdirAll(s.root, 0o700); err != nil {
		return err
	}
	return os.Chmod(s.root, 0o700)
}

func (s *Store) recordForID(id string) (Record, error) {
	file, record, err := s.openExistingRecord(id)
	if err != nil {
		return Record{}, err
	}
	_ = file.Close()
	return record, nil
}

func (s *Store) touchRecord(record Record) Record {
	if record.ID == "" {
		return record
	}
	now := time.Now()
	if err := os.Chtimes(s.pathForID(record.ID), now, now); err != nil {
		return record
	}
	record.ModTime = now
	return record
}

func (s *Store) openExistingRecord(id string) (*os.File, Record, error) {
	path := s.pathForID(id)
	if !pathWithinRoot(s.root, path) {
		return nil, Record{}, errInvalidID
	}
	file, err := openArtifactNoFollow(path)
	if err != nil {
		return nil, Record{}, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, Record{}, err
	}
	if !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, Record{}, errInvalidID
	}
	record, err := recordFromFileInfo(id, info)
	if err != nil {
		_ = file.Close()
		return nil, Record{}, err
	}
	digest := sha256.New()
	if _, err = io.Copy(digest, file); err != nil {
		_ = file.Close()
		return nil, Record{}, err
	}
	if hex.EncodeToString(digest.Sum(nil)) != record.SHA256 {
		_ = file.Close()
		return nil, Record{}, &integrityError{info: info}
	}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		_ = file.Close()
		return nil, Record{}, err
	}
	return file, record, nil
}

func (s *Store) scanRecords() ([]Record, []Record, error) {
	if s == nil {
		return nil, nil, fmt.Errorf("artifact store is unavailable")
	}
	entries, err := os.ReadDir(s.root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	records := make([]Record, 0, len(entries))
	tempFiles := make([]Record, 0)
	for _, entry := range entries {
		name := entry.Name()
		if entry.Type()&os.ModeSymlink != 0 || entry.IsDir() {
			continue
		}
		info, errInfo := entry.Info()
		if errInfo != nil || !info.Mode().IsRegular() {
			continue
		}
		if strings.HasPrefix(name, tempFilePrefix) {
			tempFiles = append(tempFiles, Record{ID: name, Size: info.Size(), ModTime: info.ModTime()})
			continue
		}
		if err = ValidateID(name); err != nil {
			continue
		}
		record, errRecord := recordFromFileInfo(name, info)
		if errRecord != nil {
			continue
		}
		records = append(records, record)
	}
	return records, tempFiles, nil
}

func (s *Store) deleteRecord(record Record) (bool, error) {
	if err := ValidateID(record.ID); err != nil {
		return false, nil
	}
	path := s.pathForID(record.ID)
	if !pathWithinRoot(s.root, path) {
		return false, errInvalidID
	}
	file, _, err := s.openExistingRecord(record.ID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, errInvalidID) {
			return false, nil
		}
		if corrupt, ok := errors.AsType[*integrityError](err); ok {
			removed, errRemove := s.removeCorruptRecord(record.ID, corrupt.info)
			if removed {
				log.WithField("artifact_id", record.ID).Warn("removed corrupt image artifact during cache cleanup")
			}
			return removed, errRemove
		}
		return false, err
	}
	_ = file.Close()
	if err = os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (s *Store) removeCorruptRecord(id string, expected os.FileInfo) (bool, error) {
	path := s.pathForID(id)
	current, errInfo := os.Lstat(path)
	if errors.Is(errInfo, os.ErrNotExist) {
		return false, nil
	}
	if errInfo != nil {
		return false, errInfo
	}
	if current.Mode()&os.ModeSymlink != 0 || !current.Mode().IsRegular() ||
		expected == nil || !os.SameFile(expected, current) {
		return false, nil
	}
	if errRemove := os.Remove(path); errRemove != nil {
		if errors.Is(errRemove, os.ErrNotExist) {
			return false, nil
		}
		return false, errRemove
	}
	if errSync := syncArtifactDirectory(s.root); errSync != nil {
		return true, errSync
	}
	return true, nil
}

func statsFromRecords(records []Record) StoreStats {
	stats := StoreStats{FileCount: len(records)}
	for _, record := range records {
		stats.TotalBytes += record.Size
	}
	return stats
}

func (s *Store) pathForID(id string) string {
	return filepath.Join(s.root, id)
}

func (s *Store) remoteHTTPClient() (*http.Client, error) {
	proxyURL := s.proxyURLSnapshot()
	setting, errParse := proxyutil.Parse(proxyURL)
	if errParse != nil {
		return nil, fmt.Errorf("configure image artifact proxy: %w", errParse)
	}
	var transport http.RoundTripper = proxyutil.NewDirectTransport()
	if setting.Mode == proxyutil.ModeProxy {
		if setting.URL == nil || strings.EqualFold(setting.URL.Scheme, "http") ||
			strings.EqualFold(setting.URL.Scheme, "https") {
			return nil, fmt.Errorf("HTTP proxies are not supported for public image artifact downloads")
		}
		proxyTransport, _, errTransport := proxyutil.BuildHTTPTransport(proxyURL)
		if errTransport != nil {
			return nil, fmt.Errorf("configure image artifact proxy: %w", errTransport)
		}
		transport = proxyTransport
	}
	client := &http.Client{
		Timeout:   30 * time.Second,
		Transport: proxyutil.PublicHostTransport(transport),
	}
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return fmt.Errorf("stopped after 5 redirects")
		}
		if req == nil || req.URL == nil {
			return fmt.Errorf("redirect URL is nil")
		}
		return proxyutil.ValidatePublicHTTPURL(req.URL.String())
	}
	return client, nil
}

func decodeDataURL(raw string, maxBytes int64) (string, []byte, error) {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(strings.ToLower(raw), "data:") {
		return "", nil, fmt.Errorf("invalid data URL")
	}
	header, encoded, ok := strings.Cut(raw[len("data:"):], ",")
	if !ok {
		return "", nil, fmt.Errorf("invalid data URL")
	}
	parts := strings.Split(header, ";")
	mediaType := ""
	if len(parts) > 0 {
		mediaType = strings.TrimSpace(parts[0])
	}
	base64Encoded := false
	for _, part := range parts[1:] {
		if strings.EqualFold(strings.TrimSpace(part), "base64") {
			base64Encoded = true
			break
		}
	}
	if !base64Encoded {
		return "", nil, fmt.Errorf("data URL must be base64 encoded")
	}
	decoder := base64.NewDecoder(base64.StdEncoding, strings.NewReader(strings.TrimSpace(encoded)))
	var reader = decoder
	if maxBytes > 0 {
		reader = io.LimitReader(decoder, maxBytes+1)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		return "", nil, fmt.Errorf("decode data URL failed: %w", err)
	}
	if maxBytes > 0 && int64(len(data)) > maxBytes {
		return "", nil, errTooLarge
	}
	return mediaType, data, nil
}

func declaredImageMIME(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	mediaType, _, err := mime.ParseMediaType(raw)
	if err != nil {
		return "", errUnsupportedMIME
	}
	mediaType = strings.ToLower(strings.TrimSpace(mediaType))
	if _, ok := extensionByMIME(mediaType); ok {
		return mediaType, nil
	}
	switch mediaType {
	case "application/octet-stream", "binary/octet-stream":
		return "", nil
	default:
		return "", errUnsupportedMIME
	}
}

func normalizeImageMIME(declared string, data []byte) (string, string, error) {
	declared, err := declaredImageMIME(declared)
	if err != nil {
		return "", "", err
	}
	sniffed := strings.ToLower(strings.TrimSpace(http.DetectContentType(data)))
	if ext, ok := extensionByMIME(sniffed); ok {
		return sniffed, ext, nil
	}
	if declared != "" {
		return "", "", errUnsupportedMIME
	}
	return "", "", errUnsupportedMIME
}

func extensionByMIME(mediaType string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(mediaType)) {
	case "image/png":
		return ".png", true
	case "image/jpeg":
		return ".jpg", true
	case "image/webp":
		return ".webp", true
	case "image/gif":
		return ".gif", true
	default:
		return "", false
	}
}

func mimeByExtension(ext string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(ext)) {
	case ".png":
		return "image/png", true
	case ".jpg", ".jpeg":
		return "image/jpeg", true
	case ".webp":
		return "image/webp", true
	case ".gif":
		return "image/gif", true
	default:
		return "", false
	}
}

func recordFromFileInfo(id string, info os.FileInfo) (Record, error) {
	if err := ValidateID(id); err != nil {
		return Record{}, err
	}
	ext := strings.ToLower(filepath.Ext(id))
	mediaType, ok := mimeByExtension(ext)
	if !ok {
		return Record{}, errInvalidID
	}
	hash := strings.TrimSuffix(strings.TrimPrefix(id, imageIDPrefix), ext)
	return Record{
		ID:       id,
		SHA256:   hash,
		MIMEType: mediaType,
		Size:     info.Size(),
		ModTime:  info.ModTime(),
	}, nil
}

func pathWithinRoot(root string, path string) bool {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != "" && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != "..")
}
