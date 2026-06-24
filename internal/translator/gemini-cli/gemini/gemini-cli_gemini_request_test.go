package gemini

import (
	"encoding/base64"
	"fmt"
	"testing"

	"github.com/tidwall/gjson"
	"google.golang.org/protobuf/encoding/protowire"
)

func validGeminiCLIThoughtSignatureForTest() string {
	payload := protowire.AppendTag(nil, 1, protowire.BytesType)
	payload = protowire.AppendBytes(payload, []byte{0x01, 0x02, 0x03, 0x04})
	return base64.StdEncoding.EncodeToString(payload)
}

func TestConvertGeminiRequestToGeminiCLIPreservesNativeSignature(t *testing.T) {
	validSignature := validGeminiCLIThoughtSignatureForTest()
	inputJSON := []byte(fmt.Sprintf(
		`{
		"model":"gemini-3-pro-preview",
		"contents":[{
			"role":"model",
			"parts":[{"functionCall":{"name":"test_tool","args":{}},"thoughtSignature":"%s"}]
		}]
	}`, validSignature,
	))

	output := ConvertGeminiRequestToGeminiCLI("gemini-3-pro-preview", inputJSON, false)
	if got := gjson.GetBytes(output, "request.contents.0.parts.0.thoughtSignature").String(); got != validSignature {
		t.Fatalf("thoughtSignature = %q, want %q; output: %s", got, validSignature, output)
	}
}
