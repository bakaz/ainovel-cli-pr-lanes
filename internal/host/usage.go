package host

import (
	"log/slog"
	"sort"
	"strings"
	"sync"

	"github.com/voocel/agentcore"
	"github.com/voocel/ainovel-cli/internal/bootstrap"
	"github.com/voocel/ainovel-cli/internal/models"
)

// recentSampleCap 是滑动窗大小：只保留每个 role 最近 N 次调用的 (cacheRead, input)
// 样本，用于在左栏对比"累计 vs 近 N 次"命中率，识别"前期拖累"vs"稳态低命中"。
const recentSampleCap = 10

// UsageTracker 累计整个会话所有 agent 的 LLM 输入/输出 token 与美元成本。
//
// 工作机制：
//   - 每次 agent 的 OnMessage 回调触发时调用 Record(agentName, msg)
//   - agentName 映射到 role（architect_* 归一为 architect），查 ModelSet 当前该 role 绑定的模型
//   - 用 models.DefaultRegistry 查模型价格，按非缓存输入/输出/缓存读/缓存写四项累乘
//   - 注册表无此模型时，退回 msg.Usage.Cost.Total（provider 自带，可能为 0）
//   - 模型热切换（/model）后续消息自动按新模型算价，旧消息保留旧成本
//
// 同时维护 per-role 维度（writer/editor/architect/coordinator）：
//   - 累计命中数据 → 整体优化效果
//   - 滑动窗最近 N 次 → 区分前期拖累 vs 稳态低命中
//   - CacheCapable 标记 → 区分"未启用"和"真的 0% 命中"
//
// 线程安全。
type UsageTracker struct {
	mu       sync.Mutex
	overall  agentTotals
	perAgent map[string]*agentTotals // key 为 agentRoleName 归一后的 role 名
	modelSet *bootstrap.ModelSet

	// missingAssistantUsage 累计"收到 assistant 消息但 Usage 为 nil"的次数。
	// 实测下来主要发生在自建 OpenAI 兼容 backend 没在 streaming 末尾按 OpenAI
	// stream_options.include_usage 协议发那条 final usage chunk 时——partial.Usage
	// 始终为 nil，所有累计字段全部停在 0。计数器让 UI 能直接告诉用户"是上游不返
	// usage 不是这边坏了"，而不是死磕缓存面板代码。
	missingAssistantUsage int
	loggedMissingUsage    bool // 整个会话只 warn 一次，避免 tui.log 被刷屏
}

// usageSample 是单次 OnMessage 的命中样本，仅记录命中率分子分母。
type usageSample struct {
	CacheRead int
	Input     int
}

// agentTotals 是一个 agent 的累计计数。
//   - Saved 是按当前命中数据反算的"如果按非缓存价计费"的差额
//   - CacheCapable 仅在该 role 至少经过一次"已知支持 cache 的模型"调用后置 true
//   - samples 是定长 ring buffer，前 recentSampleCap 次直接追加，之后按 sampleIdx 轮转
type agentTotals struct {
	Input        int
	Output       int
	CacheRead    int
	CacheWrite   int
	Cost         float64
	Saved        float64
	CacheCapable bool
	samples      []usageSample
	sampleIdx    int
}

func NewUsageTracker(set *bootstrap.ModelSet) *UsageTracker {
	return &UsageTracker{
		modelSet: set,
		perAgent: make(map[string]*agentTotals, 4),
	}
}

// Record 把一条 agent 消息分发到累加 / 诊断两条路径。
//
// 累加只看 Usage 是否存在——"哪条消息带 Usage" 是 agentcore/litellm adapter
// 装配细节（上游协议把 usage 放在响应顶层），未来装配规则变了也不用动这里。
// 诊断要求 Role=Assistant 且 Content 非空，避免 AbortMsg / 异常恢复 / tool /
// user 消息污染 missingAssistantUsage 计数。
func (t *UsageTracker) Record(agentName string, msg agentcore.AgentMessage) {
	if t == nil {
		return
	}
	m, ok := msg.(agentcore.Message)
	if !ok {
		return
	}
	if m.Usage == nil {
		if m.Role == agentcore.RoleAssistant && len(m.Content) > 0 {
			t.flagMissingUsage(agentName)
		}
		return
	}
	t.accumulate(agentRoleName(agentName), *m.Usage)
}

