package auth

import (
	"reflect"
	"testing"
)

func TestMergeAuthSaveDeltaMapChanges(t *testing.T) {
	t.Run(
		"empty-map", func(t *testing.T) {
			before := &Auth{}
			after := &Auth{Attributes: map[string]string{}, Metadata: map[string]any{}}
			target, _ := mergePersistentAuthState(before, after)
			after.UpdatedAt = target.UpdatedAt
			if target.UpdatedAt.IsZero() || !reflect.DeepEqual(target, after) {
				t.Fatal("lost explicit empty map enrichment")
			}
		},
	)
	t.Run(
		"delete-map-preserves-concurrent-key", func(t *testing.T) {
			before := &Auth{Metadata: map[string]any{"old": true}}
			after := before.Clone()
			after.Metadata = nil
			target := before.Clone()
			target.Metadata["concurrent"] = true
			delta := after.Clone()
			// 三方合并使用显式空 map 删除原字段，nil 保留已有字段
			delta.Metadata = map[string]any{}
			merged := mergeAuthContent(before, target, delta)
			if len(merged.Metadata) != 1 || merged.Metadata["concurrent"] != true {
				t.Fatalf("map deletion lost: %v", merged.Metadata)
			}
		},
	)
	t.Run(
		"conflicts", func(t *testing.T) {
			before := &Auth{Label: "old", Metadata: map[string]any{"same": "old", "other": "old"}}
			after := before.Clone()
			after.Label = "store"
			after.Metadata["same"] = "store"
			after.Metadata["other"] = "store"
			target := before.Clone()
			target.Label = "runtime"
			target.Metadata["same"] = "runtime"
			merged := mergeAuthContent(before, target, after)
			if merged.Label != "runtime" || merged.Metadata["same"] != "runtime" ||
				merged.Metadata["other"] != "store" {
				t.Fatalf("incorrect merge: %+v", merged)
			}
		},
	)
}
