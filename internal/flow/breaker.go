package flow

import (
	"fmt"
	"log/slog"
	"sync"

	"github.com/voocel/ainovel-cli/internal/domain"
	storepkg "github.com/voocel/ainovel-cli/internal/store"
	"github.com/voocel/ainovel-cli/internal/tools"
)

// ── P1-7：章节无进展熔断器（外层防护）─────────────────────────────────
//
// 背景：ch450 死锁的第三层缺陷——外层 StopGuard 只终止单轮，不持久化"章节已进入
// 不可恢复死锁"的结论；调度器（Route 的 writer 派发）会无限重派同一章烧钱。
// FSM/guard 修复（P1-5/P1-6）堵住了已知漏洞，但配置错误、数据损坏或未知组合
// 仍可能让同一章被反复派发且状态毫无变化。本熔断器在派发 writer 前记录每章
// 状态快照，连续 3 轮同一章快照完全一致且期间无新 checkpoint/草稿/ledger 变化
// → 不再自动派发该章，输出确定性 blocked 原因（含 stage/required/errorCode）
// 并标记 manual_recovery_required。
//
// 设计约束：
//   - Route（router.go）保持纯函数（穷举测试锁定其确定性）；熔断器是独立的
//     有状态包装。统一观察入口是 Guard(st, inst)——Engine 在最终确定 writer
//     指令后调用，覆盖所有派发来源（正常路由 / initial 指令 / Arbiter reroute /
//     干预派发，P1-9 阻塞项 9.3）；Route(st) 是 Guard+Commit 的便捷组合（测试
//     与简单场景用）。
//   - 只计实际派发（复核缺口 2）：Guard 只预演 + 熔断判定，计数由 Commit 在
//     Engine 实际派发点（runWorker 前）提交；trackDeadlock 咨询/reroute、Arbiter
//     abort、读错暂停、上下文取消等未派发分支的预演自然丢弃，不累计停滞计数。
//   - 只拦截 writer 章节派发；editor/architect 指令与语义停机（nil）原样透传。
//     章节号由 Engine 在派发管线归一化（复核缺口 1：Chapter<=0 的 writer 指令
//     在 precheck 统一绑定目标章节，无法推导时显式暂停，不静默绕过熔断）。
//   - 快照捕获失败（store 读错）→ 保守放行（与 LoadState 的保守默认一致：
//     读取失败倾向重派而非因熔断器自身故障卡死流水线）。
//   - 任何变化（本章 checkpoint seq / 草稿 digest / 账本状态 / FSM 阶段 /
//     required）→ 重置计数；人工干预（修复 ledger/草稿）后状态变化即自动恢复派发。
//   - 进展信号按章（本章最新 checkpoint seq，P1-9 建议项）：全局 seq 会被其他
//     章节的 checkpoint 前进掩盖本章停滞；按章信号不受影响。
//
// 持久化方案说明（最小实现 = 内存级 + 日志）：熔断标记 manual_recovery_required
// 目前只存在于进程内存（Breaker 实例），进程重启后会复发。倾向的持久化位置是
// Progress（domain.Progress 增加 per-chapter 标记字段）或独立 meta 文件
// （如 meta/manual_recovery.json，记录 {chapter, stage, reason, error_code}），
// 启动时 LoadState 读入并让 Route 直接跳过标记章节；本实现先落内存 + 明确日志，
// 待下一步接入持久化（避免重启后复发）。

// defaultStallLimit 是连续同状态派发阈值：3 轮无变化 → 熔断。
// 与 engine 的 deadlockConsultAt(3) 同级：熔断先于 Arbiter 咨询，避免"咨询→retry
// →继续烧钱"的循环（ch450 类场景 Arbiter 常无出路）。
const defaultStallLimit = 3

// ProgressSnapshot 是派发 writer 前记录的每章状态快照（熔断判定依据）。
type ProgressSnapshot struct {
	Chapter        int
	FSMStage       string // ResolveChapterStage 的 stage
	DraftDigest    string // 当前草稿 digest（空 = 无草稿）
	RequiredAction string // FSM 的 required action（空 = blocked/complete 等无建议）
	LastErrorCode  string // 最近一次派发失败的错误码（Engine 经 RecordError 注入）
	LedgerStatus   string // style review 账本当前状态（从 store 加载）
}

