package main

import "testing"

func TestProviderLoginFlagsEnableLoginMode(t *testing.T) {
	tests := []struct {
		name  string
		flags loginFlags
	}{
		{name: "xAI", flags: loginFlags{xaiLogin: true}},
		{name: "Devin", flags: loginFlags{devinLogin: true}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if !test.flags.isLoginMode() {
				t.Fatalf("%s login flag did not enable login mode", test.name)
			}
		})
	}
}