// flagMissingUsage 累计一次"看似真 LLM 响应却没拿到 usage"事件，整会话只打一次
// warn 日志避免 tui.log 被刷屏。
func (t *UsageTracker) flagMissingUsage(agentName string) {
	t.mu.Lock()
	t.missingAssistantUsage++
	shouldLog := !t.loggedMissingUsage
	t.loggedMissingUsage = true
	t.mu.Unlock()
	if shouldLog {
		slog.Warn("LLM 响应未携带 usage 数据，缓存/成本面板将无累计——通常是上游 streaming 未按 OpenAI include_usage 协议发 final usage chunk",
			"module", "usage", "agent", agentName)
	}
}

// accumulate 把一条带 Usage 的消息累计到 overall 和 per-role 两份计数。
// resolveCost 在锁外执行（它只读 modelSet/Registry），锁内只做加法。
func (t *UsageTracker) accumulate(role string, u agentcore.Usage) {
	cost, saved, capable := t.resolveCost(role, u)

	t.mu.Lock()
	defer t.mu.Unlock()

	addUsage(&t.overall, u, cost, saved, capable)

	per := t.perAgent[role]
	if per == nil {
		per = &agentTotals{}
		t.perAgent[role] = per
	}
	addUsage(per, u, cost, saved, capable)
}

// addUsage 把单次调用的 token 与成本叠加到一份 totals 上。
// 必须在持有 UsageTracker.mu 的情况下调用。
//
// CacheCapable 优先用"事实"判定：只要见过 CacheRead 或 CacheWrite > 0，就证明
// 上游确实做了 prompt caching。注册表的 CacheReadCostPer1M 仅作 fallback，
// 因为自建 backend 模型（mimo-v2.5-pro / 国内代理等）通常不在 BerriAI/litellm
// pricing 索引里，但实际 Usage 里完全有 cache 数据，UI 不该误判为"未启用"。
func addUsage(t *agentTotals, u agentcore.Usage, cost, saved float64, capable bool) {
	t.Input += u.Input
	t.Output += u.Output
	t.CacheRead += u.CacheRead
	t.CacheWrite += u.CacheWrite
	t.Cost += cost
	t.Saved += saved
	if capable || u.CacheRead > 0 || u.CacheWrite > 0 {
		t.CacheCapable = true
	}
	pushSample(t, u.CacheRead, u.Input)
}

// pushSample 向 ring buffer 推一个样本。前 recentSampleCap 次纯 append，之后轮转覆盖。
func pushSample(t *agentTotals, cacheRead, input int) {
	s := usageSample{CacheRead: cacheRead, Input: input}
	if len(t.samples) < recentSampleCap {
		t.samples = append(t.samples, s)
		return
	}
	t.samples[t.sampleIdx] = s
	t.sampleIdx = (t.sampleIdx + 1) % recentSampleCap
}

// recentSums 返回滑动窗内 cacheRead 和 input 的总和，作为"近 N 次命中率"的分子分母。
// 用 sum/sum 而非"单次比率的平均"以避免小样本（input=几百 token）放大噪声。
func recentSums(t *agentTotals) (cacheRead, input int) {
	for _, s := range t.samples {
		cacheRead += s.CacheRead
		input += s.Input
	}
	return cacheRead, input
}

