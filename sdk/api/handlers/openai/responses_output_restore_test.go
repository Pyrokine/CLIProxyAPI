package openai

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestResponsesCompletedRestoresAcceptedOutput(t *testing.T) {
	const first = `{"id":"msg-zero","type":"message","role":"assistant","content":` +
		`[{"type":"output_text","text":` +
		`"zero"}]}`
	const second = `{"id":"call-one","type":"function_call","call_id":"call-original","name":"lookup","arguments":"{}"}`
	done := func(index string, item string) string {
		return "event: response.output_item.done\ndata: {\"type\":" +
			"\"response.output_item.done\"," +
			index +
			`"item":` +
			item +
			"}\n\n"
	}
	indexed := done(`"output_index":1,`, second) + done(`"output_index":0,`, first)
	unindexed := done("", first) + done("", second)
	for _, tc := range []struct {
		name, items, output string
		want                []string
	}{
		{"indexed missing", indexed, "", []string{"msg-zero", "call-one"}},
		{"indexed empty", indexed, `,"output":[]`, []string{"msg-zero", "call-one"}},
		{"unindexed missing", unindexed, "", []string{"msg-zero", "call-one"}},
		{"unindexed empty", unindexed, `,"output":[]`, []string{"msg-zero", "call-one"}},
		{
			"mixed index then arrival order", done("", first) + done(`"output_index":1,`, second), "",
			[]string{"call-one", "msg-zero"},
		},
		{
			"duplicate index uses last item", done(`"output_index":0,`, second) + done(`"output_index":0,`, first), "",
			[]string{"msg-zero"},
		},
		{
			"nonempty output retained", indexed, `,"output":[{"id":"upstream-authoritative","type":"message"}]`,
			[]string{"upstream-authoritative"},
		},
		{"no accepted item", done(`"output_index":0,`, `{"id":"missing-type"}`), `,"output":[]`, nil},
	} {
		t.Run(
			tc.name, func(t *testing.T) {
				completed := "id: final-native-id\r\nevent: response.completed\r\ndata: {\"type\":\"" +
					"response.completed\",\r\ndata: \"response\":{\"id\":\"resp-original\"," +
					"\"status\":\"completed\"" + tc.output + "}}\r\n\r\n"
				late := done(
					`"output_index":0,`, `{"id":"late-item","type":"message"}`,
				) + ("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":" +
					"\"late-response\",\"output\":[]}}\n\n")
				payload := []byte(tc.items + completed + late)
				assertSnapshot := func(t *testing.T, frames [][]byte) {
					t.Helper()
					completedCount := 0
					for _, frame := range frames {
						data, hasData, valid := responsesSSEDataPayload(frame)
						if !hasData || !valid {
							t.Fatalf("invalid forwarded SSE data: %s", frame)
						}
						if strings.Contains(string(frame), "late-item") || strings.Contains(
							string(frame), "late-response",
						) {
							t.Fatalf("consumed data after first terminal: %s", frame)
						}
						if gjson.GetBytes(data, "type").String() != "response.completed" {
							continue
						}
						completedCount++
						if !bytes.Contains(
							frame, []byte("id: final-native-id"),
						) || responsesSSEEventName(frame) != "response.completed" || gjson.GetBytes(
							data, "response.id",
						).String() != "resp-original" {
							t.Fatalf("changed native SSE or response identity: %s", frame)
						}
						output := gjson.GetBytes(data, "response.output").Array()
						if len(output) != len(tc.want) {
							t.Fatalf(
								"completed output length = %d, want %d: %s",
								len(output),
								len(tc.want),
								data,
							)
						}
						for i, id := range tc.want {
							if output[i].Get("id").String() != id {
								t.Fatalf("output[%d] = %s, want id %q", i, output[i].Raw, id)
							}
							if id == "call-one" &&
								(output[i].Get("call_id").String() !=
									"call-original" ||
									output[i].Get("name").String() !=
										"lookup" ||
									output[i].Get("arguments").String() !=
										"{}") {
								t.Fatalf("changed accepted function-call item: %s", output[i].Raw)
							}
						}
					}
					if completedCount != 1 {
						t.Fatalf("completed count = %d, want 1", completedCount)
					}
				}
				t.Run(
					"fragmented accumulator", func(t *testing.T) {
						acc := newResponsesSSEFrameAccumulator()
						var frames [][]byte
						for start := 0; start < len(payload); start += 17 {
							frames = append(frames, acc.AddChunk(payload[start:min(start+17, len(payload))])...)
						}
						frames = append(frames, acc.Finalize()...)
						if err := acc.Err(); err != nil {
							t.Fatal(err)
						}
						assertSnapshot(t, frames)
					},
				)
				t.Run(
					"actual Responses handler Manager executor", func(t *testing.T) {
						model := "responses-output-restore-" + strings.ReplaceAll(tc.name, " ", "-")
						router := newResponsesStreamTestRouter(
							t,
							model,
							&responsesStreamTestExecutor{payload: payload},
						)
						response := httptest.NewRecorder()
						router.ServeHTTP(
							response, httptest.NewRequest(
								http.MethodPost,
								"/v1/responses",
								strings.NewReader(
									fmt.Sprintf(`{"model":%q,"stream":true,"input":"synthetic"}`, model),
								),
							),
						)
						if response.Code != http.StatusOK || !strings.HasPrefix(
							response.Header().Get("Content-Type"), "text/event-stream",
						) {
							t.Fatalf("handler returned %d: %s", response.Code, response.Body.String())
						}
						wire := response.Body.Bytes()
						var frames [][]byte
						for len(bytes.TrimSpace(wire)) > 0 {
							n := responsesSSEFrameLen(wire)
							if n == 0 {
								t.Fatalf("handler emitted incomplete SSE framing: %q", wire)
							}
							frames = append(frames, wire[:n])
							wire = wire[n:]
						}
						assertSnapshot(t, frames)
					},
				)
			},
		)
	}
}

func TestResponsesOutputRestorePreservesValidation(t *testing.T) {
	acc := newResponsesSSEFrameAccumulator()
	acc.AddChunk(
		[]byte("event: response.output_item.done\ndata: {\"type\":\"response.output_it" +
			"em.done\",\"item\":{\"id\":\"valid\",\"type\":\"message\"}}\n\n"),
	)
	acc.AddChunk([]byte("event: response.completed\ndata: {invalid}\n\n"))
	if acc.Err() == nil || acc.sawResponsesTerminal {
		t.Fatal("malformed completion was accepted as a successful terminal")
	}
}

func TestResponsesRestoredCompletionKeepsFrameBound(t *testing.T) {
	acc := newResponsesSSEFrameAccumulator()
	acc.maxPending = 256
	for index := range 2 {
		payload := fmt.Sprintf(
			"event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\""+
				":%d,\"item\":{\"id\":\"%d\",\"type\":\"message\",\"text\":%q}}\n\n",
			index, index, strings.Repeat("x", 50),
		)
		acc.AddChunk([]byte(payload))
		if acc.Err() != nil {
			t.Fatalf("valid individual frame exceeded bound: %v", acc.Err())
		}
	}
	frames := acc.AddChunk(
		[]byte(
			"event: response.completed\ndata: {\"type\":\"response.completed\"," +
				"\"response\":{\"id\":\"bounded\",\"output\":" +
				"[]}}\n\n",
		),
	)
	if len(frames) != 0 || acc.Err() == nil {
		t.Fatal("restored completion bypassed frame bound")
	}
}
