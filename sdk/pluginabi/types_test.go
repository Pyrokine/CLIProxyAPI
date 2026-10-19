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

func TestErrorEnvelopePreservesHTTPStatus(t *testing.T) {
	var _ error = (*Error)(nil)
	var _ interface{ StatusCode() int } = (*Error)(nil)

	raw, errMarshal := NewErrorEnvelope("insufficient_quota", "plan limit reached", 403)
	if errMarshal != nil {
		t.Fatalf("NewErrorEnvelope(): %v", errMarshal)
	}
	var envelope Envelope
	if errUnmarshal := json.Unmarshal(raw, &envelope); errUnmarshal != nil {
		t.Fatalf("unmarshal envelope: %v", errUnmarshal)
	}
	if envelope.OK || envelope.Error == nil || envelope.Error.Code != "insufficient_quota" ||
		envelope.Error.Error() != "plan limit reached" || envelope.Error.StatusCode() != 403 {
		t.Fatalf("error envelope = %#v", envelope)
	}

	legacy, errLegacy := NewErrorEnvelope("plugin_failed", "unavailable")
	if errLegacy != nil {
		t.Fatalf("NewErrorEnvelope() without status: %v", errLegacy)
	}
	var legacyEnvelope Envelope
	if errUnmarshal := json.Unmarshal(legacy, &legacyEnvelope); errUnmarshal != nil {
		t.Fatalf("unmarshal legacy envelope: %v", errUnmarshal)
	}
	if legacyEnvelope.Error == nil || legacyEnvelope.Error.StatusCode() != 0 {
		t.Fatalf("legacy error envelope = %#v, want no HTTP status", legacyEnvelope)
	}
	var legacyWire struct {
		Error map[string]json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(legacy, &legacyWire); err != nil {
		t.Fatal(err)
	}
	if _, exists := legacyWire.Error["http_status"]; exists {
		t.Fatalf("legacy error envelope contains omitted HTTP status: %s", legacy)
	}
	var nilError *Error
	if nilError.Error() != "" || nilError.StatusCode() != 0 {
		t.Fatal("nil error must have an empty message and no HTTP status")
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
		{"MethodHostHTTPDoStream", MethodHostHTTPDoStream, "host.http.do_stream"},
		{"MethodHostRoutingResetCooldown", MethodHostRoutingResetCooldown, "host.routing.reset_cooldown"},
		{"MethodHostHTTPOperationOpen", MethodHostHTTPOperationOpen, "host.http.operation_open"},
		{"MethodHostHTTPCancel", MethodHostHTTPCancel, "host.http.cancel"},
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