// Totals 返回累计总量的快照。
func (t *UsageTracker) Totals() (cost float64, input, output, cacheRead, cacheWrite int) {
	if t == nil {
		return 0, 0, 0, 0, 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.overall.Cost, t.overall.Input, t.overall.Output, t.overall.CacheRead, t.overall.CacheWrite
}

// SavedUSD 返回因缓存命中节省的累计美元数。
func (t *UsageTracker) SavedUSD() float64 {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.overall.Saved
}

// OverallRecent 返回滑动窗内（≤ recentSampleCap 次）的 cacheRead 总和、input 总和、样本数。
func (t *UsageTracker) OverallRecent() (cacheRead, input, samples int) {
	if t == nil {
		return 0, 0, 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	r, in := recentSums(&t.overall)
	return r, in, len(t.overall.samples)
}

// OverallCacheCapable 整体是否至少经过一次已知支持 cache 的模型。
func (t *UsageTracker) OverallCacheCapable() bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.overall.CacheCapable
}

// MissingAssistantUsage 返回累计"收到 assistant 消息但 Usage 为 nil"的次数。
// 大于 0 通常意味着上游 streaming 没发 OpenAI 的 final usage chunk，
// UI 据此显示提示而非误以为缓存模块本身坏了。
func (t *UsageTracker) MissingAssistantUsage() int {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.missingAssistantUsage
}

// AgentUsage 是一个 agent 的累计用量快照（向 UI 暴露）。
type AgentUsage struct {
	Role            string
	Input           int
	Output          int
	CacheRead       int
	CacheWrite      int
	Cost            float64
	Saved           float64
	CacheCapable    bool
	RecentCacheRead int
	RecentInput     int
	RecentSamples   int
}

// PerAgent 返回各 role 累计用量。结果按 CacheRead 数量降序，未消费过 token 的 role 跳过。
func (t *UsageTracker) PerAgent() []AgentUsage {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]AgentUsage, 0, len(t.perAgent))
	for role, v := range t.perAgent {
		if v.Input == 0 && v.Output == 0 {
			continue
		}
		recentRead, recentInput := recentSums(v)
		out = append(out, AgentUsage{
			Role:            role,
			Input:           v.Input,
			Output:          v.Output,
			CacheRead:       v.CacheRead,
			CacheWrite:      v.CacheWrite,
			Cost:            v.Cost,
			Saved:           v.Saved,
			CacheCapable:    v.CacheCapable,
			RecentCacheRead: recentRead,
			RecentInput:     recentInput,
			RecentSamples:   len(v.samples),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CacheRead != out[j].CacheRead {
			return out[i].CacheRead > out[j].CacheRead
		}
		return out[i].Input > out[j].Input
	})
	return out
}

// resolveCost 同时返回本次消息的 cost / saved / capable。
//   - cost: 注册表命中按 4 项累乘；未命中回落 provider 自带 cost
//   - saved: 仅注册表命中、CacheRead > 0、且 InputCost > CacheReadCost 时 > 0
//   - capable: 注册表命中且该模型 CacheReadCostPer1M > 0 → 已知支持 prompt caching
func (t *UsageTracker) resolveCost(role string, u agentcore.Usage) (cost, saved float64, capable bool) {
	modelName := ""
	if t.modelSet != nil {
		_, modelName, _ = t.modelSet.CurrentSelection(role)
	}
	if entry, ok := models.DefaultRegistry().Resolve(modelName); ok {
		c := computeCost(u, *entry)
		s := computeSaved(u, *entry)
		canCache := entry.CacheReadCostPer1M > 0
		if c > 0 {
			return c, s, canCache
		}
	}
	if u.Cost != nil {
		return u.Cost.Total, 0, false
	}
	return 0, 0, false
}

// agentRoleName 把 subagent 名字归一到 role 名。
// architect_short/mid/long 都归到 architect；其他原样返回。
func agentRoleName(agentName string) string {
	if strings.HasPrefix(agentName, "architect_") {
		return "architect"
	}
	return agentName
}

// computeCost 按 $/1M tokens 单价计算本次调用的美元开销。
//
// 语义前提（由 litellm 各 provider 统一保证，参见 anthropic.go / bedrock.go /
// openai.go / gemini.go / compat.go 的 Usage 装配点）：
//
//	u.Input  = 全部输入 token，**包含** CacheRead；不含 CacheWrite
//	u.Output = 输出 token
//
// 因此 nonCachedInput = u.Input - u.CacheRead 在所有 provider 都成立。
// 兜底分支保留是为了应对未来某个 provider 误返脏数据时不至于崩。
func computeCost(u agentcore.Usage, e models.ModelEntry) float64 {
	nonCachedInput := u.Input - u.CacheRead
	if nonCachedInput < 0 {
		nonCachedInput = u.Input
	}
	c := 0.0
	c += float64(nonCachedInput) * e.InputCostPer1M / 1_000_000
	c += float64(u.Output) * e.OutputCostPer1M / 1_000_000
	c += float64(u.CacheRead) * e.CacheReadCostPer1M / 1_000_000
	c += float64(u.CacheWrite) * e.CacheWriteCostPer1M / 1_000_000
	return c
}

// computeSaved 估算 CacheRead 命中相对于"按普通输入价计费"省下的美元。
// 注意 CacheWrite 的溢价不抵扣 — 它属于"为后续命中铺路"的必要投入，
// 真实收益靠后续 CacheRead 累计回收。
func computeSaved(u agentcore.Usage, e models.ModelEntry) float64 {
	if u.CacheRead <= 0 || e.InputCostPer1M <= 0 {
		return 0
	}
	delta := e.InputCostPer1M - e.CacheReadCostPer1M
	if delta <= 0 {
		return 0
	}
	return float64(u.CacheRead) * delta / 1_000_000
}
