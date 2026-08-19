# 文风参考上线方案：五臂 A/B、硬门禁与盲评

> 状态：执行方案，**不表示测试已运行，也不包含测试结果**。本轮基线提交为 `f8af95f5`；文档本身不修改 Go、Prompt 或运行时事实。

本方案把外部参考带来的文风改动拆成可比较的五臂实验：`baseline`、`scene-craft`、`reference-DNA`、`diagnostics/polisher`、`combination`。目标不是寻找“看起来最好”的一稿，而是判断在固定模型、规则、题材和状态条件下，改动是否带来可重复的收益，同时没有破坏提交、连续性、世界状态、恢复或成本边界。

## 1. 决策原则

每一臂都从同一份干净项目事实快照开始，输出目录、repeat 和随机种子隔离。先过硬门禁，再做盲评；硬门禁失败时，盲评只能用于定位，不能把主观好感抵扣为通过。

结果只支持被测的模型、规则、题材、章节功能和样本范围。通过代表“本轮证据支持继续灰度/合并评审”，不代表全题材、全篇幅或所有模型的绝对质量保证。

## 2. 五臂与运行矩阵

| 臂 | 变更 | 归因边界 |
|---|---|---|
| `baseline` | 当前正式 prompt、规则和项目事实 | 提供同 seed、同 case 的比较基线，不是质量上限 |
| `scene-craft` | 场景构造的抽象技法：目标、行动、冲突、转折、身体/感官反应、空间落点和余波 | 只测试场景组织技法；不带入外部人物、事件、专名或原句 |
| `reference-DNA` | 从参考语料提炼的抽象画像：视角距离、句群密度、节奏、细节分配、对白潜台词和 hook 形态 | 只测试可迁移的结构/技法；不复制表达、人物、事件、世界设定或专有名词 |
| `diagnostics/polisher` | 诊断产出最小 edit work order，polisher 只按工单定点修改，之后重新做事实复核 | 这是一个复合臂；不能把收益分别归给 diagnostics 或 polisher |
| `combination` | 同时启用前三个策略 | 只说明组合是否可行；不能反推每个单臂都单独有效 |

单臂变更必须有 `arm_diff`：列出新增参考包、摘要版本、允许的工具/流程开关和 hash。`combination` 只能合并已经登记过的单臂输入，不得偷偷加入第五种策略。

## 3. 固定条件与重复生成

运行前冻结并记录：

- provider、模型 ID/版本、角色模型映射、API 版本和 fallback 策略；
- `temperature`、`top_p`、最大输出、reasoning 档位、超时和重试；
- system/runtime 规则、`meta/user_rules.json`、机械 style rules、字数契约、工具 schema；
- 起始 foundation、已完成章节、角色/世界状态、伏笔台账、章节契约和依赖/二进制基线；
- case 的题材、premise、章节功能序列，以及本轮基线提交 `f8af95f5`；
- 第 `rN` 次重复的 seed。所有臂对同一 `case × repeat` 使用相同 seed；provider 不支持 seed 时记录 `unsupported`，仍保留独立重复，不把结果称为确定性复现。

最小可执行样本为：

- 3 个题材或题材簇；单一目标题材时，使用 3 个不同 premise，并在报告中限制结论范围；
- 每个 premise 生成连续 5 章，依次覆盖开篇/建立、日常或关系推进、冲突升级、揭示/高潮转折、收束后的 hook/aftermath；
- 每一臂每个 premise 独立重复 3 次，即每臂至少 9 条连续轨迹、45 个章节输出；
- 每条轨迹在同一状态窗口做一次恢复演练，例如在第 3 章草稿完成、提交前中断，然后 Resume；五臂的中断位置必须匹配；
- 盲评至少覆盖每臂全部 9 条轨迹；若预算只允许分层抽样，至少每题材 2 条、共 6 条，且只能标记为“方向性证据”，不得作为上线通过依据。

