package auth

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestPersistenceMetadataCopiesContainersWithoutEncodingOpaqueValues(t *testing.T) {
	opaque := func() string { return "retained" }
	nested := map[string]any{"marker": "before"}
	array := []any{nested}
	strings := []string{"before"}
	bytes := []byte("before")
	dictionary := map[string]string{"marker": "before"}
	metadata := map[string]any{
		"left": nested, "right": nested, "array": array, "strings": strings, "bytes": bytes,
		"strings_alias": strings, "bytes_alias": bytes, "dictionary": dictionary, "dictionary_alias": dictionary,
		"empty_strings": []string{}, "empty_bytes": []byte{}, "empty_array": []any{},
		"nil_strings": []string(nil), "nil_bytes": []byte(nil), "nil_array": []any(nil),
		"number": json.Number("9007199254740993"), "opaque": opaque,
	}
	metadata["self"] = metadata
	array = append(array, nil)
	array[1] = array
	metadata["cycle"] = array
	record := &Auth{Metadata: metadata, Disabled: true}
	if err := PrepareAuthForPersistence(record); err != nil {
		t.Fatal(err)
	}
	copied := record.Metadata
	copied["left"].(map[string]any)["marker"] = "after"
	if nested["marker"] != "before" || copied["right"].(map[string]any)["marker"] != "after" {
		t.Fatal("nested metadata did not preserve independent alias relationships")
	}
	copied["self"].(map[string]any)["copy_marker"] = true
	if copied["copy_marker"] != true || metadata["copy_marker"] != nil {
		t.Fatal("map cycle retained the original container")
	}
	copiedCycle := copied["cycle"].([]any)
	copiedCycle[1].([]any)[0] = "independent"
	if copiedCycle[0] != "independent" || !reflect.DeepEqual(array[0], nested) {
		t.Fatal("slice cycle retained the original backing array")
	}
	copied["strings"].([]string)[0] = "after"
	copied["bytes"].([]byte)[0] = 'a'
	copied["dictionary"].(map[string]string)["marker"] = "after"
	if strings[0] != "before" || string(bytes) != "before" || dictionary["marker"] != "before" {
		t.Fatal("typed metadata containers share published memory")
	}
	if copied["strings_alias"].([]string)[0] != "after" || copied["bytes_alias"].([]byte)[0] != 'a' ||
		copied["dictionary_alias"].(map[string]string)["marker"] != "after" {
		t.Fatal("typed metadata aliases were not preserved")
	}
	for _, key := range []string{
		"empty_strings", "empty_bytes", "empty_array", "nil_strings", "nil_bytes", "nil_array",
	} {
		before, after := reflect.ValueOf(metadata[key]), reflect.ValueOf(copied[key])
		if before.Type() != after.Type() || before.IsNil() != after.IsNil() || after.Len() != 0 {
			t.Fatalf("empty metadata container %s changed type or nil state", key)
		}
	}
	if copied["number"] != json.Number("9007199254740993") || copied["opaque"].(func() string)() != "retained" {
		t.Fatal("metadata copy changed original numeric or opaque values")
	}
	if copied["disabled"] != true || metadata["disabled"] != nil {
		t.Fatal("persistence preparation changed the caller's metadata map")
	}
}
