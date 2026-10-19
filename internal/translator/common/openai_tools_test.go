package common

import (
	"reflect"
	"testing"
)

func TestAlignOpenAIToolCallMessages(t *testing.T) {
	cases := []struct {
		name      string
		messages  []string
		ambiguous []string
		want      []int
	}{
		{
			name: "move matching results ahead of interleaved messages",
			messages: []string{
				`{"role":"assistant","tool_calls":[{"id":"a"},{"id":"b"}],"timestamp_ns":1789703256000000001}`,
				`{"role":"user","content":"between"}`,
				`{"role":"tool","tool_call_id":"b","content":"result b"}`,
				`{"role":"tool","tool_call_id":"a","content":"result a"}`,
				`{"role":"assistant","content":"after"}`,
			},
			want: []int{0, 2, 3, 1, 4},
		},
		{
			name: "already adjacent",
			messages: []string{
				`{"role":"assistant","tool_calls":[{"id":"a"}]}`,
				`{"role":"tool","tool_call_id":"a"}`,
			},
			want: []int{0, 1},
		},
		{
			name: "missing result does not move matched sibling",
			messages: []string{
				`{"role":"assistant","tool_calls":[{"id":"a"},{"id":"b"}]}`,
				`{"role":"user","content":"between"}`,
				`{"role":"tool","tool_call_id":"a"}`,
			},
			want: []int{0, 1, 2},
		},
		{
			name: "repeated call id is ambiguous",
			messages: []string{
				`{"role":"assistant","tool_calls":[{"id":"a"}]}`,
				`{"role":"user","content":"between"}`,
				`{"role":"assistant","tool_calls":[{"id":"a"}]}`,
				`{"role":"tool","tool_call_id":"a"}`,
			},
			want: []int{0, 1, 2, 3},
		},
		{
			name: "repeated result id is ambiguous",
			messages: []string{
				`{"role":"assistant","tool_calls":[{"id":"a"}]}`,
				`{"role":"user","content":"between"}`,
				`{"role":"tool","tool_call_id":"a"}`,
				`{"role":"tool","tool_call_id":"a"}`,
			},
			want: []int{0, 1, 2, 3},
		},
		{
			name: "empty id is ambiguous",
			messages: []string{
				`{"role":"assistant","tool_calls":[{"id":"a"},{"id":""}]}`,
				`{"role":"user","content":"between"}`,
				`{"role":"tool","tool_call_id":"a"}`,
			},
			want: []int{0, 1, 2},
		},
		{
			name: "result before call is not moved",
			messages: []string{
				`{"role":"tool","tool_call_id":"a"}`,
				`{"role":"assistant","tool_calls":[{"id":"a"}]}`,
			},
			want: []int{0, 1},
		},
		{
			name: "known ambiguous id is not moved",
			messages: []string{
				`{"role":"assistant","tool_calls":[{"id":"a"}]}`,
				`{"role":"user","content":"between"}`,
				`{"role":"tool","tool_call_id":"a"}`,
			},
			ambiguous: []string{"a"},
			want:      []int{0, 1, 2},
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			messages := make([][]byte, 0, len(test.messages))
			for _, message := range test.messages {
				messages = append(messages, []byte(message))
			}
			got := AlignOpenAIToolCallMessages(messages, test.ambiguous...)
			want := make([][]byte, 0, len(test.want))
			for _, index := range test.want {
				want = append(want, messages[index])
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("aligned messages = %s, want %s", got, want)
			}
		})
	}
}
