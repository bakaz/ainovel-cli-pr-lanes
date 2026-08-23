package agents

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/voocel/agentcore"
	corecontext "github.com/voocel/agentcore/context"
	"github.com/voocel/agentcore/llm"
	"github.com/voocel/agentcore/subagent"
	"github.com/voocel/ainovel-cli/assets"
	"github.com/voocel/ainovel-cli/internal/agents/ctxpack"
	"github.com/voocel/ainovel-cli/internal/agents/guard"
	"github.com/voocel/ainovel-cli/internal/bootstrap"
	"github.com/voocel/ainovel-cli/internal/projectprofile"
	"github.com/voocel/ainovel-cli/internal/store"
	"github.com/voocel/ainovel-cli/internal/tools"
)

// agentToRole 把 subagent name 归一为 ModelSet 认得的 role 名。
// architect_short / architect_long 都共用同一个 architect role 配置。
// 跟 host.agentRoleName 同义，因为 build 与 host 互不依赖故各持一份。
func agentToRole(name string) string {
	if strings.HasPrefix(name, "architect_") {
		return "architect"
	}
	if name == "style_critic" {
		return "critic"
	}
	return name
}

// promptCacheBase 从书目录派生稳定短哈希，作为提示词缓存身份前缀：同一本书
// 跨进程重启共享路由桶，且不向 provider 泄露本地路径。角色后缀由调用方拼接，
// subagent 每次 spawn 再追加 "#seq"（一次会话一个键）。
func promptCacheBase(bookDir string) string {
	sum := sha256.Sum256([]byte(bookDir))
	return "nvl-" + hex.EncodeToString(sum[:6])
}

// prefixBaselineFor 计算某 agent 提示词前缀（system prompt + tools schema）的
// 静态观测基线，供 Prefix Manifest 的分段估算使用：每次请求都会重发这段稳定
// 前缀，缓存命中是否覆盖它可与 CacheRead 对比（仅观测，不参与路由）。
// tools 序列化用 name/description/schema——与请求侧发送的内容同源；json.Marshal
// 对 map 按键排序，哈希跨进程稳定。
func prefixBaselineFor(system string, tools []agentcore.Tool) bootstrap.PrefixBaseline {
	sysHash := sha256.Sum256([]byte(system))
	var toolBytes []byte
	if len(tools) > 0 {
		entries := make([]any, 0, len(tools))
		for _, tl := range tools {
			entries = append(entries, map[string]any{
				"name": tl.Name(), "desc": tl.Description(), "schema": tl.Schema(),
			})
		}
		toolBytes, _ = json.Marshal(entries)
	}
	toolsHash := sha256.Sum256(toolBytes)
	return bootstrap.PrefixBaseline{
		SystemHash:      "sha256:" + hex.EncodeToString(sysHash[:8]),
		SystemEstTokens: estTokens(len(system)),
		ToolsHash:       "sha256:" + hex.EncodeToString(toolsHash[:8]),
		ToolsEstTokens:  estTokens(len(toolBytes)),
	}
}

// estTokens 粗估 token 数：UTF-8 字节数 / 4（中英混合正文的经验近似，1 token
// ≈ 4 字节）。只用于观测估算，不参与任何计费或路由。
func estTokens(n int) int {
	if n <= 0 {
		return 0
	}
	if t := n / 4; t > 0 {
		return t
	}
	return 1
}

// subagentMaxRetries 是所有 Worker 的 LLM retry 上限。
// 退避策略：指数退避（受 maxDelay 上限约束），优先服从 server Retry-After。
// retryable 错误（stream-idle / 503 / 短暂网络抖动）在 Worker 层就近重试，
// 而不是让 Engine 重跑整个任务。
// 项目铁律一保证写类工具走 checkpoint+digest 幂等，重试是安全的。
const subagentMaxRetries = 7

// UsageRecorder 是 BuildWorkers 可选的用量回调；每条带 Usage 的模型响应都会
// 调一次，由 Host 层负责聚合。
//   - runID 是本次 spawn 的实例标识（agentcore RunMeta.InstanceID，形如
//     "writer#7"，与 prompt cache key 追加的 #seq 是同一个 runSeq 拼出来的），
//     Prefix Manifest 据此区分同一 task 的多次 spawn（缓存血缘精确归因）；
//   - task 是本次 spawn 的任务文本，作为会话身份供缓存链断裂检测按会话重置基线。
//
// nil 表示不追踪。
type UsageRecorder func(agentName, task, runID string, msg agentcore.AgentMessage)

