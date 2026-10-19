package util

import "testing"

func TestJSONEqualPreservesExactNumbers(t *testing.T) {
	for _, tc := range []struct {
		name  string
		left  string
		right string
		equal bool
	}{
		{"large integer replacement", `{"serial":9007199254740992}`, `{"serial":9007199254740993}`, false},
		{"integer and decimal", `{"value":1}`, `{"value":1.0}`, true},
		{"scientific notation", `{"value":1e3}`, `{"value":1000}`, true},
		{"exact fraction", `{"value":1.0000000000000000001}`, `{"value":1}`, false},
		{"tiny distinct values", `{"value":1e-10000}`, `{"value":2e-10000}`, false},
		{"large equivalent exponent", `{"value":1e10000}`, `{"value":10e9999}`, true},
		{"nested key order", `{"values":[1,2],"active":true}`, `{"active":true,"values":[1.0,2]}`, true},
		{"multiple values", `{"value":1} {"value":2}`, `{"value":1}`, false},
		{"trailing data", `{"value":1} trailing`, `{"value":1}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := JSONEqual([]byte(tc.left), []byte(tc.right)); got != tc.equal {
				t.Fatalf("JSONEqual() = %v, want %v", got, tc.equal)
			}
		})
	}
}
