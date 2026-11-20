package artifact

import (
	"math"
	"testing"
	"time"
)

func TestCacheConfigFromLimitsSaturatesBeforeMultiplication(t *testing.T) {
	for _, days := range []int{106752, 213504, math.MaxInt} {
		cfg := CacheConfigFromLimits(days, math.MaxInt)
		if cfg.Retention != time.Duration(math.MaxInt64/int64(24*time.Hour))*24*time.Hour {
			t.Fatalf("days=%d retention=%v", days, cfg.Retention)
		}
		if cfg.MaxTotalBytes != int64(math.MaxInt64/imageArtifactCacheMegabyteBytes)*imageArtifactCacheMegabyteBytes {
			t.Fatalf("max bytes=%d", cfg.MaxTotalBytes)
		}
	}
	cfg := CacheConfigFromLimits(0, 0)
	if cfg.Retention != 0 || cfg.MaxTotalBytes != 0 {
		t.Fatalf("disabled=%+v", cfg)
	}
}
