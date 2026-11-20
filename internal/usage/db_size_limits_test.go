package usage

import (
	"math"
	"math/big"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
)

func TestUsageDBSizeLimitConversionAndExactWarning(t *testing.T) {
	for _, size := range []int{-1, 0, 1, config.MaxStorageSizeMB, config.MaxStorageSizeMB + 1} {
		got := usageDBSizeLimitBytes(size)
		want := int64(max(0, min(size, config.MaxStorageSizeMB))) << 20
		if got != want || got < 0 {
			t.Fatalf("size=%d bytes=%d want=%d", size, got, want)
		}
		p := NewPersister(t.TempDir(), config.UsageRetention{MaxDBSizeMB: size, WarningThresholdPct: 80})
		if got := p.dbSizeCapLimit(); got != want {
			t.Fatalf("cap=%d want=%d", got, want)
		}
		_, limit, _, warning, capped := p.DBSizeStatus()
		if limit != want || warning || capped {
			t.Fatalf("limit=%d warning=%t capped=%t", limit, warning, capped)
		}
	}
	for _, limit := range []int64{1, 100, 1 << 20, int64(config.MaxStorageSizeMB) << 20, math.MaxInt64} {
		for _, pct := range []int{1, 33, 80, 99, 100, 101} {
			threshold := new(big.Int).Mul(big.NewInt(limit), big.NewInt(int64(pct)))
			threshold.Add(threshold, big.NewInt(99)).Div(threshold, big.NewInt(100))
			if !threshold.IsInt64() {
				continue
			}
			at := threshold.Int64()
			sizes := []int64{max(0, at-1), at}
			if at < math.MaxInt64 {
				sizes = append(sizes, at+1)
			}
			for _, size := range sizes {
				lhs := new(big.Int).Mul(big.NewInt(size), big.NewInt(100))
				rhs := new(big.Int).Mul(big.NewInt(limit), big.NewInt(int64(pct)))
				if got, want := usageDBSizeWarning(size, limit, pct), lhs.Cmp(rhs) >= 0; got != want {
					t.Fatalf("size=%d limit=%d pct=%d got=%t want=%t", size, limit, pct, got, want)
				}
			}
		}
	}
}