// stallState 是单章的连续一致跟踪（只含已提交的实际派发计数）。
type stallState struct {
	snap         ProgressSnapshot
	errorCode    string // 最近一次派发失败错误码（RecordError 注入；独立于 snap 相等性）
	repeats      int
	lastCheckSeq int64 // 派发时本章最新 checkpoint seq：新 checkpoint 即重置计数
}

// pendingObservation 是 Guard 预演后、实际派发提交前的候选观察（缺口 2：
// Guard 只预演不提交，Commit 在真实派发点生效——咨询/abort/读错/取消等未派发
// 分支自然丢弃候选，只计实际派发）。
type pendingObservation struct {
	snap     ProgressSnapshot
	checkSeq int64
	repeats  int // 预演后的计数（已提交计数 + 本轮）
}

// NoProgressBreaker 是章节无进展熔断器（P1-7）。
// 非并发安全于单写者模型（Engine 单 goroutine 串行）；内部仍加锁以便测试并发读。
type NoProgressBreaker struct {
	mu     sync.Mutex
	limit  int
	fsmCfg tools.ChapterFSMConfig
	last   map[int]*stallState
	// pending 是本章 Guard 预演后的候选观察（本轮尚未确认派发）；Commit 消费。
	pending map[int]*pendingObservation
	// manual 是已熔断章节 → 确定性 blocked 原因（等待人工，人工干预后状态变化即解除）。
	manual map[int]string
}

// NewNoProgressBreaker 创建熔断器。fsmCfg 必须与生产 Writer 工具集的章节流水线
// 配置同源（Engine 传入其 fsmConfig），保证 stage/required 解析与工具拦截一致。
func NewNoProgressBreaker(fsmCfg tools.ChapterFSMConfig) *NoProgressBreaker {
	return &NoProgressBreaker{
		limit:   defaultStallLimit,
		fsmCfg:  fsmCfg,
		last:    map[int]*stallState{},
		pending: map[int]*pendingObservation{},
		manual:  map[int]string{},
	}
}

// Route 是带无进展熔断的便捷派发入口（测试与简单场景用）：先按原路由取得指令，
// 再统一经 Guard 预演；未熔断时按"完整派发"语义自动提交计数（Guard+Commit）。
// 生产路径由 Engine 分别调用 Guard（预演/熔断判定）与 Commit（实际派发点提交）。
func (b *NoProgressBreaker) Route(st *storepkg.Store) *Instruction {
	inst := Route(LoadState(st))
	if inst == nil || inst.Agent != "writer" || inst.Chapter <= 0 {
		return inst
	}
	if b.Guard(st, inst) == nil {
		return nil
	}
	b.Commit(inst.Chapter)
	return inst
}

// Guard 是统一熔断预演入口（P1-9 阻塞项 9.3）：对任意来源的 writer 章节指令
// （正常路由 / Engine initial 指令 / Arbiter reroute / 干预派发）做无进展熔断
// 预演——基于已提交（实际派发）的计数计算本轮预演计数，连续 limit 轮完全一致
// 且期间无新 checkpoint/草稿/账本变化 → 返回 nil（不再自动派发该章）并标记
// manual_recovery_required。预演结果暂存 pending，由 Commit 在实际派发点提交
// （缺口 2：咨询/abort/读错/取消等未派发分支不提交，只计实际派发）。
// 非 writer 指令与 nil 原样透传；快照捕获失败（store 读错）→ 保守放行。
//
// stale pending 契约（ora-1 复核）：每次合法 writer Guard 开始时先使该章旧
// pending 失效——上一轮 Guard 成功但未 Commit（咨询/reroute/abort 等未派发分支）
// 遗留的观察不得被本轮 Commit 消费；捕获失败路径因此天然保证 pending 为空
// （读失败轮次不累计停滞计数，且 Commit 提交的快照必为本轮 Guard 的快照）。
func (b *NoProgressBreaker) Guard(st *storepkg.Store, inst *Instruction) *Instruction {
	if inst == nil || inst.Agent != "writer" || inst.Chapter <= 0 {
		return inst
	}
	// 使该章旧 pending 失效（覆盖捕获失败路径：放行前 pending 已为空）。
	b.clearPending(inst.Chapter)
	snap, checkSeq, ok := b.capture(st, inst.Chapter)
	if !ok {
		// 快照捕获失败：保守放行（pending 已清除，后续 Commit 为 no-op）。
		return inst
	}
	return b.guard(inst, snap, checkSeq)
}