// runUsageObserver 把 agentcore 每次 spawn 的实例标识绑定到该 run 内每条
// 模型响应上，透传给 UsageRecorder 作为 Prefix Manifest 的 RunID。
//
// 背景：manifest 的 RunID 若只按 task 文本哈希，同一任务被再次 spawn 时会
// 被误判为同一 run（真实 cache key 已追加 #seq），RequestIndex 跨 run 累计，
// 缓存血缘诊断失真。agentcore 的 Run 事件带 RunMeta.InstanceID
// （= agentName#runSeq，与 prompt cache key 的 #seq 是同一个 runSeq 拼出来
// 的），经 Runner.SetEventObserver 可拿到，故在此透传。
//
// task 文本从 run 首条 user 消息（AgentLoop 注入的任务消息）捕获；后续
// steering / length-recovery 注入的 user 消息不覆盖（task 非空即锁定）。
// 事件语义：EventModelResponse 在每次模型调用完成后恰好发一条，携带该次
// 响应的 assistant 消息（含 Usage）——与 OnMessage 的 assistant 消息一一对应。
//
// 并发：每个 Runner 独立安装一个 observer 实例；同一 Runner 的 run 串行
// 消费事件，mu 仅兜底异常路径。
type runUsageObserver struct {
	mu       sync.Mutex
	instance string
	task     string
	record   UsageRecorder
}

func newRunUsageObserver(record UsageRecorder) *runUsageObserver {
	return &runUsageObserver{record: record}
}

// OnEvent 是 Runner.SetEventObserver 的回调形态。
func (o *runUsageObserver) OnEvent(meta subagent.RunMeta, ev agentcore.Event) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if meta.InstanceID != o.instance {
		o.instance = meta.InstanceID
		o.task = ""
	}
	switch ev.Type {
	case agentcore.EventMessageStart:
		if m, ok := ev.Message.(agentcore.Message); ok && m.Role == agentcore.RoleUser && o.task == "" {
			o.task = m.TextContent()
		}
	case agentcore.EventModelResponse:
		if m, ok := ev.Message.(agentcore.Message); ok {
			o.record(meta.Agent, o.task, meta.InstanceID, m)
		}
	}
}

// ApplyThinking 把某具体角色的推理强度应用到 Worker（运行时 /model 调整用）。
// architect → 两个 architect_* 子代理；writer/editor → 对应子代理。
// 空 level = 沿用模型/provider 默认。其它 role 名忽略。
type ApplyThinking func(role string, level agentcore.ThinkingLevel)

// ParseThinkingLevel 把配置字符串转 agentcore.ThinkingLevel。
// "" 合法（= 不覆盖/继承）；其余须是 off/low/medium/high/xhigh/max 之一，
// 否则返回 error（启动时降级当空并 warn，运行时把 error 回显给用户）。
func ParseThinkingLevel(s string) (agentcore.ThinkingLevel, error) {
	lv := agentcore.NormalizeThinkingLevel(agentcore.ThinkingLevel(s))
	switch lv {
	case "", agentcore.ThinkingOff, agentcore.ThinkingLow, agentcore.ThinkingMedium,
		agentcore.ThinkingHigh, agentcore.ThinkingXHigh, agentcore.ThinkingMax:
		return lv, nil
	default:
		return "", fmt.Errorf("无效推理强度 %q（可选：off/low/medium/high/xhigh/max）", s)
	}
}

func ResolveThinkingForModel(model agentcore.ChatModel, level agentcore.ThinkingLevel) (agentcore.ThinkingLevel, bool) {
	level = agentcore.NormalizeThinkingLevel(level)
	// 对不支持 thinking 的普通 chat 模型，显式 off 不是 no-op，而是非法参数。
	if cp, ok := model.(llm.CapabilityProvider); ok && cp.Capabilities().Thinking.Supported == llm.SupportNo {
		return agentcore.ThinkingAuto, level == agentcore.ThinkingAuto
	}
	return llm.ThinkingPolicyFor(model).Resolve(level)
}

func AvailableThinkingForModel(model agentcore.ChatModel) []agentcore.ThinkingLevel {
	if cp, ok := model.(llm.CapabilityProvider); ok && cp.Capabilities().Thinking.Supported == llm.SupportNo {
		return []agentcore.ThinkingLevel{agentcore.ThinkingAuto}
	}
	return llm.ThinkingPolicyFor(model).Available
}

// roleThinking 解析某角色生效的推理强度；非法值降级为空（不覆盖）并 warn。
func roleThinking(cfg bootstrap.Config, role string) agentcore.ThinkingLevel {
	lv, err := ParseThinkingLevel(cfg.ResolveReasoningEffort(role))
	if err != nil {
		slog.Warn("忽略无效推理强度配置", "module", "agent", "role", role, "err", err)
		return ""
	}
	return lv
}

func resolvedRoleThinking(model agentcore.ChatModel, cfg bootstrap.Config, role string) agentcore.ThinkingLevel {
	resolved, _ := ResolveThinkingForModel(model, roleThinking(cfg, role))
	return resolved
}

