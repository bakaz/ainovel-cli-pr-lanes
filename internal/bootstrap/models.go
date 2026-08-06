package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/voocel/agentcore"
	"github.com/voocel/agentcore/llm"
	"github.com/voocel/ainovel-cli/internal/errs"
)

// FailoverEvent 表示一次显式 provider 切换。
// Reason 为短标签（rate_limit / timeout / stream_idle / network），用于结构化日志。
// Epoch 是切换后目标在有序选择列表 [primary, fallbacks...] 中的序号（1-based：
// 1=primary，2=第一个备用），供缓存归因观测对齐"哪条 provider 链在服务"。
type FailoverEvent struct {
	Role         string
	Reason       string
	FromProvider string
	FromModel    string
	ToProvider   string
	ToModel      string
	Epoch        int
	Err          error
}

// FailoverReporter 在发生显式切换时被调用。
type FailoverReporter func(FailoverEvent)

type modelTarget struct {
	provider string
	name     string
	model    agentcore.ChatModel
}

// SwappableModel 是可热切换的 ChatModel 包装器。
// 已开始的请求继续使用旧实例；后续请求自动切到新实例。
type SwappableModel struct {
	*agentcore.SwappableModel
	mu       sync.RWMutex
	provider string
	name     string
}

func NewSwappableModel(provider, name string, model agentcore.ChatModel) *SwappableModel {
	return &SwappableModel{
		SwappableModel: agentcore.NewSwappableModel(model),
		provider:       provider,
		name:           name,
	}
}

func (m *SwappableModel) ProviderName() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.provider
}

func (m *SwappableModel) Info() llm.ModelInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if info, ok := m.SwappableModel.Current().(interface{ Info() llm.ModelInfo }); ok {
		modelInfo := info.Info()
		if modelInfo.Name == "" {
			modelInfo.Name = m.name
		}
		if modelInfo.Provider == "" {
			modelInfo.Provider = m.provider
		}
		return modelInfo
	}
	return llm.ModelInfo{
		Name:     m.name,
		Provider: m.provider,
	}
}

func (m *SwappableModel) Capabilities() llm.Capabilities {
	if cp, ok := m.SwappableModel.Current().(llm.CapabilityProvider); ok {
		return cp.Capabilities()
	}
	return llm.Capabilities{}
}

func (m *SwappableModel) Swap(provider, name string, model agentcore.ChatModel) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.SwappableModel.Swap(model)
	m.provider = provider
	m.name = name
}

func (m *SwappableModel) Current() (provider, name string) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.provider, m.name
}

// providerAffinityWindow 是 provider 亲和（affinity）的滑动窗口：一次调用成功后，
// 后续调用在窗口内沿用该 provider 作为起始目标（best-known），不主动回跳 primary；
// 每次成功调用续期窗口。窗口过期（长时间无调用）后重新从 primary 评估。
//
// 语义说明（启发式亲和，非 run-scoped 强租约）：
//   - 背景：旧实现每次调用都从 primary 重试，一次 run 内 go0→go1→go0 抖动会把缓存
//     前缀整段归零（各 provider 的缓存血缘不同）。亲和让 run 内的请求落在同一
//     provider，避免客户端自造轮询。
//   - 局限（已知近似）：subagent 的 run 边界（engine 派发的 runSeq）在 agentcore
//     fork 内且不随分支跟踪，无法透传到 model wrapper 层，故用"空闲窗口"近似 run
//     边界。后果：两个间隔 < 窗口的独立 run 会错误共享亲和（视为同一 run）；单次
//     超长 run（> 窗口）中途可能回跳 primary；同角色并发 run 的亲和会互相覆盖。
//     这些是近似语义的固有代价，接受。
//   - 非 failover 类错误（quota/auth 等）不触发 failover 前进（pickNext 短路，
//     保持现有语义），但若失败目标正是当前亲和目标，亲和会立即清空——"best-known
//     目标已证明不可用"与错误是否 eligible 无关，否则会在窗口内反复打同一个死
//     目标直到 10 分钟过期（旧实现每次从 primary 评估可立即恢复）。清空后下个
//     调用从 primary 重新评估。
//   - 亲和只影响"起始目标"，不阻止新调用按当前 best-known 目标走：失败路径始终沿
//     fallback 链向前 failover（不回跳 primary），全链失败即清空亲和，下个调用从
//     primary 重新评估；/model 热切换（primary Swap）也会使亲和立即失效。
const providerAffinityWindow = 10 * time.Minute

