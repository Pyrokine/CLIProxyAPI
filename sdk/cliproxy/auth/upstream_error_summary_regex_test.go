package auth

import (
	"reflect"
	"regexp"
	"regexp/syntax"
	"testing"
)

// Reference patterns stay independent of the production expressions.
//
//goland:noinspection DuplicatedCode
func TestUpstreamSummaryPatternsPreserveCapturesAndReplacements(t *testing.T) {
	for _, test := range []struct {
		name        string
		current     *regexp.Regexp
		original    string
		replacement string
	}{
		{
			name:    "secret assignment",
			current: upstreamSummaryAssignment,
			//goland:noinspection RegExpSingleCharAlternation,RegExpUnnecessaryNonCapturingGroup
			original: `(?i)((?:'|")?(?:[A-Za-z0-9_.-]*(?:key|token|secret|password|credential|credentials|` +
				`bearer|sessionid|auth|signature|sig))(?:'|")?\s*[=:]\s*)(?:"(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)` +
				`*'|(?:[^\r\n;,|]+?(?:\s+(?:and|with|for|via)\s+|[,;|]|\r|\n|$)|[^\r\n;,|]+))`,
			replacement: `${1}[REDACTED]`,
		},
		{
			name:    "Unix path",
			current: upstreamSummaryUnixPath,
			//goland:noinspection RegExpRedundantEscape
			original: `(^|[\s\(\[\{<"';,=])(/(?:[^/\s\r\n"',;?#()<>{}\[\]]+(?:\s+[^/\s\r\n"',` +
				`;?#()<>{}\[\]]+)*/)*[^/:\s\r\n"',;?#()<>{}\[\]]+(?::[^/:\s\r\n"',;?#()` +
				`<>{}\[\]]+)?)`,
			replacement: `${1}[REDACTED_PATH]`,
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
				//goland:noinspection SpellCheckingInspection
				for _, input := range []string{
					`api_key=synthetic-value`, `'client_secret'="value,with;separators"`,
					`"ACCESS_TOKEN":'single quoted value' and password=second`,
					`signature="escaped\"quote"; credentials=last`,
					"token=first\nsecret=second\r\nkey=third", `nonsecret=工具`,
					`open (/tmp/customer): denied`, `open {/tmp/customer}: denied`,
					`open [/tmp/customer]: denied`, `open </tmp/customer>: denied`,
					`rename /tmp/Alice Smith /tmp/Bob Jones: denied`,
					`open /tmp/customer:value/credentials.json: permission denied`,
					`stat /客户/凭证: no such file or directory`,
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

func TestExtractUpstreamSummaryRetainsCodeAndPathDelimiters(t *testing.T) {
	for _, test := range []struct{ input, want string }{
		{`open (/tmp/customer): denied`, `open ([REDACTED_PATH]): denied`},
		{`open {/tmp/customer}: denied`, `open {[REDACTED_PATH]}: denied`},
		{
			`{"error":{"code":"invalid_key","message":"api_key=synthetic-value"}}`,
			`invalid_key: [REDACTED]`,
		},
	} {
		if got := ExtractUpstreamErrorSummary(test.input); got != test.want {
			t.Fatalf("summary for %q = %q, want %q", test.input, got, test.want)
		}
	}
}
