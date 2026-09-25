package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor/helps"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestPrepareClaudeOAuthToolNamesForUpstream(t *testing.T) {
	body := []byte(`{"tools":[` +
		`{"name":"Bash","input_schema":{"type":"object"}},` +
		`{"name":"glob","input_schema":{"type":"object"}}` +
		`],"tool_choice":{"type":"tool","name":"glob"},"messages":[{"role":"assistant","content":[` +
		`{"type":"tool_use","id":"toolu_01","name":"Bash","input":{}},` +
		`{"type":"tool_use","id":"toolu_02","name":"glob","input":{}},` +
		`{"type":"tool_reference","tool_name":"glob"},` +
		`{"type":"tool_result","tool_use_id":"toolu_02","content":[{"type":"tool_reference","tool_name":"glob"}]}` +
		`]}]}`)

	out, reverseMap := prepareClaudeOAuthToolNamesForUpstream(
		body,
		claudeMCPAliasOptions{callerIdentity: "caller-a"},
	)
	bashAlias := gjson.GetBytes(out, "tools.0.name").String()
	globAlias := gjson.GetBytes(out, "tools.1.name").String()
	if !strings.HasPrefix(bashAlias, "mcp__") || !strings.HasSuffix(bashAlias, "_Bash") {
		t.Fatalf("Bash alias = %q", bashAlias)
	}
	if !strings.HasPrefix(globAlias, "mcp__") || !strings.HasSuffix(globAlias, "_glob") {
		t.Fatalf("glob alias = %q", globAlias)
	}
	for path, want := range map[string]string{
		"tool_choice.name":                         globAlias,
		"messages.0.content.0.name":                bashAlias,
		"messages.0.content.1.name":                globAlias,
		"messages.0.content.2.tool_name":           globAlias,
		"messages.0.content.3.content.0.tool_name": globAlias,
	} {
		if got := gjson.GetBytes(out, path).String(); got != want {
			t.Errorf("%s = %q, want %q", path, got, want)
		}
	}
	if reverseMap[bashAlias] != "Bash" || reverseMap[globAlias] != "glob" {
		t.Fatalf("reverseMap = %v", reverseMap)
	}
}

func TestPrepareClaudeOAuthToolNamesForUpstreamPreservesServerTools(t *testing.T) {
	body := []byte(`{
		"tools":[
			{"type":"advisor_20260301","name":"advisor","input_schema":{"type":"object"}},
			{"type":"agent_toolset_20260401","name":"agent_toolset","input_schema":{"type":"object"}},
			{"type":"custom","name":"caller_tool","input_schema":{"type":"object"}}
		],
		"messages":[{"role":"assistant","content":[
			{"type":"tool_use","id":"srvtoolu_01","name":"advisor","input":{}},
			{"type":"tool_use","id":"srvtoolu_02","name":"agent_toolset","input":{}},
			{"type":"tool_use","id":"toolu_03","name":"caller_tool","input":{}}
		]}]
	}`)

	out, reverseMap := prepareClaudeOAuthToolNamesForUpstream(
		body,
		claudeMCPAliasOptions{callerIdentity: "server-tool-caller"},
	)
	if got := gjson.GetBytes(out, "tools.0.type").String(); got != "advisor_20260301" {
		t.Fatalf("advisor type = %q", got)
	}
	if got := gjson.GetBytes(out, "tools.0.name").String(); got != "advisor" {
		t.Fatalf("advisor name = %q", got)
	}
	if got := gjson.GetBytes(out, "tools.1.type").String(); got != "agent_toolset_20260401" {
		t.Fatalf("agent toolset type = %q", got)
	}
	if got := gjson.GetBytes(out, "tools.1.name").String(); got != "agent_toolset" {
		t.Fatalf("agent toolset name = %q", got)
	}
	callerAlias := gjson.GetBytes(out, "tools.2.name").String()
	if !strings.HasPrefix(callerAlias, "mcp__") || !strings.HasSuffix(callerAlias, "_caller_tool") {
		t.Fatalf("caller tool alias = %q", callerAlias)
	}
	for path, want := range map[string]string{
		"messages.0.content.0.name": "advisor",
		"messages.0.content.1.name": "agent_toolset",
		"messages.0.content.2.name": callerAlias,
	} {
		if got := gjson.GetBytes(out, path).String(); got != want {
			t.Fatalf("%s = %q, want %q", path, got, want)
		}
	}
	if len(reverseMap) != 1 || reverseMap[callerAlias] != "caller_tool" {
		t.Fatalf("reverseMap = %#v", reverseMap)
	}
}