// ModelSet 持有按角色分配的模型实例，未配置的角色回退到默认模型。
type ModelSet struct {
	Default   *SwappableModel
	models    map[string]*SwappableModel
	fallbacks map[string][]modelTarget
	config    Config

	// failoverModels 记录 ForRoleWithFailover 创建的首个 wrapper，供
	// SelectionReport 读取当前 failover epoch 与亲和目标（观测用，不参与路由）。
	failoverMu     sync.RWMutex
	failoverModels map[string]*failoverModel

	// prefixBaseline 是各 agent 提示词前缀（system prompt + tools schema）的
	// 静态观测基线，由 agents.BuildWorkers 组装完 worker 配置后注册，
	// UsageTracker 写 Prefix Manifest 时读取（只记 hash 与估算 token，不记正文）。
	prefixMu       sync.RWMutex
	prefixBaseline map[string]PrefixBaseline
}

// ForRole 返回指定角色的模型，未配置时返回默认模型。
func (ms *ModelSet) ForRole(role string) agentcore.ChatModel {
	if m, ok := ms.models[role]; ok {
		return m
	}
	return ms.Default
}

// ForRoleWithFailover 返回带有单次请求级 fallback 的角色模型。
// 仅当该角色显式配置了 fallbacks 时生效；未配置时退化为普通模型。
// 首次创建的 failover wrapper 会登记到 ModelSet（供 SelectionReport 观测亲和
// epoch）；后续同 role 调用仍返回独立 wrapper（与旧行为一致，生产只调用一次）。
func (ms *ModelSet) ForRoleWithFailover(role string, report FailoverReporter) agentcore.ChatModel {
	primary, ok := ms.models[role]
	if !ok {
		return ms.Default
	}
	targets := ms.fallbacks[role]
	if len(targets) == 0 {
		return primary
	}
	fm := &failoverModel{
		role:      role,
		primary:   primary,
		fallbacks: append([]modelTarget(nil), targets...),
		report:    report,
		protocolOf: func(configKey string) string {
			pc, ok := ms.config.Providers[configKey]
			if !ok {
				return ""
			}
			t, err := pc.ProviderType(configKey)
			if err != nil {
				return ""
			}
			return t
		},
	}
	ms.failoverMu.Lock()
	if ms.failoverModels == nil {
		ms.failoverModels = make(map[string]*failoverModel)
	}
	if _, exists := ms.failoverModels[role]; !exists {
		ms.failoverModels[role] = fm
	}
	ms.failoverMu.Unlock()
	return fm
}

// SetFailoverAffinityForTest 直接把某角色 failover wrapper 的亲和目标推到指定
// 索引（0=primary，N=第 N 个备用）并续期窗口，供 host 层测试驱动 SelectionReport
// 的 failover epoch 观测（如"style_critic 归一到 critic 后命中备用账号"的
// manifest 归因断言）。测试专用 seam：生产路径的亲和只由 failoverModel 自身在
// 成功提交时维护，此方法不参与路由决策、不做单调性校验。未登记 wrapper 时 noop。
func (ms *ModelSet) SetFailoverAffinityForTest(role string, idx int) {
	ms.failoverMu.RLock()
	fm := ms.failoverModels[role]
	ms.failoverMu.RUnlock()
	if fm == nil {
		return
	}
	fm.mu.Lock()
	fm.affinityIdx = idx
	fm.affinityExp = time.Now().Add(providerAffinityWindow)
	fm.affinityPrimProv, fm.affinityPrimName = fm.primary.Current()
	fm.mu.Unlock()
}

