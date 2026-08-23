package agents

import "testing"

// TestRoleBudgetTable 验证预算表（docs/handoff-polisher-style-token-plan.md §6）
// 的精确值：输入预算 / thinking 目标 / 可见 completion 目标 / max output token。
func TestRoleBudgetTable(t *testing.T) {
	cases := []struct {
		role  string
		in    int
		think int
		vis   int
		max   int
	}{
		{"architect_short", 96_000, 12_000, 16_000, 32_768},
		{"architect_long", 192_000, 24_000, 32_000, 65_536},
		{"writer", 192_000, 24_000, 32_000, 65_536},
		{"editor", 160_000, 16_000, 16_000, 32_768},
		{"style_critic", 48_000, 8_000, 4_000, 16_384},
		{"critic", 48_000, 8_000, 4_000, 16_384},
		{"polisher", 160_000, 24_000, 24_000, 65_536},
	}
	for _, tc := range cases {
		t.Run(tc.role, func(t *testing.T) {
			b, ok := RoleBudgetFor(tc.role)
			if !ok {
				t.Fatalf("RoleBudgetFor(%q) not found", tc.role)
			}
			if b.InputTokens != tc.in || b.ThinkingTargetTokens != tc.think ||
				b.VisibleCompletionTargetTokens != tc.vis || b.MaxOutputTokens != tc.max {
				t.Errorf("RoleBudgetFor(%q) = %+v, want in=%d think=%d vis=%d max=%d",
					tc.role, b, tc.in, tc.think, tc.vis, tc.max)
			}
		})
	}
	if _, ok := RoleBudgetFor("unknown_role"); ok {
		t.Error("unknown role must not have a budget")
	}
}

// TestPolisherPerCallLimits 验证 polisher per-call 输出目标常量（计划 §6 /
// schema §11，导出供包 6 使用）。
func TestPolisherPerCallLimits(t *testing.T) {
	if PolisherPlanVisibleTokens != 8_000 {
		t.Errorf("PolisherPlanVisibleTokens = %d, want 8000", PolisherPlanVisibleTokens)
	}
	if PolisherBatchVisibleTokens != 24_000 {
		t.Errorf("PolisherBatchVisibleTokens = %d, want 24000", PolisherBatchVisibleTokens)
	}
	if PolisherBatchRawArgsBytes != 48*1024 {
		t.Errorf("PolisherBatchRawArgsBytes = %d, want %d", PolisherBatchRawArgsBytes, 48*1024)
	}
	if PolisherFinishVisibleTokens != 4_000 {
		t.Errorf("PolisherFinishVisibleTokens = %d, want 4000", PolisherFinishVisibleTokens)
	}
}

// TestPolisherHighOutputOverride 验证 131,072 override 的精确匹配语义：
// 默认关闭（未列入的模型不命中），按模型名精确匹配（不做模糊解析）。
func TestPolisherHighOutputOverride(t *testing.T) {
	if PolisherHighOutputEnabled("mimo-polisher") {
		t.Error("non-listed model must not hit the override")
	}
	if PolisherHighOutputEnabled("") {
		t.Error("empty model must not hit the override")
	}
	if !PolisherHighOutputEnabled("mimo-v2.5") {
		t.Error("mimo-v2.5 must hit the override (verified model)")
	}
	if PolisherHighOutputEnabled("mimo-v2.5-pro") {
		t.Error("mimo-v2.5-pro must NOT hit the override (exact match only)")
	}
}

// TestPolisherMaxOutputTokens 验证 max output 计算：
// min(上限, 模型 registry 上限, contextWindow-8k 安全余量)（计划 §6）。
func TestPolisherMaxOutputTokens(t *testing.T) {
	// 默认：预算表 65,536（registry 未命中、窗口足够大时不收敛）。
	if got := PolisherMaxOutputTokens("mimo-polisher", 200_000); got != 65_536 {
		t.Errorf("default = %d, want 65536", got)
	}
	// override：mimo-v2.5 → 131,072（registry 上限 131072 一致，不收敛）。
	if got := PolisherMaxOutputTokens("mimo-v2.5", 1_048_576); got != 131_072 {
		t.Errorf("override = %d, want 131072", got)
	}
	// registry 上限收敛：deepseek-chat registry MaxTokens=16000 < 65536。
	if got := PolisherMaxOutputTokens("deepseek-chat", 200_000); got != 16_000 {
		t.Errorf("registry clamp = %d, want 16000", got)
	}
	// 上下文空间收敛：窗口 32k → 32k-8k=24k < 65536。
	if got := PolisherMaxOutputTokens("mimo-polisher", 32_768); got != 24_768 {
		t.Errorf("context clamp = %d, want 24768", got)
	}
	// 窗口不足以容纳 8k 安全余量时跳过上下文约束，回落预算默认。
	if got := PolisherMaxOutputTokens("mimo-polisher", 4_000); got != 65_536 {
		t.Errorf("tiny window = %d, want 65536 (skip context clamp)", got)
	}
	// 空模型名：跳过 registry 约束。
	if got := PolisherMaxOutputTokens("", 200_000); got != 65_536 {
		t.Errorf("empty model = %d, want 65536", got)
	}
}