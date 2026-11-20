package modelcatalog

import "testing"

func TestFableCatalogGroupReachesModelMetadata(t *testing.T) {
	t.Parallel()
	catalog := DefaultFile()
	for _, name := range []string{"claude-fable-5", "claude-fable-5-1"} {
		model, ok := catalog.Find(name)
		if !ok || model.Provider != "Claude" || model.Group != "Fable" {
			t.Errorf("default model %q = %+v, found=%t", name, model, ok)
		}
		runtime := (&File{}).EnsureRuntimeModel(name, "claude", "")
		if runtime == nil || runtime.Provider != "Claude" || runtime.Group != "Fable" {
			t.Errorf("runtime model %q = %+v", name, runtime)
		}
	}
}