func TestPrepareClaudeOAuthToolNamesForUpstreamScopesAliasesByCaller(t *testing.T) {
	body := []byte(`{"tools":[{"name":"bash","input_schema":{"type":"object"}}]}`)
	first, _ := prepareClaudeOAuthToolNamesForUpstream(body, claudeMCPAliasOptions{callerIdentity: "caller-a"})
	repeated, _ := prepareClaudeOAuthToolNamesForUpstream(body, claudeMCPAliasOptions{callerIdentity: "caller-a"})
	other, _ := prepareClaudeOAuthToolNamesForUpstream(body, claudeMCPAliasOptions{callerIdentity: "caller-b"})
	firstName := gjson.GetBytes(first, "tools.0.name").String()
	if repeatedName := gjson.GetBytes(repeated, "tools.0.name").String(); repeatedName != firstName {
		t.Fatalf("repeated alias = %q, want %q", repeatedName, firstName)
	}
	if otherName := gjson.GetBytes(other, "tools.0.name").String(); otherName == firstName {
		t.Fatalf("other caller alias = %q, want a distinct value", otherName)
	}
}

func TestRestoreClaudeOAuthToolNamesFromResponse(t *testing.T) {
	alias := "mcp__abcdefghijkl__mnopqrstuvwx_glob"
	reverseMap := map[string]string{alias: "glob"}
	response := []byte(`{"content":[` +
		`{"type":"tool_use","id":"toolu_01","name":"Bash","input":{}},` +
		`{"type":"tool_use","id":"toolu_02","name":"` + alias + `","input":{}},` +
		`{"type":"tool_reference","tool_name":"` + alias + `"}` +
		`]}`)

	out, err := restoreClaudeOAuthToolNamesFromResponse(response, reverseMap)
	if err != nil {
		t.Fatalf("restoreClaudeOAuthToolNamesFromResponse() error = %v", err)
	}
	for path, want := range map[string]string{
		"content.0.name":      "Bash",
		"content.1.name":      "glob",
		"content.2.tool_name": "glob",
	} {
		if got := gjson.GetBytes(out, path).String(); got != want {
			t.Errorf("%s = %q, want %q", path, got, want)
		}
	}
}

func TestRestoreClaudeOAuthToolNamesFromStreamLine(t *testing.T) {
	alias := "mcp__abcdefghijkl__mnopqrstuvwx_glob"
	reverseMap := map[string]string{alias: "glob"}

	bashLine := []byte(`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_01","name":"Bash","input":{}}}`)
	got, err := restoreClaudeOAuthToolNamesFromStreamLine(bashLine, reverseMap)
	if err != nil {
		t.Fatalf("restore Bash stream line error = %v", err)
	}
	if !bytes.Contains(got, []byte(`"name":"Bash"`)) {
		t.Fatalf("Bash was not preserved: %s", got)
	}

	globLine := []byte(`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_02","name":"` + alias + `","input":{}}}`)
	got, err = restoreClaudeOAuthToolNamesFromStreamLine(globLine, reverseMap)
	if err != nil {
		t.Fatalf("restore glob stream line error = %v", err)
	}
	if !bytes.Contains(got, []byte(`"name":"glob"`)) {
		t.Fatalf("glob was not restored: %s", got)
	}
}

