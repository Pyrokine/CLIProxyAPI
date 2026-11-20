package openai

import (
	"errors"
	"reflect"
	"regexp"
	"regexp/syntax"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
)

// Reference patterns stay independent of the production expressions.
//
//goland:noinspection DuplicatedCode
func TestUpstreamErrorPatternsPreserveCapturesAndReplacements(t *testing.T) {
	for _, test := range []struct {
		name        string
		current     *regexp.Regexp
		original    string
		replacement string
	}{
		{
			name:    "sensitive value",
			current: upstreamErrorSensitiveValue,
			//goland:noinspection RegExpUnnecessaryNonCapturingGroup
			original: `(?i)((?:"?(?:api[_-]?key|access[_-]?token|refresh[_-]?token|token|` +
				`authorization|secret|password|passwd)"?)\s*[=:]\s*"?)([^\s"&,;}]+)`,
			replacement: `${1}[REDACTED]`,
		},
		{
			name:    "IP address",
			current: upstreamErrorIP,
			//goland:noinspection RegExpRedundantEscape
			original:    `\b(?:[0-9]{1,3}\.){3}[0-9]{1,3}\b|\[[0-9a-fA-F:.]+\]`,
			replacement: "[REDACTED]",
		},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				original := regexp.MustCompile(test.original)
				before, err := syntax.Parse(original.String(), syntax.Perl)
				if err != nil {
					t.Fatal(err)
				}
				after, err := syntax.Parse(test.current.String(), syntax.Perl)
				if err != nil {
					t.Fatal(err)
				}
				if before.Simplify().String() != after.Simplify().String() ||
					!reflect.DeepEqual(original.SubexpNames(), test.current.SubexpNames()) {
					t.Fatal("pattern syntax or capture numbering changed")
				}
				for _, input := range []string{
					`api_key=synthetic-value`, `"ACCESS-TOKEN" : "synthetic-value"`,
					`refresh_token=first; password=second, passwd=third`,
					`token:; authorization=""; public_key=value`,
					`secret=工具 password=with-space token=with&separator`,
					`private 127.0.0.1 [::1] [fe80::1] public 8.8.8.8 [2001:4860:4860::8888]`,
					`invalid [::1 999.999.999.999 [] [not-an-address]`,
				} {
					if !reflect.DeepEqual(
						original.FindAllStringSubmatchIndex(input, -1),
						test.current.FindAllStringSubmatchIndex(input, -1),
					) {
						t.Fatalf("capture boundaries changed for %q", input)
					}
					want := original.ReplaceAllString(input, test.replacement)
					if got := test.current.ReplaceAllString(input, test.replacement); got != want {
						t.Fatalf("replacement for %q = %q, want %q", input, got, want)
					}
				}
			},
		)
	}
}

func TestSanitizeOpenAIUpstreamErrorRetainsCauseAndPublicAddress(t *testing.T) {
	cause := errors.New(`api_key=synthetic-value peer=[::1] public=8.8.8.8`)
	cleaned := sanitizeOpenAIUpstreamError(&handlers.ErrorMessage{Error: cause})
	if !errors.Is(cleaned.Error, cause) {
		t.Fatal("sanitized error lost its original cause")
	}
	if got, want := cleaned.Error.Error(), `api_key=[REDACTED] peer=[REDACTED] public=8.8.8.8`; got != want {
		t.Fatalf("sanitized message = %q, want %q", got, want)
	}
}
