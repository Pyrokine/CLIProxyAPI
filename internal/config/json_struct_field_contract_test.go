package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
)

func TestJSONStructFieldsKeepLegacyWireValues(t *testing.T) {
	for _, fixture := range []struct {
		name string
		cfg  Config
	}{
		{name: "zero"},
		{
			name: "populated",
			cfg: Config{
				CredentialConcurrency: CredentialConcurrencyConfig{LifecycleConfigRevision: 17, MaxLimit: 19},
				CredentialInFlight:    DefaultCredentialInFlightConfig(),
				Client:                ClientConfig{Codex: CodexClientConfig{EnableApplyPatch: true}},
				ModelCatalog: ModelCatalogConfig{
					Models: []ModelCatalogModel{
						{
							Name: "synthetic-model", Enabled: new(false),
						},
					},
				},
				Antigravity: AntigravityConfig{
					ConnectionPool: AntigravityConnectionPoolConfig{
						Enabled: new(false), IdleConnTimeout: "37s", MaxIdleConnsPerHost: new(7),
					},
				},
			},
		},
	} {
		t.Run(
			fixture.name, func(t *testing.T) {
				for _, field := range []struct {
					owner         any
					name, jsonKey string
				}{
					{fixture.cfg, "CredentialConcurrency", "credential-concurrency"},
					{fixture.cfg, "CredentialInFlight", "credential-in-flight"},
					{fixture.cfg.SDKConfig, "Client", "client"},
					{fixture.cfg.SDKConfig, "ModelCatalog", "model-catalog"},
					{fixture.cfg.Client, "Codex", "codex"},
					{fixture.cfg.Antigravity, "ConnectionPool", "connection-pool"},
				} {
					owner := reflect.ValueOf(field.owner)
					original, exists := owner.Type().FieldByName(field.name)
					if !exists {
						t.Fatalf("missing field %s", field.name)
					}
					// 动态保留旧标签，比较实际类型的零值和非零值 JSON
					legacyField := original
					legacyField.Tag = reflect.StructTag(fmt.Sprintf("json:%q", field.jsonKey+",omitempty"))
					legacy := reflect.New(reflect.StructOf([]reflect.StructField{legacyField})).Elem()
					legacy.Field(0).Set(owner.FieldByIndex(original.Index))
					oldJSON, err := json.Marshal(legacy.Interface())
					if err != nil {
						t.Fatal(err)
					}
					actualJSON, err := json.Marshal(field.owner)
					if err != nil {
						t.Fatal(err)
					}
					var oldFields, actualFields map[string]json.RawMessage
					if err := json.Unmarshal(oldJSON, &oldFields); err != nil {
						t.Fatal(err)
					}
					if err := json.Unmarshal(actualJSON, &actualFields); err != nil {
						t.Fatal(err)
					}
					oldValue, wasPresent := oldFields[field.jsonKey]
					actualValue, isPresent := actualFields[field.jsonKey]
					if !wasPresent || !isPresent || !bytes.Equal(oldValue, actualValue) {
						t.Fatalf("%s JSON changed: legacy=%s actual=%s", field.name, oldJSON, actualJSON)
					}
				}
			},
		)
	}
}
