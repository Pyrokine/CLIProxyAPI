package config

import (
	"fmt"
	"math"
)

const MaxStorageSizeMB = int(math.MaxInt64 / (1024 * 1024))

func validateStorageSizeConfig(cfg *Config) error {
	if cfg == nil {
		return nil
	}
	if cfg.LogsMaxTotalSizeMB > MaxStorageSizeMB {
		return fmt.Errorf("logs-max-total-size-mb must not exceed %d", MaxStorageSizeMB)
	}
	if cfg.UsageRetention.MaxDBSizeMB > MaxStorageSizeMB {
		return fmt.Errorf("usage-retention.max-db-size-mb must not exceed %d", MaxStorageSizeMB)
	}
	return validateImageArtifactCacheConfig(cfg.ImageArtifactCache)
}
