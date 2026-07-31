package modelcatalog

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
)

var storeMu sync.RWMutex

type UpdateOptions struct {
	Backup bool
	Commit func(File) error
}

func Path(configPath string) string {
	if strings.TrimSpace(configPath) == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(configPath), FileName)
}

func Load(configPath string) (File, error) {
	filePath := Path(configPath)
	if filePath == "" {
		return File{}, fmt.Errorf("config file path not set")
	}
	storeMu.RLock()
	defer storeMu.RUnlock()
	return readFileUnlocked(filePath)
}

func LoadOrInit(configPath string, cfg *config.Config) (File, error) {
	filePath := Path(configPath)
	if filePath == "" {
		return File{}, fmt.Errorf("config file path not set")
	}
	storeMu.Lock()
	defer storeMu.Unlock()

	doc, previousData, exists, rewrite, err := loadOrInitializeUnlocked(filePath, cfg)
	if err != nil {
		return File{}, err
	}
	if !exists {
		if err = writeFileUnlocked(filePath, doc); err != nil {
			return File{}, err
		}
		return doc, nil
	}
	if !rewrite {
		return doc, nil
	}
	backupPath, err := writeBackupUnlocked(filePath, previousData)
	if err != nil {
		return File{}, err
	}
	if err = writeFileUnlocked(filePath, doc); err != nil {
		removeBackupUnlocked(backupPath)
		return File{}, err
	}
	return doc, nil
}

func Save(configPath string, doc File) error {
	filePath := Path(configPath)
	if filePath == "" {
		return fmt.Errorf("config file path not set")
	}
	storeMu.Lock()
	defer storeMu.Unlock()
	doc.Sanitize()
	return writeFileUnlocked(filePath, doc)
}

func Update(
	configPath string,
	cfg *config.Config,
	options UpdateOptions,
	mutate func(*File) error,
) (File, string, error) {
	filePath := Path(configPath)
	if filePath == "" {
		return File{}, "", fmt.Errorf("config file path not set")
	}

	storeMu.Lock()
	defer storeMu.Unlock()

	doc, previousData, existed, _, err := loadOrInitializeUnlocked(filePath, cfg)
	if err != nil {
		return File{}, "", err
	}
	if !existed {
		previousData, err = marshalFile(doc)
		if err != nil {
			return File{}, "", err
		}
	}
	if mutate != nil {
		if err = mutate(&doc); err != nil {
			return File{}, "", err
		}
	}
	doc.Sanitize()

	backupPath := ""
	if options.Backup {
		backupPath, err = writeBackupUnlocked(filePath, previousData)
		if err != nil {
			return File{}, "", err
		}
	}

	if err = writeFileUnlocked(filePath, doc); err != nil {
		removeBackupUnlocked(backupPath)
		return File{}, "", err
	}
	if options.Commit == nil {
		return doc, backupPath, nil
	}
	if err = options.Commit(doc); err == nil {
		return doc, backupPath, nil
	}

	commitErr := err
	rollbackErr := restoreFileUnlocked(filePath, previousData, existed)
	removeBackupUnlocked(backupPath)
	if rollbackErr != nil {
		return File{}, "", errors.Join(commitErr, fmt.Errorf("restore model catalog: %w", rollbackErr))
	}
	return File{}, "", commitErr
}

func ReplacePrices(configPath string, prices map[string]Price) (File, error) {
	doc, _, err := Update(
		configPath,
		nil,
		UpdateOptions{},
		func(doc *File) error {
			SetPrices(doc, prices)
			return nil
		},
	)
	return doc, err
}

func SetPrices(doc *File, prices map[string]Price) {
	if doc == nil {
		return
	}
	normalizedPrices := make(map[string]Price, len(prices))
	for name, price := range prices {
		name = normalizeModelName(name)
		if name != "" {
			normalizedPrices[name] = price
		}
	}
	for i := range doc.Models {
		price, ok := normalizedPrices[doc.Models[i].Name]
		if !ok {
			doc.Models[i].Price = nil
			continue
		}
		doc.Models[i].Price = &price
	}
	names := make([]string, 0, len(normalizedPrices))
	for name := range normalizedPrices {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if _, ok := doc.Find(name); ok {
			continue
		}
		model := doc.EnsureModel(name, true)
		if model == nil {
			continue
		}
		price := normalizedPrices[name]
		model.Price = &price
	}
	doc.Sanitize()
}

