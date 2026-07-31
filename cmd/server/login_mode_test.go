package main

import "testing"

func TestXAILoginFlagEnablesLoginMode(t *testing.T) {
	flags := &loginFlags{xaiLogin: true}
	if !flags.isLoginMode() {
		t.Fatal("xAI login flag did not enable login mode")
	}
}
