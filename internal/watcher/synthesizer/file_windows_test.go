package synthesizer

import (
	"testing"
	"time"
)

func TestSynthesizeAuthFileCanonicalizesWindowsCaseVariantID(t *testing.T) {
	ctx := &SynthesisContext{
		AuthDir: `C:\auth`,
		Now:     time.Now(),
	}
	auths, errSynthesize := SynthesizeAuthFile(
		ctx,
		`c:\AUTH\SHARED.json`,
		[]byte(`{"type":"codex","refresh_token":"refresh"}`),
	)
	if errSynthesize != nil {
		t.Fatal(errSynthesize)
	}
	if len(auths) != 1 {
		t.Fatalf("SynthesizeAuthFile() returned %d auths", len(auths))
	}
	if auths[0].ID != "shared.json" || auths[0].FileName != "shared.json" {
		t.Fatalf("synthesized auth identity = (%q, %q)", auths[0].ID, auths[0].FileName)
	}
}
