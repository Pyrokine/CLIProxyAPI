package cliproxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/home"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/homeplugins"
	sdkpluginstore "github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginstore"
	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
)

const homePluginStatusReportTimeout = 10 * time.Second

type homePluginTaskWork struct {
	cfg      *config.Config
	tasks    []home.PluginTask
	nextTask int
	report   *homeplugins.SyncReport
}

func (s *Service) prepareHomePlugins(ctx context.Context, cfg *config.Config) (
	homeplugins.PreparedSync,
	string,
	bool,
	error,
) {
	if s == nil || cfg == nil || !cfg.Home.Enabled {
		return homeplugins.PreparedSync{}, "", false, nil
	}
	errRecover := s.applyExternalConfigUpdate(
		func() error {
			if !cfg.Plugins.Enabled {
				return nil
			}
			targetRoot, errTarget := config.ResolvePluginsDir(cfg.Plugins.Dir)
			if errTarget != nil {
				return errTarget
			}
			info, errStat := os.Stat(targetRoot)
			if errors.Is(errStat, os.ErrNotExist) || (errStat == nil && !info.IsDir()) {
				return nil
			}
			if errStat != nil {
				return errStat
			}
			pending, errPending := sdkpluginstore.PendingInstallPluginIDs(targetRoot)
			if errPending != nil {
				return errPending
			}
			if len(pending) == 0 {
				return nil
			}
			recoveryCfg := *cfg
			recoveryCfg.Plugins.Configs = nil
			s.cfgMu.RLock()
			published := s.cfg
			s.cfgMu.RUnlock()
			if published != nil {
				publishedRoot, errPublished := config.ResolvePluginsDir(published.Plugins.Dir)
				if errPublished != nil {
					return errPublished
				}
				publishedInfo, errPublishedStat := os.Stat(publishedRoot)
				if errPublishedStat != nil && !errors.Is(errPublishedStat, os.ErrNotExist) {
					return errPublishedStat
				}
				if errPublishedStat == nil && os.SameFile(info, publishedInfo) {
					recoveryCfg.Plugins.Configs = published.Plugins.Configs
				}
			}
			return homeplugins.RecoverPluginTransactions(&recoveryCfg)
		},
	)
	if errRecover != nil {
		return homeplugins.PreparedSync{}, "", false, fmt.Errorf(
			"home plugins: recover published state before preparation: %w", errRecover,
		)
	}
	syncKey := homePluginSyncKey(cfg)
	if syncKey != "" {
		s.homePluginSyncMu.Lock()
		if s.homePluginSyncKey == syncKey {
			s.homePluginSyncMu.Unlock()
			return homeplugins.PreparedSync{}, syncKey, false, nil
		}
		s.homePluginSyncMu.Unlock()
	}
	prepared, errPrepare := homeplugins.PrepareSyncWithReport(ctx, cfg)
	return prepared, syncKey, true, errPrepare
}

func (s *Service) markHomePluginsSynced(syncKey string) {
	if s == nil || strings.TrimSpace(syncKey) == "" {
		return
	}
	s.homePluginSyncMu.Lock()
	s.homePluginSyncKey = syncKey
	s.homePluginSyncMu.Unlock()
}

func (s *Service) reportHomePluginStatusWithClient(
	ctx context.Context,
	cfg *config.Config,
	report homeplugins.SyncReport,
	client *home.Client,
) error {
	if s == nil || cfg == nil {
		return nil
	}
	if client == nil {
		log.Warn("failed to report home plugin status: home client is unavailable")
		return fmt.Errorf("home client is unavailable")
	}
	nodeID := strings.TrimSpace(cfg.Home.NodeID)
	if nodeID == "" {
		log.Warn("failed to report home plugin status: node id is empty")
		return fmt.Errorf("home node id is empty")
	}
	report.NodeID = nodeID
	if report.UpdatedAt.IsZero() {
		report.UpdatedAt = time.Now().UTC()
	}
	raw, errMarshal := json.Marshal(report)
	if errMarshal != nil {
		log.Warnf("failed to marshal home plugin status: %v", errMarshal)
		return errMarshal
	}
	if ctx == nil {
		ctx = context.Background()
	}
	reportCtx, cancel := context.WithTimeout(ctx, homePluginStatusReportTimeout)
	defer cancel()
	if errReport := client.RPushPluginStatus(reportCtx, raw); errReport != nil {
		log.Warnf("failed to report home plugin status: %v", errReport)
		return errReport
	}
	return nil
}

