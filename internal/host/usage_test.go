package host

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/voocel/agentcore"
	"github.com/voocel/ainovel-cli/internal/bootstrap"
	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/models"
	storepkg "github.com/voocel/ainovel-cli/internal/store"
)

func TestUsageTrackerReplaySessionsReadsWorkerLogs(t *testing.T) {
	dir := t.TempDir()
	sessionsDir := filepath.Join(dir, "meta", "sessions", "agents")
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		t.Fatalf("mkdir sessions: %v", err)
	}
	rec := sessionRecord{
		Role: agentcore.RoleAssistant,
		Usage: &agentcore.Usage{
			Input: 1200, Output: 300, CacheRead: 800,
		},
		Meta: &sessionRecordMeta{Provider: "openrouter", Model: "test-model"},
	}
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal session: %v", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(filepath.Join(sessionsDir, "writer-ch01.jsonl"), data, 0o644); err != nil {
		t.Fatalf("write session: %v", err)
	}

	tk := NewUsageTracker(nil, nil)
	n, err := tk.ReplaySessions(dir)
	if err != nil {
		t.Fatalf("ReplaySessions: %v", err)
	}
	if n != 1 {
		t.Fatalf("replayed messages = %d, want 1", n)
	}
	_, input, output, cacheRead, _ := tk.Totals()
	if input != 1200 || output != 300 || cacheRead != 800 {
		t.Fatalf("replayed totals = input:%d output:%d cache:%d", input, output, cacheRead)
	}
}

// makeUsageMsg 构造一条 OnMessage 回调能接受的消息（带 Usage）。
// Role 必须显式置成 assistant：UsageTracker.Record 现在按角色筛，
// 只有 assistant 消息才会被累计（其它角色天然不带 usage）。
func makeUsageMsg(input, cacheRead, cacheWrite, output int) agentcore.AgentMessage {
	return agentcore.Message{
		Role: agentcore.RoleAssistant,
		Usage: &agentcore.Usage{
			Input: input, Output: output, CacheRead: cacheRead, CacheWrite: cacheWrite,
		},
	}
}

// Test_pushSample_RingBuffer 验证滑动窗的轮转语义：
// 前 N 次直接 append；之后按 sampleIdx 覆盖最旧条目。recentSums 始终反映"最近 N 次"。
func Test_pushSample_RingBuffer(t *testing.T) {
	var tot agentTotals

	for i := 1; i <= recentSampleCap; i++ {
		pushSample(&tot, i, i*100)
	}
	if got := len(tot.samples); got != recentSampleCap {
		t.Fatalf("after %d pushes, samples len=%d want %d", recentSampleCap, got, recentSampleCap)
	}

	pushSample(&tot, 999, 99900)
	if got := len(tot.samples); got != recentSampleCap {
		t.Fatalf("after overflow, samples len=%d want %d (no growth)", got, recentSampleCap)
	}
	cacheRead, input := recentSums(&tot)
	expectedCacheRead := 999
	expectedInput := 99900
	for i := 2; i <= recentSampleCap; i++ {
		expectedCacheRead += i
		expectedInput += i * 100
	}
	if cacheRead != expectedCacheRead || input != expectedInput {
		t.Fatalf("recentSums after overflow = (%d, %d), want (%d, %d)",
			cacheRead, input, expectedCacheRead, expectedInput)
	}
}

// Test_UsageTracker_RecordAccumulates 验证 Record 多 role 累计正确，
// 整体合并 = 所有 role 之和；per-role 各自独立。
func Test_UsageTracker_RecordAccumulates(t *testing.T) {
	tk := NewUsageTracker(nil, nil) // modelSet=nil → 走 provider Cost 兜底，不影响累计逻辑

	tk.Record("writer", "", makeUsageMsg(1000, 800, 0, 200))
	tk.Record("writer", "", makeUsageMsg(1500, 1200, 100, 300))
	tk.Record("editor", "", makeUsageMsg(500, 0, 0, 100))

	cost, in, out, cr, cw := tk.Totals()
	if in != 3000 || out != 600 || cr != 2000 || cw != 100 {
		t.Fatalf("totals = (in=%d out=%d cr=%d cw=%d), want (3000 600 2000 100)", in, out, cr, cw)
	}
	if cost != 0 {
		t.Errorf("cost should be 0 when modelSet=nil and no provider Cost, got %v", cost)
	}

	per := tk.PerAgent()
	if len(per) != 2 {
		t.Fatalf("per-agent len=%d want 2", len(per))
	}
	// PerAgent 按 CacheRead 降序：writer (2000) 应排在 editor (0) 前
	if per[0].Role != "writer" || per[1].Role != "editor" {
		t.Fatalf("per-agent order = %s,%s want writer,editor", per[0].Role, per[1].Role)
	}
	if per[0].Input != 2500 || per[0].CacheRead != 2000 {
		t.Errorf("writer totals = (in=%d cr=%d), want (2500 2000)", per[0].Input, per[0].CacheRead)
	}
}

