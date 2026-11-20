package artifact

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestSVGArtifactRequiresAnActualSVGDocument(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, body string
		valid      bool
	}{
		{
			"svg", `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10"><rect width="10" height="10"/></svg>`,
			true,
		},
		{"declaration", `<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg"/>`, true},
		{
			"script retained as download", `<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`,
			true,
		},
		{"arbitrary xml", `<?xml version="1.0"?><document/>`, false},
		{"html", `<html><svg/></html>`, false},
		{"foreign namespace", `<svg xmlns="urn:foreign"/>`, false},
		{"second root", `<svg/><html/>`, false},
		{"malformed", `<svg><path></svg>`, false},
		{"trailing text", `<svg/>trailing`, false},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				data := []byte(test.body)
				record, err := store.PutBytes(t.Context(), data, "image/svg+xml")
				if !test.valid {
					if !errors.Is(err, errUnsupportedMIME) {
						t.Fatalf("invalid SVG error=%v", err)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if record.MIMEType != "image/svg+xml" || !strings.HasSuffix(record.ID, ".svg") {
					t.Fatalf("SVG record: %+v", record)
				}
				file, opened, err := store.Open(record.ID)
				if err != nil {
					t.Fatal(err)
				}
				defer file.Close()
				result, err := io.ReadAll(file)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(result, data) || opened.MIMEType != record.MIMEType {
					t.Fatal("SVG bytes or MIME changed in store")
				}
			},
		)
	}
}

func TestSVGDoesNotReplaceRasterContentSniffing(t *testing.T) {
	if _, _, err := normalizeImageMIME("image/png", []byte(`<svg/>`)); !errors.Is(err, errUnsupportedMIME) {
		t.Fatalf("SVG accepted as PNG: %v", err)
	}
	if _, _, err := normalizeImageMIME("image/svg+xml", testPNGBytes(t)); !errors.Is(err, errUnsupportedMIME) {
		t.Fatalf("PNG accepted as SVG: %v", err)
	}
}