// workerToolsets is the production tool composition for each Engine worker.
// BuildWorkers and contract tests share this helper so lists cannot drift.
type workerToolsets struct {
	ArchitectShort []agentcore.Tool
	ArchitectLong  []agentcore.Tool
	Writer         []agentcore.Tool
	Editor         []agentcore.Tool
	Polisher       []agentcore.Tool
	// commitTool 是 Writer 列表使用的 commit_chapter 实例；
	// BuildWorkers 在构建后按配置注入精修流水线门控（SetPolishPipeline）。
	commitTool *tools.CommitChapterTool
}

func buildWorkerToolsets(store *store.Store, bundle assets.Bundle, style string, contract *projectprofile.SceneBeatContract) workerToolsets {
	return buildWorkerToolsetsWithApproval(store, bundle, style, contract, nil)
}

func buildWorkerToolsetsWithApproval(store *store.Store, bundle assets.Bundle, style string, contract *projectprofile.SceneBeatContract, askUser *tools.AskUserTool) workerToolsets {
	readChapter := tools.NewReadChapterTool(store)
	architectCtx := tools.NewContextToolForRole(store, bundle.References, style, "architect")
	writerCtx := tools.NewContextToolForRole(store, bundle.References, style, "writer")
	editorCtx := tools.NewContextToolForRole(store, bundle.References, style, "editor")
	saveFoundation := tools.NewSaveFoundationTool(store, contract)
	if askUser != nil {
		saveFoundation.SetLongApproval(askUser, tools.DefaultLongApprovalTimeout)
	}
	commitTool := tools.NewCommitChapterTool(store)
	return workerToolsets{
		ArchitectShort: []agentcore.Tool{
			architectCtx,
			saveFoundation,
		},
		ArchitectLong: []agentcore.Tool{
			architectCtx,
			saveFoundation,
			tools.NewReadPlanningReferenceTool(store),
			tools.NewReadPlanningArchiveTool(store),
			tools.NewSavePlanningArchiveEntryTool(store),
		},
		Writer: []agentcore.Tool{
			writerCtx,
			readChapter,
			tools.NewPlanChapterTool(store, contract),
			tools.NewDraftChapterTool(store, contract),
			tools.NewEditChapterTool(store),
			tools.NewCheckConsistencyTool(store),
			commitTool,
		},
		Editor: []agentcore.Tool{
			editorCtx,
			readChapter,
			tools.NewSaveReviewTool(store),
			tools.NewSaveArcSummaryTool(store),
			tools.NewSaveVolumeSummaryTool(store),
		},
		// Polisher 工具集 = 三个候选提交工具（schema docs/polisher-candidate-tools-schema.md §2）：
		// submit_polish_plan / submit_edit_batch / finish_polish。accumulator 是 per-run 的
		//（polish_draft 每次 run 创建，包 6 接线），工具在构建时以 nil accumulator 注册，
		// 运行时经 SetAccumulator 注入（见 BuildWorkers 的 holder 接线）。
		Polisher: []agentcore.Tool{
			tools.NewSubmitPolishPlanTool(),
			tools.NewSubmitEditBatchTool(),
			tools.NewFinishPolishTool(),
		},
		commitTool: commitTool,
	}
}

// appendSceneGuidance 将场景节拍指导追加到系统提示词末尾。
// 若 guidance 为空，返回原 prompt 不变。导出供测试验证。
func appendSceneGuidance(prompt, guidance string) string {
	if guidance == "" {
		return prompt
	}
	return prompt + "\n\n" + guidance
}

// ChapterFSMConfigFor 计算章节流水线 FSM 的运行配置——BuildWorkers 注入六工具
// 与 host 构造 Engine 时共用同一来源，保证引擎反思/失败裁定解析出的
// stage/required 与真实工具拦截严格一致（残缺配置缺 PipelineEnabled/
// ExpectedPolisherModel 会让反思报告的 stage 偏离生产判定）。
func ChapterFSMConfigFor(cfg bootstrap.Config, models *bootstrap.ModelSet) tools.ChapterFSMConfig {
	pipelineEnabled := cfg.ChapterPipelineEnabled()
	expectedPolisherModel := ""
	if pipelineEnabled {
		if _, ok := cfg.Roles["polisher"]; ok {
			_, expectedPolisherModel, _ = models.CurrentSelection("polisher")
		}
	}
	return tools.ChapterFSMConfig{
		Enabled:               true,
		PipelineEnabled:       pipelineEnabled,
		ExpectedPolisherModel: expectedPolisherModel,
	}
}

