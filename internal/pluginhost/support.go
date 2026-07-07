package pluginhost

const SupportPluginHeader = "X-CPA-SUPPORT-PLUGIN"

// SupportPluginHeaderValue reports whether the current binary was built with CGO enabled.
func SupportPluginHeaderValue() string {
	return supportPluginValue
}
