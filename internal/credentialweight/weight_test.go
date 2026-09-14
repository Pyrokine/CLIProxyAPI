package credentialweight

import (
	"encoding/json"
	"math"
	"testing"
)

func TestParseValue(t *testing.T) {
	tests := []struct {
		name    string
		value   any
		want    int64
		wantErr bool
	}{
		{name: "default string", value: "", want: Default},
		{name: "negative", value: json.Number("-5"), want: 0},
		{name: "zero", value: 0, want: 0},
		{name: "integer float", value: float64(7), want: 7},
		{name: "numeric string", value: "9", want: 9},
		{name: "maximum", value: json.Number("1000000"), want: Max},
		{name: "fraction", value: json.Number("1.5"), wantErr: true},
		{name: "float fraction", value: 1.5, wantErr: true},
		{name: "nan", value: math.NaN(), wantErr: true},
		{name: "positive infinity", value: math.Inf(1), wantErr: true},
		{name: "above maximum", value: json.Number("1000001"), wantErr: true},
		{name: "integer overflow", value: json.Number("9223372036854775808"), wantErr: true},
		{name: "nonnumeric", value: "invalid", wantErr: true},
	}
	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				got, err := ParseValue(test.value)
				if (err != nil) != test.wantErr {
					t.Fatalf("ParseValue(%v) error = %v, wantErr = %v", test.value, err, test.wantErr)
				}
				if !test.wantErr && got != test.want {
					t.Fatalf("ParseValue(%v) = %d, want %d", test.value, got, test.want)
				}
			},
		)
	}
}

func TestParseString(t *testing.T) {
	for _, test := range []struct {
		raw     string
		want    int64
		wantErr bool
	}{
		{raw: "", want: Default},
		{raw: " 5 ", want: 5},
		{raw: "-1", want: 0},
		{raw: "1000001", wantErr: true},
		{raw: "1.5", wantErr: true},
	} {
		got, err := ParseString(test.raw)
		if (err != nil) != test.wantErr {
			t.Fatalf("ParseString(%q) error = %v, wantErr = %v", test.raw, err, test.wantErr)
		}
		if !test.wantErr && got != test.want {
			t.Fatalf("ParseString(%q) = %d, want %d", test.raw, got, test.want)
		}
	}
}
