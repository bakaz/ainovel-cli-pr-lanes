# Full-context Polisher 改造背景与用户需求

> 配套实施计划：[handoff-polisher-style-token-plan.md](./handoff-polisher-style-token-plan.md)
>
> 本文用于回答三个问题：用户真正想解决什么、事情为什么发展到当前方案、接手者不能误解哪些已经确认的需求。

## 1. 用户的核心需求

用户要解决的不是单纯的 token 超限，也不是把 Polisher 做成更便宜的文本替换器。核心目标是：

> 在保留甚至提升全章文风判断能力的前提下，解决 Polisher/Critic 输出截断、审计超限、错误重试、CPU 放大和 style review 错误耗尽的问题。

具体需求包括：

1. 小说文风需要继续提高，不能只追求流程稳定或成本下降。
2. Polisher 必须是阅读全文、主动审查问题并实施精修的编辑角色。
3. Polisher 必须完整获得章节正文、事实大纲和当前章节 findings。
4. Polisher 不只能执行 Critic 给出的工单，还要发现 Critic 或 Writer 没发现的问题。
5. 必须保留“约 20 个 edits”和“最多 32 个 edits”。
6. “约 20 个”是正常工作量目标，不是强制最低数；原稿好时不能为凑数过度修改。
7. 解决长输出问题时，应拆分候选提交，而不是拆掉全文阅读和全局判断。
8. Polisher 可以借鉴 Writer 的多轮工具机制，但不能获得 Writer 的事实、存储和提交权限。
9. 技术错误不能被计入内容审查次数，也不能错误触发 `style review exhausted`。
10. token、重试、审计和 CPU 必须有明确上限，但不能用降低文风能力换取稳定。
11. 改造要能拆给不同 Agent 开发，并有清晰分支、文件所有权、集成顺序和回滚开关。
12. 本地项目已经形成的语义优先；外部项目和上游只用于参考，不能直接覆盖本地需求。

## 2. 事情的由来

### 阶段一：文风能力比较

讨论最初不是从性能优化开始，而是从“当前项目是否还有更好文风的可能性”开始。用户要求分析和参考：

- `AI_NovelGenerator`；
- `show-me-the-story`；
- `harnessNovel`；
- 上游更新后的 `ainovel-cli`。

目的不是机械同步代码，而是判断这些项目在规划、写作、审稿、风格控制和多 Agent 协作方面有没有值得吸收的机制。

用户随后要求采用 Sol/Luna 工作流做审查，并把不同能力拆成分支和不同 Agent 开发。本地逻辑被明确设为优先，远程或参考实现只能选择性吸收。

### 阶段二：多 Agent 和自动运行能力进入实际使用

项目逐步形成 Writer、Critic、Polisher 等 Agent 角色，同时增加了 prompt 注入、TUI 开关、闲时写作、峰时自动暂停/恢复和运行状态恢复等功能。

这些功能投入实际运行后，用户开始持续检查真实日志和运行状态，而不是只看单元测试。由此暴露出：

- 启动后自动恢复不稳定；
- TUI 刷新、窗口重绘和 CPU 占用问题；
- 运行中存在不需要的历史子智能体；
- 调度、Agent 切换和重试可能放大资源消耗；
- style 审查频繁出现超限或 exhausted。

部分 TUI/调度问题已经单独修复。本 handoff 不重新处理所有 TUI 问题，但这些运行现象促使用户进一步审查 Agent 调用量、token 和重试链路。

### 阶段三：审计超限与 token 问题

用户发现 Polisher、Critic 偶尔超过输出限制，并提出是否把所有 `max output token` 调整到 65,536，同时询问 `style_critic`/Polisher 应怎样处理。

代码和日志检查显示，问题不只是一个配置值：

- Polisher 一次接收完整章节、完整事实依据和 findings；
- 一次性输出约 20、最多 32 个 edits；
- reasoning 模型的隐藏 thinking 与最终 JSON 共用 output budget；
- 流式请求可能出现 EOF、missing DONE 或空输出；
- JSON 可能被 Markdown fence 包裹；
- runner、HTTP 和空输出恢复可能形成重试乘法；
- 完整任务失败后可能从头执行，导致 token、CPU 和审计体积同步放大。

旧 one-shot Polisher 曾把 `max output token` 提高到 131,072，主要是为了缓解特定 reasoning 模型在 65,536 下的 thinking+JSON 截断。这个值是旧协议的止血措施，不应直接扩散为所有角色的全局默认。

### 阶段四：提出把 Polisher 拆成工具

用户进一步询问：“Polisher 的任务能不能拆分成不同的 tools？”随后又提出是否应模仿 Writer 的机制，并要求 Sol 评审。

这个方向的原始意图是：

