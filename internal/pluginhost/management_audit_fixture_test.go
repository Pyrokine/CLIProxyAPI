package pluginhost

import "github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"

func NewManagementAuditTestHost(routes []pluginapi.ManagementRoute) *Host {
	return newHostWithRecords(
		capabilityRecord{
			id: "mutation-audit-fixture",
			plugin: pluginapi.Plugin{
				Capabilities: pluginapi.Capabilities{
					ManagementAPI: &managementPluginDouble{routes: routes},
				},
			},
		},
	)
}