// Test_UsageTracker_ArchitectAliasNormalized 验证 architect_short/mid/long
// 都归一到同一个 "architect" key（避免被 /model 切换的子角色拆成多行）。
func Test_UsageTracker_ArchitectAliasNormalized(t *testing.T) {
	tk := NewUsageTracker(nil, nil)
	tk.Record("architect_short", "", makeUsageMsg(100, 50, 0, 20))
	tk.Record("architect_mid", "", makeUsageMsg(200, 100, 0, 40))
	tk.Record("architect_long", "", makeUsageMsg(300, 150, 0, 60))

	per := tk.PerAgent()
	if len(per) != 1 {
		t.Fatalf("aliases must merge to single role, got %d entries: %+v", len(per), per)
	}
	if per[0].Role != "architect" {
		t.Fatalf("merged role name = %q, want architect", per[0].Role)
	}
	if per[0].Input != 600 || per[0].CacheRead != 300 {
		t.Errorf("merged totals = (in=%d cr=%d), want (600 300)", per[0].Input, per[0].CacheRead)
	}
}

func Test_UsageTracker_PerModelAccumulates(t *testing.T) {
	tk := NewUsageTracker(nil, nil)
	tk.accumulate("writer", "openrouter", "model-a", agentcore.Usage{Input: 1000, Output: 200, CacheRead: 700})
	tk.accumulate("editor", "openrouter", "model-b", agentcore.Usage{Input: 500, Output: 100})
	tk.accumulate("writer", "openrouter", "model-a", agentcore.Usage{Input: 300, Output: 80, CacheRead: 200})

	perModel := tk.PerModel()
	if len(perModel) != 2 {
		t.Fatalf("per-model len=%d want 2", len(perModel))
	}
	seen := map[string]AgentUsage{}
	for _, m := range perModel {
		seen[m.Model] = m
	}
	if seen["openrouter/model-a"].Input != 1300 || seen["openrouter/model-a"].CacheRead != 900 {
		t.Errorf("model-a totals = %+v", seen["openrouter/model-a"])
	}
	if seen["openrouter/model-b"].Output != 100 {
		t.Errorf("model-b totals = %+v", seen["openrouter/model-b"])
	}

	snap := tk.Snapshot()
	restored := NewUsageTracker(nil, nil)
	restored.applyState(snap)
	if got := restored.PerModel(); len(got) != 2 {
		t.Fatalf("restored per-model len=%d want 2: %+v", len(got), got)
	}
}

func Test_UsageTracker_RecordUsesActualUsageModel(t *testing.T) {
	tk := NewUsageTracker(nil, nil)
	tk.Record("writer", "", agentcore.Message{
		Role: agentcore.RoleAssistant,
		Usage: &agentcore.Usage{
			Provider: "openrouter",
			Model:    "google/gemini-2.5-pro",
			Input:    1000,
			Output:   200,
		},
	})

	perModel := tk.PerModel()
	if len(perModel) != 1 {
		t.Fatalf("per-model len=%d want 1: %+v", len(perModel), perModel)
	}
	if perModel[0].Model != "openrouter/google/gemini-2.5-pro" {
		t.Fatalf("model key = %q, want openrouter/google/gemini-2.5-pro", perModel[0].Model)
	}
	if perModel[0].Input != 1000 || perModel[0].Output != 200 {
		t.Fatalf("model totals = %+v", perModel[0])
	}
}

func Test_UsageTracker_ProviderOnlyDoesNotInventModelKey(t *testing.T) {
	tk := NewUsageTracker(nil, nil)
	tk.Record("writer", "", agentcore.Message{
		Role: agentcore.RoleAssistant,
		Usage: &agentcore.Usage{
			Provider: "openrouter",
			Input:    1000,
			Output:   200,
		},
	})

	if got := tk.PerModel(); len(got) != 0 {
		t.Fatalf("provider-only usage must not create model stats without a model, got %+v", got)
	}
}

// Test_UsageTracker_RecentWindowReflectsLatest 验证滑动窗反映"最近 N 次"，
// 不被早期低命中拖累 — 这正是 P1 要解决的"前期拖累 vs 稳态低命中"问题。
func Test_UsageTracker_RecentWindowReflectsLatest(t *testing.T) {
	tk := NewUsageTracker(nil, nil)

	// 前 5 次极低命中（首章场景）
	for i := 0; i < 5; i++ {
		tk.Record("writer", "", makeUsageMsg(1000, 0, 0, 200))
	}
	// 后 8 次（>5）高命中（稳态场景）
	for i := 0; i < 8; i++ {
		tk.Record("writer", "", makeUsageMsg(1000, 900, 0, 200))
	}

	per := tk.PerAgent()
	if len(per) != 1 {
		t.Fatalf("len=%d want 1", len(per))
	}
	w := per[0]

	// 累计：13 次中 8 次有命中 → 7200/13000 ≈ 55.4%
	cumulativeRate := float64(w.CacheRead) / float64(w.Input) * 100
	if cumulativeRate < 50 || cumulativeRate > 60 {
		t.Errorf("cumulative hit rate = %.1f%%, want ~55%%", cumulativeRate)
	}

	// 滑动窗：最近 10 次中 8 次高命中 + 2 次零命中 → 7200/10000 = 72%
	if w.RecentSamples != recentSampleCap {
		t.Errorf("recent samples = %d, want %d (window full)", w.RecentSamples, recentSampleCap)
	}
	recentRate := float64(w.RecentCacheRead) / float64(w.RecentInput) * 100
	if recentRate < 70 || recentRate > 75 {
		t.Errorf("recent hit rate = %.1f%%, want ~72%% (proves window dropped early misses)", recentRate)
	}
	// 关键：近 N 次明显高于累计，证明早期 0 已被丢出窗
	if recentRate <= cumulativeRate {
		t.Errorf("recent (%.1f%%) must exceed cumulative (%.1f%%) once window slides past early misses",
			recentRate, cumulativeRate)
	}
}

