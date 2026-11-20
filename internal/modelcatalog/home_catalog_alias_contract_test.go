package modelcatalog

import "testing"

func TestHomeCatalogPreservesCamelMaxCompletionTokens(t *testing.T) {
	catalog, err := DecodeHomeCatalog([]byte(`{"codex-plus":[{"id":"home-alias-fixture","maxCompletionTokens":8192}]}`))
	if err != nil {
		t.Fatal(err)
	}
	models := catalog.Models()
	if len(models) != 1 || models[0]["max_completion_tokens"] != 8192 {
		t.Fatalf("maxCompletionTokens alias lost: %#v", models)
	}
}
