package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestRunRejectsProviderCallsAndMissingArguments(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := run(context.Background(), []string{"scene-beat-v3", "draft", "--book-dir", "missing", "--allow-provider-calls"}, &stdout, &stderr); err == nil || !strings.Contains(err.Error(), "--provider-config is required") {
		t.Fatalf("provider-call gate error = %v", err)
	}
	if err := run(context.Background(), []string{"scene-beat-v3", "verify"}, &stdout, &stderr); err == nil || !strings.Contains(err.Error(), "required") {
		t.Fatalf("missing argument error = %v", err)
	}
	if err := run(context.Background(), []string{"scene-beat-v3", "apply"}, &stdout, &stderr); err == nil || !strings.Contains(err.Error(), "confirm-apply") {
		t.Fatalf("unconfirmed apply error = %v", err)
	}
}