func TestReverseRemapOAuthToolNamesRecoversMangledAliases(t *testing.T) {
	body := []byte(`{"tools":[{"name":"glob","input_schema":{"type":"object"}},{"name":"read","input_schema":{"type":"object"}}]}`)
	remapped, reverseMap := remapOAuthToolNamesWithOptions(
		body,
		claudeMCPAliasOptions{callerIdentity: "mangled-alias-caller"},
	)
	globAlias := gjson.GetBytes(remapped, "tools.0.name").String()
	readAlias := gjson.GetBytes(remapped, "tools.1.name").String()
	globParts, ok := parseClaudeMCPAlias(globAlias)
	if !ok {
		t.Fatalf("glob alias is invalid: %q", globAlias)
	}
	readParts, ok := parseClaudeMCPAlias(readAlias)
	if !ok {
		t.Fatalf("read alias is invalid: %q", readAlias)
	}

	repeatedAlias := "mcp__" + globParts.server + "__" + globAlias
	mixedAlias := "mcp__" + globParts.server + "__" + globParts.toolID + "_" + readParts.semantic
	response := []byte(fmt.Sprintf(
		`{"content":[
		{"type":"tool_use","id":"toolu_glob","name":%q,"input":{}},
		{"type":"tool_reference","tool_name":%q},
		{"type":"tool_result","tool_use_id":"toolu_read","content":[{"type":"tool_reference","tool_name":%q}]}
	]}`, repeatedAlias, mixedAlias, mixedAlias,
	))

	restored, errReverse := reverseRemapOAuthToolNames(response, reverseMap)
	if errReverse != nil {
		t.Fatalf("reverseRemapOAuthToolNames() error = %v", errReverse)
	}
	if got := gjson.GetBytes(restored, "content.0.name").String(); got != "glob" {
		t.Fatalf("repeated alias restored to %q, want glob", got)
	}
	if got := gjson.GetBytes(restored, "content.1.tool_name").String(); got != "read" {
		t.Fatalf("mixed alias restored to %q, want read", got)
	}
	if got := gjson.GetBytes(restored, "content.2.content.0.tool_name").String(); got != "read" {
		t.Fatalf("nested mixed alias restored to %q, want read", got)
	}

	streamTests := []struct {
		name      string
		block     string
		fieldPath string
		want      string
	}{
		{
			name:      "repeated tool use alias",
			block:     fmt.Sprintf(`{"type":"tool_use","id":"toolu_glob","name":%q,"input":{}}`, repeatedAlias),
			fieldPath: "content_block.name",
			want:      "glob",
		},
		{
			name:      "mixed tool reference alias",
			block:     fmt.Sprintf(`{"type":"tool_reference","tool_name":%q}`, mixedAlias),
			fieldPath: "content_block.tool_name",
			want:      "read",
		},
	}
	for _, test := range streamTests {
		t.Run(
			test.name, func(t *testing.T) {
				line := []byte(`data: {"type":"content_block_start","index":0,"content_block":` + test.block + `}`)
				restoredLine, errStream := reverseRemapOAuthToolNamesFromStreamLine(line, reverseMap)
				if errStream != nil {
					t.Fatalf("reverseRemapOAuthToolNamesFromStreamLine() error = %v", errStream)
				}
				if got := gjson.GetBytes(jsonPayload(restoredLine), test.fieldPath).String(); got != test.want {
					t.Fatalf("restored stream name = %q, want %q", got, test.want)
				}
			},
		)
	}
}

func TestReverseRemapOAuthToolNamesStripsRepeatedServerPrefixes(t *testing.T) {
	body := []byte(`{"tools":[{"name":"read_file"}]}`)
	remapped, reverseMap := remapOAuthToolNamesWithOptions(
		body,
		claudeMCPAliasOptions{callerIdentity: "repeated-prefix-caller"},
	)
	alias := gjson.GetBytes(remapped, "tools.0.name").String()
	parts, ok := parseClaudeMCPAlias(alias)
	if !ok {
		t.Fatalf("generated alias is invalid: %q", alias)
	}
	toolComponent := strings.SplitN(alias, "__", 3)[2]
	for _, repeated := range []string{
		"mcp__" + parts.server + "__" + parts.server + "__" + toolComponent,
		"mcp__" + parts.server + "__" + parts.server + "__" + parts.server + "__" + toolComponent,
		"mcp__" + parts.server + "__" + alias,
		"mcp__" + parts.server + "__mcp__" + parts.server + "__" + alias,
	} {
		t.Run(repeated, func(t *testing.T) {
			response := []byte(fmt.Sprintf(
				`{"content":[{"type":"tool_use","id":"toolu_1","name":%q,"input":{}}]}`,
				repeated,
			))
			restored, errRestore := reverseRemapOAuthToolNames(response, reverseMap)
			if errRestore != nil {
				t.Fatal(errRestore)
			}
			if got := gjson.GetBytes(restored, "content.0.name").String(); got != "read_file" {
				t.Fatalf("restored name = %q, want read_file", got)
			}
		})
	}
}

