# Full-context Polisher / Style / Token 改造 Handoff

> 状态：设计已确认，尚未修改业务代码。
>
> 核心修订：Polisher 是阅读全文、主动审查并执行精修的角色。拆分的是候选输出，不是阅读范围。

## 1. 最终角色定义

Polisher 必须完整接收并阅读：

- 当前章节全文；
- 完整事实大纲；
- 当前章节完整 findings，包括状态和来源；
- 当前风格依据、用户规则、rewrite brief 和必要章节约束。

Polisher 需要独立审查全文，处理既有 findings，并主动发现节奏、重复、句式、叙述密度、人物声音、场景衔接等新问题。它应提交约 20 个有价值的 edits，最多 32 个。

“约 20 个 edits”是软目标，不是最低配额。问题少时允许少量修改或 no-op；严禁为凑数过度修改。32 是单次精修的硬上限。

角色边界：

- Writer：章节规划、正文生成、结构、情节、事实、人物状态和最终提交。
- Polisher：阅读全文、主动审稿和整体文风精修，但不能改变事实和结构。
- `style_critic`：润色后的独立终审，判断 pass/revise，不是 Polisher 的上游大脑。
- `polish_draft`：冻结输入、承载 Polisher 会话、收集候选、统一验证并一次性写入。

## 2. 架构裁定

Polisher 采用受限的 Writer 式多轮机制：保留完整上下文和多轮推理能力，只提供候选提交工具。

```text
polish_draft 捕获完整 baseline
  → Polisher 阅读全文和全部依据
  → submit_polish_plan
  → submit_edit_batch（每批最多 8 项，最多 4 批）
  → finish_polish
  → 主程序统一 validate
  → CAS
  → 一次 SaveDraft + 一次 polish checkpoint
  → check_consistency → style review → commit
```

Polisher 只拥有：

1. `submit_polish_plan`
2. `submit_edit_batch`
3. `finish_polish`

不得暴露 `read_chapter`、`edit_chapter`、`draft_chapter`、世界状态写入、事实账本写入、`check_consistency`、`review_style` 或 `commit_chapter`。这些工具只能提交候选，不能直接保存 draft 或 checkpoint。

## 3. 完整输入契约

一次 run 必须冻结：

```json
{
  "chapter": 123,
  "baseline_digest": "sha256:...",
  "chapter_text": "完整章节",
  "factual_outline": "完整事实大纲",
  "findings": ["当前章节完整 findings"],
  "style_basis": {},
  "rewrite_brief": {},
  "ledger_epoch": 1,
  "ledger_fingerprint": "...",
  "polish_seq": 4,
  "prompt_version": "polisher-v3",
  "protocol_version": 3
}
```

完整章节、完整事实大纲和完整 findings 不得为了 token 预算静默删除、截断或替换成局部摘要。若 provider 上下文无法容纳核心输入，应返回明确的 technical/degraded 结果，不能以局部处理冒充完整审阅。

可以压缩的只有重复说明、历史对话、已被结构化字段表达的冗余规则和非当前章节辅助材料。

## 4. 候选工具协议

### 4.1 `submit_polish_plan`

全文审阅后必须首先调用：

```json
{
  "operation_id": "...",
  "baseline_digest": "sha256:...",
  "overall_assessment": "全章风格判断",
  "planned_edit_count": 20,
  "issues": [
    {
      "issue_id": "p-001",
      "source_finding_ids": ["f-12"],
      "origin": "existing_finding|polisher_discovered",
      "category": "rhythm|repetition|voice|density|clarity|imagery|transition",
      "priority": "high|medium|low",
      "anchor": "精确连续原文",
      "problem": "问题",
      "edit_goal": "修改目标",
      "fact_risk": "none|low|high",
      "action": "edit|defer_to_writer|no_op"
    }
  ]
}
```

最多规划 32 个 edit issues。必须区分既有 finding 和自主发现；涉及事实、结构、时间线、人物状态或因果主体的问题必须 `defer_to_writer`。

### 4.2 `submit_edit_batch`

每批最多 8 个 edits，最多 4 批。所有 `old_string` 必须来自同一个原始 baseline，不能依赖前一批修改后的文本。

