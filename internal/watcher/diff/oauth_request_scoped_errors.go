package diff

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
)

// SummarizeOAuthRequestScopedErrors summarizes request-scoped error rules per OAuth channel.
func SummarizeOAuthRequestScopedErrors(
	entries map[string][]config.RequestScopedErrorRule,
) map[string]modelsSummary {
	if len(entries) == 0 {
		return nil
	}
	out := make(map[string]modelsSummary, len(entries))
	for rawChannel, rules := range entries {
		channel := strings.ToLower(strings.TrimSpace(rawChannel))
		if channel == "" {
			continue
		}
		out[channel] = summarizeOAuthRequestScopedErrorsList(rules)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// DiffOAuthRequestScopedErrorsChanges compares OAuth request-scoped error maps.
func DiffOAuthRequestScopedErrorsChanges(
	oldMap,
	newMap map[string][]config.RequestScopedErrorRule,
) ([]string, []string) {
	return diffSummaryMapChanges(
		SummarizeOAuthRequestScopedErrors(oldMap),
		SummarizeOAuthRequestScopedErrors(newMap),
		"oauth-request-scoped-errors",
	)
}

func summarizeOAuthRequestScopedErrorsList(rules []config.RequestScopedErrorRule) modelsSummary {
	if len(rules) == 0 {
		return modelsSummary{}
	}
	var serialized strings.Builder
	count := 0
	for _, rule := range rules {
		match := requestScopedErrorSummaryValues(rule.Match)
		matchRegexr := requestScopedErrorSummaryValues(rule.MatchRegexr)
		action := strings.TrimSpace(rule.Action)
		if rule.Status <= 0 || (len(match) == 0 && len(matchRegexr) == 0) || action == "" {
			continue
		}
		count++
		_, _ = fmt.Fprintf(
			&serialized,
			"%d|%s|%s|%s\n",
			rule.Status,
			strings.Join(match, ","),
			strings.Join(matchRegexr, ","),
			action,
		)
	}
	if count == 0 {
		return modelsSummary{}
	}
	sum := sha256.Sum256([]byte(serialized.String()))
	return modelsSummary{hash: hex.EncodeToString(sum[:]), count: count}
}

func requestScopedErrorSummaryValues(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			out = append(out, value)
		}
	}
	return out
}
