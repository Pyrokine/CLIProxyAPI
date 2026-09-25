package common

import "strings"

const ExternalToolPrefix = "external_"

func AntigravityToolNameToUpstream(name string) string {
	if isAntigravityIntrinsicToolNameChain(name) {
		return ExternalToolPrefix + name
	}
	return name
}

func AntigravityUpstreamToolNameToClient(name string) string {
	if strings.HasPrefix(name, ExternalToolPrefix) && isAntigravityIntrinsicToolNameChain(name) {
		return strings.TrimPrefix(name, ExternalToolPrefix)
	}
	return name
}

func isAntigravityIntrinsicToolNameChain(name string) bool {
	for strings.HasPrefix(name, ExternalToolPrefix) {
		name = strings.TrimPrefix(name, ExternalToolPrefix)
	}
	switch name {
	case "read_file", "write_file", "execute_code":
		return true
	default:
		return false
	}
}
