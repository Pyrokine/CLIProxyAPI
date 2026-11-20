package util

import (
	"bytes"

	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// EnsureResponsesUsageDetails preserves explicit zero detail counters in Responses JSON and SSE payloads.
func EnsureResponsesUsageDetails(payload []byte) []byte {
	if gjson.ValidBytes(payload) {
		return ensureResponsesUsageDetailsJSON(payload)
	}
	lines := bytes.SplitAfter(payload, []byte("\n"))
	changed := false
	for i, line := range lines {
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		data := line[len("data:"):]
		if !gjson.ValidBytes(data) {
			continue
		}
		updated := ensureResponsesUsageDetailsJSON(data)
		if !bytes.Equal(data, updated) {
			lines[i] = append([]byte("data:"), updated...)
			changed = true
		}
	}
	if !changed {
		return payload
	}
	return bytes.Join(lines, nil)
}

func ensureResponsesUsageDetailsJSON(payload []byte) []byte {
	root := gjson.ParseBytes(payload)
	if root.Get("object").String() == "response.compaction" {
		return payload
	}
	for _, path := range []string{"usage", "response.usage"} {
		if !root.Get(path).IsObject() {
			continue
		}
		for _, detail := range []struct{ bucket, counter string }{
			{"input_tokens_details", "cached_tokens"},
			{"output_tokens_details", "reasoning_tokens"},
		} {
			bucket := path + "." + detail.bucket
			key := bucket + "." + detail.counter
			value := root.Get(key)
			if value.Exists() && value.Type != gjson.Null {
				continue
			}
			var replacement any = 0
			if details := root.Get(bucket); details.Exists() && !details.IsObject() {
				key = bucket
				replacement = map[string]int{detail.counter: 0}
			}
			updated, err := sjson.SetBytes(payload, key, replacement)
			if err != nil {
				log.WithError(err).Warn("could not preserve Responses usage detail counters")
				return payload
			}
			payload = updated
		}
	}
	return payload
}
