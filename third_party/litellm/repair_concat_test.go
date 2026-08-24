package litellm

import (
	"encoding/json"
	"testing"
)

func TestRepairConcatenatedJSONObjects(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    string
		wantOK  bool
	}{
		{
			name:   "empty object concatenated with real object (relay bug shape)",
			raw:    `{}{"chapter":40}`,
			want:   `{"chapter":40}`,
			wantOK: true,
		},
		{
			name:   "later keys win on conflict",
			raw:    `{"a":1}{"a":2,"b":3}`,
			want:   `{"a":2,"b":3}`,
			wantOK: true,
		},
		{
			name:   "three objects merged",
			raw:    `{"a":1}{"b":2}{"c":3}`,
			want:   `{"a":1,"b":2,"c":3}`,
			wantOK: true,
		},
		{
			name:   "single valid object is not repaired",
			raw:    `{"chapter":40}`,
			wantOK: false,
		},
		{
			name:   "truncated json is not repaired",
			raw:    `{"q":`,
			wantOK: false,
		},
		{
			name:   "empty input is not repaired",
			raw:    ``,
			wantOK: false,
		},
		{
			name:   "non-object leading value is not repaired",
			raw:    `[1,2]{"a":1}`,
			wantOK: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := repairConcatenatedJSONObjects([]byte(tt.raw))
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if !tt.wantOK {
				return
			}
			var gotVal, wantVal any
			if err := json.Unmarshal(got, &gotVal); err != nil {
				t.Fatalf("repaired output is not valid JSON: %v (%s)", err, got)
			}
			if err := json.Unmarshal([]byte(tt.want), &wantVal); err != nil {
				t.Fatalf("bad test expectation: %v", err)
			}
			gotJSON, _ := json.Marshal(gotVal)
			wantJSON, _ := json.Marshal(wantVal)
			if string(gotJSON) != string(wantJSON) {
				t.Fatalf("merged = %s, want %s", gotJSON, wantJSON)
			}
		})
	}
}

// TestNormalizeInvalidToolArgumentsRepairsConcatenated verifies the stream-side
// hook repairs the relay bug shape in place instead of flagging it malformed.
func TestNormalizeInvalidToolArgumentsRepairsConcatenated(t *testing.T) {
	tool := ToolUseBlock{
		ID:        "call_1",
		Name:      "read_chapter",
		Arguments: []byte(`{}{"chapter":40}`),
	}
	if w := normalizeInvalidToolArguments(&tool); w != nil {
		t.Fatalf("expected repair (nil warning), got warning: %+v", w)
	}
	var val map[string]any
	if err := json.Unmarshal(tool.Arguments, &val); err != nil {
		t.Fatalf("arguments still invalid after repair: %v (%s)", err, tool.Arguments)
	}
	if v, ok := val["chapter"].(float64); !ok || v != 40 {
		t.Fatalf("chapter = %v, want 40", val["chapter"])
	}
}

// TestAppendMalformedToolArgumentWarningsRepairs verifies the non-stream path
// repairs the arguments in place and emits no warning for the repaired call.
func TestAppendMalformedToolArgumentWarningsRepairs(t *testing.T) {
	resp := &Response{
		Blocks: []Block{
			ToolUseBlock{ID: "call_1", Name: "read_chapter", Arguments: []byte(`{}{"chapter":40}`)},
			TextBlock{Text: "hi"},
		},
	}
	appendMalformedToolArgumentWarnings(resp)
	if len(resp.Warnings) != 0 {
		t.Fatalf("expected no warnings after repair, got %+v", resp.Warnings)
	}
	tool, ok := resp.Blocks[0].(ToolUseBlock)
	if !ok {
		t.Fatal("block 0 is not a ToolUseBlock")
	}
	var val map[string]any
	if err := json.Unmarshal(tool.Arguments, &val); err != nil {
		t.Fatalf("arguments invalid after repair: %v (%s)", err, tool.Arguments)
	}
	if v, ok := val["chapter"].(float64); !ok || v != 40 {
		t.Fatalf("chapter = %v, want 40", val["chapter"])
	}
}

// TestAppendMalformedToolArgumentWarningsStillFlagsUnrepairable keeps the
// existing fail-visible behavior for shapes the repair cannot fix.
func TestAppendMalformedToolArgumentWarningsStillFlagsUnrepairable(t *testing.T) {
	resp := &Response{
		Blocks: []Block{
			ToolUseBlock{ID: "call_bad", Name: "lookup", Arguments: []byte(`{"q":`)},
		},
	}
	appendMalformedToolArgumentWarnings(resp)
	if len(resp.Warnings) != 1 {
		t.Fatalf("expected 1 warning for unrepairable args, got %d", len(resp.Warnings))
	}
	if resp.Warnings[0].Code != "tool_arguments_invalid" {
		t.Fatalf("unexpected code: %s", resp.Warnings[0].Code)
	}
}
