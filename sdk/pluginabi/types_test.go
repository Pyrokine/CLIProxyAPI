package pluginabi

import (
	"encoding/json"
	"testing"
)

func TestSchemaVersionCapabilities(t *testing.T) {
	// These contract tests intentionally compare compile-time ABI constants.
	// noinspection GoBoolExpressions
	if SchemaVersion != 6 {
		t.Fatalf("SchemaVersion = %d, want 6", SchemaVersion)
	}
	if SchemaVersionStreamChunkOmitRequestBody != 3 {
		t.Fatalf("SchemaVersionStreamChunkOmitRequestBody = %d, want 3", SchemaVersionStreamChunkOmitRequestBody)
	}
	if SchemaVersionWebSocketResponseObserver != 4 {
		t.Fatalf("SchemaVersionWebSocketResponseObserver = %d, want 4", SchemaVersionWebSocketResponseObserver)
	}
	if SchemaVersionStreamChunkOmitHistory != 5 {
		t.Fatalf("SchemaVersionStreamChunkOmitHistory = %d, want 5", SchemaVersionStreamChunkOmitHistory)
	}
	if SchemaVersionRawManagementResponse != 6 {
		t.Fatalf("SchemaVersionRawManagementResponse = %d, want 6", SchemaVersionRawManagementResponse)
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
		{"MethodPluginQuiesce", MethodPluginQuiesce, "plugin.quiesce"},
		{"MethodRequestInterceptBefore", MethodRequestInterceptBefore, "request.intercept_before"},
		{"MethodRequestInterceptAfter", MethodRequestInterceptAfter, "request.intercept_after"},
		{"MethodRequestComplete", MethodRequestComplete, "request.complete"},
		{"MethodResponseInterceptAfter", MethodResponseInterceptAfter, "response.intercept_after"},
		{"MethodResponseInterceptStreamChunk", MethodResponseInterceptStreamChunk, "response.intercept_stream_chunk"},
		{"MethodWebSocketResponseEvent", MethodWebSocketResponseEvent, "websocket.response_event"},
		{"MethodQuotaIdentifier", MethodQuotaIdentifier, "quota.identifier"},
		{"MethodQuotaDescribe", MethodQuotaDescribe, "quota.describe"},
		{"MethodQuotaFetch", MethodQuotaFetch, "quota.fetch"},
		{"MethodQuotaReset", MethodQuotaReset, "quota.reset"},
		{"MethodHostAffinityLookup", MethodHostAffinityLookup, "host.affinity.lookup"},
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
