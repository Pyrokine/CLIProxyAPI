package config_test

import (
	"bytes"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	"gopkg.in/yaml.v3"
)

func TestPublishedYAMLNodeAndDecoderContract(t *testing.T) {
	cfg, err := config.ParseConfigBytes(
		[]byte(`plugins:
  enabled: true
  configs:
    fixture:
      enabled: false
      priority: 0
      opaque_int: 9223372036854775807 # retained
`),
	)
	if err != nil {
		t.Fatal(err)
	}
	item, exists := cfg.Plugins.Configs["fixture"]
	if !exists {
		t.Fatal("plugin configuration was not retained")
	}
	var node = item.Raw
	var decoder interface {
		UnmarshalYAML(*yaml.Node) error
	} = &item
	if errDecode := decoder.UnmarshalYAML(&node); errDecode != nil {
		t.Fatal(errDecode)
	}
	if item.Enabled == nil || *item.Enabled || item.Priority != 0 {
		t.Fatal("published YAML decoder changed explicit false or zero values")
	}
	var original struct {
		OpaqueInt int64 `yaml:"opaque_int"`
	}
	if errDecode := item.Raw.Decode(&original); errDecode != nil {
		t.Fatal(errDecode)
	}
	if original.OpaqueInt != 1<<63-1 {
		t.Fatalf("preserved plugin integer = %d", original.OpaqueInt)
	}
	encoded, errMarshal := yaml.Marshal(&item.Raw)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	if !bytes.Contains(encoded, []byte("# retained")) {
		t.Fatalf("plugin YAML comment was lost: %s", encoded)
	}
}