// PrefixBaseline 是某 agent 提示词前缀（system prompt + tools schema）的静态
// 观测基线：每次请求都会重发的稳定前缀。CacheRead 停滞在该基线总量附近说明
// 命中停在稳定前缀；随 Input 增长说明命中延伸进动态段（basis/draft）。
type PrefixBaseline struct {
	SystemHash      string
	SystemEstTokens int
	ToolsHash       string
	ToolsEstTokens  int
}

// SetAgentPrefixBaseline 注册某 agent（"writer"/"architect_short"/...）的提示词
// 前缀基线。由 agents.BuildWorkers 在组装完各 worker 配置后调用；仅观测用，
// 不参与路由。重复注册以最后一次为准。
func (ms *ModelSet) SetAgentPrefixBaseline(agent string, b PrefixBaseline) {
	ms.prefixMu.Lock()
	defer ms.prefixMu.Unlock()
	if ms.prefixBaseline == nil {
		ms.prefixBaseline = make(map[string]PrefixBaseline)
	}
	ms.prefixBaseline[agent] = b
}

// AgentPrefixBaseline 返回某 agent 的提示词前缀基线；未注册返回零值。
func (ms *ModelSet) AgentPrefixBaseline(agent string) PrefixBaseline {
	ms.prefixMu.RLock()
	defer ms.prefixMu.RUnlock()
	return ms.prefixBaseline[agent]
}

// SelectionReport 是角色当前生效选择的观测快照：配置键 + 模型 + 协议类型 +
// failover epoch。用于 Prefix Manifest / cache-break 归因——Usage.Provider 只有
// 协议名（"openai"），无法区分 go0/go1/go2 账号，这里补上配置键维度。
type SelectionReport struct {
	ConfigKey string // 配置键（go0/go1/go2），不是协议名
	Model     string // 模型名
	Protocol  string // 协议类型（openai/anthropic/...），ProviderType 解析结果
	Epoch     int    // failover epoch：1=primary，2+=第 N 个备用；无 failover 配置恒为 1
}

// SelectionReport 返回角色当前生效选择的观测快照。role 为空或 "default" 时
// 返回默认模型。走 failover wrapper 的角色反映亲和当前目标（可能已 failover
// 到备用，epoch > 1）。
func (ms *ModelSet) SelectionReport(role string) SelectionReport {
	if role != "" && role != "default" {
		ms.failoverMu.RLock()
		fm := ms.failoverModels[role]
		ms.failoverMu.RUnlock()
		if fm != nil {
			return fm.selectionReport()
		}
		if sw, ok := ms.models[role]; ok {
			return ms.selectionReportFor(sw, 1)
		}
	}
	return ms.selectionReportFor(ms.Default, 1)
}

func (ms *ModelSet) selectionReportFor(sw *SwappableModel, epoch int) SelectionReport {
	provider, model := sw.Current()
	rep := SelectionReport{ConfigKey: provider, Model: model, Epoch: epoch}
	if provider != "" {
		if pc, ok := ms.config.Providers[provider]; ok {
			if t, err := pc.ProviderType(provider); err == nil {
				rep.Protocol = t
			}
		}
	}
	return rep
}

// Summary 返回模型分配摘要（供日志使用）。
func (ms *ModelSet) Summary() string {
	var parts []string
	for role, m := range ms.models {
		provider, name := m.Current()
		parts = append(parts, fmt.Sprintf("%s=%s/%s", role, provider, name))
	}
	if len(parts) == 0 {
		provider, name := ms.Default.Current()
		return fmt.Sprintf("default=%s/%s", provider, name)
	}
	provider, name := ms.Default.Current()
	return fmt.Sprintf("default=%s/%s %s", provider, name, strings.Join(parts, " "))
}

