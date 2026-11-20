package session

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
)

func TestExplicitLegacyTaskAndAmpHeadersPrecedeDerivedIdentity(t *testing.T) {
	for _, test := range []struct{ header, want string }{
		{header: "X-Task-ID", want: "task:fixture-thread"},
		{header: "X-Task_ID", want: "task:fixture-thread"},
		{header: "X-Amp-Thread-Id", want: "amp:fixture-thread"},
	} {
		t.Run(
			test.header, func(t *testing.T) {
				payload := []byte(
					`{"model":"fixture-model","messages":[{"role":"user","content":"shared prefix"}]}`,
				)
				headers := http.Header{test.header: {"fixture-thread"}}
				metadata := map[string]any{coreexecutor.CallerScopeMetadataKey: "fixture-caller"}
				info, ok := ExtractSessionInfo(headers, payload, metadata)
				if !ok || info.SessionID != test.want {
					t.Fatalf("explicit session=%+v, exists=%t", info, ok)
				}
				req, opts := Enrich(
					coreexecutor.Request{Payload: payload}, coreexecutor.Options{
						Headers: headers, SourceFormat: sdktranslator.FormatOpenAI, Metadata: metadata,
					},
				)
				if req.Metadata[coreexecutor.CanonicalSessionIDMetadataKey] != test.want ||
					opts.Metadata[coreexecutor.CanonicalSessionIDMetadataKey] != test.want {
					t.Fatalf(
						"explicit identity was replaced by a derived identity: request=%v, options=%v",
						req.Metadata,
						opts.Metadata,
					)
				}
			},
		)
	}
}

func TestDerivedSessionIdentityPreservesStructuredNumericInput(t *testing.T) {
	for _, pair := range [][2]string{
		{"2", "3"}, {"9007199254740992", "9007199254740993"}, {"1", "1.0000000000000000001"},
	} {
		left := []byte(fmt.Sprintf(
			`{"messages":[{"role":"user","content":[{"type":"json","value":{"n":%s}}]}]}`, pair[0],
		))
		right := []byte(fmt.Sprintf(
			`{"messages":[{"role":"user","content":[{"type":"json","value":{"n":%s}}]}]}`, pair[1],
		))
		leftID := DeriveID(sdktranslator.FormatOpenAI, left, "fixture-caller")
		if rightID := DeriveID(
			sdktranslator.FormatOpenAI, right, "fixture-caller",
		); leftID == "" || rightID == "" || leftID == rightID {
			t.Errorf(
				"different structured values %s/%s share identity %s/%s",
				pair[0],
				pair[1],
				leftID,
				rightID,
			)
		}
	}
}

func TestDistinctNumericToolArgumentsRemainSessionForks(t *testing.T) {
	for _, protocol := range []struct {
		name   string
		format sdktranslator.Format
		body   string
	}{
		{
			name: "gemini", format: sdktranslator.FormatGemini,
			body: `{"contents":[{"role":"user","parts":[{"text":"fixture root"}]},{"role":"model","parts":[{"` +
				`functionCall":{"name":"lookup","args":{"n":%s}}}]},{"role":"user","parts":[{"` +
				`functionResponse":{"name":"lookup","response":{"text":"ok"}}}]}]}`,
		},
		{
			name: "claude", format: sdktranslator.FormatClaude,
			body: `{"messages":[{"role":"user","content":"fixture root"},{"role":"assistant","content":[{"` +
				`type":"tool_use","id":"fixture-call","name":"lookup","input":{"n":%s}}]},{"role":"user","` +
				`content":[{"type":"tool_result","tool_use_id":"fixture-call","content":"ok"}]}]}`,
		},
	} {
		for _, numbers := range []struct{ name, left, right string }{
			{name: "small integer", left: "2", right: "3"},
			{name: "large integer", left: "9007199254740992", right: "9007199254740993"},
			{name: "precise decimal", left: "1", right: "1.0000000000000000001"},
		} {
			t.Run(
				protocol.name+"/"+numbers.name, func(t *testing.T) {
					left := ExtractCanonicalTurns(
						protocol.format,
						[]byte(fmt.Sprintf(protocol.body, numbers.left)),
					)
					right := ExtractCanonicalTurns(
						protocol.format,
						[]byte(fmt.Sprintf(protocol.body, numbers.right)),
					)
					if len(left) != 3 || len(right) != 3 {
						t.Fatalf("turn count=%d/%d", len(left), len(right))
					}
					matcher := NewMerklePrefixMatcher(time.Hour)
					root := matcher.BindWithResult("fixture-numeric-arguments", left, "fixture-auth")
					match, ok := matcher.Match("fixture-numeric-arguments", right)
					if !ok || !match.IsFork || match.SessionID == root.SessionID ||
						match.ParentSessionID == "" ||
						match.PrefixLength != 1 {
						t.Fatalf(
							"distinct tool arguments merged: match=%+v, exists=%t, root=%+v",
							match,
							ok,
							root,
						)
					}
					if !matcher.Touch("fixture-numeric-arguments", right, "fixture-auth") {
						t.Fatal("failed to bind the selected branch")
					}
					if repeated, exists := matcher.Match(
						"fixture-numeric-arguments", right,
					); !exists ||
						repeated.NodeKind !=
							"fork" ||
						repeated.SessionID !=
							match.SessionID ||
						repeated.ParentSessionID !=
							match.ParentSessionID {
						t.Fatalf(
							"bound branch lost its identity: match=%+v, exists=%t, original=%+v",
							repeated,
							exists,
							match,
						)
					}
				},
			)
		}
	}
}
