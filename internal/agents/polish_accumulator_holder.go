package agents

import (
	"sync"

	"github.com/voocel/ainovel-cli/internal/tools"
)

// PolishAccumulatorHolder 线程安全持有 *tools.PolishAccumulator（初始 nil）。
//
// 背景：三个候选工具（submit_polish_plan / submit_edit_batch / finish_polish）
// 在 build.go 构建时注册进 polisher runner，但 accumulator 是 per-run 的——
// polish_draft 每次 run 创建（包 6 接线）。holder 是构建时注册与运行时注入之间
// 的桥：build.go 创建 holder 并把 holder.Get()（构建时为 nil）注入三个工具；
// 包 6 在每次 run 开始时创建 accumulator 并经 holder 注入工具（工具 Execute 在
// 未注入时返回明确错误，不会静默空转）。
type PolishAccumulatorHolder struct {
	mu  sync.RWMutex
	acc *tools.PolishAccumulator
}

// NewPolishAccumulatorHolder 构造 holder（初始持有 nil）。
func NewPolishAccumulatorHolder() *PolishAccumulatorHolder {
	return &PolishAccumulatorHolder{}
}

// Set 注入/替换当前 run 的 accumulator（包 6 每次 run 开始时调用）。
func (h *PolishAccumulatorHolder) Set(acc *tools.PolishAccumulator) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.acc = acc
}

// Get 返回当前持有的 accumulator（未注入时为 nil）。
func (h *PolishAccumulatorHolder) Get() *tools.PolishAccumulator {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.acc
}