// CurrentSelection 返回角色当前生效的 provider/model。
// role 为空或 "default" 时返回默认模型。
func (ms *ModelSet) CurrentSelection(role string) (provider, model string, explicit bool) {
	if role == "" || role == "default" {
		provider, model = ms.Default.Current()
		return provider, model, true
	}
	if sw, ok := ms.models[role]; ok {
		provider, model = sw.Current()
		return provider, model, true
	}
	provider, model = ms.Default.Current()
	return provider, model, false
}

// Swap 切换默认模型或指定角色模型。
// role 为空或 "default" 时切换默认模型；其他角色切换为显式覆盖。
func (ms *ModelSet) Swap(role, provider, model string) error {
	pc, ok := ms.config.Providers[provider]
	if !ok {
		return fmt.Errorf("provider %q is not configured: %w", provider, errs.ErrConfig)
	}
	next, err := createModelFromConfig(provider, model, pc, make(map[string]agentcore.ChatModel))
	if err != nil {
		return fmt.Errorf("切换模型失败: %w", err)
	}

	if role == "" || role == "default" {
		ms.Default.Swap(provider, model, next)
		return nil
	}

	if !knownRoles[role] {
		return fmt.Errorf("unknown role %q: %w", role, errs.ErrConfig)
	}

	if existing, ok := ms.models[role]; ok {
		existing.Swap(provider, model, next)
		return nil
	}
	ms.models[role] = NewSwappableModel(provider, model, next)
	return nil
}

// ModelName 从 ChatModel 中提取当前模型名，失败返回空字符串。
// 支持 SwappableModel 的热切换：调用时总是返回最新值。
func ModelName(m agentcore.ChatModel) string {
	if info, ok := m.(interface{ Info() llm.ModelInfo }); ok {
		return info.Info().Name
	}
	return ""
}

// NewModelSet 根据配置创建多模型集合。
// 相同 provider+model 组合复用同一个实例。
func NewModelSet(cfg Config) (*ModelSet, error) {
	cache := make(map[string]agentcore.ChatModel)

	// 创建默认模型
	defaultPC := cfg.DefaultProviderConfig()
	defaultModel, err := createModelFromConfig(cfg.Provider, cfg.ModelName, defaultPC, cache)
	if err != nil {
		return nil, fmt.Errorf("default model: %w", err)
	}

	ms := &ModelSet{
		Default:        NewSwappableModel(cfg.Provider, cfg.ModelName, defaultModel),
		models:         make(map[string]*SwappableModel),
		fallbacks:      make(map[string][]modelTarget),
		config:         cfg,
		failoverModels: make(map[string]*failoverModel),
		prefixBaseline: make(map[string]PrefixBaseline),
	}

	// 创建角色覆盖模型
	for role, rc := range cfg.Roles {
		pc, ok := cfg.Providers[rc.Provider]
		if !ok {
			return nil, fmt.Errorf("role %s references unknown provider %q: %w", role, rc.Provider, errs.ErrConfig)
		}
		m, err := createModelFromConfig(rc.Provider, rc.Model, pc, cache)
		if err != nil {
			return nil, fmt.Errorf("role %s model: %w", role, err)
		}
		ms.models[role] = NewSwappableModel(rc.Provider, rc.Model, m)
		slog.Info("角色模型分配", "module", "config", "role", role, "provider", rc.Provider, "model", rc.Model)
		if len(rc.Fallbacks) == 0 {
			continue
		}

		targets := make([]modelTarget, 0, len(rc.Fallbacks))
		for _, fallback := range rc.Fallbacks {
			fpc, ok := cfg.Providers[fallback.Provider]
			if !ok {
				return nil, fmt.Errorf("role %s fallback references unknown provider %q: %w", role, fallback.Provider, errs.ErrConfig)
			}
			fm, err := createModelFromConfig(fallback.Provider, fallback.Model, fpc, cache)
			if err != nil {
				return nil, fmt.Errorf("role %s fallback %s/%s: %w", role, fallback.Provider, fallback.Model, err)
			}
			targets = append(targets, modelTarget{
				provider: fallback.Provider,
				name:     fallback.Model,
				model:    fm,
			})
		}
		ms.fallbacks[role] = targets
	}

	return ms, nil
}