func TestReverseRemapOAuthToolNamesRejectsUnsafeMangledAliases(t *testing.T) {
	body := []byte(`{"tools":[{"name":"tool.name"},{"name":"tool/name"}]}`)
	remapped, reverseMap := remapOAuthToolNamesWithOptions(
		body,
		claudeMCPAliasOptions{callerIdentity: "ambiguous-alias-caller"},
	)
	firstAlias := gjson.GetBytes(remapped, "tools.0.name").String()
	secondAlias := gjson.GetBytes(remapped, "tools.1.name").String()
	firstParts, ok := parseClaudeMCPAlias(firstAlias)
	if !ok {
		t.Fatalf("first alias is invalid: %q", firstAlias)
	}
	secondParts, ok := parseClaudeMCPAlias(secondAlias)
	if !ok {
		t.Fatalf("second alias is invalid: %q", secondAlias)
	}
	if firstParts.semantic != secondParts.semantic {
		t.Fatalf("semantic suffixes differ: %q != %q", firstParts.semantic, secondParts.semantic)
	}

	unknownToolID := "aaaaaaaaaaaa"
	if unknownToolID == firstParts.toolID || unknownToolID == secondParts.toolID {
		unknownToolID = "bbbbbbbbbbbb"
	}
	tests := []struct {
		name      string
		alias     string
		wantError string
	}{
		{
			name:      "ambiguous semantic suffix",
			alias:     "mcp__" + firstParts.server + "__" + unknownToolID + "_" + firstParts.semantic,
			wantError: "semantic suffix matches multiple declared tools",
		},
		{
			name:      "ambiguous semantic suffix with malformed tool ID",
			alias:     "mcp__" + firstParts.server + "__" + unknownToolID[:len(unknownToolID)-1] + "_" + firstParts.semantic,
			wantError: "semantic suffix matches multiple declared tools",
		},
		{
			name:      "unrecoverable semantic suffix",
			alias:     "mcp__" + firstParts.server + "__" + unknownToolID + "_missing_tool",
			wantError: "no unique request-local match",
		},
	}
	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				response := []byte(fmt.Sprintf(
					`{"content":[{"type":"tool_use","id":"toolu_1","name":%q,"input":{}}]}`,
					test.alias,
				))
				_, errReverse := reverseRemapOAuthToolNames(response, reverseMap)
				if errReverse == nil || !strings.Contains(errReverse.Error(), test.wantError) {
					t.Fatalf("reverseRemapOAuthToolNames() error = %v, want %q", errReverse, test.wantError)
				}
				var requestScoped cliproxyexecutor.RequestScopedError
				if !errors.As(errReverse, &requestScoped) || requestScoped == nil || !requestScoped.IsRequestScoped() {
					t.Fatalf("reverseRemapOAuthToolNames() error = %T %v, want request-scoped", errReverse, errReverse)
				}

				line := []byte(fmt.Sprintf(
					`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":%q,"input":{}}}`,
					test.alias,
				))
				_, errStream := reverseRemapOAuthToolNamesFromStreamLine(line, reverseMap)
				if errStream == nil || !strings.Contains(errStream.Error(), test.wantError) {
					t.Fatalf(
						"reverseRemapOAuthToolNamesFromStreamLine() error = %v, want %q", errStream, test.wantError,
					)
				}
				requestScoped = nil
				if !errors.As(errStream, &requestScoped) || requestScoped == nil || !requestScoped.IsRequestScoped() {
					t.Fatalf(
						"reverseRemapOAuthToolNamesFromStreamLine() error = %T %v, want request-scoped",
						errStream,
						errStream,
					)
				}
			},
		)
	}
}

func TestConsumeClaudeStreamReportsUnsafeMangledAlias(t *testing.T) {
	body := []byte(`{"tools":[{"name":"tool.name"},{"name":"tool/name"}]}`)
	remapped, reverseMap := remapOAuthToolNamesWithOptions(
		body,
		claudeMCPAliasOptions{callerIdentity: "ambiguous-stream-caller"},
	)
	firstParts, ok := parseClaudeMCPAlias(gjson.GetBytes(remapped, "tools.0.name").String())
	if !ok {
		t.Fatal("first generated alias is invalid")
	}
	unknownToolID := "aaaaaaaaaaaa"
	if unknownToolID == firstParts.toolID {
		unknownToolID = "bbbbbbbbbbbb"
	}
	ambiguousAlias := "mcp__" + firstParts.server + "__" + unknownToolID + "_" + firstParts.semantic
	stream := strings.Join(
		[]string{
			`data: {"type":"message_start","message":{"id":"msg_1","model":"claude-opus-4-7"}}`,
			fmt.Sprintf(
				`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":%q,"input":{}}}`,
				ambiguousAlias,
			),
		}, "\n",
	)
	out := make(chan cliproxyexecutor.StreamChunk, 4)
	consumeClaudeStream(
		context.Background(),
		nil,
		strings.NewReader(stream),
		out,
		nil,
		true,
		false,
		reverseMap,
		false,
		http.StatusOK,
		func(line []byte) [][]byte { return [][]byte{line} },
		nil,
	)

	var streamErr error
	for len(out) > 0 {
		chunk := <-out
		if chunk.Err != nil {
			streamErr = chunk.Err
		}
	}
	if streamErr == nil || !strings.Contains(streamErr.Error(), "semantic suffix matches multiple declared tools") {
		t.Fatalf("stream error = %v, want ambiguous alias diagnostic", streamErr)
	}
}

