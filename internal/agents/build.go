package agents

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
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

// subagentMaxRetries 是所有 Worker 的 LLM retry 上限。
// 退避策略：指数退避（受 maxDelay 上限约束），优先服从 server Retry-After。
// retryable 错误（stream-idle / 503 / 短暂网络抖动）在 Worker 层就近重试，
// 而不是让 Engine 重跑整个任务。
// 项目铁律一保证写类工具走 checkpoint+digest 幂等，重试是安全的。
const subagentMaxRetries = 7

// UsageRecorder 是 BuildWorkers 可选的用量回调；签名与 OnMessage 一致，
// 每条 agent 消息都会调一次，由 Host 层负责聚合。task 是本次 spawn 的任务文本
// 作为会话身份，供缓存链断裂检测按会话重置基线。
// nil 表示不追踪。
type UsageRecorder func(agentName, task string, msg agentcore.AgentMessage)

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
		// Polisher 无工具：纯文本转换的嵌套模型，全部上下文（草稿全文 + 精修依据 +
		// 既有 findings + rewrite brief）已由 polish_draft 的 buildPolishTask 塞进任务文本。
		Polisher:   []agentcore.Tool{},
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

	// 精修流水线开关（chapter_pipeline="ds_mimo_critic" 或 roles.polisher 显式配置）：
	// 开启时注入 polish_draft 强制（工具 enabled + writer 协议）与 commit gate 校验
	// （fresh polish checkpoint + 模型一致性 + seq 绑定）。未配置时所有工具保持旧行为。
	// 与 StyleReviewMode 的关系（C1-H4）：pipeline 开关只控制 polish 工具与 pipeline
	// commit gate，不强制 critic mode——两者独立。pipeline 启用 + mode=off 是可接受
	// 组合：此时 commit 的 pipeline gate 仍要求 fresh polish checkpoint（现状保留），
	// 但评审门控（CheckCommitStyleGate）不生效。
	pipelineEnabled := cfg.ChapterPipelineEnabled()
	// expectedPolisherModel 提升到 BuildWorkers 作用域：pipeline 开启且 Roles 显式
	// 配置 polisher 时记录其当前模型名，供 commit gate 与章节 FSM（polish 绑定）
	// 共用；空 = 未显式配置（跳过模型一致性校验）。
	expectedPolisherModel := ""
	if pipelineEnabled {
		if _, ok := cfg.Roles["polisher"]; ok {
			_, expectedPolisherModel, _ = models.CurrentSelection("polisher")
		}
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

	architectModel := models.ForRoleWithFailover("architect", reportFailover)
	writerModel := models.ForRoleWithFailover("writer", reportFailover)
	editorModel := models.ForRoleWithFailover("editor", reportFailover)

	// Writer 的 ContextManager 由工厂每次调用重建，窗口随模型 swap 动态跟随（见下方工厂）。
	_, writerModelName, _ := models.CurrentSelection("writer")
	writerContextWindow, writerSource := cfg.ResolveContextWindow(writerModelName)
	bootstrap.LogContextWindowChoice("writer", writerModelName, writerContextWindow, writerSource)

	// modelLookup 写入 session 时给每条 assistant 消息附 _meta:{provider,model}，
	// 让 replay 不再依赖"当前 ModelSet"来反推历史 cost，运行中切换模型也能精确算。
	modelLookup := func(agentName string) (string, string) {
		role := agentToRole(agentName)
		provider, name, _ := models.CurrentSelection(role)
		return provider, name
	}
	baseOnMsg := store.Sessions.SubAgentLogger(modelLookup)
	onMsg := func(agentName, task string, msg agentcore.AgentMessage) {
		baseOnMsg(agentName, task, msg)
		if recordUsage != nil {
			recordUsage(agentName, task, msg)
		}
	}

	// 提示词缓存：一书一基、一角色一名、一会话一键（subagent spawn 追加 #seq）。
	// OpenAI 系用 prompt_cache_key 做路由亲和；Claude 系用 cache_control 滚动断点
	//（system 地板 + 末消息尖端）。provider 不支持时由 agentcore 按能力静默丢弃，
	// 多轮会话下读缓存收益恒为正，故不设开关。
	cacheBase := promptCacheBase(store.Dir())

	// ── 风格批评者子代理（纯文本一次性评审，无工具） ──
	criticModel := models.ForRoleWithFailover("critic", reportFailover)
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
	fsmCfg := tools.ChapterFSMConfig{
		Enabled:               true,
		PipelineEnabled:       pipelineEnabled,
		ExpectedPolisherModel: expectedPolisherModel,
	}
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

	// ── 文风精修师子代理（无工具、纯文本转换的嵌套模型：独立 Runner / system prompt /
	// ContextManager / cache key / provenance）。由 polish_draft 工具在 Writer 单次 Run 内
	// 嵌套调用——模式与 review_style 内部启动 critic runner 一致；不 Swap 同一 Writer 会话。
	// 全部上下文（草稿全文 + 精修依据 + 既有 findings + rewrite brief）由 buildPolishTask
	// 塞进任务文本，模型只输出整章正文作为最终响应。 ──
	// MaxTurns=3（1 初始 + 最多 2 次 length recovery）：polisher 模型 thinking 极长，
	// 实测频繁被 max_tokens 截断（StopReason=length）。agentcore 对 length 截断自动注入
	// recovery prompt 继续下一轮，但循环在每轮顶部按 turnCount(>=MaxTurns) fail-closed；
	// MaxTurns=1 会让首次 recovery 立即撞 MaxTurnsError（生产 55 章 rewrite 卡死根因）。
	// MaxTurns=3 允许连续 2 次 recovery；连续 3 次 length 时第 4 次调用前报 MaxTurnsError
	// fail-closed（比 agentcore 内部 recovery 预算放行截断结果更保守，见 defaultMaxLengthRecoveries）。
	// LengthRecoveryPrompt 要求从头重新输出完整章节：默认 recovery prompt 是"从截断处续写"，
	// 若第一轮已输出部分正文、第二轮只续写尾段，RunResult 只返回尾段，polish_draft 会把
	// 尾段当完整章节保存（章节截头覆盖草稿）——故必须显式覆盖为整章重写。
	// MaxTokens=131072：mimo-v2.5（小米 MiMo-V2.5）真实输出上限 131072 tokens
	//（OpenRouter top_provider.max_completion_tokens 四方一致）；agentcore 默认
	// GenerationConfig.MaxTokens=65536 会把 75-97K 字符的 thinking 截断（thinking 与
	// 最终回答共用 max_tokens 预算），与 MaxTurns 卡死同源——显式放宽为模型真实上限，
	// 为 thinking+正文留足余量且不超过模型上限。其他 agent 不设置（保持默认 65536）。
	polisherModel := models.ForRoleWithFailover("polisher", reportFailover)
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
		MaxTurns:         3,
		MaxRetries:       subagentMaxRetries,
		ThinkingLevel:    resolvedRoleThinking(polisherModel, cfg, "polisher"),
		OnMessage:        onMsg,
		CacheLastMessage: "ephemeral",
		PromptCacheKey:   cacheBase + "-polisher",
		// 覆盖 agentcore 默认的"从截断处续写"recovery prompt：length 截断后要求模型
		// 从章节标题开始完整重输出，避免仅返回尾段导致章节截头覆盖草稿（见上方注释）。
		LengthRecoveryPrompt: "上一次输出被截断。不要从中间续写；请从章节标题开始，重新输出完整的精修后章节。只输出完整正文。",
		// mimo-v2.5 真实 max output = 131072（见上方注释），显式覆盖默认 65536。
		MaxTokens: 131072,
		StopGuardFactory: func(_, _ string) agentcore.StopGuard {
			// polisher 正常路径以最终文本响应结束（产物由 polish_draft 工具
			// 校验并落盘），不能用 writer 同款 guard：polisher 协议禁止自行
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

	subagentRunner := subagent.NewRunner(architectShort, architectLong, writer, editor, polisher)

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