// Test_computeSaved 验证 saved 算法：CacheRead × (Input价 - CacheRead价)；
// 价差 ≤ 0 或 InputCost ≤ 0 时返回 0（CacheWrite 溢价不抵扣）。
func Test_computeSaved(t *testing.T) {
	cases := []struct {
		name  string
		usage agentcore.Usage
		entry models.ModelEntry
		want  float64
	}{
		{
			name:  "anthropic 5m 命中节省 90%",
			usage: agentcore.Usage{Input: 100_000, CacheRead: 80_000},
			entry: models.ModelEntry{InputCostPer1M: 3.0, CacheReadCostPer1M: 0.3},
			want:  80_000 * (3.0 - 0.3) / 1_000_000, // 0.216
		},
		{
			name:  "无命中 saved=0",
			usage: agentcore.Usage{Input: 100_000, CacheRead: 0},
			entry: models.ModelEntry{InputCostPer1M: 3.0, CacheReadCostPer1M: 0.3},
			want:  0,
		},
		{
			name:  "模型未标价 saved=0",
			usage: agentcore.Usage{Input: 100_000, CacheRead: 50_000},
			entry: models.ModelEntry{InputCostPer1M: 0, CacheReadCostPer1M: 0},
			want:  0,
		},
		{
			name:  "异常价差 saved=0",
			usage: agentcore.Usage{Input: 100_000, CacheRead: 50_000},
			entry: models.ModelEntry{InputCostPer1M: 1.0, CacheReadCostPer1M: 2.0}, // 缓存反而更贵
			want:  0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := computeSaved(tc.usage, tc.entry)
			if got != tc.want {
				t.Errorf("computeSaved=%v want %v", got, tc.want)
			}
		})
	}
}

// Test_UsageTracker_CacheCapableSticky 验证 CacheCapable 一旦置 true 就不回退。
// 历史上跑过支持 cache 的模型 → 累计命中数据有效；中途切到不支持的模型不应让标记回退。
//
// 通过构造 perAgent 直接赋值模拟（resolveCost 路径需要 ModelSet+Registry，集成层已覆盖）。
func Test_UsageTracker_CacheCapableSticky(t *testing.T) {
	tk := NewUsageTracker(nil, nil)

	// 模拟"曾经跑过支持 cache 的模型 + 命中过"
	tk.perAgent["writer"] = &agentTotals{
		Input: 1000, CacheRead: 500, Output: 200, CacheCapable: true,
	}
	// 后续追加一次"不支持 cache 的模型调用"
	tk.Record("writer", "", makeUsageMsg(500, 0, 0, 100))

	per := tk.PerAgent()
	if len(per) != 1 || per[0].Role != "writer" {
		t.Fatalf("expected single writer entry, got %+v", per)
	}
	if !per[0].CacheCapable {
		t.Errorf("CacheCapable must remain true after switching to non-capable model")
	}
	if per[0].CacheRead != 500 || per[0].Input != 1500 {
		t.Errorf("totals after merge = (in=%d cr=%d), want (1500 500)",
			per[0].Input, per[0].CacheRead)
	}
}

// Test_UsageTracker_PerAgentSkipsZero 验证未消费 token 的 role 不出现在 PerAgent 中。
func Test_UsageTracker_PerAgentSkipsZero(t *testing.T) {
	tk := NewUsageTracker(nil, nil)
	// 构造一个 role 但不消费 token（极端情况）
	tk.perAgent["ghost"] = &agentTotals{}
	tk.Record("writer", "", makeUsageMsg(100, 50, 0, 20))

	per := tk.PerAgent()
	if len(per) != 1 || per[0].Role != "writer" {
		t.Fatalf("ghost role with zero tokens must be skipped, got %+v", per)
	}
}

