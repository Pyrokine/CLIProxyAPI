//go:build windows

package pluginhost

var processPluginLoadAttempts pluginLoadAttemptRegistry

func pluginReloadRequiresRestart() bool {
	return true
}

func defaultPluginLoadAttemptRegistry() *pluginLoadAttemptRegistry {
	return &processPluginLoadAttempts
}