// createModelFromConfig 创建或复用 ChatModel 实例。
func createModelFromConfig(providerKey, model string, pc ProviderConfig, cache map[string]agentcore.ChatModel) (agentcore.ChatModel, error) {
	cacheKey := providerKey + "|" + model
	if m, ok := cache[cacheKey]; ok {
		return m, nil
	}

	providerType, err := pc.ProviderType(providerKey)
	if err != nil {
		return nil, fmt.Errorf("解析 provider 类型失败: %w", err)
	}
	providerExtra := cloneMap(pc.Extra)
	if pc.API != "" {
		if providerExtra == nil {
			providerExtra = make(map[string]any, 1)
		}
		providerExtra["api"] = pc.API
	}

	streamIdle, err := pc.StreamIdleTimeoutValue()
	if err != nil {
		return nil, fmt.Errorf("provider %s stream_idle_timeout: %w: %w", providerKey, errs.ErrConfig, err)
	}

	m, err := llm.NewModel(providerType, model,
		llm.WithAPIKey(pc.APIKey),
		llm.WithBaseURL(pc.BaseURL),
		llm.WithStreamIdleTimeout(streamIdle),
		llm.WithProviderExtra(providerExtra),
		llm.WithExtra(pc.ExtraBody),
	)
	if err != nil {
		return nil, fmt.Errorf("provider %s (%s): %w: %w", providerKey, providerType, errs.ErrProvider, err)
	}
	cache[cacheKey] = m
	return m, nil
}

// failoverModel 是带 provider 亲和（affinity）的 failover 包装器。
//
// 亲和语义（见 providerAffinityWindow）：
//   - 每次调用从"亲和目标"开始（窗口内成功过的目标），而不是每次都从 primary 重试；
//   - 仅当当前目标失败时才向后 failover（按 fallbacks 顺序），**成功**后亲和才移到
//     新目标，本次 run 剩余调用保持用新目标（不回跳 primary）；
//   - 失败路径不建立/不更新亲和——全链失败时调用报错并**清空亲和**，下个调用从
//     primary 重新评估（不会在窗口内卡死在已死的目标上）；
//   - 窗口过期（长时间无调用 = 近似新 run）或 primary 被 /model 热切换时亲和失效。
//   - 并发安全约束：亲和更新采用"单调推进 + primary 快照校验"（见 commitAffinity）：
//     并发完成的旧结果不能把亲和推回更早目标，在途请求也不能在 primary 被 Swap
//     后重建旧语义的亲和。
//
// 线程安全：亲和状态由 mu 保护；Generate/GenerateStream 可并发（并行 subagent）。
type failoverModel struct {
	role       string
	primary    *SwappableModel
	fallbacks  []modelTarget
	report     FailoverReporter
	protocolOf func(configKey string) string // 配置键 → 协议类型（观测用）

	mu               sync.Mutex
	affinityIdx      int       // 当前亲和目标索引（0=primary，>=1 为 fallbacks[idx-1]）；-1=无亲和
	affinityExp      time.Time // 亲和窗口过期时间
	affinityPrimProv string    // 亲和建立时 primary 的 provider 快照（检测 /model Swap）
	affinityPrimName string
}

// targetAt 返回有序选择列表 [primary, fallbacks...] 中第 idx 个目标。
// idx<=0 为 primary（热切换感知）；idx>=1 为 fallbacks[idx-1]。
func (m *failoverModel) targetAt(idx int) modelTarget {
	if idx <= 0 {
		return m.currentTarget()
	}
	i := idx - 1
	if i < len(m.fallbacks) {
		return m.fallbacks[i]
	}
	return modelTarget{}
}