每个输出目录必须全新创建，不能用上一次生成的章节或状态续写。推荐目录形状：

```text
workspace/evals/style-reference/<experiment-id>/
  manifest.json
  report.md
  report.json
  artifacts/<case-id>/<arm>/r<repeat>/
  blind-review.jsonl
  adjudication.jsonl
```

## 4. 硬门禁

以下五项是上线前的硬门禁。任一项在任一必测轨迹失败，该臂不能进入上线候选；baseline 失败则本轮实验整体无效。`warning` 可以记录，但不能把 `unknown` 当作 `pass`。

| 门禁 | 通过条件 | 证据 |
|---|---|---|
| `commit` | 每条目标轨迹的目标章节都有且仅有稳定的 `commit` checkpoint；`completed_chapters` 与提交一致；`pending_commit` 清零；恢复重试不产生第二份事实提交或覆盖已提交章节 | checkpoint、progress、章节文件 digest、`diag`/case contract |
| `continuity` | 没有阻塞级连续性冲突；章节契约、时间顺序、角色已知信息和前文承接均可解释；新增 warning 必须逐条记录，不得用盲评分数抵销 | `diag` Findings、`check_consistency`/commit 事实、章节前后状态快照、人工证据定位 |
| `world state` | `world_rules`、`character_state`、伏笔/大纲等持久状态的变化都能由当前章节契约和项目事实解释；没有参考包偷偷写入新设定、角色事实或事件事实 | 运行前后快照 diff、commit payload、当前项目事实源、`diag` |
| `recovery` | 在预定窗口 kill 后 Resume 能继续同一 saga；无重复 step/commit、无重写已稳定落盘的前章、无遗留 pending；恢复后的状态与中断前可解释一致 | checkpoints、progress、pending 状态、章节 digest、resume trace |
| `cost` | 每条轨迹都有完整 usage/cost；不超过预先批准的单轨迹预算；相对配对 baseline 的成本比率 `p90 ≤ 1.30`，且任何单次不超过 `1.50`；缺失计费数据即暂停裁定 | `meta/usage.json`、provider 账单/估算、配对 ratio、预算记录 |

成本阈值应在首个 run 前写入 manifest，运行中不得为了让某臂通过而改阈值。若项目另有预算，必须在运行前替换并冻结；阈值是资源决策，不是“更便宜就一定更好”。

## 5. 盲评维度与评分

盲评者只看到随机化的 `A/B/C/D/E` 标签、同一项目事实摘要和连续五章输出，不看到 arm 名称、参考项目名称、模型或成本。每个维度 1–5 分：`1` 为明显失败，`3` 为可用但混合，`5` 为稳定、具体且无明显副作用。评分须写短证据定位（章号/段落锚点），不粘贴长篇原文。

| 维度 | 评什么 | 方向 |
|---|---|---|
| `voice identity` | 叙述声音是否有辨识度、前后一致，是否脱离泛化的“正确作文腔” | 高分更好 |
| `character voice` | 人物的说话、思考、行动和反应是否彼此可区分，并与已知性格/状态相容 | 高分更好 |
| `embodied scene` | 身体反应、空间关系、感官和动作后果是否让场景可感，而非只报心理结论 | 高分更好 |
| `subtext` | 言外之意、关系压力和未说出口的动机是否存在且不过度解释 | 高分更好 |
| `rhythm` | 句群、段落、对白和停顿的速度变化是否服务于本章功能 | 高分更好 |
| `hook/aftermath` | 转折后的余波、下一步欲望/问题和章末牵引是否成立，不靠虚假悬停 | 高分更好 |
| `cross-chapter repetition` | 五章之间是否重复相同句式、节拍、开场/收尾姿势或情绪解决方式 | **低重复为高分** |
| `similarity risk` | 是否出现对外部参考的人物、事件、专名、表达或可识别复刻痕迹 | **低相似风险为高分** |