```json
{
  "operation_id": "...",
  "batch_index": 1,
  "edits": [
    {
      "issue_id": "p-001",
      "old_string": "baseline 中的精确连续原文",
      "new_string": "候选修改",
      "reason": "修改理由",
      "category": "rhythm",
      "fact_check": "unchanged"
    }
  ]
}
```

Host 每批只做确定性预检并存入内存 accumulator：

- operation/baseline 是否一致；
- issue 是否存在于 plan；
- `old_string` 是否唯一命中 baseline；
- 是否与其他候选重叠；
- 单项和批次是否超限；
- 是否改变数字、专名、称谓、时间或因果主体。

工具只返回 accepted/rejected 计数和错误码，不回显完整候选文本。

### 4.3 `finish_polish`

```json
{
  "operation_id": "...",
  "status": "complete|partial|no_op|escalate",
  "submitted_edit_count": 20,
  "covered_issue_ids": ["p-001"],
  "unresolved": [
    {
      "issue_id": "p-021",
      "reason": "requires_structural_rewrite",
      "recommended_owner": "writer"
    }
  ],
  "summary": "本次精修摘要"
}
```

`finish_polish` 是 runner 的停止工具，但不直接保存正文。

## 5. CAS、验证和落盘

整个 run 使用同一个冻结 baseline。最终应用前必须校验：

- draft digest 仍等于 `baseline_digest`；
- ledger epoch/fingerprint 未变化且仍允许 mutation；
-最新 polish checkpoint seq 未变化；
- plan、batch 和 finish 使用同一 `operation_id`；
- edits 不重叠并唯一命中 baseline；
- 总 edits ≤32；
- 修改覆盖率仍在现有安全阈值内；
- 受保护事实没有变化。

通过后按 baseline offset 逆序应用候选，只调用一次现有 `CommitPolishCandidate`，维持一次 SaveDraft、一次 draft checkpoint 和一次逻辑 polish checkpoint。

状态语义：

- `changed`：至少一个候选成功应用。
- `no_op`：完整审阅后认为无需修改。
- `partial`：部分成功，部分被拒绝或转 Writer。
- `degraded`：技术失败耗尽，正文不变。
- `stale`：CAS 失败，不保存、不写 checkpoint、不自动 rebase。
- `error`：协议或安全不变量失败，不保存、不写 checkpoint。

候选第一阶段只保存在内存。进程中断后重新运行完整审阅；暂不建设持久化 mini-runner。

## 6. Token、turn 和调用预算

完整输入和分批输出是两个独立问题。保留完整输入，通过工具批次控制输出。

| Agent | 输入预算 | thinking 目标 | 可见 completion 目标 | max output token |
|---|---:|---:|---:|---:|
| `architect_short` | 96k | 12k | 16k | 32,768 |
| `architect_long` | 192k | 24k | 32k | 65,536 |
| Writer | 192k | 24k | 32k | 65,536 |
| editor | 160k | 16k | 16k | 32,768 |
| `style_critic` | 48k | 8k | 4k | 16,384 |
| critic | 48k | 8k | 4k | 16,384 |
| Polisher full-context | 160k | 24k | 每轮 ≤24k | 65,536 |

Polisher 限制：

- 正常：plan 1 次、约 3 个 batch、finish 1 次，通常 5 次模型调用；
- 最大逻辑调用：plan 1 次、4 个 batch、finish 1 次，共 6 次；
- 加技术恢复后，单 run 实际 API 调用硬上限 8 次；
- plan 可见输出目标 ≤8k tokens；
- 单 batch 可见输出目标 ≤24k tokens，原始工具参数 ≤48 KiB；
- finish 可见输出目标 ≤4k tokens；
- `finish_polish` 使用 StopAfterTools 结束，不能复用 Writer StopGuard。

实际 max output 取 65,536、模型 registry 上限、provider 确认上限和剩余上下文空间中的最小值，并保留至少 8k 安全余量。

131,072 只作为经过验证的模型/provider 兼容开关。若 65,536 在特定 reasoning 模型上仍因隐藏 thinking 截断，可按精确模型开启 override；不能全局提升所有角色。

## 7. 技术错误与重试

