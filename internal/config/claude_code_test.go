package config

import "testing"

func TestParseConfigBytesClaudeCodeModelListCloaking(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want bool
	}{
		{
			name: "defaults to cloaking enabled",
			yaml: "port: 8317\n",
			want: false,
		},
		{
			name: "disables model list cloaking",
			yaml: "claude-code:\n  disable-cloaking-model-list: true\n",
			want: true,
		},
	}

	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				cfg, errParse := ParseConfigBytes([]byte(test.yaml))
				if errParse != nil {
					t.Fatalf("ParseConfigBytes() error = %v", errParse)
				}
				if got := cfg.ClaudeCode.DisableCloakingModelList; got != test.want {
					t.Fatalf("DisableCloakingModelList = %t, want %t", got, test.want)
				}
			},
		)
	}
}
