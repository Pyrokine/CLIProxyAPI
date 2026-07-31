package pluginstore

import "testing"

func TestPluginVersionRejectsFilenameDelimiter(t *testing.T) {
	for _, version := range []string{"1-v2", "1.0.0-rc-v2"} {
		if validPluginVersion(version) {
			t.Fatalf("validPluginVersion(%q) = true", version)
		}
	}
}