// Test_UsageTracker_MissingAssistantUsageCounted 验证 missingAssistantUsage
// 计数的判定边界：
//   - 累加路径只看 Usage != nil（不绑死 Role）
//   - 诊断路径要求 Role=Assistant 且 Content 非空 — 这才像"一次真 LLM 响应却
//     没拿到 usage"，对应上游 streaming 没发 OpenAI include_usage 那条 final
//     chunk 的典型表现。其它情形（user/tool 消息、空 content 的 assistant）
//     都不算 missing。
func Test_UsageTracker_MissingAssistantUsageCounted(t *testing.T) {
	tk := NewUsageTracker(nil, nil)

	withContent := func(text string) agentcore.Message {
		return agentcore.Message{
			Role:    agentcore.RoleAssistant,
			Content: []agentcore.ContentBlock{agentcore.TextBlock(text)},
		}
	}

	// assistant + 有 Content + nil Usage → 看起来是真响应但缺 usage，计入诊断
	tk.Record("writer", "", withContent("hi"))
	tk.Record("writer", "", withContent("again"))
	// assistant 但 Content 为空 → 异常恢复路径或占位消息，不算 missing
	tk.Record("writer", "", agentcore.Message{Role: agentcore.RoleAssistant})
	// user/tool 消息天然不携带 usage，无论 Content 是否为空都不算 missing
	tk.Record("writer", "", agentcore.Message{Role: agentcore.RoleUser, Content: []agentcore.ContentBlock{agentcore.TextBlock("u")}})
	tk.Record("writer", "", agentcore.Message{Role: agentcore.RoleTool, Content: []agentcore.ContentBlock{agentcore.TextBlock("t")}})
	// 正常带 usage → 走累加路径，不计入诊断
	tk.Record("writer", "", makeUsageMsg(100, 50, 0, 20))

	if got := tk.MissingAssistantUsage(); got != 2 {
		t.Errorf("MissingAssistantUsage=%d, want 2", got)
	}
	_, in, _, _, _ := tk.Totals()
	if in != 100 {
		t.Errorf("正常路径累计被破坏，input=%d want 100", in)
	}
}

// Test_UsageTracker_CacheCapableFromFacts 验证 CacheCapable 在注册表查不到该模型时
// 仍能根据"事实"标记为 true：自建 / 国内代理后端的模型经常不在 BerriAI/litellm
// 的 pricing 索引里，resolveCost 返回 capable=false；但只要 backend 真的返回了
// CacheRead 或 CacheWrite > 0，就证明该模型客观支持 prompt cache，per-role 行
// 不该显示"未启用"。
func Test_UsageTracker_CacheCapableFromFacts(t *testing.T) {
	tk := NewUsageTracker(nil, nil) // modelSet=nil → resolveCost 永远 capable=false

	// 一次有 CacheWrite（模拟首次写入 cache，注册表没标 capable，但事实证明支持）
	tk.Record("writer", "", makeUsageMsg(1000, 0, 200, 100))
	per := tk.PerAgent()
	if len(per) != 1 || !per[0].CacheCapable {
		t.Fatalf("CacheWrite > 0 应立即标记 CacheCapable=true，got %+v", per)
	}
	if !tk.OverallCacheCapable() {
		t.Errorf("overall CacheCapable 也应同步置 true")
	}

	// 反向：完全无 cache 活动的 role，CacheCapable 必须保持 false
	tk.Record("editor", "", makeUsageMsg(500, 0, 0, 100))
	per = tk.PerAgent()
	for _, a := range per {
		if a.Role == "editor" && a.CacheCapable {
			t.Errorf("editor 全程无 CacheRead/Write，CacheCapable 不应被错误标记为 true")
		}
	}
}

// Test_UsageTracker_AccumulatesAnyRoleWithUsage 验证累加路径解耦于 Role：
// 即使将来某个 adapter 把 usage 装配到非 assistant 角色的 message 上，
// 仍能正确累计。守住"装配规则与累加规则解耦"的契约。
func Test_UsageTracker_AccumulatesAnyRoleWithUsage(t *testing.T) {
	tk := NewUsageTracker(nil, nil)
	// 构造一条理论上不太常见的、带 Usage 的非 assistant 消息
	hypothetical := agentcore.Message{
		Role:  agentcore.RoleSystem,
		Usage: &agentcore.Usage{Input: 200, Output: 50, CacheRead: 100},
	}
	tk.Record("writer", "", hypothetical)

	_, in, out, cr, _ := tk.Totals()
	if in != 200 || out != 50 || cr != 100 {
		t.Errorf("未按 Usage 字段累加，got (in=%d out=%d cr=%d) want (200 50 100)", in, out, cr)
	}
	if tk.MissingAssistantUsage() != 0 {
		t.Errorf("有 Usage 不应计入 missing")
	}
}

// Test_UsageTracker_OnCostCallback 验证预算哨兵的接线点：每次记账后
// 锁外回调携带最新累计成本（含 provider 自报 cost 路径）。
func Test_UsageTracker_OnCostCallback(t *testing.T) {
	tk := NewUsageTracker(nil, nil)
	var got []float64
	tk.SetOnCost(func(total float64) { got = append(got, total) })

	msg := func(cost float64) agentcore.AgentMessage {
		return agentcore.Message{
			Role:  agentcore.RoleAssistant,
			Usage: &agentcore.Usage{Input: 100, Output: 10, Cost: &agentcore.Cost{Total: cost}},
		}
	}
	tk.Record("writer", "", msg(0.5))
	tk.Record("writer", "", msg(0.25))

	if len(got) != 2 || got[0] != 0.5 || got[1] != 0.75 {
		t.Fatalf("onCost should carry growing totals, got %v", got)
	}
}

// Test_UsageTracker_OnMissingUsageOnce 验证盲区回调只在首次触发。
func Test_UsageTracker_OnMissingUsageOnce(t *testing.T) {
	tk := NewUsageTracker(nil, nil)
	fired := 0
	tk.SetOnMissingUsage(func() { fired++ })

	noUsage := agentcore.Message{Role: agentcore.RoleAssistant, Content: []agentcore.ContentBlock{agentcore.TextBlock("正文")}}
	tk.Record("writer", "", noUsage)
	tk.Record("writer", "", noUsage)
	tk.Record("editor", "", noUsage)

	if fired != 1 {
		t.Fatalf("onMissingUsage should fire exactly once, got %d", fired)
	}
}