- 让 Polisher 仍然完成完整审稿；
- 不再要求一次响应塞下全部 20–32 个 edits；
- 已成功生成的候选不因后续批次失败而重新输出；
- 保持最终一次性验证和写入；
- 降低 length recovery、重试风暴和 audit-over-limit。

也就是说，用户要拆的是输出过程，不是 Polisher 的认知范围。

### 阶段五：第一版方案发生角色偷换

第一版 Sol 计划采用了“diagnostics 生成最小工单、Polisher 只做局部 span”的思路，提出：

- Polisher 不再接收完整章节；
- 最多处理 3 个局部 spans；
- 每个 span 约 1,200 runes；
- 总覆盖率约 15%；
- Polisher 保持 zero-tool；
- 广泛或全局问题交回 Writer。

这套方案有利于降低输出和重试成本，但改变了角色本身：Polisher 从“阅读全文并主动精修的资深编辑”变成了“根据上游 finding 做局部替换的执行器”。

用户明确否决了这种角色变化，并指出必须保留：

1. “完整阅读全章”；
2. “约 20 个 edits”；
3. “最多 32 个 edits”；
4. 完整事实大纲；
5. 完整 findings。

这是本 handoff 最重要的需求修正。

### 阶段六：形成当前共识

重新讨论后，用户确认了以下逻辑：

> Polisher 继续阅读全文、完整审稿和主动发现问题；借鉴 Writer 的多轮机制，把计划和 edits 分批提交；Polisher 只提交候选，Host 最后统一校验、CAS 和落盘。

因此，当前方案采用：

```text
完整上下文
  → submit_polish_plan
  → submit_edit_batch × 1..4
  → finish_polish
  → 统一 validate/CAS
  → 一次 SaveDraft/checkpoint
```

## 3. 为什么全文阅读不可替代

局部 span 可以修句子，但不能稳定判断：

- 全章节奏前紧后松或前松后紧；
- 同类句式、比喻、动作和情绪是否反复；
- 人物声音是否在章节内逐渐漂移；
- 不同场景的叙述密度是否失衡；
- 单句是否正确但放在全文中显得冗余；
- 场景衔接、情绪曲线和信息释放是否顺畅；
- 既有 findings 是否遗漏了真正影响阅读体验的问题。

这也是 Polisher 与普通 replace/edit 工具的区别。若 Polisher 只看局部文本，`style_critic` 就会被迫承担完整审稿责任，Polisher 本身会失去作为独立角色的必要性。

## 4. 当前代码中的角色和流程

### Writer

Writer 拥有章节生命周期：读取上下文、规划、起草、编辑、一致性检查、风格链路和最终提交。它有工具、FSM、StopGuard、ContextManager 和 checkpoint。

Writer 负责事实和结构，Polisher 不应复制这些权限。

### style_critic

`style_critic` 是独立、无工具的审查 runner，负责在 Polisher 之后进行终审。它应发现剩余阻断问题和 Polisher 引入的问题，但不应成为 Polisher 唯一的问题来源。

### 当前 one-shot Polisher

当前 `polish_draft` 大致执行：

1. 捕获完整 `PolishBaseline`；
2. 构造完整章节、风格依据、事实大纲、findings 和 rewrite brief；
3. 调用无工具 Polisher；
4. 接收一个完整 JSON edit list；
5. 在同一 baseline 上验证和应用；
6. 一次 SaveDraft 和一次 polish checkpoint；
7. 继续 consistency/style review/commit。

现有安全基础包括：

- draft digest CAS；
- style-review ledger fingerprint；
- polish sequence 绑定；
- mutation permission；
- edit 重叠、覆盖率、长度和匹配校验；
- partial/degraded/error category；
- legacy full-text fallback；
- 一次最终原子提交语义。

新方案应复用这些机制，不应重写章节存储模型。

## 5. 已观察到的运行问题

历史日志和运行检查中出现过：

- `stream ended before [DONE]`；
- `unexpected EOF`；
- missing DONE；
- 空输出；
- length 截断；
- JSON 被 Markdown fence 包裹；
- audit-over-limit；
- 同一任务多层重复重试；
- 技术失败后 style review 进入 exhausted；
- 运行中的 Agent 数量和 CPU 占用偏高。

对 Polisher/style 链路的风险假设是：

```text
完整大输入
  + 一次性大 edit list
  + 隐藏 thinking
  + 流/JSON 错误
  + runner/HTTP/empty 多层重试
  → 完整任务反复执行
  → token、CPU、API 调用和审计体积同步放大
```

这仍需要通过 actual API call、finish reason、usage、retry source 和 CPU 指标验证。不能在没有数据时把全部 CPU 问题简单归因于调度器或 Polisher。

## 6. 已冻结的产品决策

后续 Agent 不得自行推翻以下决定：