两名评审独立打分；任一维度相差超过 1 分，交给第三名裁定并保留原分。盲评结束前不得揭盲。`similarity risk` 先按盲评者的可见文本打分，揭盲后再由独立审计者用已登记的参考指纹做复核；后者不能回写盲评分数。

### 5.1 通过阈值

“通过”是本轮候选条件，不是总体文学质量承诺。某臂只有同时满足以下条件，才可提交主线决定：

1. 五项硬门禁在全部最小样本上通过，且没有因数据缺失而跳过的 run。
2. 相对 baseline 的配对中位数：`voice identity`、`character voice`、`embodied scene`、`subtext`、`rhythm`、`hook/aftermath` 六项中至少两项提升 `≥0.5` 分；这六项没有任一项下降超过 `0.5` 分。
3. `cross-chapter repetition` 不下降；`similarity risk` 不下降。任一轨迹出现明确的复制人物/事件/表达证据，直接不通过并停止该臂。
4. 两位评审在至少 80% 的维度上相差不超过 1 分；超出部分完成第三评审后，报告同时保留分歧与裁定理由。
5. `combination` 除满足上述条件外，不得比已通过的最佳单臂增加硬门禁失败，也不得以超过成本阈值换取盲评优势。

若多个臂都满足条件，优先选择收益相当而变更面、成本和相似风险更小的臂；若没有臂满足条件，结论为 `revise` 或 `reject`，不得凭单个好样本合并。

## 6. 四个外部项目的参考边界

以下四类参考只提供抽象结构/技法。它们不是本项目的事实源，也不具有覆盖用户要求、当前大纲、已提交章节、角色状态或世界规则的权限。

| 外部贡献包 | 可以抽象 | 明确禁止 |
|---|---|---|
| `scene-craft` | 场景目标、动作链、冲突升级、转折、身体/感官落点、空间调度和余波的组织方法 | 复制参考项目的人物关系、事件顺序、场景布置、专名或原句 |
| `reference-DNA` | 视角距离、信息释放顺序、句群/段落密度、节奏变化、细节分配、对白潜台词等可描述特征 | 把原文句式逐句仿写，复制人物、事件、世界设定、专名、标志性比喻或表达指纹 |
| `diagnostics` | 症状分类、证据定位、跨章模式发现和“问题 → 最小修正”的诊断流程 | 让诊断器替项目决定事实，借参考包补写缺失设定，或输出整章重写稿 |
| `polisher` | 按明确工单做局部、可回滚、可验证的文风/节奏编辑 | 自行增删人物和事件、改世界规则/角色状态、重排事实时间线、整章洗稿或绕过复核/commit |

当前项目事实的唯一权威是已落盘的项目工件和运行时契约：foundation、已提交章节、chapter contract、`meta` 中的角色/世界/伏笔/进度/checkpoint、user rules 及工具的确定性校验。外部参考与这些事实冲突时，保留项目事实并丢弃参考要求；参考不能制造“已经发生”的事件，也不能把参考文本当作 canon。

### 6.1 最小 edit work order

`diagnostics` 的输出必须是可定位、可限制、可复核的最小工单，而不是“请重写本章”：

```json
{
  "chapter": 3,
  "anchor": "paragraph-07 / scene-02",
  "symptom": "冲突段连续三次直接解释情绪",
  "evidence": "以事实定位描述问题，不粘贴长原文",
  "edit_goal": "用一个可观察动作承载其中一次情绪转折",
  "allowed_operations": ["tighten", "replace_local_sentence", "add_one_embodied_detail"],
  "forbidden_changes": ["plot_fact", "character_state", "world_rule", "timeline", "new_named_entity"],
  "priority": "high",
  "max_spans": 1
}
```

工单至少包含章节、位置、症状、证据、编辑目标和禁改项；单章默认最多 3 个独立 span，单个问题默认最多 1 个 span。polisher 只能返回修改后的局部结果和 `changed`/`no-op`，随后必须重新执行连续性、世界状态和提交门禁。若需要改变事实或超过工单范围，停止 polisher，回到项目正常的编辑/规划流程，不由参考臂越权决定。

