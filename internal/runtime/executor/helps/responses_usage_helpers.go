package helps

import "github.com/Pyrokine/CLIProxyAPI/v6/internal/util"

// EnsureResponsesUsageDetails preserves explicit zero detail counters in Responses JSON and SSE payloads.
func EnsureResponsesUsageDetails(payload []byte) []byte {
	return util.EnsureResponsesUsageDetails(payload)
}
