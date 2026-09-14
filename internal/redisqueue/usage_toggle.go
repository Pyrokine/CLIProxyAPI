package redisqueue

// SetUsageStatisticsEnabled toggles whether records are enqueued into the
// process-wide compatibility queue.
func SetUsageStatisticsEnabled(enabled bool) {
	defaultQueue.SetUsageStatisticsEnabled(enabled)
}

// usageStatisticsEnabled reports the compatibility queue's usage setting.
func usageStatisticsEnabled() bool {
	return defaultQueue.usageStatisticsOn()
}