// effectiveIdxLocked 返回本次调用应使用的起始目标索引：亲和有效则沿用亲和目标，
// 否则回到 primary（0）并清空亲和。亲和失效条件：窗口过期（近似新 run），或
// primary 被 /model 热切换（用户显式重选，亲和作废）。必须在持有 mu 时调用。
func (m *failoverModel) effectiveIdxLocked() int {
	if m.affinityIdx < 0 {
		return 0
	}
	if time.Now().After(m.affinityExp) {
		m.invalidateAffinityLocked()
		return 0
	}
	primProv, primName := m.primary.Current()
	if primProv != m.affinityPrimProv || primName != m.affinityPrimName {
		m.invalidateAffinityLocked()
		return 0
	}
	return m.affinityIdx
}

// renewAffinityLocked 把亲和更新到 idx（本次调用成功的目标），续期窗口并快照
// primary。必须在持有 mu 时调用（commitAffinity 已校验单调性与 primary 快照）。
func (m *failoverModel) renewAffinityLocked(idx int) {
	m.affinityIdx = idx
	m.affinityExp = time.Now().Add(providerAffinityWindow)
	m.affinityPrimProv, m.affinityPrimName = m.primary.Current()
}

// invalidateAffinityLocked 清空亲和：窗口过期 / primary 热切换 / 目标链全部失败时
// 调用，下个调用重新从 primary 评估。必须在持有 mu 时调用。
func (m *failoverModel) invalidateAffinityLocked() {
	m.affinityIdx = -1
	m.affinityExp = time.Time{}
	m.affinityPrimProv, m.affinityPrimName = "", ""
}

// pickNext 从 fromIdx+1 起在 fallback 链中找第一个 provider/model 与 current
// 不同的目标（只在亲和目标之后向前找，绝不回跳）。返回目标、索引、failover
// reason；无可用目标返回 ok=false。
func (m *failoverModel) pickNext(current modelTarget, fromIdx int, err error) (modelTarget, int, string, bool) {
	if err == nil || current.model == nil {
		return modelTarget{}, 0, "", false
	}
	if errors.Is(err, context.Canceled) {
		return modelTarget{}, 0, "", false
	}
	if !agentcore.IsFailoverEligible(err) {
		return modelTarget{}, 0, agentcore.FailoverReason(err), false
	}
	reason := agentcore.FailoverReason(err)
	for i := fromIdx + 1; i <= len(m.fallbacks); i++ {
		target := m.fallbacks[i-1]
		if target.provider == current.provider && target.name == current.name {
			continue
		}
		if target.model == nil {
			continue
		}
		return target, i, reason, true
	}
	return modelTarget{}, 0, reason, false
}

// Epoch 返回当前亲和目标序号（1=primary，2+=第 N 个备用）。
// 无亲和时按"下个调用将从 primary 开始"返回 1。
func (m *failoverModel) Epoch() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.affinityIdx < 0 {
		return 1
	}
	return m.affinityIdx + 1
}

// report 返回当前生效选择的观测快照（亲和目标 + 配置键 + 协议类型 + epoch）。
func (m *failoverModel) selectionReport() SelectionReport {
	m.mu.Lock()
	defer m.mu.Unlock()
	idx := m.effectiveIdxLocked()
	target := m.targetAt(idx)
	rep := SelectionReport{ConfigKey: target.provider, Model: target.name, Epoch: idx + 1}
	if m.protocolOf != nil {
		rep.Protocol = m.protocolOf(target.provider)
	}
	return rep
}

