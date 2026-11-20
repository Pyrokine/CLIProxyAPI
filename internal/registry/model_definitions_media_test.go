package registry

import "testing"

func TestPublicImage26CatalogKeepsCodexOAuthSeparate(t *testing.T) {
	for _, model := range GetOpenAIAPIImageModels() {
		if !IsOpenAIImageModel(model.ID) || IsCodexImageModel(model.ID) {
			t.Fatalf("public image classification: %+v", model)
		}
		if info := LookupStaticModelInfo(model.ID); info == nil || len(info.SupportedOutputModalities) != 1 || info.SupportedOutputModalities[0] != "image" {
			t.Fatalf("public image lookup: %+v", info)
		}
		for _, catalog := range [][]*ModelInfo{
			GetCodexFreeModels(), GetCodexTeamModels(), GetCodexPlusModels(), GetCodexProModels(),
		} {
			for _, oauthModel := range catalog {
				if oauthModel.ID == model.ID {
					t.Fatalf("public image %s added to OAuth catalog", model.ID)
				}
			}
		}
	}
	for _, id := range []string{"muse-image-1.0", "muse-voice-transcribe-1.0"} {
		info := LookupStaticModelInfo(id)
		if info == nil || info.Type != "meta" || len(info.SupportedInputModalities) == 0 || len(info.SupportedOutputModalities) == 0 {
			t.Fatalf("Meta media metadata missing: %+v", info)
		}
	}
}
