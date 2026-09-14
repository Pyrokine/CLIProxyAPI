package common

import (
	"bytes"
	"testing"

	"github.com/tidwall/gjson"
)

func TestNewRawArrayItems(t *testing.T) {
	if items := NewRawArrayItems(0); items != nil {
		t.Fatalf("NewRawArrayItems(0) = %#v, want nil", items)
	}
	if items := NewRawArrayItems(3); len(items) != 0 || cap(items) != 3 {
		t.Fatalf("NewRawArrayItems(3) len = %d, cap = %d; want len 0, cap 3", len(items), cap(items))
	}
}

func TestJoinRawArray(t *testing.T) {
	tests := []struct {
		name  string
		items [][]byte
		want  string
	}{
		{name: "empty", want: "[]"},
		{name: "single", items: [][]byte{[]byte(`{"id":1}`)}, want: `[{"id":1}]`},
		{name: "multiple", items: [][]byte{[]byte(`{"id":1}`), []byte(`{"id":2}`)}, want: `[{"id":1},{"id":2}]`},
	}

	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				if got := string(JoinRawArray(test.items)); got != test.want {
					t.Fatalf("JoinRawArray() = %s, want %s", got, test.want)
				}
			},
		)
	}
}

func TestSetRawArrayItems(t *testing.T) {
	tests := []struct {
		name  string
		data  string
		path  string
		items [][]byte
		want  string
	}{
		{name: "empty", data: `{"items":[]}`, path: "items", want: `{"items":[]}`},
		{
			name: "single nested", data: `{"before":1,"request":{"contents":[]},"after":2}`, path: "request.contents",
			items: [][]byte{[]byte(`{"id":1}`)}, want: `{"before":1,"request":{"contents":[{"id":1}]},"after":2}`,
		},
		{
			name: "single fallback", data: `{"items":[{"old":1},{"old":2}]}`, path: "items",
			items: [][]byte{[]byte(`{"id":1}`)}, want: `{"items":[{"id":1}]}`,
		},
		{
			name: "multiple", data: `{"items":[]}`, path: "items",
			items: [][]byte{[]byte(`{"id":1}`), []byte(`{"id":2}`)}, want: `{"items":[{"id":1},{"id":2}]}`,
		},
	}

	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				got := SetRawArrayItems([]byte(test.data), test.path, test.items)
				if string(got) != test.want {
					t.Fatalf("SetRawArrayItems() = %s, want %s", got, test.want)
				}
			},
		)
	}
}

func TestEchoRequestFieldsCopiesTypedValues(t *testing.T) {
	request := gjson.Parse(
		`{"instructions":"answer","max_output_tokens":42,"parallel_tool_calls":true,"temperature":0.25,"reasoning":{"effort":"high"},"metadata":{"team":"sdk"},"unknown":"ignored"}`,
	)
	got := EchoRequestFields([]byte(`{"response":{"id":"resp_1"}}`), request, "response.")
	for _, test := range []struct {
		path string
		want string
	}{
		{path: "response.id", want: `"resp_1"`},
		{path: "response.instructions", want: `"answer"`},
		{path: "response.max_output_tokens", want: "42"},
		{path: "response.parallel_tool_calls", want: "true"},
		{path: "response.temperature", want: "0.25"},
		{path: "response.reasoning", want: `{"effort":"high"}`},
		{path: "response.metadata", want: `{"team":"sdk"}`},
	} {
		if value := gjson.GetBytes(got, test.path); value.Raw != test.want {
			t.Fatalf("EchoRequestFields() %s = %s, want %s", test.path, value.Raw, test.want)
		}
	}
	if value := gjson.GetBytes(got, "response.unknown"); value.Exists() {
		t.Fatalf("EchoRequestFields() copied unknown field: %s", value.Raw)
	}
}

func TestSSEEventDataWritesCompleteFrame(t *testing.T) {
	got := SSEEventData(
		"response.completed",
		[]byte(`{"type":"response.completed"}`),
	)
	want := []byte("event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n")
	if !bytes.Equal(got, want) {
		t.Fatalf("SSEEventData() = %q, want %q", got, want)
	}
}