## 7. 停止条件

停止是为了保留证据和控制损失，不是把未跑完的实验包装成通过：

- baseline 任一硬门禁失败、配置/模型/规则发生漂移、初始事实快照不一致或 usage 缺失：停止整套实验，修复后从新 experiment-id 重跑；
- 任一候选臂出现 commit、continuity、world state 或 recovery 的数据完整性错误：立即停止该臂并保留 artifacts；同一硬失败连续出现两次时，不再消耗剩余 repeat；
- 任一轨迹超过单轨迹预算 `1.50×`，或该臂 `p90` 超过冻结阈值：停止该臂，不能通过删掉昂贵样本重算；
- 发现外部人物、事件、专名或表达的可识别复制：立即停止该臂，标为 `similarity-risk`，不得以“文风提升”抵扣；
- 达到最小样本前不提前宣布上线；达到最小样本后若没有满足全部阈值，停止扩样并标记 `revise/reject/inconclusive`，除非主线明确批准新的样本计划；
- 揭盲前任何人要求按 arm 名称挑样、替换失败样本或改阈值：停止并重新生成 manifest，原实验只作无效记录。

## 8. 记录格式与裁定

每个实验至少保存一份不可变 `manifest.json`、每次 run 的原始工件、确定性门禁结果、盲评 JSONL 和最终裁定。最小 manifest 形状如下，字段值必须填实际值，不能用“默认”代替：

```json
{
  "experiment_id": "style-reference-20260819-01",
  "baseline_commit": "f8af95f5",
  "case_id": "genre-premise-01",
  "genre": "<实际题材>",
  "chapter_functions": ["opening", "relationship", "escalation", "turn", "aftermath_hook"],
  "arm": "reference-DNA",
  "repeat": 1,
  "seed": "<matched-seed-or-unsupported>",
  "provider": "<provider>",
  "model": "<model-and-version>",
  "temperature": 0.7,
  "rules_sha256": "<sha256>",
  "fact_snapshot_sha256": "<sha256>",
  "reference_sources": [{"id": "<source-id>", "repo": "<repo>", "ref": "<commit-or-version>", "license": "<license>"}],
  "arm_diff": [{"path": "<derived-profile-or-work-order>", "sha256": "<sha256>"}],
  "hard_gates": {"commit": "pass", "continuity": "pass", "world_state": "pass", "recovery": "pass", "cost": "pass"},
  "cost_usd": 0.0,
  "blind_review_id": "<random-label>",
  "decision": "pending",
  "stop_reason": null
}
```

盲评 JSONL 每行对应一个 `blind_id × reviewer × dimension`，至少包含 `case_id`、`genre`、`repeat`、随机 `blind_id`、维度、1–5 分、证据定位、评审时间和是否需要 adjudication。汇总报告同时列出 baseline 配对差值、每臂的 hard-gate 通过率、成本 min/median/p90/max、样本缺失、评审分歧、揭盲后的来源审计和最终 `promote-candidate / revise / reject / inconclusive`。

执行顺序固定为：冻结 manifest → 生成五臂匹配样本 → 先跑硬门禁 → 随机化并盲评 → 完成分歧裁定 → 揭盲做相似风险审计 → 汇总阈值 → 由主线决定是否灰度/合并。报告中的指标是本次决策的证据和风险信号，不是对未来所有作品的绝对质量保证。

## 9. 与评测体系的关系

本方案补充 [评测体系](evaluation-system.md) 的五臂矩阵；确定性事实仍由项目已有的 `diag`、checkpoint、progress、usage 和 style statistics 提供，盲评只负责内容质量与风险的排序/裁定。普通 `--variant` 运行可以作为单臂执行入口，但五臂汇总、揭盲、最小样本和本方案门禁必须按本文件记录，不能把一次单臂成功输出冒充完整验证。
