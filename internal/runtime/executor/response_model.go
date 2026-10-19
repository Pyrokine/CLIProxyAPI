package executor

import (
	"context"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking"
	"github.com/tidwall/gjson"
)

const (
	maxResponseModelLength = 128
	modelWarningInterval   = 10 * time.Minute
	modelWarningLimit      = 1024
)

type modelWarningKey struct {
	provider string
	authID   string
	expected string
	served   string
}

var responseModelWarnings = struct {
	sync.Mutex
	last map[modelWarningKey]time.Time
}{last: make(map[modelWarningKey]time.Time)}

func validResponseModel(model string) bool {
	if model == "" || len(model) > maxResponseModelLength {
		return false
	}
	return !strings.ContainsFunc(model, unicode.IsControl)
}

func responseModelEvent(payload []byte, provider string) (string, bool) {
	data := jsonPayload(payload)
	if len(data) == 0 || !gjson.ValidBytes(data) {
		return "", false
	}
	kind := gjson.GetBytes(data, "type").String()
	model := gjson.Result{}
	terminal := false
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "codex":
		switch kind {
		case "response.created", "response.in_progress":
		case "response.completed", "response.incomplete", "response.done":
			terminal = true
		default:
			return "", false
		}
		model = gjson.GetBytes(data, "response.model")
	case "claude":
		switch kind {
		case "message_start":
			model = gjson.GetBytes(data, "message.model")
		case "message_stop":
			return "", true
		case "message":
			model = gjson.GetBytes(data, "model")
			terminal = true
		default:
			model = gjson.GetBytes(data, "message.model")
			if model.Type != gjson.String {
				model = gjson.GetBytes(data, "model")
			}
		}
	case "gemini", "gemini-interactions", "vertex", "aistudio", "antigravity":
		model = gjson.GetBytes(data, "response.modelVersion")
		if model.Type != gjson.String {
			model = gjson.GetBytes(data, "modelVersion")
		}
		if model.Type != gjson.String {
			model = gjson.GetBytes(data, "interaction.model")
		}
		if model.Type != gjson.String {
			model = gjson.GetBytes(data, "model")
		}
		finish := gjson.GetBytes(data, "candidates.0.finishReason")
		if !finish.Exists() {
			finish = gjson.GetBytes(data, "response.candidates.0.finishReason")
		}
		terminal = finish.Exists() && finish.String() != ""
		if !terminal {
			terminal = isInteractionsModelTerminal(data, kind)
		}
	default:
		model = gjson.GetBytes(data, "response.model")
		if model.Type != gjson.String {
			model = gjson.GetBytes(data, "interaction.model")
		}
		if model.Type != gjson.String {
			model = gjson.GetBytes(data, "modelVersion")
		}
		if model.Type != gjson.String {
			model = gjson.GetBytes(data, "message.model")
		}
		if model.Type != gjson.String {
			model = gjson.GetBytes(data, "model")
		}
		terminal = kind == "response.completed" || kind == "response.incomplete" || kind == "response.done" || isInteractionsModelTerminal(data, kind)
		status := gjson.GetBytes(data, "status").String()
		if gjson.GetBytes(data, "object").String() == "chat.completion" ||
			gjson.GetBytes(data, "choices.0.finish_reason").String() != "" ||
			status == "completed" || status == "incomplete" {
			terminal = true
		}
	}
	if model.Type != gjson.String {
		return "", terminal
	}
	served := strings.TrimSpace(model.String())
	if !validResponseModel(served) {
		return "", terminal
	}
	return served, terminal
}

func isInteractionsModelTerminal(data []byte, kind string) bool {
	if kind == "" {
		kind = gjson.GetBytes(data, "event_type").String()
	}
	switch kind {
	case "interaction.completed", "interaction.done", "interaction.failed", "interaction.cancelled":
		return true
	}
	switch gjson.GetBytes(data, "interaction.status").String() {
	case "completed", "incomplete", "cancelled", "failed":
		return true
	}
	return false
}

func normalizedModelName(model string) string {
	return strings.TrimSpace(thinking.ParseSuffix(strings.ToLower(strings.TrimSpace(model))).ModelName)
}

func modelIsSubstituted(expected, served string) bool {
	expected = normalizedModelName(expected)
	served = normalizedModelName(served)
	if expected == "" || served == "" || expected == served {
		return false
	}
	if sameModelAlias(expected, served) {
		return false
	}
	if slash := strings.LastIndexByte(expected, '/'); slash >= 0 {
		expected = expected[slash+1:]
	}
	if slash := strings.LastIndexByte(served, '/'); slash >= 0 {
		served = served[slash+1:]
	}
	return !sameModelAlias(expected, served) && !sameModelAlias(strings.TrimSuffix(expected, "-latest"), strings.TrimSuffix(served, "-latest"))
}

func sameModelAlias(a, b string) bool {
	return a == b || datedModelAlias(a, b) || datedModelAlias(b, a)
}

func datedModelAlias(base, dated string) bool {
	if base == "" || !strings.HasPrefix(dated, base+"-") {
		return false
	}
	suffix := dated[len(base)+1:]
	if len(suffix) == 10 && suffix[4] == '-' && suffix[7] == '-' {
		suffix = suffix[:4] + suffix[5:7] + suffix[8:]
	}
	if len(suffix) != 8 && len(suffix) != 3 {
		return false
	}
	for _, c := range suffix {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func (r *usageReporter) warnModelSubstitution(ctx context.Context, upstreamModel, served string) {
	if r == nil || served == "" {
		return
	}
	if upstreamModel == "" {
		upstreamModel = r.model
	}
	if !modelIsSubstituted(upstreamModel, served) || !modelIsSubstituted(r.model, served) {
		return
	}
	key := modelWarningKey{
		provider: r.provider,
		authID:   r.authID,
		expected: normalizedModelName(upstreamModel),
		served:   normalizedModelName(served),
	}
	responseModelWarnings.Lock()
	now := time.Now()
	if last, ok := responseModelWarnings.last[key]; ok && now.Sub(last) < modelWarningInterval {
		responseModelWarnings.Unlock()
		return
	}
	if len(responseModelWarnings.last) >= modelWarningLimit {
		for saved, last := range responseModelWarnings.last {
			if now.Sub(last) >= modelWarningInterval {
				delete(responseModelWarnings.last, saved)
			}
		}
		if len(responseModelWarnings.last) >= modelWarningLimit {
			clear(responseModelWarnings.last)
		}
	}
	responseModelWarnings.last[key] = now
	responseModelWarnings.Unlock()
	index := r.authIndex
	if index == "" {
		index = "unknown"
	}
	logWithRequestID(ctx).Warnf("%s executor: upstream served model %q for requested model %q (auth_index=%s)", r.provider, served, r.model, index)
}
