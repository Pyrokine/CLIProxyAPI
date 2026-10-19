package executor

import (
	"context"
	"fmt"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

func TestPublishCodexImageToolUsageRequiresExecution(t *testing.T) {
	tests := []struct {
		name      string
		output    string
		imageUse  string
		wantImage bool
	}{
		{
			name: "unused zero bucket", output: `[]`,
			imageUse: `{"input_tokens":0,"output_tokens":0,"total_tokens":0}`,
		},
		{
			name:     "text response with unused image capability",
			output:   `[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"synthetic text"}]}]`,
			imageUse: `{"input_tokens":0,"output_tokens":0,"total_tokens":0}`,
		},
		{
			name: "completed zero usage image", output: `[{"type":"image_generation_call","status":"completed","result":"AA=="}]`,
			imageUse: `{"input_tokens":0,"output_tokens":0,"total_tokens":0}`, wantImage: true,
		},
		{
			name: "legacy completed image result", output: `[{"type":"image_generation_call","result":"AA=="}]`,
			imageUse: `{"input_tokens":0,"output_tokens":0,"total_tokens":0}`, wantImage: true,
		},
		{
			name: "unfinished zero usage image", output: `[{"type":"image_generation_call","status":"in_progress"}]`,
			imageUse: `{"input_tokens":0,"output_tokens":0,"total_tokens":0}`,
		},
		{
			name: "failed zero usage image", output: `[{"type":"image_generation_call","status":"failed"}]`,
			imageUse: `{"input_tokens":0,"output_tokens":0,"total_tokens":0}`,
		},
		{
			name: "positive image accounting without output", output: `[]`,
			imageUse: `{"input_tokens":7,"output_tokens":9,"total_tokens":16}`, wantImage: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager := usage.NewManager(4)
			capture := &captureUsagePlugin{records: make(chan usage.Record, 4)}
			manager.Register(capture)
			ctx := usage.WithManager(context.Background(), manager)
			reporter := &usageReporter{provider: "codex", model: "gpt-main", alias: "gpt-main"}
			reporter.publish(ctx, usage.Detail{InputTokens: 10, OutputTokens: 2, TotalTokens: 12})
			completed := []byte(fmt.Sprintf(
				`{"response":{"output":%s,"tool_usage":{"image_gen":%s}}}`,
				tt.output, tt.imageUse,
			))
			publishCodexImageToolUsage(ctx, reporter,
				[]byte(`{"tools":[{"type":"image_generation","model":"gpt-image-2"}]}`), completed)
			manager.Stop()
			close(capture.records)
			mainCount, imageCount := 0, 0
			for record := range capture.records {
				switch record.Model {
				case "gpt-main":
					mainCount++
					if record.Detail.TotalTokens != 12 {
						t.Fatalf("main usage changed: %+v", record.Detail)
					}
				case "gpt-image-2":
					imageCount++
				default:
					t.Fatalf("unexpected model: %q", record.Model)
				}
			}
			wantImages := 0
			if tt.wantImage {
				wantImages = 1
			}
			if mainCount != 1 || imageCount != wantImages {
				t.Fatalf("published main=%d image=%d, want main=1 image=%d", mainCount, imageCount, wantImages)
			}
		})
	}
}
