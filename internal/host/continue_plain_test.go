package host

import (
	"testing"

	"github.com/voocel/ainovel-cli/internal/domain"
)

func TestContinuePlainSkipsArbiterAndCancelsHold(t *testing.T) {
	h := newTestHost(t, initTestProject(t))
	defer h.Close()

	progress, err := h.store.Progress.Load()
	if err != nil || progress == nil {
		t.Fatalf("load progress: %v", err)
	}
	// 完本状态只用于让测试停在恢复入口，不启动真实 Worker；核心断言是
	// 精确“继续”不经过 Arbiter，且仍能消费遗留的一次性暂停。
	progress.Phase = domain.PhaseComplete
	if err := h.store.Progress.Save(progress); err != nil {
		t.Fatalf("save progress: %v", err)
	}
	if err := h.store.RunMeta.SetAdvanceHold(domain.AdvanceHold{
		After:  domain.AdvanceHoldAtBoundary,
		Reason: "上一轮误设置的暂停",
	}); err != nil {
		t.Fatalf("set hold: %v", err)
	}

	testBeforeArbiter = func() error {
		t.Fatal("精确继续不应调用 Arbiter")
		return nil
	}
	defer func() { testBeforeArbiter = nil }()

	outcome, err := h.ContinueAndWait(" \n继续\t")
	if err != nil {
		t.Fatalf("ContinueAndWait: %v", err)
	}
	if !outcome.OK || outcome.EngineRunning {
		t.Fatalf("outcome = %+v, want successful non-running recovery", outcome)
	}
	meta, err := h.store.RunMeta.Load()
	if err != nil || meta == nil {
		t.Fatalf("load run meta: %v", err)
	}
	if meta.AdvanceHold != nil {
		t.Fatalf("plain continue must cancel hold, got %+v", meta.AdvanceHold)
	}
}

func TestContinuePlainRefusesPendingRealIntervention(t *testing.T) {
	h := newTestHost(t, initTestProject(t))
	defer h.Close()

	if err := h.store.RunMeta.SetPendingSteer("重写第782章的结尾"); err != nil {
		t.Fatalf("set pending steer: %v", err)
	}
	outcome, err := h.ContinueAndWait("继续")
	if err != nil {
		t.Fatalf("ContinueAndWait: %v", err)
	}
	if outcome.OK {
		t.Fatalf("plain continue must not bypass pending intervention: %+v", outcome)
	}
	meta, err := h.store.RunMeta.Load()
	if err != nil || meta == nil {
		t.Fatalf("load run meta: %v", err)
	}
	if meta.PendingSteer != "重写第782章的结尾" {
		t.Fatalf("pending intervention was modified: %q", meta.PendingSteer)
	}
}
