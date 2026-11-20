package common

import "strings"

const externalToolPrefix = "external_"

func AntigravityToolNameToUpstream(name string) string {
	if isAntigravityIntrinsicToolNameChain(name) {
		return externalToolPrefix + name
	}
	return name
}

func AntigravityUpstreamToolNameToClient(name string) string {
	if strings.HasPrefix(name, externalToolPrefix) && isAntigravityIntrinsicToolNameChain(name) {
		return strings.TrimPrefix(name, externalToolPrefix)
	}
	return name
}

func isAntigravityIntrinsicToolNameChain(name string) bool {
	for strings.HasPrefix(name, externalToolPrefix) {
		name = strings.TrimPrefix(name, externalToolPrefix)
	}
	switch name {
	case "read_file", "write_file", "execute_code":
		return true
	default:
		return false
	}
}
