package domain

import "time"

// UsageSchemaVersion 是 meta/usage.json 的兼容版本号。
// 未来若 AgentUsageTotals 字段语义变化，递增此值；UsageStore.Load 见到不同版本应忽略并触发 replay 重建。
const UsageSchemaVersion = 2

// UsageState 是累计 token / cost 用量的可持久化快照。
// 内存中由 UsageTracker 维护，定期 debounce 落盘到 meta/usage.json。
//
// 注意：UsageTracker 内部的滑动窗 samples（"近 N 次命中率"）**不持久化**——
// 它只服务 UI 短期诊断，进程重启从空开始重新积累几轮即可恢复语义。
// MissingAssistantUsage 保留持久化，跨重启累积更有诊断价值。
type UsageState struct {
	Schema       int                         `json:"schema"`
	UpdatedAt    time.Time                   `json:"updated_at"`
	Overall      AgentUsageTotals            `json:"overall"`
	PerAgent     map[string]AgentUsageTotals `json:"per_agent"`
	PerModel     map[string]AgentUsageTotals `json:"per_model,omitempty"`
	MissingUsage int                         `json:"missing_assistant_usage"`
}

// AgentUsageTotals 是单个角色（或 overall）累计计数的可持久化形态。
type AgentUsageTotals struct {
	Input        int     `json:"input"`
	Output       int     `json:"output"`
	CacheRead    int     `json:"cache_read"`
	CacheWrite   int     `json:"cache_write"`
	Cost         float64 `json:"cost_usd"`
	Saved        float64 `json:"saved_usd"`
	CacheCapable bool    `json:"cache_capable"`
	// CacheBreaks 是 live 检测到的缓存链断裂次数（前缀未缩短而命中骤降）。
	// 只在实时路径累计，session replay 不重放检测。
	CacheBreaks int `json:"cache_breaks,omitempty"`
}

// PrefixManifest 是单次 LLM 请求的缓存分层观测记录，append-only 落盘到
// meta/prefix_manifest.jsonl。只记录 hash 与 token 数，绝不记录正文内容——
// 它是诊断"命中的是 system/basis/草稿哪一段"的数据面，不承载任何文本。
//
// v1 说明：
//   - ToolsHash/SystemHash（+估算 token）来自 agents.BuildWorkers 注册的静态
//     基线（system prompt + tools schema 是每次请求重发的稳定前缀）；
//   - basis/draft 分段由 internal/tools/ 的 prompt 构造处产出，暂未注入（另一
//     lane 改造中），对应 Hash/EstTokens 字段保持空；
//   - 空缺期间可用跨请求增量推断边界：CacheReadTokens 停滞在
//     SystemEstTokens+ToolsEstTokens 总量附近 → 命中停在稳定前缀；
//     CacheReadTokens 随 InputTokens 同步增长 → 命中延伸进动态段。
//
// Gap 是距同 role+task 上一次请求的间隔（纳秒）；Status 当前恒为 "ok"
// （Record 只在成功响应后触发），Error 为失败路径预留。
type PrefixManifest struct {
	Role                  string        `json:"role"`
	RunID                 string        `json:"run_id,omitempty"`
	RequestIndex          int           `json:"request_index"`
	ProviderConfigKey     string        `json:"provider_config_key,omitempty"` // go0/go1/go2（配置键）
	ProtocolProvider      string        `json:"protocol_provider,omitempty"`   // openai/anthropic/...（协议名）
	Model                 string        `json:"model,omitempty"`
	FailoverEpoch         int           `json:"failover_epoch"` // 1=primary，2+=第 N 个备用；0=未知
	ToolsHash             string        `json:"tools_hash,omitempty"`
	ToolsEstTokens        int           `json:"tools_est_tokens,omitempty"`
	SystemHash            string        `json:"system_hash,omitempty"`
	SystemEstTokens       int           `json:"system_est_tokens,omitempty"`
	StableBasisHash       string        `json:"stable_basis_hash,omitempty"`
	StableBasisEstTokens  int           `json:"stable_basis_est_tokens,omitempty"`
	DynamicBasisHash      string        `json:"dynamic_basis_hash,omitempty"`
	DynamicBasisEstTokens int           `json:"dynamic_basis_est_tokens,omitempty"`
	DraftHash             string        `json:"draft_hash,omitempty"`
	DraftEstTokens        int           `json:"draft_est_tokens,omitempty"`
	InputTokens           int           `json:"input_tokens"`
	CacheReadTokens       int           `json:"cache_read_tokens"`
	CacheMissTokens       int           `json:"cache_miss_tokens"`
	Gap                   time.Duration `json:"gap_ns"`
	Status                string        `json:"status,omitempty"`
	Error                 string        `json:"error,omitempty"`
}