func Prices(configPath string) (map[string]Price, error) {
	doc, err := Load(configPath)
	if err != nil {
		return nil, err
	}
	out := make(map[string]Price)
	for _, model := range doc.Models {
		if model.Price == nil || normalizeModelName(model.Name) == "" {
			continue
		}
		out[model.Name] = *model.Price
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

func Backup(configPath string) (string, error) {
	filePath := Path(configPath)
	if filePath == "" {
		return "", fmt.Errorf("config file path not set")
	}
	storeMu.Lock()
	defer storeMu.Unlock()
	data, err := os.ReadFile(filePath)
	if err != nil {
		return "", err
	}
	return writeBackupUnlocked(filePath, data)
}

func loadOrInitializeUnlocked(
	filePath string,
	cfg *config.Config,
) (File, []byte, bool, bool, error) {
	data, err := os.ReadFile(filePath)
	if err == nil {
		doc, rewrite, errDecode := decodeFileForLoad(data)
		return doc, data, true, rewrite, errDecode
	}
	if !errors.Is(err, os.ErrNotExist) {
		return File{}, nil, false, false, err
	}

	doc := DefaultFile()
	if err = applyLegacyPrices(filepath.Join(filepath.Dir(filePath), "model-prices.json"), &doc); err != nil {
		return File{}, nil, false, false, err
	}
	applyLegacyConfig(cfg, &doc)
	doc.Sanitize()
	return doc, nil, false, false, nil
}

func readFileUnlocked(filePath string) (File, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return File{}, err
	}
	return decodeFile(data)
}

func decodeFile(data []byte) (File, error) {
	doc, _, err := decodeFileForLoad(data)
	return doc, err
}

func decodeFileForLoad(data []byte) (File, bool, error) {
	var doc File
	if err := json.Unmarshal(data, &doc); err != nil {
		return File{}, false, err
	}
	rewrite := doc.SchemaVersion < SchemaVersion || strings.TrimSpace(doc.DefaultsVersion) == ""
	if !rewrite {
		for i := range doc.Models {
			if len(doc.Models[i].Aliases) > 0 || len(doc.Models[i].ForceMappedAliases) > 0 {
				rewrite = true
				break
			}
		}
	}
	doc.Sanitize()
	return doc, rewrite, nil
}

func marshalFile(doc File) ([]byte, error) {
	doc.Sanitize()
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func writeFileUnlocked(filePath string, doc File) error {
	data, err := marshalFile(doc)
	if err != nil {
		return err
	}
	return writeBytesAtomic(filePath, data)
}

func writeBytesAtomic(filePath string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(filePath), 0o700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(filePath), "."+filepath.Base(filePath)+".tmp-*")
	if err != nil {
		return err
	}
	tempPath := file.Name()
	fileOpen := true
	defer func() {
		if fileOpen {
			_ = file.Close()
		}
		if tempPath != "" {
			_ = os.Remove(tempPath)
		}
	}()
	if err = file.Chmod(0o600); err != nil {
		return err
	}
	written, err := file.Write(data)
	if err != nil {
		return err
	}
	if written != len(data) {
		return io.ErrShortWrite
	}
	if err = file.Sync(); err != nil {
		return err
	}
	if err = file.Close(); err != nil {
		fileOpen = false
		return err
	}
	fileOpen = false
	if err = os.Rename(tempPath, filePath); err != nil {
		if errors.Is(err, syscall.EBUSY) {
			return fmt.Errorf(
				"replace model catalog: %w; mount the config directory instead of a single file",
				err,
			)
		}
		return err
	}
	tempPath = ""
	return syncDirectory(filepath.Dir(filePath))
}

func writeBackupUnlocked(filePath string, data []byte) (string, error) {
	if err := os.MkdirAll(filepath.Dir(filePath), 0o700); err != nil {
		return "", err
	}
	pattern := fmt.Sprintf(
		"%s.%s-*.bak",
		filepath.Base(filePath),
		time.Now().Format("20060102-150405"),
	)
	file, err := os.CreateTemp(filepath.Dir(filePath), pattern)
	if err != nil {
		return "", err
	}
	backupPath := file.Name()
	removeBackup := true
	defer func() {
		_ = file.Close()
		if removeBackup {
			_ = os.Remove(backupPath)
		}
	}()
	if err = file.Chmod(0o600); err != nil {
		return "", err
	}
	written, err := file.Write(data)
	if err != nil {
		return "", err
	}
	if written != len(data) {
		return "", io.ErrShortWrite
	}
	if err = file.Sync(); err != nil {
		return "", err
	}
	if err = file.Close(); err != nil {
		return "", err
	}
	if err = syncDirectory(filepath.Dir(filePath)); err != nil {
		return "", err
	}
	removeBackup = false
	return backupPath, nil
}

func restoreFileUnlocked(filePath string, previousData []byte, existed bool) error {
	if existed {
		return writeBytesAtomic(filePath, previousData)
	}
	if err := os.Remove(filePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncDirectory(filepath.Dir(filePath))
}

func removeBackupUnlocked(backupPath string) {
	if backupPath == "" {
		return
	}
	_ = os.Remove(backupPath)
	_ = syncDirectory(filepath.Dir(backupPath))
}

func syncDirectory(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err = directory.Sync(); err != nil {
		_ = directory.Close()
		return err
	}
	return directory.Close()
}

func applyLegacyPrices(filePath string, doc *File) error {
	if doc == nil || strings.TrimSpace(filePath) == "" {
		return nil
	}
	data, err := os.ReadFile(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read legacy model-prices.json: %w", err)
	}
	var prices map[string]Price
	if err := json.Unmarshal(data, &prices); err != nil {
		return fmt.Errorf("parse legacy model-prices.json: %w", err)
	}
	normalizedPrices := make(map[string]Price, len(prices))
	for name, price := range prices {
		name = normalizeModelName(name)
		if name != "" {
			normalizedPrices[name] = price
		}
	}
	names := make([]string, 0, len(normalizedPrices))
	for name := range normalizedPrices {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		model := doc.EnsureModel(name, true)
		if model == nil {
			continue
		}
		price := normalizedPrices[name]
		model.Price = &price
	}
	return nil
}

func ReplaceOAuthModelAlias(doc *File, aliases map[string][]config.OAuthModelAlias) {
	if doc == nil {
		return
	}
	doc.OAuthModelAlias = nil
	for i := range doc.Models {
		doc.Models[i].Aliases = nil
		doc.Models[i].ForceMappedAliases = nil
	}
	ApplyOAuthModelAlias(doc, aliases)
	doc.Sanitize()
}

func ReplaceModelAliases(doc *File, name string, aliases []string) {
	if doc == nil {
		return
	}
	model, ok := doc.Find(name)
	if !ok {
		return
	}
	key := modelKey(model.Name)
	for channel, entries := range doc.OAuthModelAlias {
		kept := entries[:0]
		for _, entry := range entries {
			if modelKey(entry.Name) != key {
				kept = append(kept, entry)
			}
		}
		if len(kept) == 0 {
			delete(doc.OAuthModelAlias, channel)
			continue
		}
		doc.OAuthModelAlias[channel] = kept
	}
	model.Aliases = append([]string(nil), aliases...)
	model.ForceMappedAliases = nil
	doc.Sanitize()
}

func ReplaceOAuthModelAliasChannel(doc *File, channel string, aliases []config.OAuthModelAlias) {
	if doc == nil {
		return
	}
	deleteOAuthModelAliasChannel(doc, channel)
	ApplyOAuthModelAlias(doc, map[string][]config.OAuthModelAlias{channel: aliases})
	doc.Sanitize()
}

func DeleteOAuthModelAliasChannel(doc *File, channel string) bool {
	if doc == nil {
		return false
	}
	changed := deleteOAuthModelAliasChannel(doc, channel)
	if changed {
		doc.Sanitize()
	}
	return changed
}

func deleteOAuthModelAliasChannel(doc *File, channel string) bool {
	channel = strings.ToLower(strings.TrimSpace(channel))
	if doc == nil || channel == "" {
		return false
	}
	doc.materializeModelAliases()
	changed := false
	if _, ok := doc.OAuthModelAlias[channel]; ok {
		delete(doc.OAuthModelAlias, channel)
		if len(doc.OAuthModelAlias) == 0 {
			doc.OAuthModelAlias = nil
		}
		changed = true
	}
	for i := range doc.Models {
		if !modelMatchesOAuthAliasChannel(doc.Models[i], channel) || len(doc.Models[i].Aliases) == 0 {
			continue
		}
		doc.Models[i].Aliases = nil
		doc.Models[i].ForceMappedAliases = nil
		changed = true
	}
	return changed
}

func modelMatchesOAuthAliasChannel(model Model, channel string) bool {
	channel = strings.ToLower(strings.TrimSpace(channel))
	if channel == "" {
		return false
	}
	modelChannel := strings.ToLower(strings.TrimSpace(model.Channel))
	if modelChannel == channel {
		return true
	}
	for _, aliasChannel := range oauthAliasChannelsForModel(model.Provider, modelChannel) {
		if aliasChannel == channel {
			return true
		}
	}
	return false
}

func ApplyOAuthModelAlias(doc *File, aliases map[string][]config.OAuthModelAlias) bool {
	if doc == nil {
		return false
	}
	normalized := config.SanitizeOAuthModelAliasMap(aliases)
	if len(normalized) == 0 {
		return false
	}
	if doc.OAuthModelAlias == nil {
		doc.OAuthModelAlias = make(map[string][]config.OAuthModelAlias)
	}
	changed := false
	channels := make([]string, 0, len(normalized))
	for channel := range normalized {
		channels = append(channels, channel)
	}
	sort.Strings(channels)
	for _, channel := range channels {
		existingAliases := doc.OAuthModelAlias[channel]
		existingByAlias := make(map[string]struct{}, len(existingAliases))
		for _, existing := range existingAliases {
			existingByAlias[strings.ToLower(existing.Alias)] = struct{}{}
		}
		for _, alias := range normalized[channel] {
			if ensureOAuthAliasModel(doc, channel, alias.Name) {
				changed = true
			}
			aliasKey := strings.ToLower(alias.Alias)
			if _, exists := existingByAlias[aliasKey]; exists {
				continue
			}
			doc.OAuthModelAlias[channel] = append(doc.OAuthModelAlias[channel], alias)
			existingByAlias[aliasKey] = struct{}{}
			changed = true
		}
	}
	if changed {
		doc.Sanitize()
	}
	return changed
}

func ensureOAuthAliasModel(doc *File, channel string, name string) bool {
	name = normalizeModelName(name)
	if doc == nil || name == "" {
		return false
	}
	_, existed := doc.Find(name)
	model := doc.EnsureModel(name, true)
	if model == nil {
		return false
	}
	changed := !existed
	if provider := inferProvider(channel, name); model.Provider == "" || model.Provider == "Other" {
		if model.Provider != provider {
			model.Provider = provider
			changed = true
		}
	}
	if inferredChannel := inferChannel(channel, name); model.Channel == "" || model.Channel == "other" {
		if model.Channel != inferredChannel {
			model.Channel = inferredChannel
			changed = true
		}
	}
	return changed
}

func applyLegacyConfig(cfg *config.Config, doc *File) {
	if cfg == nil || doc == nil {
		return
	}
	ApplyOAuthModelAlias(doc, cfg.OAuthModelAlias)
	for _, entry := range cfg.ModelCatalog.Models {
		name := normalizeModelName(entry.Name)
		if name == "" {
			continue
		}
		model := doc.EnsureModel(name, true)
		if model == nil {
			continue
		}
		if provider := strings.TrimSpace(entry.Provider); provider != "" {
			model.Provider = provider
		}
		if channel := strings.TrimSpace(entry.Channel); channel != "" {
			model.Channel = strings.ToLower(channel)
		}
		if group := strings.TrimSpace(entry.Group); group != "" {
			model.Group = group
		}
		if displayName := strings.TrimSpace(entry.DisplayName); displayName != "" {
			model.DisplayName = displayName
		}
		if entry.Enabled != nil {
			model.Enabled = *entry.Enabled
		}
	}
}
