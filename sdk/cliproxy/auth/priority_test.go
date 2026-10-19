package auth

import "testing"

func TestApplyAuthPriorityMetadataKeepsPluginPriorityWhenFilePriorityIsInvalid(t *testing.T) {
	for _, metadata := range []map[string]any{nil, {}, {"priority": "invalid"}} {
		auth := &Auth{
			Attributes: map[string]string{"priority": "7", AttributeFilePriority: "true"},
			Metadata:   map[string]any{"priority": float64(7)},
		}
		ApplyAuthPriorityMetadata(auth, metadata)
		if _, inherited := auth.Attributes[AttributeFilePriority]; inherited {
			t.Fatalf("invalid file priority left inherited marker: %#v", metadata)
		}
		if auth.Attributes["priority"] != "7" || auth.Metadata["priority"] != float64(7) {
			t.Fatalf("invalid file priority replaced plugin value: %#v", metadata)
		}
	}
}

func TestApplyAuthPriorityMetadataCopiesFilePriority(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  any
		want string
	}{
		{name: "numeric", raw: float64(1.5), want: "1"},
		{name: "string", raw: " 4 ", want: "4"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			auth := &Auth{Attributes: map[string]string{"priority": "7"}}
			ApplyAuthPriorityMetadata(auth, map[string]any{"priority": tc.raw})
			if auth.Attributes["priority"] != tc.want || auth.Attributes[AttributeFilePriority] != "true" ||
				auth.Metadata["priority"] != tc.raw {
				t.Fatalf("file priority not propagated: %+v", auth)
			}
		})
	}
}