func TestReverseRemapOAuthToolNamesPreservesUnrelatedMCPName(t *testing.T) {
	body := []byte(`{"tools":[{"name":"glob"}]}`)
	_, reverseMap := remapOAuthToolNamesWithOptions(
		body,
		claudeMCPAliasOptions{callerIdentity: "unrelated-mcp-caller"},
	)
	response := []byte(`{"content":[{"type":"tool_use","id":"toolu_1","name":"mcp__external__query","input":{}}]}`)

	restored, errReverse := reverseRemapOAuthToolNames(response, reverseMap)
	if errReverse != nil {
		t.Fatalf("reverseRemapOAuthToolNames() error = %v", errReverse)
	}
	if got := gjson.GetBytes(restored, "content.0.name").String(); got != "mcp__external__query" {
		t.Fatalf("unrelated MCP name = %q, want unchanged", got)
	}
}

func TestReverseRemapOAuthToolNamesMarksMalformedAliasRequestScoped(t *testing.T) {
	const alias = "mcp__hmzqrngkulqv__xuo7jlxlpzee_clear_thinking"
	malformedAlias := alias + "</parameter>\n<parameter name=\"merge\""
	reverseMap := map[string]string{alias: "clear_thinking"}
	response := []byte(fmt.Sprintf(
		`{"content":[{"type":"tool_use","id":"toolu_1","name":%q,"input":{}}]}`,
		malformedAlias,
	))

	restored, errReverse := reverseRemapOAuthToolNames(response, reverseMap)
	if errReverse == nil {
		t.Fatal("reverseRemapOAuthToolNames() error = nil, want fail-closed alias error")
	}
	if !bytes.Equal(restored, response) {
		t.Fatalf("reverseRemapOAuthToolNames() returned modified response: %s", restored)
	}
	var requestScoped cliproxyexecutor.RequestScopedError
	if !errors.As(errReverse, &requestScoped) || requestScoped == nil || !requestScoped.IsRequestScoped() {
		t.Fatalf("reverseRemapOAuthToolNames() error = %T %v, want request-scoped", errReverse, errReverse)
	}

	line := []byte(fmt.Sprintf(
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":%q,"input":{}}}`,
		malformedAlias,
	))
	restoredLine, errStream := reverseRemapOAuthToolNamesFromStreamLine(line, reverseMap)
	if errStream == nil {
		t.Fatal("reverseRemapOAuthToolNamesFromStreamLine() error = nil, want fail-closed alias error")
	}
	if !bytes.Equal(restoredLine, line) {
		t.Fatalf("reverseRemapOAuthToolNamesFromStreamLine() returned modified line: %s", restoredLine)
	}
	requestScoped = nil
	if !errors.As(errStream, &requestScoped) || requestScoped == nil || !requestScoped.IsRequestScoped() {
		t.Fatalf("reverseRemapOAuthToolNamesFromStreamLine() error = %T %v, want request-scoped", errStream, errStream)
	}
}

