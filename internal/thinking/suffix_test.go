package thinking

import "testing"

func TestParseSuffix_StripsTrailingContext1MTagWithoutThinkingSuffix(t *testing.T) {
	tests := []struct {
		name      string
		model     string
		wantModel string
	}{
		{name: "gpt 5.4 1m", model: "gpt-5.4[1m]", wantModel: "gpt-5.4"},
		{name: "gpt 5.5 1m", model: "gpt-5.5[1m]", wantModel: "gpt-5.5"},
		{name: "claude 1m", model: "claude-opus-4-7[1m]", wantModel: "claude-opus-4-7"},
	}

	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				parsed := ParseSuffix(tt.model)
				if parsed.HasSuffix {
					t.Fatalf("ParseSuffix(%q).HasSuffix = true, want false", tt.model)
				}
				if parsed.ModelName != tt.wantModel {
					t.Fatalf("ParseSuffix(%q).ModelName = %q, want %q", tt.model, parsed.ModelName, tt.wantModel)
				}
			},
		)
	}
}

func TestParseSuffix_StripsTrailingContext1MTagAfterThinkingSuffixBase(t *testing.T) {
	parsed := ParseSuffix("gpt-5.4[1m](high)")
	if !parsed.HasSuffix {
		t.Fatal("expected HasSuffix=true")
	}
	if parsed.ModelName != "gpt-5.4" {
		t.Fatalf("ModelName = %q, want %q", parsed.ModelName, "gpt-5.4")
	}
	if parsed.RawSuffix != "high" {
		t.Fatalf("RawSuffix = %q, want %q", parsed.RawSuffix, "high")
	}
}

func TestHasContext1MTag(t *testing.T) {
	if !HasContext1MTag("gpt-5.4[1m]") {
		t.Fatal("expected gpt-5.4[1m] to have 1m tag")
	}
	if !HasContext1MTag("claude-opus-4-7[1m](high)") {
		t.Fatal("expected claude-opus-4-7[1m](high) to have 1m tag")
	}
	if HasContext1MTag("gpt-5.4(high)") {
		t.Fatal("expected gpt-5.4(high) to not have 1m tag")
	}
}