// BuildWorkers 组装三个 Worker(architect_short/long、writer、editor)为可程序化
// 调用的 subagent.Runner——Engine 直接调用其 Run(类型化入口),无 LLM 中间层
// (docs/engine-rfc.md §1)。
// 返回 Runner、AskUserTool、WriterRestorePack 与 ApplyThinking(运行时 /model 联动
// 各角色推理强度;writer/architect/editor 的 ContextManager 走工厂自动重建)。
// onGuardBlock 可选(nil 安全):各 Worker StopGuard 的拦截/升级审计回调。
func BuildWorkers(
	cfg bootstrap.Config,
	store *store.Store,
	models *bootstrap.ModelSet,
	bundle assets.Bundle,
	recordUsage UsageRecorder,
	onGuardBlock guard.BlockHook,
	contract *projectprofile.SceneBeatContract,
) (*subagent.Runner, *tools.AskUserTool, *ctxpack.WriterRestorePack, ApplyThinking) {
	askUser := tools.NewAskUserTool()
	ts := buildWorkerToolsetsWithApproval(store, bundle, cfg.Style, contract, askUser)
	architectShortTools := ts.ArchitectShort
	architectLongTools := ts.ArchitectLong
	writerTools := ts.Writer
	editorTools := ts.Editor
	polisherTools := ts.Polisher

	// 候选工具运行时注入：accumulator 由 polish_draft 每次 run 创建（包 6 接线），
	// 经 PolishAccumulatorHolder 注入三个候选工具。构建时 holder.Get()==nil——
	// 工具 Execute 在未注入 accumulator 时返回明确错误，不会静默空转。
	polisherAccHolder := NewPolishAccumulatorHolder()
	for _, tl := range polisherTools {
		if injectable, ok := tl.(interface{ SetAccumulator(*tools.PolishAccumulator) }); ok {
			injectable.SetAccumulator(polisherAccHolder.Get())
		}
	}

	// 精修流水线开关（chapter_pipeline="ds_mimo_critic" 或 roles.polisher 显式配置）：
	// 开启时注入 polish_draft 强制（工具 enabled + writer 协议）与 commit gate 校验
	// （fresh polish checkpoint + 模型一致性 + seq 绑定）。未配置时所有工具保持旧行为。
	// 与 StyleReviewMode 的关系（C1-H4）：pipeline 开关只控制 polish 工具与 pipeline
	// commit gate，不强制 critic mode——两者独立。pipeline 启用 + mode=off 是可接受
	// 组合：此时 commit 的 pipeline gate 仍要求 fresh polish checkpoint（现状保留），
	// 但评审门控（CheckCommitStyleGate）不生效。
	// 章节流水线 FSM 运行配置的单一来源（六工具注入与 host 构造 Engine 共用，
	// 见 ChapterFSMConfigFor）。
	fsmCfg := ChapterFSMConfigFor(cfg, models)
	pipelineEnabled := fsmCfg.PipelineEnabled
	// expectedPolisherModel：pipeline 开启且 Roles 显式配置 polisher 时记录其
	// 当前模型名，供 commit gate 与章节 FSM（polish 绑定）共用；空 = 未显式
	// 配置（跳过模型一致性校验）。
	expectedPolisherModel := fsmCfg.ExpectedPolisherModel
	if pipelineEnabled {
		ts.commitTool.SetPolishPipeline(&tools.PolishPipelineConfig{ExpectedModel: expectedPolisherModel})
	}
	for _, list := range [][]agentcore.Tool{writerTools, polisherTools} {
		for _, tl := range list {
			if cc, ok := tl.(*tools.CheckConsistencyTool); ok {
				cc.SetPipelineEnabled(pipelineEnabled)
			}
		}
	}

	// Provider failover 只记日志,不通知宿主
	reportFailover := func(ev bootstrap.FailoverEvent) {
		slog.Warn("provider 切换",
			"module", "agent",
			"role", ev.Role,
			"reason", ev.Reason,
			"from", fmt.Sprintf("%s/%s", ev.FromProvider, ev.FromModel),
			"to", fmt.Sprintf("%s/%s", ev.ToProvider, ev.ToModel),
			"err", ev.Err,
		)
	}

	architectModel := WithTrailingAntiRefusal(models.ForRoleWithFailover("architect", reportFailover), store)
	writerModel := WithTrailingAntiRefusal(models.ForRoleWithFailover("writer", reportFailover), store)
	editorModel := WithTrailingAntiRefusal(models.ForRoleWithFailover("editor", reportFailover), store)

	// Writer 的 ContextManager 由工厂每次调用重建，窗口随模型 swap 动态跟随（见下方工厂）。
	_, writerModelName, _ := models.CurrentSelection("writer")
	writerContextWindow, writerSource := cfg.ResolveContextWindow(writerModelName)
	bootstrap.LogContextWindowChoice("writer", writerModelName, writerContextWindow, writerSource)

	// modelLookup 写入 session 时给每条 assistant 消息附 _meta:{provider,model}，
	// 让 replay 不再依赖"当前 ModelSet"来反推历史 cost，运行中切换模型也能精确算。
	// 数据源必须用 SelectionReport（读 failover 租约目标）：fallback 实际服务的是
	// 备用配置键（go1），若用 CurrentSelection 只读 primary，会把 go1 的请求记成
	// go0，甚至产生不存在的 go0/备用模型组合。局限：SelectionReport 在响应完成
	// 后（OnMessage 时机）查询，并发 run 交错时可能读到全局最新租约目标而非本次
	// 响应的目标——agentcore Usage 只透传协议名（openai/anthropic），项目侧无法
	// 把配置键绑定到响应级；租约窗口（10min）内目标稳定，实际模型名仍以
	// Usage.Model 为准。
	modelLookup := func(agentName string) (string, string) {
		role := agentToRole(agentName)
		rep := models.SelectionReport(role)
		return rep.ConfigKey, rep.Model
	}
	baseOnMsg := store.Sessions.SubAgentLogger(modelLookup)
	onMsg := func(agentName, task string, msg agentcore.AgentMessage) {
		baseOnMsg(agentName, task, msg)
	}

	// 提示词缓存：一书一基、一角色一名、一会话一键（subagent spawn 追加 #seq）。
	// OpenAI 系用 prompt_cache_key 做路由亲和；Claude 系用 cache_control 滚动断点
	//（system 地板 + 末消息尖端）。provider 不支持时由 agentcore 按能力静默丢弃，
	// 多轮会话下读缓存收益恒为正，故不设开关。
	cacheBase := promptCacheBase(store.Dir())

	// ── 风格批评者子代理（纯文本一次性评审，无工具） ──
	criticModel := WithTrailingAntiRefusal(models.ForRoleWithFailover("critic", reportFailover), store)
	// 批评者提示词身份：实际 prompt 内容的 sha256 前缀（可溯源）
	promptHash := sha256.Sum256([]byte(bundle.Prompts.StyleCritic))
	criticPromptHash := "prompt:" + hex.EncodeToString(promptHash[:8])
	criticCfg := subagent.Config{
		Name:           "style_critic",
		Description:    "文风审查工具：评价草稿是否符合风格要求",
		Model:          criticModel,
		SystemPrompt:   bundle.Prompts.StyleCritic,
		MaxTurns:       1,
		MaxRetries:     subagentMaxRetries,
		ThinkingLevel:  resolvedRoleThinking(criticModel, cfg, "critic"),
		MaxTokens:      roleMaxOutput("style_critic"), // 预算表 §6：16,384
		OnMessage:      onMsg,
		PromptCacheKey: cacheBase + "-style_critic",
	}
	criticRunner := subagent.NewRunner(criticCfg)

	// 把 review_style 注入 writer 工具集（传递 prompt 内容哈希作为版本标识）。
	// polisher 无工具：评审与提交由流水线调用方在本工具返回后执行。
	reviewStyle := tools.NewReviewStyleTool(store, criticRunner, criticPromptHash)
	reviewStyle.SetPipelineEnabled(pipelineEnabled)
	writerTools = append(writerTools, reviewStyle)

	// 把 polish_draft 注入 writer 工具集（嵌套调用 polisher runner；pipeline 关闭时工具
	// 返回 skipped）。必须在 writer subagent 配置之前完成，writer 配置捕获的是追加后的列表。
	polishDraft := tools.NewPolishDraftTool(store, nil, "")
	polishDraft.SetEnabled(pipelineEnabled)
	writerTools = append(writerTools, polishDraft)

	// 统一注入章节流水线强制状态机配置（六工具：draft/edit/check/polish/review/commit）。
	// 必须在 writer 工具集完整（含 review_style/polish_draft 追加）之后执行；
	// plan_chapter 不进 FSM（写 plan 不改正文候选 digest）。六工具均实现
	// ChapterFSMConfigurable；其余工具（novel_context/read_chapter 等）不实现，
	// 类型断言自动跳过。
	for _, tl := range writerTools {
		if configurable, ok := tl.(tools.ChapterFSMConfigurable); ok {
			configurable.SetChapterFSMConfig(fsmCfg)
		}
	}

	architectStopGuardFactory := func(_, _ string) agentcore.StopGuard {
		return guard.NewArchitectStopGuard(store, onGuardBlock)
	}
	architectThinking, _ := ResolveThinkingForModel(architectModel, roleThinking(cfg, "architect"))
	architectShort := subagent.Config{
		Name:             "architect_short",
		Description:      "短篇规划师：为单卷、单冲突、高密度故事生成紧凑设定与扁平大纲",
		Model:            architectModel,
		SystemPrompt:     appendSceneGuidance(bundle.Prompts.ArchitectShort, contract.GuidanceForRole("architect_short")),
		Tools:            architectShortTools,
		MaxTurns:         15,
		MaxRetries:       subagentMaxRetries,
		ThinkingLevel:    architectThinking,
		MaxTokens:        roleMaxOutput("architect_short"), // 预算表 §6：32,768
		OnMessage:        onMsg,
		CacheLastMessage: "ephemeral",
		PromptCacheKey:   cacheBase + "-architect_short",
		StopAfterToolResult: func(toolName string, result json.RawMessage) bool {
			r := decodeSaveFoundationResult(toolName, result)
			return r.Type == "outline" && r.FoundationReady
		},
		StopGuardFactory: architectStopGuardFactory,
	}
	architectLong := subagent.Config{
		Name:                "architect_long",
		Description:         "长篇规划师：为连载型、可持续升级的故事生成分层设定与卷弧大纲",
		Model:               architectModel,
		SystemPrompt:        appendSceneGuidance(bundle.Prompts.ArchitectLong, contract.GuidanceForRole("architect_long")),
		Tools:               architectLongTools,
		MaxTurns:            20,
		MaxRetries:          subagentMaxRetries,
		ThinkingLevel:       architectThinking,
		OnMessage:           onMsg,
		CacheLastMessage:    "ephemeral",
		PromptCacheKey:      cacheBase + "-architect_long",
		StopAfterToolResult: architectLongShouldStopAfterToolResult,
		StopGuardFactory:    architectStopGuardFactory,
	}

	// 唯一组装路径:协议模板 {{VOICE}} 原位回填文风段,再追加风格预设。
	// eval 的 voice A/B 走同一函数,保证两臂等价(docs/voice-layer.md §3.2)。
	writerPrompt := assets.BuildWriterPrompt(bundle.Prompts.Writer, bundle.Voice, bundle.Styles[cfg.Style])
	writerPrompt = appendSceneGuidance(writerPrompt, contract.GuidanceForRole("writer"))

	restore := &ctxpack.WriterRestorePack{}
	restore.Refresh(store)

	writer := subagent.Config{
		Name:             "writer",
		Description:      "创作者：自主完成一章的构思、写作、自审和提交",
		Model:            writerModel,
		SystemPrompt:     writerPrompt,
		Tools:            writerTools,
		MaxTurns:         45,
		MaxRetries:       subagentMaxRetries,
		ThinkingLevel:    resolvedRoleThinking(writerModel, cfg, "writer"),
		StopAfterTools:   []string{"commit_chapter"},
		OnMessage:        onMsg,
		CacheLastMessage: "ephemeral",
		PromptCacheKey:   cacheBase + "-writer",
		StopGuardFactory: func(_, _ string) agentcore.StopGuard {
			return guard.NewWriterStopGuard(store, onGuardBlock)
		},
		ContextManagerFactory: func(model agentcore.ChatModel) agentcore.ContextManager {
			// 每次 subagent(writer) 调用都会重建，从当前 runModel 读取最新模型名。
			// /model 切换 writer 后下一章自动用新窗口。
			window, _ := cfg.ResolveContextWindow(bootstrap.ModelName(model))
			return newContextManager(contextManagerConfig{
				Model:            model,
				ContextWindow:    window,
				ReserveTokens:    bootstrap.CompactReserveTokens(window),
				KeepRecentTokens: 20000,
				Agent:            "writer",
				// 投影提交为新 baseline。瞬态投影在越阈后每次调用都重投影、
				// 切点滑动，等于每轮改写请求前缀（缓存全灭）；提交后回到
				// append-only，直到下次越阈。
				CommitOnProject: true,
				ToolMicrocompact: &corecontext.ToolResultMicrocompactConfig{
					IdleThreshold: 5 * time.Minute,
				},
				ExtraStrategies: []corecontext.Strategy{
					ctxpack.NewStoreSummaryCompact(ctxpack.StoreSummaryCompactConfig{
						Store:            store,
						KeepRecentTokens: 20000,
					}),
				},
				Summary: &corecontext.FullSummaryConfig{
					PostSummaryHooks:    []corecontext.PostSummaryHook{restore.Hook()},
					SystemPrompt:        ctxpack.WriterSummarySystemPrompt,
					SummaryPrompt:       ctxpack.WriterSummaryPrompt,
					UpdateSummaryPrompt: ctxpack.WriterUpdateSummaryPrompt,
					TurnPrefixPrompt:    ctxpack.WriterTurnPrefixPrompt,
				},
			})
		},
	}

	editor := subagent.Config{
		Name:             "editor",
		Description:      "审阅者：阅读原文，从结构和审美两个层面发现问题",
		Model:            editorModel,
		SystemPrompt:     appendSceneGuidance(bundle.Prompts.Editor, contract.GuidanceForRole("editor")),
		Tools:            editorTools,
		MaxTurns:         20,
		MaxRetries:       subagentMaxRetries,
		ThinkingLevel:    resolvedRoleThinking(editorModel, cfg, "editor"),
		MaxTokens:        roleMaxOutput("editor"), // 预算表 §6：32,768
		OnMessage:        onMsg,
		CacheLastMessage: "ephemeral",
		PromptCacheKey:   cacheBase + "-editor",
		// 终态产物命中即停。终态退出仍会咨询 StopGuard（契约测试 TestContract_
		// TerminalToolExitConsultsStopGuard），任务感知的 NewEditorStopGuard 负责
		// 否决"被派生成摘要却只做了复核"的提前退出，所以 save_review 可以安全硬停。
		StopAfterToolResult: func(toolName string, _ json.RawMessage) bool {
			return toolName == "save_review" || toolName == "save_arc_summary" || toolName == "save_volume_summary"
		},
		StopGuardFactory: func(_, task string) agentcore.StopGuard {
			return guard.NewEditorStopGuard(store, task, onGuardBlock)
		},
	}

	// ── 文风精修师子代理（候选工具协议：独立 Runner / system prompt / ContextManager /
	// cache key / provenance）。由 polish_draft 工具在 Writer 单次 Run 内嵌套调用——
	// 模式与 review_style 内部启动 critic runner 一致；不 Swap 同一 Writer 会话。
	// 全部上下文（草稿全文 + 精修依据 + 既有 findings + rewrite brief）由 buildPolishTask
	// 塞进任务文本；模型通过三个候选工具（submit_polish_plan / submit_edit_batch /
	// finish_polish）提交 plan/batch/finish（schema docs/polisher-candidate-tools-schema.md
	// §2），产物由 polish_draft 统一验证并一次性落盘。
	// MaxTurns=6（初始 + plan + 最多 4 批 + finish，schema §11）；finish_polish 是
	// 停止工具（StopAfterTools），调用成功后 runner 结束。StopGuard 仍用
	// NewPolisherStopGuard：agentcore 在 StopAfterTools 命中后同样咨询 StopGuard，
	// 该 guard 恒放行 end_turn（仅 provider 拒答升级），与停止工具不冲突。
	// MaxTokens=预算表默认 65,536（计划 §6）；131,072 仅作为经过验证的精确模型
	// override（polisherHighOutputModels，见 budgets.go），不能全局提升。
	// LengthRecoveryPrompt 要求从头重新输出完整内容：默认 recovery prompt 是
	// "从截断处续写"，若第一轮已输出部分 JSON、第二轮只续写尾段，RunResult 只返回
	// 尾段，解析必然失败且内容残缺——故必须显式覆盖为整表重输出。
	polisherModel := WithTrailingAntiRefusal(models.ForRoleWithFailover("polisher", reportFailover), store)
	_, polisherModelName, _ := models.CurrentSelection("polisher")
	polisherContextWindow, polisherSource := cfg.ResolveContextWindow(polisherModelName)
	bootstrap.LogContextWindowChoice("polisher", polisherModelName, polisherContextWindow, polisherSource)

	// 精修者提示词身份：实际 prompt 内容的 sha256 前缀（可溯源，随 checkpoint 落盘）
	polisherHash := sha256.Sum256([]byte(bundle.Prompts.Polisher))
	polisherPromptHash := "prompt:" + hex.EncodeToString(polisherHash[:8])

	polisher := subagent.Config{
		Name:             "polisher",
		Description:      "文风精修师：在不改变剧情事实的前提下打磨草稿文风/节奏/色气",
		Model:            polisherModel,
		SystemPrompt:     bundle.Prompts.Polisher,
		Tools:            polisherTools,
		MaxTurns:         6,
		MaxRetries:       subagentMaxRetries,
		ThinkingLevel:    resolvedRoleThinking(polisherModel, cfg, "polisher"),
		OnMessage:        onMsg,
		CacheLastMessage: "ephemeral",
		PromptCacheKey:   cacheBase + "-polisher",
		// 覆盖 agentcore 默认的"从截断处续写"recovery prompt：length 截断后要求模型
		// 从头完整重输出 edit 列表 JSON，避免仅返回尾段导致 JSON 残缺（见上方注释）。
		LengthRecoveryPrompt: "上一次输出被截断。不要从中间续写；请从第一条 edit 开始，重新输出完整的精修 edit 列表 JSON（{\"version\":1,\"edits\":[{\"old_string\":\"原文片段\",\"new_string\":\"精修后片段\"}]}）。只输出该 JSON，不要附加任何其他内容。",
		// 预算表默认 65,536（计划 §6）；131,072 仅对 polisherHighOutputModels 中
		// 经过验证的模型生效（见 budgets.go），并按 min(上限, registry 上限,
		// contextWindow-8k 安全余量) 收敛。
		MaxTokens:      PolisherMaxOutputTokens(polisherModelName, polisherContextWindow),
		StopAfterTools: []string{"finish_polish"},
		StopGuardFactory: func(_, _ string) agentcore.StopGuard {
			// polisher 正常路径以最终 edit 列表 JSON 响应结束（产物由 polish_draft
			// 工具校验并落盘），不能用 writer 同款 guard：polisher 协议禁止自行
			// commit，要求 commit checkpoint 会让每次正常结束都被拦截、连续
			// 空转后 escalate，polish_draft 永远失败（实测 63 章 rewrite
			// 死循环）。恒放行 end_turn，仅对 provider 拒答立即升级。
			return guard.NewPolisherStopGuard()
		},
		ContextManagerFactory: func(model agentcore.ChatModel) agentcore.ContextManager {
			// 与 writer 同款配置（窗口随模型动态解析），agent 名="polisher"。
			window, _ := cfg.ResolveContextWindow(bootstrap.ModelName(model))
			return newContextManager(contextManagerConfig{
				Model:            model,
				ContextWindow:    window,
				ReserveTokens:    bootstrap.CompactReserveTokens(window),
				KeepRecentTokens: 20000,
				Agent:            "polisher",
				CommitOnProject:  true,
				ToolMicrocompact: &corecontext.ToolResultMicrocompactConfig{
					IdleThreshold: 5 * time.Minute,
				},
				ExtraStrategies: []corecontext.Strategy{
					ctxpack.NewStoreSummaryCompact(ctxpack.StoreSummaryCompactConfig{
						Store:            store,
						KeepRecentTokens: 20000,
					}),
				},
				Summary: &corecontext.FullSummaryConfig{
					PostSummaryHooks:    []corecontext.PostSummaryHook{restore.Hook()},
					SystemPrompt:        ctxpack.WriterSummarySystemPrompt,
					SummaryPrompt:       ctxpack.WriterSummaryPrompt,
					UpdateSummaryPrompt: ctxpack.WriterUpdateSummaryPrompt,
					TurnPrefixPrompt:    ctxpack.WriterTurnPrefixPrompt,
				},
			})
		},
	}
	polisherRunner := subagent.NewRunner(polisher)

	// 把嵌套调用的 polisher runner 挂到早期注入 writer 工具集的 polish_draft 实例上。
	polishDraft.SetPolisherRunner(polisherRunner, polisherPromptHash)

	// 注册提示词前缀观测基线（Prefix Manifest 的分段估算数据源）：system prompt
	// 与 tools schema 是每次请求都会重发的稳定前缀；basis/draft 是工具动态产出，
	// 由 usage manifest 用 Input/CacheRead 的跨请求增量推断边界（见 domain.PrefixManifest）。
	// 注意 writerTools 已是最终列表（已追加 review_style / polish_draft）。
	models.SetAgentPrefixBaseline("architect_short", prefixBaselineFor(architectShort.SystemPrompt, architectShortTools))
	models.SetAgentPrefixBaseline("architect_long", prefixBaselineFor(architectLong.SystemPrompt, architectLongTools))
	models.SetAgentPrefixBaseline("writer", prefixBaselineFor(writer.SystemPrompt, writerTools))
	models.SetAgentPrefixBaseline("editor", prefixBaselineFor(editor.SystemPrompt, editorTools))
	models.SetAgentPrefixBaseline("polisher", prefixBaselineFor(polisher.SystemPrompt, polisherTools))
	models.SetAgentPrefixBaseline("style_critic", prefixBaselineFor(criticCfg.SystemPrompt, nil))

	subagentRunner := subagent.NewRunner(architectShort, architectLong, writer, editor, polisher)

	// 用量记录改由 run observer 驱动：OnMessage 回调没有 run 实例标识，而
	// RunMeta.InstanceID（= agent#runSeq，与 prompt cache key 的 #seq 同源）
	// 只有事件流能拿到——故每个 runner 挂一个独立 observer，把 InstanceID 透传
	// 给 UsageRecorder（Prefix Manifest 的 RunID 据此对齐真实缓存血统；task 由
	// observer 从 run 首条 user 消息捕获）。session 日志仍走 OnMessage 路径。
	if recordUsage != nil {
		for _, r := range []*subagent.Runner{criticRunner, polisherRunner, subagentRunner} {
			r.SetEventObserver(newRunUsageObserver(recordUsage).OnEvent)
		}
	}

	// 运行时联动各角色推理强度(经 subagentRunner override;/model 调整用)。
	applyThinking := func(role string, level agentcore.ThinkingLevel) {
		switch role {
		case "architect":
			level, _ = ResolveThinkingForModel(models.ForRole("architect"), level)
			subagentRunner.SetThinkingLevel("architect_short", level)
			subagentRunner.SetThinkingLevel("architect_long", level)
		case "writer", "editor", "polisher":
			level, _ = ResolveThinkingForModel(models.ForRole(role), level)
			subagentRunner.SetThinkingLevel(role, level)
		}
	}

	return subagentRunner, askUser, restore, applyThinking
}

type saveFoundationResult struct {
	Type            string `json:"type"`
	FoundationReady bool   `json:"foundation_ready"`
}

func decodeSaveFoundationResult(toolName string, result json.RawMessage) saveFoundationResult {
	if toolName != "save_foundation" {
		return saveFoundationResult{}
	}
	var r saveFoundationResult
	_ = json.Unmarshal(result, &r)
	return r
}

func architectLongShouldStopAfterToolResult(toolName string, result json.RawMessage) bool {
	r := decodeSaveFoundationResult(toolName, result)
	switch r.Type {
	case "expand_arc", "complete_book":
		return true
	default:
		return false
	}
}