// TestCacheBreakDetection 验证缓存链断裂检测的四种走向：
// 同会话内前缀增长+命中骤降 → 断裂；换 task（新 spawn）→ 换基线不比较；
// 前缀缩短（会话内压缩）→ 只重置基线不告警；降幅不满足双阈值（相对 5%
// 且绝对 2000）→ 不告警。
func TestCacheBreakDetection(t *testing.T) {
	tk := NewUsageTracker(nil, nil)

	// 建立基线：前缀 30k，命中 28k。
	tk.Record("writer", "写第1章", makeUsageMsg(30000, 28000, 0, 100))
	if got := tk.OverallCacheBreaks(); got != 0 {
		t.Fatalf("首条消息不应判断裂，got %d", got)
	}

	// 同会话内前缀增长而命中骤降（28k→4k）→ 断裂。
	tk.Record("writer", "写第1章", makeUsageMsg(34000, 4096, 0, 100))
	if got := tk.OverallCacheBreaks(); got != 1 {
		t.Fatalf("前缀增长+命中骤降应判 1 次断裂，got %d", got)
	}

	// 同会话内前缀缩短（上下文压缩，4.4k < 34k）→ 重置基线，不告警。
	tk.Record("writer", "写第1章", makeUsageMsg(4400, 0, 0, 100))
	if got := tk.OverallCacheBreaks(); got != 1 {
		t.Fatalf("前缀缩短应视为压缩重置，got %d", got)
	}

	// 新基线上小幅回落（降幅 < 2000 绝对阈值）→ 不告警。
	tk.Record("writer", "写第1章", makeUsageMsg(36000, 30000, 0, 100))
	tk.Record("writer", "写第1章", makeUsageMsg(38000, 28500, 0, 100))
	if got := tk.OverallCacheBreaks(); got != 1 {
		t.Fatalf("降幅 1.5k 未过绝对阈值不应告警，got %d", got)
	}

	// 换 task = 新 spawn = 新缓存血统：即使首请求前缀不比上一会话末请求短
	// （38k → 40k）且命中骤降（28.5k→0），也不比较不告警。这是"连续短会话
	// 误报"回归用例：检测维度必须跟 prompt_cache_key 的会话粒度对齐。
	tk.Record("writer", "写第2章", makeUsageMsg(40000, 0, 0, 100))
	if got := tk.OverallCacheBreaks(); got != 1 {
		t.Fatalf("换 task 换基线，跨会话不应比较，got %d", got)
	}

	// 新会话内再次断裂 → 正常告警（证明新基线已生效）。
	tk.Record("writer", "写第2章", makeUsageMsg(45000, 38000, 0, 100))
	tk.Record("writer", "写第2章", makeUsageMsg(48000, 5000, 0, 100))
	if got := tk.OverallCacheBreaks(); got != 2 {
		t.Fatalf("新会话内断裂应正常检测，got %d", got)
	}

	// 相对降幅 <5%（100k→96k，降 4%）→ 不告警（即使绝对降幅 4k > 2000）。
	tk.Record("editor", "审阅第一弧", makeUsageMsg(120000, 100000, 0, 100))
	tk.Record("editor", "审阅第一弧", makeUsageMsg(125000, 96000, 0, 100))
	if got := tk.OverallCacheBreaks(); got != 2 {
		t.Fatalf("相对降幅 4%% 未过 5%% 阈值不应告警，got %d", got)
	}

	// per-role 归属：断裂记在 writer 名下并进 Snapshot。
	snap := tk.Snapshot()
	if snap.Overall.CacheBreaks != 2 || snap.PerAgent["writer"].CacheBreaks != 2 {
		t.Fatalf("断裂计数应进快照：overall=%d writer=%d", snap.Overall.CacheBreaks, snap.PerAgent["writer"].CacheBreaks)
	}
}

