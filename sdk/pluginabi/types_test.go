package pluginabi

import (
	"encoding/json"
	"testing"
)

func TestSchemaVersionSupportsRequestLifecycle(t *testing.T) {
	// This contract test intentionally compares a compile-time ABI constant.
	// noinspection GoBoolExpressions
	if SchemaVersion != 2 {
		t.Fatalf("SchemaVersion = %d, want 2", SchemaVersion)
	}
}

func TestEnvelopeRoundTrip(t *testing.T) {
	payload := json.RawMessage(`{"name":"example"}`)
	env := Envelope{
		OK:     true,
		Result: payload,
	}

	raw, errMarshal := json.Marshal(env)
	if errMarshal != nil {
		t.Fatalf("marshal envelope: %v", errMarshal)
	}

	var decoded Envelope
	if errUnmarshal := json.Unmarshal(raw, &decoded); errUnmarshal != nil {
		t.Fatalf("unmarshal envelope: %v", errUnmarshal)
	}
	if !decoded.OK || string(decoded.Result) != string(payload) {
		t.Fatalf("decoded envelope = %#v, want ok payload", decoded)
	}
}

func TestMethodNamesAreStable(t *testing.T) {
	tests := []struct {
		name string
		got  string
		want string
	}{
		{"MethodPluginRegister", MethodPluginRegister, "plugin.register"},
		{"MethodRequestInterceptBefore", MethodRequestInterceptBefore, "request.intercept_before"},
		{"MethodRequestInterceptAfter", MethodRequestInterceptAfter, "request.intercept_after"},
		{"MethodRequestComplete", MethodRequestComplete, "request.complete"},
		{"MethodResponseInterceptAfter", MethodResponseInterceptAfter, "response.intercept_after"},
		{"MethodResponseInterceptStreamChunk", MethodResponseInterceptStreamChunk, "response.intercept_stream_chunk"},
		{"MethodHostHTTPDo", MethodHostHTTPDo, "host.http.do"},
		{"MethodHostHTTPStreamRead", MethodHostHTTPStreamRead, "host.http.stream_read"},
		{"MethodHostModelExecute", MethodHostModelExecute, "host.model.execute"},
		{"MethodHostModelExecuteStream", MethodHostModelExecuteStream, "host.model.execute_stream"},
		{"MethodHostModelStreamRead", MethodHostModelStreamRead, "host.model.stream_read"},
		{"MethodHostModelStreamClose", MethodHostModelStreamClose, "host.model.stream_close"},
		{"MethodExecutorExecuteStream", MethodExecutorExecuteStream, "executor.execute_stream"},
	}
	for _, test := range tests {
		if test.got != test.want {
			t.Errorf("%s = %q, want %q", test.name, test.got, test.want)
		}
	}
}

func TestSchedulerPickMethodName(t *testing.T) {
	// This contract test intentionally compares a compile-time ABI constant.
	// noinspection GoBoolExpressions
	if MethodSchedulerPick != "scheduler.pick" {
		t.Fatalf("MethodSchedulerPick = %q", MethodSchedulerPick)
	}
}