// commitAffinity 在尝试成功后提交亲和（单调推进 + primary 快照校验）：
//   - 单调性：只允许向前（idx > 当前亲和目标）或同目标续期（idx == 当前）；
//     回跳的旧完成结果被丢弃——并发完成不能让亲和倒退（A 慢执行于 go1、B 已续到
//     go2 时，A 的完成不得把亲和写回 go1）。
//   - primary 快照：仅当当前 primary 与尝试开始时捕获的快照一致才生效——在途
//     fallback 请求在 /model Swap 之后完成时，不得用旧 primary 语义重建亲和
//     （Swap 是用户显式重选，其意图优先）。
func (m *failoverModel) commitAffinity(idx int, primProv, primName string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.affinityIdx >= 0 && idx < m.affinityIdx {
		return
	}
	curProv, curName := m.primary.Current()
	if curProv != primProv || curName != primName {
		return
	}
	m.renewAffinityLocked(idx)
}

// nextOrStall 是统一 attempt 循环的"失败后推进"步骤：从 idx 沿 fallback 链找下一
// 个目标（只向前，不回跳）。ok=true 时返回下一目标供重试；ok=false 表示无法前进
// （全链耗尽 / 非 failover 错误 / 取消）。无法前进时清空亲和的两种情形：
//   - 错误为 failover 类：当前亲和目标已确认失效（全链耗尽），下个调用从 primary
//     重新评估，避免在窗口内卡死在已死的目标上；
//   - 错误非 failover 类（quota/auth 等）但失败目标正是当前亲和目标（idx ==
//     affinityIdx）：非 eligible 错误不触发 failover 前进（pickNext 短路，保持
//     现有语义），但"best-known 目标已证明不可用"与错误是否 eligible 无关——只
//     清空亲和让下个调用从 primary 重新评估，否则会卡死在配额耗尽的备用上直到
//     窗口过期（旧实现每次从 primary 评估可立即恢复）。
func (m *failoverModel) nextOrStall(current modelTarget, idx int, err error) (next modelTarget, nextIdx int, reason string, ok bool) {
	next, nextIdx, reason, ok = m.pickNext(current, idx, err)
	if ok {
		return
	}
	m.mu.Lock()
	if agentcore.IsFailoverEligible(err) || idx == m.affinityIdx {
		m.invalidateAffinityLocked()
	}
	m.mu.Unlock()
	return
}

// Generate 在亲和约束下执行一次生成：从亲和起始目标开始；失败时沿 fallback 链
// 向前 failover（不回跳 primary），遍历完整链（与 GenerateStream 一致）。亲和只
// 在目标真正成功时移动（单调推进 + primary 快照校验）——失败路径不建立亲和，
// 全链失败时清空亲和，下个调用从 primary 重新评估。
func (m *failoverModel) Generate(ctx context.Context, messages []agentcore.Message, tools []agentcore.ToolSpec, opts ...agentcore.CallOption) (*agentcore.LLMResponse, error) {
	m.mu.Lock()
	start := m.effectiveIdxLocked()
	primProv, primName := m.primary.Current()
	m.mu.Unlock()

	idx := start
	current := m.targetAt(idx)
	for {
		resp, err := current.model.Generate(ctx, messages, tools, opts...)
		if err == nil {
			m.commitAffinity(idx, primProv, primName)
			return resp, nil
		}
		next, nextIdx, reason, ok := m.nextOrStall(current, idx, err)
		if !ok {
			return nil, err
		}
		m.reportFailover(current, next, reason, err, nextIdx+1)
		current = next
		idx = nextIdx
	}
}