- 每个逻辑调用最多 1 次恢复；
- 整个 run 最多 2 次技术恢复；
- 总 API 调用硬上限 8；
- HTTP transport 每个逻辑调用最多 2 attempts；
- 技术失败不增加 style content attempt。

| 错误 | 处理 |
|---|---|
| length | 只重试当前 plan/batch/finish，不重传已接受 batch |
| empty output | 当前逻辑调用重试 1 次 |
| EOF / missing DONE | 当前调用重试，必要时 failover，受 run 总预算约束 |
| malformed arguments | 本地容忍 BOM/单层 fence；格式修复 1 次 |
| audit-over-limit | 审计仅存 digest/计数；压缩当前 capsule 后重试 1 次 |
| 429/5xx | 遵循 Retry-After，受总预算约束 |
| auth/quota/filter | 不重试，按 degraded 收敛 |
| CAS stale | 丢弃所有候选，不重跑模型 |
| fact/scope rejection | 拒绝候选或转 Writer，不计技术重试 |

agentcore 需要显式 `MaxLengthRecoveries` 和 run-level actual-call budget；升级前不得只依赖 `MaxTurns` 与多层 `MaxRetries`。

## 8. Prompt 修改

### `writer.md`

- 明确 Polisher 会阅读全文并主动审查，不只是执行 findings。
- Writer 负责 factual/structural/global rewrite；Polisher 负责不改变事实的文风精修。
- `required_next_action` 保持唯一下一步来源。
- 保留 `polish → check → review → commit` 门禁。
- 技术错误只能根据剩余技术预算重试。
- 不要求 Writer 在调用 Polisher 前手工完成全部局部 style findings。

### `polisher.md`

必须保留并强化：

- 完整阅读全文；
- 完整事实大纲；
- 完整 findings；
- 主动发现新问题；
- 约 20 edits 的软目标；
- 最多 32 edits 的硬上限。

新增：先提交 plan，每批最多 8 edits，全部 edits 基于 baseline，最后必须调用 finish。事实、结构和因果问题转 Writer；原稿问题少时不得为了凑 20 个 edits 过度修改。

### `style-critic.md`

- 保持独立终审，不承担 Polisher 的完整审稿职责。
- 只判断润色后正文是否仍有阻断性问题。
- 输出严格 JSON，禁止 Markdown fence。
- 每轮最多返回 3 个最重要的 blocking findings。
- 明确区分内容问题和技术/协议失败。

## 9. Style review budget

每个 epoch 最多 3 次有效内容 revise。只有 style critic 对当前有效候选返回内容性 `revise` 才增加计数。

| 事件 | 内容计数 | 技术计数 | 可触发 exhausted |
|---|---:|---:|---:|
| 有效内容 revise | +1 | 0 | 是 |
| pass | 0 | 0 | 否 |
| length/empty/EOF/network/timeout | 0 | +1 | 否 |
| malformed JSON/audit 超限 | 0 | +1 | 否 |
| CAS stale | 0 | 单独记录 | 否 |
| Polisher 候选越界 | 0 | 0 | 否 |
| user override | 0 | 0 | 否 |

Writer 下一步：技术失败且仍有预算时重试 `polish_draft`；技术预算耗尽时 degraded 后进入 `check_consistency`；stale 返回 `check_consistency`；critic 局部 revise 再次 `polish_draft`；事实/结构问题进入 `edit_chapter`；内容次数耗尽进入 `style_override`。

## 10. 审计边界

完整章节、事实大纲和 findings 进入模型上下文，但不得重复写入 checkpoint 或普通 audit metadata。

审计只记录：版本、输入 digest/token、plan digest、issue/edit 数量、batch digest/字节数/接受拒绝计数、finish reason、重试分类、实际 API 调用数、result digest 和验证结果。

原始候选只存在于 run 内存 accumulator 和最终章节文件中。工具返回不得回显完整 edit 内容。

## 11. 开发包与所有权

### 包 1：Prompt 与 golden tests

分支：`feat/full-context-polisher-prompts`

负责 `assets/prompts/writer.md`、`polisher.md`、`style-critic.md` 及 prompt tests。禁止触碰 tools、store 和 agent config。

Commit：`refactor(prompts): define full-context polisher workflow`

### 包 2：候选工具协议

分支：`feat/polisher-candidate-tools`