func (s *Service) processHomePluginTasksWithClient(ctx context.Context, cfg *config.Config, client *home.Client) {
	if s == nil || cfg == nil || !cfg.Home.Enabled || client == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	s.homePluginTasksMu.Lock()
	defer s.homePluginTasksMu.Unlock()
	if s.homePluginTasks == nil {
		tasks, errTasks := client.GetPluginTasks(ctx)
		if errTasks != nil {
			log.Warnf("failed to fetch home plugin tasks: %v", errTasks)
			return
		}
		s.homePluginTasks = &homePluginTaskWork{cfg: cfg, tasks: tasks}
	}
	s.finalizeHomePluginTasksWithClient(ctx, client)
}

// finalizeHomePluginTasksWithClient requires homePluginTasksMu.
func (s *Service) finalizeHomePluginTasksWithClient(ctx context.Context, client *home.Client) {
	work := s.homePluginTasks
	if work == nil {
		return
	}
	for work.nextTask < len(work.tasks) {
		if ctx.Err() != nil {
			return
		}
		task := work.tasks[work.nextTask]
		nodeID := strings.TrimSpace(work.cfg.Home.NodeID)
		targetType := strings.TrimSpace(task.TargetNodeType)
		targetID := strings.TrimSpace(task.TargetNodeID)
		if nodeID == "" ||
			(!strings.EqualFold(targetType, "cpa") && targetType != "*") ||
			(targetID != nodeID && targetID != "*") ||
			!strings.EqualFold(strings.TrimSpace(task.Operation), "delete") {
			work.nextTask++
			continue
		}
		if work.report == nil {
			errApply := s.applyExternalConfigUpdate(
				func() error {
					if err := ctx.Err(); err != nil {
						return err
					}
					report := s.processHomePluginDeleteTask(ctx, work.cfg, task)
					work.report = &report
					return nil
				},
			)
			if errApply != nil {
				log.Warnf("failed to sequence home plugin delete task %d for %s: %v", task.ID, task.PluginID, errApply)
				return
			}
			if !work.report.OK && strings.TrimSpace(work.report.Error) != "" {
				log.Warnf(
					"failed to process home plugin delete task %d for %s: %v",
					task.ID,
					task.PluginID,
					work.report.Error,
				)
			}
		}
		if errReport := s.reportHomePluginStatusWithClient(ctx, work.cfg, *work.report, client); errReport != nil {
			return
		}
		work.nextTask++
		work.report = nil
	}
	s.homePluginTasks = nil
}

func (s *Service) runHomePluginTaskReports(ctx context.Context, client *home.Client) {
	for waitForHomeSubscriberRetry(ctx, homeSubscriberRetryBackoff) {
		s.homePluginTasksMu.Lock()
		s.finalizeHomePluginTasksWithClient(ctx, client)
		s.homePluginTasksMu.Unlock()
	}
}

func (s *Service) processHomePluginDeleteTask(
	ctx context.Context,
	cfg *config.Config,
	task home.PluginTask,
) homeplugins.SyncReport {
	return homeplugins.DeleteWithReport(ctx, cfg, s.pluginHost, task.ID, task.PluginID)
}

func homePluginSyncKey(cfg *config.Config) string {
	if cfg == nil || !cfg.Home.Enabled {
		return ""
	}
	hash := sha256.New()
	_, _ = fmt.Fprintf(
		hash,
		"enabled=%t\ndir=%s\nauth-revision=%d\n",
		cfg.Plugins.Enabled,
		strings.TrimSpace(cfg.Plugins.Dir),
		cfg.Plugins.AuthRevision,
	)
	ids := make([]string, 0, len(cfg.Plugins.Configs))
	for id := range cfg.Plugins.Configs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		item := cfg.Plugins.Configs[id]
		enabled := false
		if item.Enabled != nil {
			enabled = *item.Enabled
		}
		_, _ = fmt.Fprintf(hash, "plugin=%s\nenabled=%t\npriority=%d\n", strings.TrimSpace(id), enabled, item.Priority)
		if item.Raw.Kind != 0 {
			raw, errMarshal := yaml.Marshal(&item.Raw)
			if errMarshal == nil {
				_, _ = hash.Write(raw)
			}
		}
		_, _ = hash.Write([]byte{'\n'})
	}
	return hex.EncodeToString(hash.Sum(nil))
}