// GenerateStream 在亲和约束下执行一次流式生成：从亲和起始目标开始；失败时沿
// fallback 链向前 failover（不回跳 primary），遍历完整链。亲和只在目标真正成功
// 时移动（单调推进 + primary 快照校验）——失败路径不建立亲和，全链失败时清空
// 亲和，下个调用从 primary 重新评估。
func (m *failoverModel) GenerateStream(ctx context.Context, messages []agentcore.Message, tools []agentcore.ToolSpec, opts ...agentcore.CallOption) (<-chan agentcore.StreamEvent, error) {
	out := make(chan agentcore.StreamEvent, 100)

	go func() {
		defer close(out)

		m.mu.Lock()
		start := m.effectiveIdxLocked()
		primProv, primName := m.primary.Current()
		m.mu.Unlock()
		current := m.targetAt(start)
		idx := start

	retry:
		source, resp, err := m.startAttempt(ctx, current, messages, tools, opts...)
		if err != nil {
			next, nextIdx, reason, ok := m.nextOrStall(current, idx, err)
			if ok {
				m.reportFailover(current, next, reason, err, nextIdx+1)
				current = next
				idx = nextIdx
				goto retry
			}
			out <- agentcore.StreamEvent{Type: agentcore.StreamEventError, Err: err}
			return
		}
		if resp != nil {
			m.commitAffinity(idx, primProv, primName)
			out <- agentcore.StreamEvent{
				Type:       agentcore.StreamEventDone,
				Message:    resp.Message,
				StopReason: resp.Message.StopReason,
			}
			return
		}

		forwarded := false
		for ev := range source {
			switch ev.Type {
			case agentcore.StreamEventError:
				if ev.Err != nil && !forwarded {
					next, nextIdx, reason, ok := m.nextOrStall(current, idx, ev.Err)
					if ok {
						m.reportFailover(current, next, reason, ev.Err, nextIdx+1)
						current = next
						idx = nextIdx
						goto retry
					}
				} else if ev.Err != nil {
					// 已转发过部分输出：同一次 stream 内不再透明切 provider
					// （nextOrStall 不前进，避免两个 provider 的文本拼接），但失败
					// 目标若是当前亲和目标，说明 best-known 目标已确认不可用——
					// 错误传播前清空亲和（与 nextOrStall 的非 eligible 清空语义
					// 一致，与错误是否 eligible 无关），下个调用从 primary 重新
					// 评估，避免外层 AgentLoop retry 连续钉死在已失败的目标上
					// （如 stream idle / 连接中断）。
					m.mu.Lock()
					if idx == m.affinityIdx {
						m.invalidateAffinityLocked()
					}
					m.mu.Unlock()
				}
				out <- ev
				return
			case agentcore.StreamEventDone:
				m.commitAffinity(idx, primProv, primName)
				out <- ev
				return
			default:
				forwarded = true
				out <- ev
			}
		}
	}()

	return out, nil
}

func (m *failoverModel) SupportsTools() bool {
	return m.primary != nil && m.primary.SupportsTools()
}

func (m *failoverModel) ProviderName() string {
	if m.primary == nil {
		return ""
	}
	return m.primary.ProviderName()
}

func (m *failoverModel) Info() llm.ModelInfo {
	if m.primary == nil {
		return llm.ModelInfo{}
	}
	return m.primary.Info()
}

func (m *failoverModel) currentTarget() modelTarget {
	if m.primary == nil {
		return modelTarget{}
	}
	provider, name := m.primary.Current()
	return modelTarget{
		provider: provider,
		name:     name,
		model:    m.primary,
	}
}

func (m *failoverModel) reportFailover(from, to modelTarget, reason string, err error, epoch int) {
	if m.report != nil {
		m.report(FailoverEvent{
			Role:         m.role,
			Reason:       reason,
			FromProvider: from.provider,
			FromModel:    from.name,
			ToProvider:   to.provider,
			ToModel:      to.name,
			Epoch:        epoch,
			Err:          err,
		})
	}
}

func (m *failoverModel) startAttempt(ctx context.Context, target modelTarget, messages []agentcore.Message, tools []agentcore.ToolSpec, opts ...agentcore.CallOption) (<-chan agentcore.StreamEvent, *agentcore.LLMResponse, error) {
	if target.model == nil {
		return nil, nil, fmt.Errorf("no model configured")
	}

	streamCh, err := target.model.GenerateStream(ctx, messages, tools, opts...)
	if err == nil {
		return streamCh, nil, nil
	}

	resp, genErr := target.model.Generate(ctx, messages, tools, opts...)
	if genErr != nil {
		return nil, nil, genErr
	}
	return nil, resp, nil
}
