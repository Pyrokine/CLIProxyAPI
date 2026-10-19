package common

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestResponsesPatchInputFragments(t *testing.T) {
	const patch = "*** Begin Patch\n*** Update File: 中文.txt\n@@\n-旧\\\"\n+新 😀\n*** End Patch\n"
	encoded, err := json.Marshal(map[string]string{"input": patch})
	if err != nil {
		t.Fatal(err)
	}
	for _, arguments := range []string{
		string(encoded),
		` { "input" : "*** Begin Patch\n*** Update File: 中文.txt\n@@\n-旧\\\"\n+新 😀\n*** End Patch\n" } `,
	} {
		for boundary := 0; boundary <= len(arguments); boundary++ {
			var decoder ResponsesPatchInput
			first, errFirst := decoder.Push(arguments[:boundary])
			second, errSecond := decoder.Push(arguments[boundary:])
			tail, errFinish := decoder.Finish(arguments)
			if errFirst != nil || errSecond != nil || errFinish != nil || first+second+tail != patch || decoder.Input() != patch {
				t.Fatalf("boundary=%d first=%q second=%q tail=%q errors=%v/%v/%v", boundary, first, second, tail, errFirst, errSecond, errFinish)
			}
			if repeat, errRepeat := decoder.Finish(arguments); errRepeat != nil || repeat != "" {
				t.Fatalf("repeat finish: %q %v", repeat, errRepeat)
			}
		}
		var decoder ResponsesPatchInput
		var input strings.Builder
		for offset := range len(arguments) {
			delta, errPush := decoder.Push(arguments[offset : offset+1])
			if errPush != nil {
				t.Fatalf("byte %d: %v", offset, errPush)
			}
			input.WriteString(delta)
		}
		if input.String() != patch {
			t.Fatalf("one-byte fragments = %q, want %q", input.String(), patch)
		}
	}
}

func TestResponsesPatchInputInvalid(t *testing.T) {
	for _, arguments := range []string{
		``, `null`, `[]`, `"patch"`, `{}`, `{"patch":"x"}`, `{"input":null}`, `{"input":1}`, `{"input":{}}`,
		`{"input":"x","input":"y"}`, `{"input":"x","other":0}`, `{"input":"x"} {}`, `{"input":"x"`,
		`{"input":"x\q"}`, `{"input":"\ud800"}`, `{"input":"\udfff"}`, `{"input":"\ud800\u0000"}`,
		`{"input":"\ud800x"}`, `{"input":"\u+123"}`, `{"input":"\u00-1"}`, `{"input":"\u12gg"}`,
		"{\"input\":\"raw\nnewline\"}", "{\"input\":\"\xff\"}", "{\"input\":\"\xc0\xaf\"}",
	} {
		var decoder ResponsesPatchInput
		if _, err := decoder.Finish(arguments); err == nil {
			t.Errorf("accepted invalid arguments %q", arguments)
		}
		if _, err := decoder.Push(`{"input":"valid"}`); err == nil {
			t.Errorf("cleared retained failure after %q", arguments)
		}
	}
}

func TestResponsesPatchInputSnapshots(t *testing.T) {
	var decoder ResponsesPatchInput
	first, err := decoder.Push(`{"input":"line\n\ud83d`)
	if err != nil || first != "line\n" {
		t.Fatalf("partial surrogate input=%q error=%v", first, err)
	}
	tail, err := decoder.Finish(`{"input":"line\n😀"}`)
	if err != nil || tail != "😀" || decoder.Input() != "line\n😀" {
		t.Fatalf("snapshot tail=%q input=%q error=%v", tail, decoder.Input(), err)
	}
	if _, err = decoder.Finish(`{"input":"different"}`); err == nil {
		t.Fatal("accepted conflicting final input")
	}

	var completed ResponsesPatchInput
	if _, err = completed.Push(`{"input":"x"}`); err != nil {
		t.Fatal(err)
	}
	if _, err = completed.Push(`{"input":"x"}`); err == nil {
		t.Fatal("accepted a second argument object")
	}
}