// clearPending 使章节的候选观察失效（Guard 入口调用；单写者模型下与 guard 的
// 熔断路径删除互斥，均持有 b.mu）。
func (b *NoProgressBreaker) clearPending(chapter int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.pending, chapter)
}

// Commit 在实际派发点提交本轮 Guard 预演的观察（Engine 在 runWorker 前调用）。
// 无预演（非 writer 轮次 / 未经过 Guard）时 no-op。未调用 Commit 的轮次
// （trackDeadlock 咨询/reroute、Arbiter abort、读错暂停、上下文取消等）预演
// 自然丢弃——计数只反映实际派发。
func (b *NoProgressBreaker) Commit(chapter int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	p := b.pending[chapter]
	if p == nil {
		return
	}
	delete(b.pending, chapter)
	cur := b.last[chapter]
	if cur == nil {
		b.last[chapter] = &stallState{snap: p.snap, errorCode: p.snap.LastErrorCode, repeats: p.repeats, lastCheckSeq: p.checkSeq}
		return
	}
	sameState := snapEqual(cur.snap, p.snap) && cur.lastCheckSeq == p.checkSeq
	cur.snap, cur.repeats, cur.lastCheckSeq = p.snap, p.repeats, p.checkSeq
	if !sameState {
		// 状态变化（含人工干预）：重置错误码并解除熔断（恢复自动派发）。
		cur.errorCode = p.snap.LastErrorCode
		delete(b.manual, chapter)
	}
	// 同状态递增路径保留 errorCode（RecordError 注入的失败码跨轮持续，
	// 供熔断理由报告；Commit 不得用快照的空值覆盖）。
}

// capture 从 store 加载章节状态快照（FSM stage/required + 草稿 digest + 账本
// 状态 + 本章最新 checkpoint seq——按章进展信号，P1-9 建议项）。
// 任一读失败返回 ok=false（调用方保守放行：跳过本轮计数、不因熔断器自身读故障
// 卡死流水线）。复核缺口 3：草稿/账本二次读取失败同样返回 ok=false——不得静默
// 忽略错误、拿"看似完整"的缺字段快照参与计数（注释与实现一致）。
func (b *NoProgressBreaker) capture(st *storepkg.Store, chapter int) (ProgressSnapshot, int64, bool) {
	snap := ProgressSnapshot{Chapter: chapter}
	d, err := tools.ResolveChapterStage(st, chapter, b.fsmCfg)
	if err != nil {
		return ProgressSnapshot{}, 0, false
	}
	snap.FSMStage = string(d.Stage)
	snap.RequiredAction = string(d.Required)
	draft, err := st.Drafts.LoadDraft(chapter)
	if err != nil {
		return ProgressSnapshot{}, 0, false
	}
	if draft != "" {
		snap.DraftDigest = domain.DigestDraft(draft)
	}
	ledger, err := st.StyleReview.Load(chapter)
	if err != nil {
		return ProgressSnapshot{}, 0, false
	}
	if ledger != nil {
		snap.LedgerStatus = string(ledger.CurrentStatus())
	}
	var checkSeq int64
	if cp := st.Checkpoints.Latest(domain.ChapterScope(chapter)); cp != nil {
		checkSeq = cp.Seq
	}
	return snap, checkSeq, true
}