// TestUsageTracker_PrefixManifestAppended 验证每次请求落一条 Prefix Manifest
// 到 meta/prefix_manifest.jsonl，字段来源正确（modelSet=nil 时配置键为空）。
func TestUsageTracker_PrefixManifestAppended(t *testing.T) {
	dir := t.TempDir()
	st := storepkg.NewStore(dir)
	tk := NewUsageTracker(nil, st)

	tk.Record("writer", "写第 1 章", agentcore.Message{
		Role: agentcore.RoleAssistant,
		Usage: &agentcore.Usage{
			Provider: "openai", Model: "gpt-4o",
			Input: 5000, CacheRead: 4000, Output: 100,
		},
	})
	tk.Record("writer", "写第 1 章", agentcore.Message{
		Role: agentcore.RoleAssistant,
		Usage: &agentcore.Usage{
			Provider: "openai", Model: "gpt-4o",
			Input: 5200, CacheRead: 4800, Output: 100,
		},
	})

	data, err := os.ReadFile(filepath.Join(dir, "meta/prefix_manifest.jsonl"))
	if err != nil {
		t.Fatalf("读 manifest: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("manifest 行数 = %d, want 2\n%s", len(lines), data)
	}
	var pm1, pm2 domain.PrefixManifest
	if err := json.Unmarshal([]byte(lines[0]), &pm1); err != nil {
		t.Fatalf("unmarshal pm1: %v", err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &pm2); err != nil {
		t.Fatalf("unmarshal pm2: %v", err)
	}

	if pm1.Role != "writer" || pm1.RequestIndex != 1 || pm2.RequestIndex != 2 {
		t.Errorf("role/request_index = (%s,%d)/(%s,%d)", pm1.Role, pm1.RequestIndex, pm2.Role, pm2.RequestIndex)
	}
	if pm1.RunID == "" || pm1.RunID != pm2.RunID {
		t.Errorf("同 task 的 RunID 应稳定非空：%q vs %q", pm1.RunID, pm2.RunID)
	}
	if pm1.ProtocolProvider != "openai" || pm1.Model != "gpt-4o" {
		t.Errorf("协议/model = %s/%s", pm1.ProtocolProvider, pm1.Model)
	}
	if pm1.ProviderConfigKey != "" {
		t.Errorf("modelSet=nil 时配置键应为空，got %q", pm1.ProviderConfigKey)
	}
	if pm1.CacheMissTokens != 1000 || pm2.CacheMissTokens != 400 {
		t.Errorf("cache_miss = %d/%d, want 1000/400", pm1.CacheMissTokens, pm2.CacheMissTokens)
	}
	if pm2.Gap <= 0 {
		t.Errorf("第二条 Gap 应 > 0，got %v", pm2.Gap)
	}
	if pm1.Status != "ok" || pm1.InputTokens != 5000 || pm1.CacheReadTokens != 4000 {
		t.Errorf("基础字段异常：%+v", pm1)
	}
}

// TestUsageTracker_PrefixManifestWithModelSet 验证 manifest 携带配置键
// （go0/go1）、failover epoch 与静态前缀基线（system/tools）。
func TestUsageTracker_PrefixManifestWithModelSet(t *testing.T) {
	cfg := bootstrap.Config{
		Provider:  "go0",
		ModelName: "model-a",
		Providers: map[string]bootstrap.ProviderConfig{
			"go0": {Type: "openai", APIKey: "sk-x"},
			"go1": {Type: "openai", APIKey: "sk-y"},
		},
		Roles: map[string]bootstrap.RoleConfig{
			"writer": {Provider: "go0", Model: "model-a",
				Fallbacks: []bootstrap.ModelRef{{Provider: "go1", Model: "model-b"}}},
		},
	}
	ms, err := bootstrap.NewModelSet(cfg)
	if err != nil {
		t.Fatalf("NewModelSet: %v", err)
	}
	ms.SetAgentPrefixBaseline("writer", bootstrap.PrefixBaseline{
		SystemHash: "s1", SystemEstTokens: 100, ToolsHash: "t1", ToolsEstTokens: 200,
	})

	dir := t.TempDir()
	st := storepkg.NewStore(dir)
	tk := NewUsageTracker(ms, st)
	tk.Record("writer", "写第 1 章", agentcore.Message{
		Role: agentcore.RoleAssistant,
		Usage: &agentcore.Usage{
			Provider: "openai", Model: "model-a",
			Input: 5000, CacheRead: 3000, Output: 100,
		},
	})

	data, err := os.ReadFile(filepath.Join(dir, "meta/prefix_manifest.jsonl"))
	if err != nil {
		t.Fatalf("读 manifest: %v", err)
	}
	var pm domain.PrefixManifest
	if err := json.Unmarshal(data, &pm); err != nil {
		t.Fatalf("unmarshal manifest: %v", err)
	}
	if pm.ProviderConfigKey != "go0" {
		t.Errorf("ProviderConfigKey = %q, want go0", pm.ProviderConfigKey)
	}
	if pm.ProtocolProvider != "openai" || pm.FailoverEpoch != 1 {
		t.Errorf("protocol/epoch = %s/%d, want openai/1", pm.ProtocolProvider, pm.FailoverEpoch)
	}
	if pm.SystemHash != "s1" || pm.SystemEstTokens != 100 || pm.ToolsHash != "t1" || pm.ToolsEstTokens != 200 {
		t.Errorf("基线字段异常：%+v", pm)
	}
}

// TestUsageTracker_StyleCriticFailoverAttribution 是 ora-1 中-2 必补测试：
// style_critic 必须归一为 critic role——critic 由备用账号（go1）服务时，
// SelectionReport 与 manifest 的 provider_config_key/failover_epoch 必须记
// go1/2，而不是回落 default（go0/1）。此前 agentRoleName 只归一 architect_*，
// style_critic 回落 default 导致 manifest、cacheBreak 告警、live 成本全记到
// primary 账号（agents.agentToRole 已归一，host 侧漏了同一映射）。
func TestUsageTracker_StyleCriticFailoverAttribution(t *testing.T) {
	cfg := bootstrap.Config{
		Provider:  "go0",
		ModelName: "model-a",
		Providers: map[string]bootstrap.ProviderConfig{
			"go0": {Type: "openai", APIKey: "sk-x"},
			"go1": {Type: "openai", APIKey: "sk-y"},
		},
		Roles: map[string]bootstrap.RoleConfig{
			"critic": {Provider: "go0", Model: "model-a",
				Fallbacks: []bootstrap.ModelRef{{Provider: "go1", Model: "model-b"}}},
		},
	}
	ms, err := bootstrap.NewModelSet(cfg)
	if err != nil {
		t.Fatalf("NewModelSet: %v", err)
	}
	// 登记 failover wrapper（SelectionReport 需要）并模拟 critic failover：
	// 亲和推到备用 go1（epoch 2）。
	ms.ForRoleWithFailover("critic", nil)
	ms.SetFailoverAffinityForTest("critic", 1)

	// 归一名 + SelectionReport 端到端：style_critic 必须命中 critic 的亲和目标
	if got := agentRoleName("style_critic"); got != "critic" {
		t.Fatalf("agentRoleName(style_critic) = %q, want critic", got)
	}
	rep := ms.SelectionReport(agentRoleName("style_critic"))
	if rep.ConfigKey != "go1" || rep.Epoch != 2 {
		t.Fatalf("critic failover 后 SelectionReport = %+v, want go1/epoch 2", rep)
	}

	// manifest 端到端：Record("style_critic") 走 agentRoleName → critic →
	// SelectionReport → 备用账号归因
	dir := t.TempDir()
	st := storepkg.NewStore(dir)
	tk := NewUsageTracker(ms, st)
	tk.Record("style_critic", "评第 1 章", makeUsageMsg(3000, 2000, 0, 100))

	data, err := os.ReadFile(filepath.Join(dir, "meta/prefix_manifest.jsonl"))
	if err != nil {
		t.Fatalf("读 manifest: %v", err)
	}
	var pm domain.PrefixManifest
	if err := json.Unmarshal(data, &pm); err != nil {
		t.Fatalf("unmarshal manifest: %v", err)
	}
	if pm.Role != "critic" {
		t.Errorf("manifest role = %q, want critic（style_critic 归一）", pm.Role)
	}
	if pm.ProviderConfigKey != "go1" || pm.FailoverEpoch != 2 {
		t.Errorf("manifest 归因 = %q/epoch %d, want go1/2（critic 由备用账号服务）",
			pm.ProviderConfigKey, pm.FailoverEpoch)
	}
}

// TestCacheBreakHint 验证归因修正：5m TTL 只对 Anthropic 协议路径保留，
// 未知 provider 长间隔 → TTL 未知，短间隔 → 路由漂移/逐出。
func TestCacheBreakHint(t *testing.T) {
	cases := []struct {
		gap      time.Duration
		protocol string
		model    string
		want     string
	}{
		{2 * time.Minute, "openai", "gpt-4o", "疑似路由漂移或逐出（短间隔 miss，非 TTL 过期）"},
		{10 * time.Minute, "openai", "gpt-4o", "长间隔后 miss，TTL 未知（非 Anthropic 5m 契约路径）"},
		{10 * time.Minute, "anthropic", "claude-sonnet-4-6", "疑似 5m TTL 过期"},
		{2 * time.Hour, "anthropic", "claude-sonnet-4-6", "疑似 1h TTL 过期"},
		{2 * time.Minute, "anthropic", "claude-sonnet-4-6", "疑似服务端逐出/路由漂移（中转站轮询上游是常见原因）"},
		{10 * time.Minute, "", "claude-3-5-sonnet", "疑似 5m TTL 过期"}, // 仅模型名识别 Anthropic
	}
	for _, c := range cases {
		if got := cacheBreakHint(c.gap, c.protocol, c.model); got != c.want {
			t.Errorf("cacheBreakHint(%v, %q, %q) = %q, want %q", c.gap, c.protocol, c.model, got, c.want)
		}
	}
}

// TestIsAnthropicProtocol 验证协议识别覆盖协议名与模型名前缀两种路径。
func TestIsAnthropicProtocol(t *testing.T) {
	if !isAnthropicProtocol("anthropic", "claude-sonnet") {
		t.Error("anthropic 协议应识别")
	}
	if !isAnthropicProtocol("openai", "claude-3-5-sonnet") {
		t.Error("claude 模型名前缀应识别")
	}
	if isAnthropicProtocol("openai", "gpt-4o") {
		t.Error("openai/gpt 不应识别为 anthropic")
	}
	if isAnthropicProtocol("", "gpt-4o") {
		t.Error("空协议/非 claude 模型不应识别")
	}
}

// TestRunIDForTask 验证 run 标识对同 task 稳定、不同 task 不同、空 task 为空。
func TestRunIDForTask(t *testing.T) {
	a := runIDForTask("写第 1 章")
	b := runIDForTask("写第 1 章")
	c := runIDForTask("写第 2 章")
	if a == "" || a != b {
		t.Errorf("同 task 应稳定非空：%q vs %q", a, b)
	}
	if a == c {
		t.Errorf("不同 task 应不同：%q vs %q", a, c)
	}
	if runIDForTask("") != "" {
		t.Error("空 task 应返回空 run id")
	}
}

// TestEffectiveRunID 验证透传的 runID 优先；未透传退回 task 哈希。
func TestEffectiveRunID(t *testing.T) {
	if got := effectiveRunID("writer#3", "写第 1 章"); got != "writer#3" {
		t.Errorf("透传 runID 应原样使用, got %q", got)
	}
	fallback := effectiveRunID("", "写第 1 章")
	if fallback == "" || fallback != runIDForTask("写第 1 章") {
		t.Errorf("空 runID 应退回 task 哈希, got %q", fallback)
	}
}

// TestUsageTracker_ManifestRunIDDistinctPerSpawn 验证（ora-1 必补测试 6）：
// 同一 task 的两次独立 spawn 必须使用不同 RunID，且 RequestIndex 各自从 1
// 重新计数——此前 RunID 只是 task 文本哈希，同任务再 spawn 时 manifest 视为
// 同一 run，RequestIndex 继续累计，缓存血缘诊断失真。
// runID 来自 agentcore RunMeta.InstanceID（agent#runSeq，与 prompt cache key
// 的 #seq 同源）。
func TestUsageTracker_ManifestRunIDDistinctPerSpawn(t *testing.T) {
	dir := t.TempDir()
	st := storepkg.NewStore(dir)
	tk := NewUsageTracker(nil, st)

	msg := func() agentcore.AgentMessage {
		return agentcore.Message{
			Role: agentcore.RoleAssistant,
			Usage: &agentcore.Usage{
				Provider: "openai", Model: "gpt-4o",
				Input: 5000, CacheRead: 4000, Output: 100,
			},
		}
	}
	// 第一次 spawn：两条请求（同一 run 内 RequestIndex 递增）。
	tk.RecordRun("writer", "写第 1 章", "writer#3", msg())
	tk.RecordRun("writer", "写第 1 章", "writer#3", msg())
	// 同一 task 第二次 spawn：新 InstanceID。
	tk.RecordRun("writer", "写第 1 章", "writer#4", msg())

	data, err := os.ReadFile(filepath.Join(dir, "meta/prefix_manifest.jsonl"))
	if err != nil {
		t.Fatalf("读 manifest: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 3 {
		t.Fatalf("manifest 行数 = %d, want 3", len(lines))
	}
	var runs [3]domain.PrefixManifest
	for i, ln := range lines {
		if err := json.Unmarshal([]byte(ln), &runs[i]); err != nil {
			t.Fatalf("unmarshal %d: %v", i, err)
		}
	}
	// 同一 spawn 内 RunID 稳定、RequestIndex 递增。
	if runs[0].RunID != "writer#3" || runs[1].RunID != "writer#3" {
		t.Errorf("同 spawn RunID 应稳定：%q / %q", runs[0].RunID, runs[1].RunID)
	}
	if runs[0].RequestIndex != 1 || runs[1].RequestIndex != 2 {
		t.Errorf("同 spawn RequestIndex 应 1,2：%d, %d", runs[0].RequestIndex, runs[1].RequestIndex)
	}
	// 新 spawn：RunID 不同，RequestIndex 重新从 1 开始。
	if runs[2].RunID == runs[0].RunID {
		t.Errorf("不同 spawn RunID 必须不同：%q", runs[2].RunID)
	}
	if runs[2].RequestIndex != 1 {
		t.Errorf("新 spawn RequestIndex 应重新从 1 开始, got %d", runs[2].RequestIndex)
	}
}

// TestCacheBreak_RunIDChangeResetsBaseline 验证同 task 换 spawn（runID 变化）
// 时缓存链基线重置——换 spawn = 新缓存血统，与换 task 语义一致。此前只按
// task 重置，同一任务第二次 spawn 的首请求会误跟上一 spawn 的末请求比较。
func TestCacheBreak_RunIDChangeResetsBaseline(t *testing.T) {
	tk := NewUsageTracker(nil, nil)

	tk.RecordRun("writer", "写第 1 章", "writer#3", makeUsageMsg(30000, 28000, 0, 100))
	tk.RecordRun("writer", "写第 1 章", "writer#3", makeUsageMsg(34000, 4096, 0, 100))
	if got := tk.OverallCacheBreaks(); got != 1 {
		t.Fatalf("同 spawn 内断裂应检测, got %d", got)
	}

	// 新 spawn 首请求：即使命中骤降（28k→0）也不比较不告警。
	tk.RecordRun("writer", "写第 1 章", "writer#4", makeUsageMsg(40000, 0, 0, 100))
	if got := tk.OverallCacheBreaks(); got != 1 {
		t.Fatalf("换 spawn 应重置基线不告警, got %d", got)
	}

	// 新 spawn 内再次断裂 → 正常告警。
	tk.RecordRun("writer", "写第 1 章", "writer#4", makeUsageMsg(45000, 38000, 0, 100))
	tk.RecordRun("writer", "写第 1 章", "writer#4", makeUsageMsg(48000, 5000, 0, 100))
	if got := tk.OverallCacheBreaks(); got != 2 {
		t.Fatalf("新 spawn 内断裂应正常检测, got %d", got)
	}
}

// TestMissTokensClamped 验证 miss = input - cache_read，下限 0。
func TestMissTokensClamped(t *testing.T) {
	if got := missTokens(100, 60); got != 40 {
		t.Errorf("missTokens(100, 60) = %d, want 40", got)
	}
	if got := missTokens(50, 100); got != 0 {
		t.Errorf("missTokens(50, 100) = %d, want 0", got)
	}
}
