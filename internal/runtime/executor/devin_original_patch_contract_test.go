package executor

import (
	"bytes"
	"context"
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
	"google.golang.org/protobuf/encoding/protowire"
)

const executorPatchRequest = `{"tools":[{"type":"namespace","name":"functions","tools":[{"type":"cus` +
	`tom","name":"apply_patch","format":{"type":"grammar","definition":"sta` +
	`rt: patch"}}]}],"input":"patch a file"}`
const executorPatchInput = "  *** Begin Patch\n*** End Patch\n "

func assertExecutorPatchOutput(t *testing.T, payload []byte) {
	t.Helper()
	item := gjson.GetBytes(payload, `output.#(type=="custom_tool_call")`)
	if item.Get("type").String() != "custom_tool_call" || item.Get("input").String() != executorPatchInput ||
		item.Get("name").String() != "apply_patch" ||
		item.Get("namespace").String() != "functions" {
		t.Fatalf("wrong patch output: %s", payload)
	}
}
func assertExecutorPatchStream(
	t *testing.T,
	chunks <-chan cliproxyexecutor.StreamChunk,
	acknowledgeSnapshot ...func(),
) {
	t.Helper()
	acknowledged := len(acknowledgeSnapshot) == 0
	var delta strings.Builder
	var done, item, final gjson.Result
	for chunk := range chunks {
		if chunk.Err != nil {
			t.Fatal(chunk.Err)
		}
		for line := range strings.SplitSeq(string(chunk.Payload), "\n") {
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			ev := gjson.Parse(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			switch ev.Get("type").String() {
			case "response.output_text.delta":
				if !acknowledged {
					acknowledged = true
					acknowledgeSnapshot[0]()
				}
			case "response.custom_tool_call_input.delta":
				if !acknowledged {
					t.Fatal("fabricated patch progress before upstream snapshot")
				}
				delta.WriteString(ev.Get("delta").String())
			case "response.custom_tool_call_input.done":
				done = ev
			case "response.output_item.done":
				if ev.Get("item.type").String() == "custom_tool_call" {
					item = ev.Get("item")
				}
			case "response.completed":
				final = ev.Get("response")
			}
		}
	}
	if !acknowledged || delta.String() != executorPatchInput || done.Get("input").String() != executorPatchInput ||
		item.Get("input").String() != executorPatchInput ||
		done.Get("item_id").String() != item.Get("id").String() ||
		done.Get("call_id").String() != item.Get("call_id").String() {
		t.Fatalf("inconsistent stream delta=%q done=%s item=%s", delta.String(), done.Raw, item.Raw)
	}
	assertExecutorPatchOutput(t, []byte(final.Raw))
}

func TestDevinApplyPatchExecutorReuse(t *testing.T) {
	wireField := func(data []byte, target protowire.Number) []byte {
		for len(data) > 0 {
			number, kind, n := protowire.ConsumeTag(data)
			if n < 0 {
				return nil
			}
			data = data[n:]
			if kind == protowire.BytesType {
				value, m := protowire.ConsumeBytes(data)
				if m < 0 {
					return nil
				}
				if number == target {
					return value
				}
				data = data[m:]
			} else {
				m := protowire.ConsumeFieldValue(number, kind, data)
				if m < 0 {
					return nil
				}
				data = data[m:]
			}
		}
		return nil
	}
	args := `{"input":"  *** Begin Patch\n*** End Patch\n "}`
	var frames bytes.Buffer
	for i, fragment := range []string{args[:22], args[22:]} {
		var tool []byte
		tool = protowire.AppendTag(tool, 1, protowire.BytesType)
		tool = protowire.AppendString(tool, "call_patch")
		if i == 0 {
			tool = protowire.AppendTag(tool, 2, protowire.BytesType)
			tool = protowire.AppendString(tool, "functions__apply_patch")
		}
		tool = protowire.AppendTag(tool, 3, protowire.BytesType)
		tool = protowire.AppendString(tool, fragment)
		var frame []byte
		frame = protowire.AppendTag(frame, 6, protowire.BytesType)
		frame = protowire.AppendBytes(frame, tool)
		frames.Write(helps.WrapConnectEnvelope(frame))
	}
	frames.Write(helps.WrapConnectEnvelopeWithFlag(helps.ConnectFlagEndStream, []byte(`{}`)))
	requests := make(chan []byte, 2)
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				body, errReadAll := io.ReadAll(r.Body)
				if errReadAll != nil {
					t.Error(errReadAll)
					w.WriteHeader(500)
					return
				}
				requests <- body
				w.Header().Set("Content-Type", "application/connect+proto")
				_, _ = w.Write(frames.Bytes())
			},
		),
	)
	defer server.Close()
	exec := NewDevinExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID: "devin-patch", Provider: "devin",
		Attributes: map[string]string{"api_key": "test-key", "base_url": server.URL},
	}
	req := cliproxyexecutor.Request{Model: "devin/swe-2", Payload: []byte(executorPatchRequest)}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, OriginalRequest: req.Payload}
	assertWire := func(body []byte) {
		if len(body) < 5 {
			t.Fatal("missing envelope")
		}
		tool := wireField(body[5:], 10)
		name, desc, params := wireField(tool, 1), wireField(tool, 2), wireField(tool, 3)
		if string(name) != "functions__apply_patch" || !strings.Contains(
			string(desc), "*** Begin Patch",
		) || !strings.Contains(string(desc), "@@") || !strings.Contains(
			string(desc), "start: patch",
		) || gjson.GetBytes(params, "properties.input.type").String() != "string" || !gjson.GetBytes(
			params, "additionalProperties",
		).Exists() || gjson.GetBytes(params, "additionalProperties").Bool() {
			t.Fatalf("wire contract: name=%s desc=%s params=%s", name, desc, params)
		}
	}
	response, errExecute := exec.Execute(context.Background(), auth, req, opts)
	if errExecute != nil {
		t.Fatal(errExecute)
	}
	assertWire(<-requests)
	assertExecutorPatchOutput(t, response.Payload)
	stream, errExecuteStream := exec.ExecuteStream(context.Background(), auth, req, opts)
	if errExecuteStream != nil {
		t.Fatal(errExecuteStream)
	}
	assertWire(<-requests)
	assertExecutorPatchStream(t, stream.Chunks)
}