func TestClaudeOAuthToolSearchReferencesRoundTrip(t *testing.T) {
	body := []byte(`{
		"tools":[
			{"name":"task_list","input_schema":{"type":"object"}},
			{"name":"fetch_url","input_schema":{"type":"object"}}
		],
		"messages":[{"role":"assistant","content":[{
			"type":"tool_search_tool_result",
			"tool_use_id":"srvtoolu_01",
			"content":{"type":"tool_search_tool_search_result","tool_references":[
				{"type":"tool_reference","tool_name":"task_list"},
				{"type":"tool_reference","tool_name":"fetch_url"},
				{"type":"tool_reference","tool_name":"mcp__external__query"}
			]}
		}]}]
	}`)
	remapped, reverseMap := prepareClaudeOAuthToolNamesForUpstream(
		body,
		claudeMCPAliasOptions{callerIdentity: "tool-search-caller"},
	)
	taskAlias := gjson.GetBytes(remapped, "tools.0.name").String()
	fetchAlias := gjson.GetBytes(remapped, "tools.1.name").String()
	if taskAlias == "task_list" || fetchAlias == "fetch_url" {
		t.Fatalf("tools were not aliased: task=%q fetch=%q", taskAlias, fetchAlias)
	}
	for path, want := range map[string]string{
		"messages.0.content.0.content.tool_references.0.tool_name": taskAlias,
		"messages.0.content.0.content.tool_references.1.tool_name": fetchAlias,
		"messages.0.content.0.content.tool_references.2.tool_name": "mcp__external__query",
	} {
		if got := gjson.GetBytes(remapped, path).String(); got != want {
			t.Fatalf("%s = %q, want %q", path, got, want)
		}
	}

	response := []byte(fmt.Sprintf(`{
		"content":[{
			"type":"tool_search_tool_result",
			"tool_use_id":"srvtoolu_01",
			"content":{"type":"tool_search_tool_search_result","tool_references":[
				{"type":"tool_reference","tool_name":%q},
				{"type":"tool_reference","tool_name":%q},
				{"type":"tool_reference","tool_name":"mcp__external__query"}
			]}
		}]
	}`, taskAlias, fetchAlias))
	restored, errRestore := restoreClaudeOAuthToolNamesFromResponse(response, reverseMap)
	if errRestore != nil {
		t.Fatal(errRestore)
	}
	for path, want := range map[string]string{
		"content.0.content.tool_references.0.tool_name": "task_list",
		"content.0.content.tool_references.1.tool_name": "fetch_url",
		"content.0.content.tool_references.2.tool_name": "mcp__external__query",
	} {
		if got := gjson.GetBytes(restored, path).String(); got != want {
			t.Fatalf("%s = %q, want %q", path, got, want)
		}
	}

	line := []byte(fmt.Sprintf(
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_search_tool_result","tool_use_id":"srvtoolu_01","content":{"type":"tool_search_tool_search_result","tool_references":[{"type":"tool_reference","tool_name":%q},{"type":"tool_reference","tool_name":%q},{"type":"tool_reference","tool_name":"mcp__external__query"}]}}}`,
		taskAlias,
		fetchAlias,
	))
	restoredLine, errStream := restoreClaudeOAuthToolNamesFromStreamLine(line, reverseMap)
	if errStream != nil {
		t.Fatal(errStream)
	}
	streamPayload := jsonPayload(restoredLine)
	for path, want := range map[string]string{
		"content_block.content.tool_references.0.tool_name": "task_list",
		"content_block.content.tool_references.1.tool_name": "fetch_url",
		"content_block.content.tool_references.2.tool_name": "mcp__external__query",
	} {
		if got := gjson.GetBytes(streamPayload, path).String(); got != want {
			t.Fatalf("stream %s = %q, want %q", path, got, want)
		}
	}

	errorResponse := []byte(`{"content":[{"type":"tool_search_tool_result","tool_use_id":"srvtoolu_02","content":{"type":"tool_search_tool_result_error","error_code":"regex_compilation_failed"}}]}`)
	restoredError, errRestore := restoreClaudeOAuthToolNamesFromResponse(errorResponse, reverseMap)
	if errRestore != nil {
		t.Fatal(errRestore)
	}
	if !bytes.Equal(restoredError, errorResponse) {
		t.Fatalf("tool search error response changed: %s", restoredError)
	}
}

func TestReverseRemapOAuthToolNamesRecoversBIP39WordDrift(t *testing.T) {
	body := []byte(`{"tools":[{"name":"read_file"}]}`)
	remapped, reverseMap := remapOAuthToolNamesWithOptions(
		body,
		claudeMCPAliasOptions{callerIdentity: "word-drift-caller"},
	)
	alias := gjson.GetBytes(remapped, "tools.0.name").String()
	parts, ok := parseClaudeMCPAlias(alias)
	if !ok {
		t.Fatalf("generated alias is invalid: %q", alias)
	}

	driftedNames := []string{
		"mcp__" + parts.server + "__abandon_" + parts.semantic,
		"mcp__" + parts.server + "__" + parts.toolID + "_" + parts.toolID + "_" + parts.semantic,
		"mcp__" + parts.server + "__abandon_" + parts.toolID + "_" + parts.semantic,
	}
	for _, drifted := range driftedNames {
		response := []byte(fmt.Sprintf(
			`{"content":[{"type":"tool_use","id":"toolu_1","name":%q,"input":{}}]}`,
			drifted,
		))
		restored, err := reverseRemapOAuthToolNames(response, reverseMap)
		if err != nil {
			t.Fatalf("restore %q: %v", drifted, err)
		}
		if got := gjson.GetBytes(restored, "content.0.name").String(); got != "read_file" {
			t.Fatalf("restore %q = %q, want read_file", drifted, got)
		}
	}
}