1. Polisher 阅读完整章节。
2. Polisher 接收完整事实大纲。
3. Polisher 接收当前章节完整 findings。
4. Polisher 主动发现新问题。
5. 约 20 edits 是软目标。
6. 32 edits 是硬上限。
7. 原稿质量高时允许少量 edits 或 no-op。
8. 输出拆成 plan、最多 4 个 batch 和 finish。
9. 每批最多 8 edits。
10. 所有 edits 基于同一个 baseline。
11. Polisher 只有候选提交工具，没有 SaveDraft、事实写入或 commit 权限。
12. Host 统一验证，最终只进行一次章节写入和一次逻辑 polish checkpoint。
13. `style_critic` 保持独立终审。
14. 技术失败不增加 content attempt，不触发 exhausted。
15. 旧 one-shot 路径在 shadow/canary 完成前必须保留用于回滚。

## 7. 与历史 rollout 文档的关系

[style-reference-rollout.md](./style-reference-rollout.md) 曾提出 diagnostics 生成少量局部 work order、Polisher 做局部可逆编辑。该文档仍可用于参考：

- 事实保护；
- anchor 和 digest；
- 可逆修改；
- 后续 consistency/world/commit gates。

但其中“最多 3 spans”等实验性限制不能再被用作 Polisher 正式角色定义。当前已确认的产品需求以本文和配套 handoff 为准。

## 8. Token 决策背景

全文输入主要消耗 context window；一次性 20–32 edits 和隐藏 thinking 主要消耗 output budget。这两件事必须分开解决。

新方案保留完整输入，通过多轮有界工具提交降低单次输出压力。当前计划建议：

- Polisher 默认每轮 `max output token=65,536`；
- plan + 约 3 batches + finish，正常约 5 次逻辑调用；
- 最大 4 batches，共 6 次逻辑调用；
- 加技术恢复后单 run 实际 API 调用硬上限 8；
- 131,072 仅作为经过验证的精确模型/provider override；
- Critic 等短结构化角色使用更低的角色级预算。

是否启用 131,072 必须由实际 usage、finish reason 和截断率决定，不能只按模型名称或全局偏好设置。

## 9. Style review budget 背景

需要区分：

- 内容 revise：Critic 对当前有效稿件提出真实风格问题；
- 技术失败：length、EOF、空输出、网络、timeout、malformed JSON、audit-over-limit；
- 并发状态：CAS stale；
- 安全拒绝：候选改变事实或超出权限；
- 用户裁决：style override。

只有内容 revise 可以增加 content attempt 并最终触发 exhausted。其他事件可以记录各自的技术、stale 或 override 状态，但不得消耗内容预算。

## 10. 上游和依赖约束

agentcore 的只读依赖源码位于：

```text
.slim/clonedeps/repos/voocel__agentcore/
```

该目录不能编辑。若需要 `MaxLengthRecoveries`、实际 API 调用总预算或 streaming tool-call 恢复，应在正式 agentcore 上游分支实现，再更新主项目依赖。

协议改造、provider/model 切换、agentcore recovery、token/thinking 和 review budget 不得全部同时上线，否则无法判断质量和性能变化来自哪里。

## 11. 本轮范围与非目标

范围：

- 完整上下文 Polisher；
- plan/batch/finish 候选工具；
- Writer/Polisher/style critic prompt 协作；
- 角色级 output token；
- run 级 retry/API call 总预算；
- style content/technical budget；
- CAS、审计、shadow 和 A/B 验收。

非目标：

- 让 Polisher 获得章节提交权；
- 改写 Writer 的事实和存储模型；
- 第一阶段建设持久化 Polisher mini-runner；
- 同时更换主模型/provider；
- 用本方案声称解决所有 TUI/调度器 CPU 问题；
- 立即删除 legacy one-shot Polisher。

## 12. 接手者阅读顺序

1. 本背景文档。
2. [handoff-polisher-style-token-plan.md](./handoff-polisher-style-token-plan.md)。
3. `assets/prompts/writer.md`。
4. `assets/prompts/polisher.md`。
5. `assets/prompts/style-critic.md`。
6. `internal/tools/polish_draft.go` 和 edit-plan 校验代码。
7. `internal/agents/build.go` 和 Polisher StopGuard。
8. style review domain/store/checkpoint 代码。
9. [style-reference-rollout.md](./style-reference-rollout.md)，仅作为历史和局部安全参考。
10. agentcore 只读 clone 中的 streaming/length recovery 实现。

## 13. 当前交接状态

- 用户需求、事情由来和设计转折已记录。
- 实施方案见配套 handoff。
- 尚未修改业务代码。
- 尚未创建实现分支。
- 两份 handoff 文档尚未提交。
- 下一步应先冻结三个候选工具 schema，再按照实施 handoff 拆分 Agent 开发包。