新增 `internal/tools/polish_plan.go`、`polish_edit_batch.go`、`polish_finish.go`、内存 accumulator、schema 和纯单元测试。禁止触碰 `polish_draft.go`、store、prompts 和 `build.go`。

Commit：`feat(polish): add bounded candidate submission tools`

### 包 3：Style review 预算

分支：`feat/style-budget-ledger`

负责 `internal/domain/style_review.go`、checkpoint/ledger 兼容字段、`internal/store` 对应代码和测试。禁止触碰 prompts、Polisher 工具和 runner。

Commit：`feat(style): separate content and technical attempts`

### 包 4：角色级 token/retry policy

分支：`feat/agent-output-budgets`

负责新增 `internal/agents/budgets.go`，修改 `build.go` 中角色 token、Polisher MaxTurns/StopAfterTools 和专用工具注册，并更新配置及 runner 测试。禁止触碰 store、prompts 和 `polish_draft.go`。

Commit：`feat(agents): configure full-context polisher budgets`

### 包 5：agentcore recovery

独立上游分支：`feat/configurable-length-recovery`。实现显式 length recovery 和实际 API 调用总预算。不得编辑 `.slim/clonedeps`；主项目最后更新正式依赖。

### 包 6：运行时集成

分支：`feat/full-context-polisher-runtime`

负责 `internal/tools/polish_draft.go`、plan/batch/finalize 编排、最终 CAS/原子应用/checkpoint、review/chapter-stage/next-action 接线和故障测试。依赖包 1–4，由主线程最后集成。

包 1、2、3 可并行；包 4 在 schema 冻结后接入；包 6 最后集成。包 5 可独立进行，但不能与 provider/model 切换同批上线。

## 12. 灰度与回滚

建议 flags：

- `full_context_polisher_v3`
- `polisher_candidate_tools_v3`
- `agent_output_budget_v2`
- `style_content_budget_v2`
- `legacy_polisher_high_output`

上线顺序：先观测当前路径至少 200 章；再用新路径做不写正文的 shadow；随后 5% canary 至少 50 章、25% canary 至少 200 章；100% 后继续观察 500 章。

旧 one-shot 路径保留至少两个正式版本和 30 天，回滚只翻 flag。新模型/provider、新 prompt、token/thinking、style budget 和 agentcore recovery 不得多个项目同时进入一个灰度批次。

## 13. 验收指标

- 100% Polisher run 包含完整章节、完整事实大纲和完整当前章节 findings。
- 100% run 先 plan、后 batch、最后 finish。
- 正常 edits 中位数围绕 20，但不设置最低数；硬上限 32。
- 每 batch ≤8 edits，硬上限 4 批。
- 正常逻辑 API 调用 p50 ≤5，实际调用硬上限 8。
- Polisher length、empty、malformed JSON 各低于 0.5%。
- 技术 retry 率低于 5%。
- 技术失败增加 content attempt：0；exhausted 误报：0。
- CAS stale 导致错误保存/checkpoint：0。
- 任意 batch 数仍只有一次 SaveDraft 和一次逻辑 polish checkpoint。
- 事实、数字、时间线、人物状态和因果主体的未授权变化：0。
- checkpoint metadata ≤16 KiB，单章普通 audit metadata ≤64 KiB。
- 风格 A/B 不低于旧完整阅读路径，并在节奏、重复、声音一致性中至少两个维度改善。

必须测试工具调用顺序、32-edit 边界、重复/重叠 anchor、事实保护、技术重试总预算、EOF/length/空输出、CAS 并发、进程中断、旧 checkpoint 读取和 style exhausted 计数。

## 14. 第一批执行顺序

1. 冻结三个候选工具的 schema。
2. 增加当前 one-shot Polisher 的实际调用数、token、edit 数、截断和 CPU 基线观测。
3. 并行开发 prompt/golden tests、候选工具协议和 style budget 兼容层。
4. schema 稳定后接入 Polisher runner 的工具、turn 和 token 限制。
5. 最后由主线程完成 `polish_draft` 集成和 shadow A/B。

在 shadow 和 canary 验收前，不得删除“完整阅读全文”“约 20 edits”“最多 32 edits”“完整事实大纲”“完整 findings”，也不得删除 legacy one-shot 回滚路径。