// guard 预演本轮观察并决定是否放行（调用方必须已持有 b.mu 或单写者串行）。
// 只基于已提交（实际派发）的计数计算预演值，不直接写回 last——结果存 pending，
// 由 Commit 在实际派发点提交（缺口 2：未派发分支不计）。
func (b *NoProgressBreaker) guard(inst *Instruction, snap ProgressSnapshot, checkSeq int64) *Instruction {
	b.mu.Lock()
	defer b.mu.Unlock()

	cur := b.last[snap.Chapter]
	repeats := 1
	if cur != nil && snapEqual(cur.snap, snap) && checkSeq == cur.lastCheckSeq {
		repeats = cur.repeats + 1
	}
	if repeats >= b.limit {
		// 熔断返回 nil 时同样清理该章 pending（避免长期残留；后续 Commit 不生效）。
		delete(b.pending, snap.Chapter)
		reason := b.blockedReason(snap.Chapter, snap, cur)
		b.manual[snap.Chapter] = reason
		slog.Warn("章节无进展熔断：同一状态快照连续派发无变化，停止自动重派，等待人工恢复",
			"module", "flow", "chapter", snap.Chapter, "stage", snap.FSMStage,
			"required", snap.RequiredAction, "ledger", snap.LedgerStatus,
			"error_code", b.errorCodeOf(cur), "repeats", repeats, "reason", reason)
		return nil
	}
	b.pending[snap.Chapter] = &pendingObservation{snap: snap, checkSeq: checkSeq, repeats: repeats}
	return inst
}

// errorCodeOf 读取已提交状态的错误码（未跟踪章节返回空）。
func (b *NoProgressBreaker) errorCodeOf(cur *stallState) string {
	if cur == nil {
		return ""
	}
	return cur.errorCode
}

// snapEqual 比较两份快照的判定字段（不含 LastErrorCode——capture 侧恒为空串，
// 错误码由 RecordError 单独跟踪；不含 lastCheckSeq——单独比较）。
func snapEqual(a, b ProgressSnapshot) bool {
	return a.Chapter == b.Chapter &&
		a.FSMStage == b.FSMStage &&
		a.DraftDigest == b.DraftDigest &&
		a.RequiredAction == b.RequiredAction &&
		a.LedgerStatus == b.LedgerStatus
}

// orDash 把空串显示为 none（blocked 原因的可读性）。
func orDash(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// blockedReason 生成确定性 blocked 原因（含 stage/requiredAction/errorCode），
// 供日志与 BlockedReason 查询输出。errorCode 取已提交状态的最近失败错误码。
func (b *NoProgressBreaker) blockedReason(chapter int, snap ProgressSnapshot, cur *stallState) string {
	return fmt.Sprintf("chapter=%d stage=%s required=%s error_code=%s ledger=%s 连续 %d 轮同一状态无任何 checkpoint/草稿/账本变化，已停止自动重派（manual_recovery_required），等待人工修复 ledger/候选状态后继续",
		chapter, orDash(snap.FSMStage), orDash(snap.RequiredAction), orDash(b.errorCodeOf(cur)), orDash(snap.LedgerStatus), b.limit)
}

// ManualRecoveryRequired 报告章节是否处于熔断（等待人工）状态。
// 人工干预使快照变化后自动解除（见 guard 的变化分支）。
func (b *NoProgressBreaker) ManualRecoveryRequired(chapter int) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ok := b.manual[chapter]
	return ok
}

// BlockedReason 返回章节熔断时的确定性 blocked 原因；未熔断返回空串。
func (b *NoProgressBreaker) BlockedReason(chapter int) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.manual[chapter]
}

// RecordError 记录章节最近一次派发失败的稳定错误码（Engine 在 handleWorkerError
// 调用）。错误码变化视为状态变化（重置计数）；相同错误码保持计数。错误码只参与
// 快照相等性的"变化检测"一侧（不同的拒绝原因不应累计为同一无进展序列），
// 失败轮次本身不改变草稿/账本/checkpoint，故不会重置 checkpoint 计数。
func (b *NoProgressBreaker) RecordError(chapter int, code string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	st := b.last[chapter]
	if st == nil {
		return // 未在跟踪（非 writer 派发）
	}
	if st.errorCode != code {
		st.errorCode = code
		st.repeats = 1
	}
}

// ClearError 清理章节的错误码（worker 成功后调用，P1-9 阻塞项 9.2）：只更新
// 熔断理由的错误码字段，不重置无进展计数——成功但无状态变化仍应累计停滞
// （否则"每轮都成功但毫无产出"的循环永远不会熔断）。未跟踪章节 no-op。
func (b *NoProgressBreaker) ClearError(chapter int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if st := b.last[chapter]; st != nil {
		st.errorCode = ""
	}
}