func TestReverseRemapOAuthToolNamesChoosesLongestSemanticSuffix(t *testing.T) {
	body := []byte(`{"tools":[{"name":"file"},{"name":"read_file"}]}`)
	remapped, reverseMap := remapOAuthToolNamesWithOptions(
		body,
		claudeMCPAliasOptions{callerIdentity: "overlapping-caller"},
	)
	readAlias := gjson.GetBytes(remapped, "tools.1.name").String()
	parts, ok := parseClaudeMCPAlias(readAlias)
	if !ok {
		t.Fatalf("generated alias is invalid: %q", readAlias)
	}
	drifted := "mcp__" + parts.server + "__" + parts.toolID + "_" + parts.toolID + "_read_file"
	response := []byte(fmt.Sprintf(
		`{"content":[{"type":"tool_use","id":"toolu_1","name":%q,"input":{}}]}`,
		drifted,
	))
	restored, err := reverseRemapOAuthToolNames(response, reverseMap)
	if err != nil {
		t.Fatal(err)
	}
	if got := gjson.GetBytes(restored, "content.0.name").String(); got != "read_file" {
		t.Fatalf("restored tool name = %q, want read_file", got)
	}
}

func TestReverseRemapPassesThroughCallerMCPToolsOnVirtualServerCollision(t *testing.T) {
	const caller = "virtual-server-collision"
	server := strings.Split(helps.ClaudeMCPToolAlias(caller, "probe", 0), "__")[1]
	native := []string{
		"mcp__" + server + "__read_file",
		"mcp__" + server + "__grep_read_file",
		"mcp__" + server + "__write_file",
	}
	body := []byte(fmt.Sprintf(
		`{"tools":[{"name":"read_file"},{"name":%q},{"name":%q},{"name":%q}]}`,
		native[0],
		native[1],
		native[2],
	))
	upstream, reverseMap := remapOAuthToolNamesWithOptions(
		body,
		claudeMCPAliasOptions{callerIdentity: caller},
	)
	alias := gjson.GetBytes(upstream, "tools.0.name").String()
	if alias == "read_file" {
		t.Fatal("read_file was not aliased")
	}
	for index, name := range native {
		if got := gjson.GetBytes(upstream, fmt.Sprintf("tools.%d.name", index+1)).String(); got != name {
			t.Fatalf("caller MCP tool %d = %q, want %q", index, got, name)
		}
		if reverseMap[name] != name {
			t.Fatalf("caller MCP reverse entry %q = %q, want identity", name, reverseMap[name])
		}
		response := []byte(fmt.Sprintf(
			`{"content":[{"type":"tool_use","id":"toolu_1","name":%q,"input":{}}]}`,
			name,
		))
		restored, err := reverseRemapOAuthToolNames(response, reverseMap)
		if err != nil {
			t.Fatalf("restore caller MCP tool %q: %v", name, err)
		}
		if got := gjson.GetBytes(restored, "content.0.name").String(); got != name {
			t.Fatalf("caller MCP tool %q restored as %q", name, got)
		}
		line := []byte(fmt.Sprintf(
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":%q,"input":{}}}`,
			name,
		))
		restoredLine, errStream := reverseRemapOAuthToolNamesFromStreamLine(line, reverseMap)
		if errStream != nil {
			t.Fatalf("restore caller MCP stream tool %q: %v", name, errStream)
		}
		if !bytes.Equal(restoredLine, line) {
			t.Fatalf("caller MCP stream tool %q was modified: %s", name, restoredLine)
		}
	}

	aliasParts, ok := parseClaudeMCPAlias(alias)
	if !ok {
		t.Fatalf("generated alias is invalid: %q", alias)
	}
	for _, drifted := range []string{
		alias,
		"mcp__" + server + "__" + server + "__" + strings.SplitN(alias, "__", 3)[2],
		"mcp__" + server + "__abandon_" + aliasParts.semantic,
	} {
		response := []byte(fmt.Sprintf(
			`{"content":[{"type":"tool_use","id":"toolu_1","name":%q,"input":{}}]}`,
			drifted,
		))
		restored, err := reverseRemapOAuthToolNames(response, reverseMap)
		if err != nil {
			t.Fatalf("restore proxied alias %q: %v", drifted, err)
		}
		if got := gjson.GetBytes(restored, "content.0.name").String(); got != "read_file" {
			t.Fatalf("proxied alias %q restored as %q", drifted, got)
		}
	}
}

