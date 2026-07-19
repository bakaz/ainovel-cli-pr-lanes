package tui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/voocel/ainovel-cli/internal/tools"
)

func TestAskUserBridgeContextCancellationClosesMatchingModal(t *testing.T) {
	bridge := newAskUserBridge()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := bridge.handler(ctx, []tools.Question{{
			Header: "长线更新审批", Question: "批准吗？",
			Options: []tools.Option{{Label: "批准", Description: "写入"}, {Label: "拒绝", Description: "不写入"}},
		}})
		done <- err
	}()

	req := <-bridge.requests
	cancel()
	select {
	case cancellation := <-bridge.cancellations:
		if cancellation.id != req.id {
			t.Fatalf("cancel id=%d, want %d", cancellation.id, req.id)
		}
	case <-time.After(time.Second):
		t.Fatal("timed-out request did not emit modal cancellation")
	}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("handler err=%v, want context.Canceled", err)
	}
}

func TestLongApprovalModalUsesDedicatedTitle(t *testing.T) {
	req := askUserRequest{id: 1, questions: []tools.Question{{
		Header: "长线更新审批", Question: "批准吗？",
		Options: []tools.Option{{Label: "批准", Description: "写入"}, {Label: "拒绝", Description: "不写入"}},
	}}}
	view := renderAskUserModal(100, 40, newAskUserState(req))
	if !strings.Contains(view, "compass.long 更新审批") {
		t.Fatalf("dedicated approval title missing: %q", view)
	}
}
