package auth

import "testing"

func TestClonePreservesAndIsolatesEmptyMaps(t *testing.T) {
	for _, empty := range []bool{false, true} {
		a := &Auth{}
		if empty {
			a.Attributes = map[string]string{}
			a.Metadata = map[string]any{}
			a.ModelStates = map[string]*ModelState{}
		}
		b := a.Clone()
		if !empty {
			if b.Attributes != nil || b.Metadata != nil || b.ModelStates != nil {
				t.Fatal("nil maps changed shape")
			}
			continue
		}
		b.Attributes["profile"] = "cli"
		b.Metadata["account_uuid"] = "synthetic"
		b.ModelStates["model"] = &ModelState{}
		if len(a.Attributes) != 0 || len(a.Metadata) != 0 || len(a.ModelStates) != 0 {
			t.Fatal("clone shares empty maps")
		}
	}
}
