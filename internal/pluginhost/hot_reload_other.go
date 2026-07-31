//go:build !windows

package pluginhost

func pluginReloadRequiresRestart() bool {
	return false
}

func defaultPluginLoadAttemptRegistry() *pluginLoadAttemptRegistry {
	return &pluginLoadAttemptRegistry{}
}
