package agents

import (
	"github.com/voocel/ainovel-cli/internal/models"
)

// ── 角色级 token 预算表（docs/handoff-polisher-style-token-plan.md §6） ──────
//
// 每个角色一行：输入预算（完整上下文目标）、thinking 目标、可见 completion 目标
// 与 max output token 硬上限。build.go 据此配置各 subagent 的 MaxTokens；
// 其余列是观测/规划目标，供后续包与诊断使用。

// RoleBudget 描述单个角色的 token 预算（计划 §6 表格的一行）。
type RoleBudget struct {
	// InputTokens 输入预算（完整上下文目标上限）。
	InputTokens int
	// ThinkingTargetTokens thinking 目标（推理 token 软目标）。
	ThinkingTargetTokens int
	// VisibleCompletionTargetTokens 可见 completion 目标（软目标）。
	VisibleCompletionTargetTokens int
	// MaxOutputTokens max output token 硬上限（MaxTokens 配置用）。
	MaxOutputTokens int
}

// roleBudgets 是计划 §6 表格的精确值。key 为 subagent 名
// （architect_short/architect_long/writer/editor/style_critic/critic/polisher）。
var roleBudgets = map[string]RoleBudget{
	"architect_short": {InputTokens: 96_000, ThinkingTargetTokens: 12_000, VisibleCompletionTargetTokens: 16_000, MaxOutputTokens: 32_768},
	"architect_long":  {InputTokens: 192_000, ThinkingTargetTokens: 24_000, VisibleCompletionTargetTokens: 32_000, MaxOutputTokens: 65_536},
	"writer":          {InputTokens: 192_000, ThinkingTargetTokens: 24_000, VisibleCompletionTargetTokens: 32_000, MaxOutputTokens: 65_536},
	"editor":          {InputTokens: 160_000, ThinkingTargetTokens: 16_000, VisibleCompletionTargetTokens: 16_000, MaxOutputTokens: 32_768},
	"style_critic":    {InputTokens: 48_000, ThinkingTargetTokens: 8_000, VisibleCompletionTargetTokens: 4_000, MaxOutputTokens: 16_384},
	"critic":          {InputTokens: 48_000, ThinkingTargetTokens: 8_000, VisibleCompletionTargetTokens: 4_000, MaxOutputTokens: 16_384},
	"polisher":        {InputTokens: 160_000, ThinkingTargetTokens: 24_000, VisibleCompletionTargetTokens: 24_000, MaxOutputTokens: 65_536},
}

// RoleBudgetFor 返回某角色的预算；未知角色返回零值与 false。
func RoleBudgetFor(role string) (RoleBudget, bool) {
	b, ok := roleBudgets[role]
	return b, ok
}

// roleMaxOutput 返回某角色的预算表 max output token（角色必须已知；未知返回 0）。
func roleMaxOutput(role string) int {
	b, ok := RoleBudgetFor(role)
	if !ok {
		return 0
	}
	return b.MaxOutputTokens
}

// ── Polisher per-call 输出目标（计划 §6 / schema §11，导出供包 6 使用） ──────

const (
	// PolisherPlanVisibleTokens plan 可见输出目标 ≤8k tokens。
	PolisherPlanVisibleTokens = 8_000
	// PolisherBatchVisibleTokens 单 batch 可见输出目标 ≤24k tokens。
	PolisherBatchVisibleTokens = 24_000
	// PolisherBatchRawArgsBytes 单 batch 原始工具参数 ≤48 KiB。
	PolisherBatchRawArgsBytes = 48 * 1024
	// PolisherFinishVisibleTokens finish 可见输出目标 ≤4k tokens。
	PolisherFinishVisibleTokens = 4_000
)

// ── 131,072 override（计划 §6：仅作为经过验证的精确模型/provider 兼容开关） ──

// polisherHighOutputOverrideTokens 是 override 放宽后的 max output 上限。
const polisherHighOutputOverrideTokens = 131_072

// polisherHighOutputModels 是经过验证、真实输出上限为 131,072 tokens 的
// polisher 模型名（精确匹配，不做模糊解析）。默认关闭：未列入的模型一律使用
// 预算表默认 65,536；新增条目必须先验证该模型/provider 的真实 max output 上限
//（如 OpenRouter top_provider.max_completion_tokens 多方一致），不能全局提升。
//
// 已验证：mimo-v2.5（小米 MiMo-V2.5，真实输出上限 131072 tokens，OpenRouter
// top_provider.max_completion_tokens 四方一致——原 build.go 硬编码 131072 的
// 依据，现收敛为按模型名精确匹配的 override 条目）。
var polisherHighOutputModels = map[string]bool{
	"mimo-v2.5": true,
}

// PolisherHighOutputEnabled 报告模型名是否命中 131,072 override（精确匹配）。
func PolisherHighOutputEnabled(modelName string) bool {
	return polisherHighOutputModels[modelName]
}

// minOutputSafetyMargin 是 max output 相对上下文窗口保留的安全余量（计划 §6：
// 保留至少 8k 安全余量）。
const minOutputSafetyMargin = 8_000

// PolisherMaxOutputTokens 计算 polisher 角色实际生效的 max output token：
//   - 默认上限 = 预算表 65,536；
//   - 模型名精确命中 polisherHighOutputModels 时放宽到 131,072（已验证兼容开关）；
//   - 最终取 min(上限, 模型 registry 上限(若已知), contextWindow-8k 安全余量)。
//
// provider 确认上限由 provider 侧在运行时 clamp，此处不重复建模。
func PolisherMaxOutputTokens(modelName string, contextWindow int) int {
	b, _ := RoleBudgetFor("polisher")
	limit := b.MaxOutputTokens
	if PolisherHighOutputEnabled(modelName) {
		limit = polisherHighOutputOverrideTokens
	}
	return clampMaxOutput(limit, modelName, contextWindow)
}

// clampMaxOutput 按 min(上限, registry 上限, contextWindow-安全余量) 收敛。
// modelName 为空或 registry 未命中时跳过 registry 约束；contextWindow<=0 或
// 窗口不足以容纳安全余量时跳过上下文空间约束。
func clampMaxOutput(limit int, modelName string, contextWindow int) int {
	if modelName != "" {
		if entry, ok := models.DefaultRegistry().Resolve(modelName); ok && entry.MaxTokens > 0 && entry.MaxTokens < limit {
			limit = entry.MaxTokens
		}
	}
	if contextWindow > 0 {
		if remain := contextWindow - minOutputSafetyMargin; remain > 0 && remain < limit {
			limit = remain
		}
	}
	return limit
}