func TestRemapKeepsReverseMapEmptyWhenOnlyCallerMCPToolsArePresent(t *testing.T) {
	body := []byte(`{"tools":[{"name":"mcp__context7__query-docs"},{"type":"web_search_20250305","name":"web_search"}]}`)
	out, reverseMap := remapOAuthToolNamesWithOptions(
		body,
		claudeMCPAliasOptions{callerIdentity: "no-proxied-tools"},
	)
	if len(reverseMap) != 0 {
		t.Fatalf("reverseMap = %v, want empty", reverseMap)
	}
	if !bytes.Equal(out, body) {
		t.Fatalf("body = %s, want unchanged %s", out, body)
	}
}

func TestRemapOAuthToolNamesForwardsOriginalAfterAliasSpaceExhaustion(t *testing.T) {
	const caller = "alias-space-exhaustion"
	const original = "tool.name"
	var body strings.Builder
	body.WriteString(`{"tools":[`)
	for attempt := 0; attempt < helps.ClaudeMCPAliasWordCount(); attempt++ {
		if attempt > 0 {
			body.WriteByte(',')
		}
		fmt.Fprintf(&body, `{"name":%q}`, helps.ClaudeMCPToolAlias(caller, original, uint32(attempt)))
	}
	fmt.Fprintf(&body, `,{"name":%q}]}`, original)

	out, reverseMap := remapOAuthToolNamesWithOptions(
		[]byte(body.String()),
		claudeMCPAliasOptions{callerIdentity: caller},
	)
	if got := gjson.GetBytes(out, fmt.Sprintf("tools.%d.name", helps.ClaudeMCPAliasWordCount())).String(); got != original {
		t.Fatalf("exhausted tool name = %q, want %q", got, original)
	}
	if len(reverseMap) != 0 {
		t.Fatalf("reverseMap = %v, want empty after allocation exhaustion", reverseMap)
	}
}

func TestClaudeExecutorExecuteOpenAINonStreamRestoresOAuthToolNames(t *testing.T) {
	upstreamBody := func(toolName string) string {
		return strings.Join(
			[]string{
				`event: message_start`,
				`data: {"type":"message_start","message":{"id":"msg_123","model":"claude-3-5-sonnet-20241022","usage":{"input_tokens":10,"output_tokens":1}}}`,
				`event: content_block_start`,
				`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_01","name":"` + toolName + `","input":{}}}`,
				`event: content_block_delta`,
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"command\":\"echo hi\"}"}}`,
				`event: content_block_stop`,
				`data: {"type":"content_block_stop","index":0}`,
				`event: message_delta`,
				`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":30}}`,
				`event: message_stop`,
				`data: {"type":"message_stop"}`,
				``,
			}, "\n",
		)
	}

	type upstreamRequest struct {
		toolName string
		stream   bool
	}
	upstreamRequests := make(chan upstreamRequest, 1)
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, request *http.Request) {
				body, err := io.ReadAll(request.Body)
				if err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				toolName := gjson.GetBytes(body, "tools.0.name").String()
				upstreamRequests <- upstreamRequest{
					toolName: toolName,
					stream:   gjson.GetBytes(body, "stream").Bool(),
				}
				parts, _ := parseClaudeMCPAlias(toolName)
				mangledToolName := "mcp__" + parts.server + "__" + toolName
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte(upstreamBody(mangledToolName)))
			},
		),
	)
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"api_key":  "sk-ant-oat01-test",
			"base_url": server.URL,
		},
		Metadata: map[string]any{
			"account_uuid":      "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
			"claude_device_ids": []string{strings.Repeat("a", 64)},
		},
	}
	payload := []byte(`{"model":"claude-3-5-sonnet-20241022","messages":[{"role":"user","content":"run echo hi"}],` +
		`"tools":[{"type":"function","function":{"name":"bash","description":"run shell",` +
		`"parameters":{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}}}]}`)

	response, err := executor.Execute(
		context.Background(), auth, cliproxyexecutor.Request{
			Model:   "claude-3-5-sonnet-20241022",
			Payload: payload,
		}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai")},
	)
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}

	upstream := <-upstreamRequests
	if !upstream.stream {
		t.Fatal("upstream stream = false, want true")
	}
	if !strings.HasPrefix(upstream.toolName, "mcp__") || !strings.HasSuffix(upstream.toolName, "_bash") {
		t.Fatalf("upstream tools.0.name = %q, want a semantic MCP alias", upstream.toolName)
	}
	if got := gjson.GetBytes(response.Payload, "choices.0.message.tool_calls.0.function.name").String(); got != "bash" {
		t.Fatalf("response tool name = %q, want bash; payload=%s", got, response.Payload)
	}
}
