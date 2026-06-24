package registry

import "testing"

func TestWithXAIBuiltinsIncludesVideo15Preview(t *testing.T) {
	models := WithXAIBuiltins(nil)
	for _, model := range models {
		if model != nil && model.ID == xaiBuiltinVideo15PreviewModelID {
			return
		}
	}
	t.Fatalf("missing %s builtin model", xaiBuiltinVideo15PreviewModelID)
}
