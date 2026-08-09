# 四层状态机制设计定稿 v2（feat/four-layer-state）

> 状态：设计定稿 v2（经 lib-1 酒馆调研 + ora-1 两轮审核 + exp-2 事实核查修订）
> 分支：feat/four-layer-state
> 目标：拆分 世界状态 / 角色状态 / 剧情状态 / 伏笔 四个权威投影，解决 foreshadow_ledger 混装导致的召回污染、回收责任不清、诊断失明
> v2 修订：快照路径修正（meta/snapshots/）、注入容器修正（selected_memory/顶层/互斥）、新增能力标注（preflight/world_rules 限制/紧凑视图）、retire 状态机、CharacterState 根 schema、数值常量、staged commit 恢复协议、迁移回填规则、代码影响面补全

---

## 1. 核心原则

**四个权威投影 + 三类审计摘要：**

| 权威投影 | 载体 | 职责 |
|---|---|---|
| 世界状态 | `world_rules.json` | 稳定机制/规则（规则如何运作） |
| 角色状态 | `meta/character_state.json`（新） | 当前值（身上装着什么/处于什么状态） |
| 剧情状态 | progress + 当前弧/章大纲 + chapter plan + compass.current | 当前剧情位置（不新建 plot_state 文件） |
| 伏笔承诺 | `foreshadow_ledger.json` | 未来必须兑现的承诺（仅长线） |

| 审计/摘要 | 载体 | 职责 |
|---|---|---|
| 剧情事件流 | `timeline.json` | 已发生事件（窗口注入） |
| 变更流水 | `meta/state_changes.json` | append 审计（由 character_state_updates 自动派生） |
| 弧快照 | `meta/snapshots/v%02da%02d.json`（**事实核查修正**：非 characters/snapshots/） | 弧末叙事摘要（非当前值权威） |

**写入哲学**：Writer 只报告本章事实变化，系统派生流水；Architect 维护世界规则与剧情规划。
**层间关系**：职责分离，但**一次事件可更新多个投影**（非互斥）——如右环解除 = timeline 事件 + 角色状态更新 + 伏笔 resolve 三写。

## 2. 长短线三档（伏笔台账定位）

| 档 | 判定标准 | 承载机制 | 进 ledger？ |
|---|---|---|---|
| 即时钩子 | 1-3 章内兑现 | `outline.hook` → 下一章 `ChapterContract.PayoffPoints/continuity_checks` | 否 |
| 弧内线程 | 当前弧内兑现 | `layered_outline` + `compass.current.open_threads` → plan_chapter 下沉章节契约 | 否 |
| 长线承诺 | 跨弧/跨卷，或兑现位置不确定 | `foreshadow_ledger` | **是（唯一入口）** |

- `plant` 必填 `horizon: cross_arc | book`，强制声明为何进长期台账；**后续动作不得修改 horizon**
- 100 章阈值（`foreshadowAgingChapters=100`，约 4 卷；全书 1000+ 章量级）只对长线承诺生效，含义 = "距最近**有证据的推进**超 100 章需复核"（stale，非 overdue）
- 既有 11 条铺垫类（wax-drop/chain-beat/window-compress 等）在迁移时逐条 resolve/retire，不靠新代码自然淘汰

**Writer 判定决策树**（prompt 写入）：
1. 本章后是否仍有未回答的承诺？否 → 只写 timeline/state
2. 有明确目标章且 1-3 章内？→ 即时钩子，不进 ledger
3. 确定当前弧内兑现？→ 弧内线程，交给 outline/compass/contract
4. 可能跨弧或位置不定？→ 进 foreshadow ledger（必填 horizon）

## 3. 数据层

### 3.1 meta/character_state.json（新）：角色状态当前值权威

根结构为**数组**（与现有 world_rules/ledger 风格一致）：

```json
[
  { "entity": "主角", "field": "body_device.右乳禁乳环", "value": "已解除，乳肉红肿淤紫",
    "updated_chapter": 234, "evidence": "…正文精确引文…" }
]
```

约束（新增能力，实施固化）：
- `(entity, field)` 唯一键；同键新提交**覆盖**旧值（upsert 语义）
- entity/field 非空；field 受控命名空间，初始清单：
  `body_device.*`（身体装置）/ `health.*` / `location.*` / `capability.*` / `resource.*` / `inventory.*` / `status.*` / `knowledge.<fact-id>`（信息差：value ∈ unknown|suspects|knows|believes:<内容>）
- 近义字段不得新增 key（迁移时归并，如"身体状态/双乳状态/右乳状态"→ `body_device.*`）
- 单 value ≤800 字、单 evidence ≤300 字、单实体字段数 ≤100（可测试常量）
- 允许删除：显式 `value=""` + reason 或 delete action（二选一，定稿：`value="已移除"` 语义 + reason，不新增 delete action）
- 权威顺序：`character_state 当前值 > 最新 snapshot > characters 基础设定`

### 3.2 foreshadow_ledger.json（迁移后只留长线承诺）

```json
{ "id": "...", "description": "...", "horizon": "cross_arc",
  "status": "planted|advanced|resolved|retired",
  "planted_at": 11, "last_touched_at": 198, "last_evidence": "…", "resolution_evidence": "…",
  "resolved_at": 0, "closed_at": 0, "close_reason": "" }
```

- `retired` 为正式终态（closed_at + close_reason），**中央过滤**：`LoadActiveForeshadow` 排除 resolved **和 retired**（单一过滤点，所有消费方自动生效：story_threads 召回 / active_foreshadow_count / commit_chapter 完结判定 / diag open 统计）
- 状态时间不变量：resolved 必有 resolved_at>0；retired 必有 closed_at>0 + close_reason；非 resolved 不得有 resolved_at（写入时自动清理）；非 retired 不得有 closed_at
- **修正**（ora-1 终审措辞错误）："resolved **设置** resolved_at"（非"清理"）

## 4. 写入机制

### 4.1 ForeshadowUpdate 动作与状态转换表（新增能力）

`ForeshadowUpdate` 扩展：`{id, action, description?, horizon?, evidence?, reason?}`

| 当前状态 | plant | advance | resolve | retire |
|---|---|---|---|---|
| 不存在 | 允许（description+horizon 必填） | 拒绝 | 拒绝 | 拒绝 |
| planted | 幂等或拒绝 | 允许（evidence 必填） | 允许（evidence 必填） | 允许（reason 必填） |
| advanced | 拒绝 | 允许（evidence 必填） | 允许（evidence 必填） | 允许（reason 必填） |
| resolved | 拒绝 | 拒绝 | 幂等或拒绝 | 拒绝 |
| retired | 拒绝 | 拒绝 | 拒绝 | 幂等或拒绝 |

- action 扩展为 `plant|advance|resolve|retire`；不做 reopen
- horizon 仅 plant 可设，后续动作修改 horizon → 拒绝
- advance/resolve 的 evidence = **正文精确短引文**（长度 ≤300 字，`MaxForeshadowEvidenceRunes`），preflight 用 Contains 校验其存在于本章草稿；advance 无证据不得更新 last_touched_at（防空泛"推进"刷新账龄）
- retire：reason 必填，不要求正文引文（取消承诺非剧情事实）
- plant 对已存在 ID 的"只填空字段"行为（world.go:132-141 现状）保留语义：不复位状态

### 4.2 通道矩阵

| 角色 | 通道 | 落点 | 备注 |
|---|---|---|---|
| writer（commit） | `timeline_events`（已有） | timeline.json | |
| | `foreshadow_updates`（已有 + evidence/horizon/reason） | foreshadow_ledger.json | |
| | `relationship_changes`（已有） | relationship_state.json | |
| | `character_state_updates`（**新增，唯一新增正式通道**） | character_state.json（upsert）+ **自动派生** append state_changes.json | 派生复用现有 `stateChangeKey` 幂等去重（world.go:572-574），key 按 `chapter\|entity\|field\|old\|new` 对齐 |
| | `state_changes`（旧通道降为兼容入口） | state_changes.json | 与 character_state_updates **同 (entity,field) 双写 → 拒绝**（冲突校验） |
| | `feedback`（复用现有通道，**不新增 world_rule_proposal schema**） | OutlineFeedback 池（commit_chapter.go:116,406-413 → architect foundation writer_feedback） | 规则变更建议按固定格式写入 feedback.suggestion（如 `[world_rule] 奶税转型：…`），architect 消费后 save_foundation 落库 |
| architect | `save_foundation(type=world_rules)`（权威修订，全量保存） | world_rules.json | **保存前校验**（新增）：条数 >30 warning；总字节 >24576 拒绝 |
| | `save_arc_summary`（弧末快照） | meta/snapshots/ | 工具加载 current state 生成结构化基底（按 entity 分组摘要），与 LLM 快照合并；明显冲突（永久装置缺失）→ warning |
| editor/reviewer | `save_review`（只读+意见） | reviews/ | |

- **Writer 不直接改 world_rules**（WorldRule 无稳定 ID，无法增量更新；save_foundation 是全量保存）
- rewrite/preserve 路径（world_state_mode=preserve）**不应用** character_state_updates（与 timeline/foreshadow 等现有跳过逻辑一致，commit_chapter.go:511 区域）

## 5. 读取机制（注入矩阵）

| 层 | writer/editor | architect | 预算/裁剪 |
|---|---|---|---|
| world_rules | **顶层** `result["world_rules"]`（buildBaseContext，非 foundation——事实核查修正） | foundation（buildArchitectFoundation） | **新增限制**（非现状）：条数软限 30（超限 warning 要求合并/移除）+ 总字节硬限 24576 bytes（save_foundation 保存时拒绝，禁静默截断）；不在 trimOrder、计入预算不可裁 → 必须靠自身量控 |
| character_state | 常驻摘要（主角，投影 ≤12KiB）+ **按大纲/plan 预计出场召回**（复用 `loadFilteredCharacters` 骨架，novel_context_builders.go:380-411；实际出场名单只能用于下一章更新） | 规划时读取（buildArchitectFoundation 扩展） | 主角核心状态高保护；次要角色状态独立 key，**加入 trimOrder**（在 relationship_state 之前裁剪，维持其余相对顺序） |
| character_snapshots | 弧首注入（**需实施门控**；现状 Layered Writer 每章加载最新快照） | foundation（LoadLatestSnapshots，novel_context_builders.go:1480） | 无快照时回退 characters 基础设定（:471-482） |
| timeline | working 近 N 章窗口（TimelineWindow：≤15 章→10；≤50→8；>50→5） | — | 可裁（低保护） |
| foreshadow | **`selected_memory.story_threads` 精选**（事实核查修正：非 episodic）：配额 = **最多 10 条相关 + 至少 1 条最旧 stale 槽（总量 12）**；与 episodic 全量 ledger **互斥**（有精选即不下全量，builders.go:533-536） | foundation 全量活跃（**"紧凑"为新增目标**：全量注入现状 + 后续做紧凑视图） | 触发门槛保留：活跃 <12 不召回、选中 <2 整组丢弃（novel_context.go:1020-1021） |
| relationship_state | episodic | — | 可裁列表最高保护（trimOrder 末位） |

**stale 槽边界行为**（定稿）：
```
存在 stale：最多 10 条相关 + ≥1 条最旧 stale；空位继续由 stale 补足至 12
无 stale：相关线程补满至 12
无相关但有 stale：最旧 stale 补满至 12
两者皆无：不输出 story_threads
```
相关与 stale 同 ID 去重（现有 picked 机制）。

**stale/诊断基准统一**（三处同步）：
- `agingForeshadow`（novel_context.go:868-883）：`chapter - effectiveTouchedAt`，effectiveTouchedAt = `last_touched_at>0 ? last_touched_at : planted_at`（旧数据回退）
- `StaleForeshadow`（diag/rules_planning.go:20-22）：按全部活跃条目 + effectiveTouchedAt（不再只查 planted）；阈值**改用固定 100 章**（事实核查：现行动态阈值 max(completed/3, 8) 是 planted-only 时代的公式，迁移后统一）
- `diag.go:121-124` 的 open 统计：排除 retired + 按 effectiveTouchedAt

## 6. 联动机制

1. **writer 提交闭环**：现有前置闸门（FSM/literary/style/polish/world_state_mode，commit_chapter.go:146-236）之后、任何写入之前，**新增 preflight**（事实核查：preflight 全代码库不存在，是新增非前移）：
   - ID 存在性（plant 除外）/ action 合法 / 状态转换合法（§4.1 表）/ 同 ID 冲突动作拒绝 / evidence 存在于本章草稿（Contains）/ character_state 的 (entity,field) 双写冲突拒绝 / world_rule 提案格式校验
   - 任一失败 → 整体拒绝、**所有文件零变化**（验收基准）
   - **staged commit 恢复协议**（新增，不宣称原子）：~~PendingCommit 扩展保存 mutation payload（含 character_state_updates）~~（**已决策简化，见 §已接受偏差 B3**）；重试时按阶段识别已写入项；character_state upsert 与派生 state_changes 幂等（~~failpoint 测试覆盖每个写入边界~~ **B4：延后**）
2. **architect 规划闭环**：读 foundation 四层 → 兑现点写 layered_outline → 修订 world_rules/compass（save_foundation）→ writer 下章带上新规则；弧内线程在 plan_chapter 阶段下沉到章节契约（writer 不读完整 compass）；消费 writer_feedback 中的 `[world_rule]` 提案
3. **弧边界闭环**：save_arc_summary 工具加载 current state 生成结构化基底 → 与 LLM 快照合并 → 冲突（永久装置缺失等）warning；快照是派生摘要，不覆盖 current state
4. **reviewer 闭环**：check_consistency 扩展注入（character state 按正文角色筛选 + 当前章节 plan/contract + timeline 窗口 + 活跃伏笔紧凑视图 + world_rules + relationships；**不破坏 consistency_check seq 语义**，check_consistency.go:129-135 的 checkpoint 追加保持）。区分：代码可确定校验 vs LLM 语义判断（正文违反规则/疑似回收/认知冲突——P3）。**实施现状**：代码可确定校验仅实现 character_state 重复 (entity,field) key 检测；非法转换/evidence 校验由 commit preflight 强制保证（见 §6.1）；未知实体/状态时间晚于快照等其余确定性检查**未实现，登记为后续任务**
5. 伏笔 resolve/retire → active_foreshadow_count（builders.go:1453-1455）与完结判定（commit_chapter.go:796）经中央过滤自动排除 retired
6. 误判处理：不静默改层；结构性错误硬拒绝；高置信语义误判返回建议通道；低置信 warning 交 reviewer；正文不因分类错误丢失，修正声明后重试 commit

## 7. 实施范围

### 7.1 数据迁移（一次性幂等工具）
- 迁移决策清单 D1-D7 待作者拍板
- 48 条 → resolve（约 12 条，含 hk-lactation-ring-01）/ retire（铺垫类 11 条 + 转层项）/ 迁 world_rules（5-8 条稳定机制）/ 迁 character_state（装置安装状态）/ 保留 ledger（真长线）
- **迁移回填规则**（新增）：
  - 保留的 active 条目：补 horizon（跨弧候选默认 `cross_arc`，作者确认或 retire）；last_touched_at 回填 = 最近一次有证据的推进章，找不到保持 0（运行时 fallback planted_at）
  - 无 last_evidence 的旧条目允许存在（仅新写入要求 evidence）
  - 迁移后 Store 拒绝无 horizon 的新 plant；兼容读取旧条目
  - 转层项保留为 **retired**（close_reason=`moved-to-<layer>`），保存迁移审计，不物理删除
  - character_state 初始值来源优先级：最新明确正文/state change > 最新 snapshot（meta/snapshots/）> 弧摘要/评审 > 原 ledger 描述（仅候选）；冲突项进 manifest 人工确认
  - 近义 field 归并（"身体状态/双乳状态/右乳状态"→ body_device.*）
- 工具：备份 + manifest + dry-run 分类 → 人工确认 → 执行（调新 Store 方法）→ 重建 md 镜像（renderForeshadow 扩展展示 retired/horizon/close 信息）
- 现状缺陷一并覆盖：1 条空 description 伏笔（deep-probe-02）修复或 retire；device-reconfigure-75 advanced+resolved_at 矛盾修复

### 7.2 代码改动（按实施顺序）
1. `internal/domain/review.go`：ForeshadowEntry + horizon/last_touched_at/last_evidence/resolution_evidence/closed_at/close_reason；ForeshadowUpdate + evidence/horizon/reason；新 CharacterStateEntry
2. `internal/store/world.go`：UpdateForeshadow 状态机（§4.1 表：未知 action/ID 拒绝、evidence 必填、retire、时间不变量、resolved 设置 resolved_at/非 resolved 清理）；**LoadActiveForeshadow 中央过滤 retired**；新增 CharacterStateStore（upsert + 派生 state_changes，复用 stateChangeKey 幂等）
3. `internal/tools/commit_chapter.go`：+character_state_updates 通道（schema）；**新增 preflight**（置于现有闸门之后、所有写入之前）；双写冲突校验 + 状态转换合法性（收敛到 `domain.ForeshadowTransitionAllowed` 纯函数，与 store 共用）；~~PendingCommit 扩展 mutation payload + 恢复阶段~~（**已决策简化，见 §已接受偏差 B3**）；preserve 路径跳过 character_state
4. `internal/host/imp/analyzer.go`：解析/校验新字段（evidence/horizon/reason/character_state）
5. `internal/tools/novel_context.go`：agingForeshadow 按 effectiveTouchedAt；selectStoryThreads stale 槽配额（§5 四种边界）；trimOrder + 次要角色状态 key（relationship 前裁）
6. `internal/diag/rules_planning.go` + `internal/diag/diag.go`：stale 按全部活跃 + effectiveTouchedAt + 固定 100 章；open 统计排除 retired
7. `internal/tools/novel_context_builders.go`：character_state 注入（writer 顶层/architect foundation 扩展）；弧首快照门控；world_rules 量控 warning；（ctxpack/builder.go 独立加载路径同步）
8. `internal/tools/check_consistency.go`：四层交叉校验注入扩展（不破坏 checkpoint seq）
9. `internal/tools/save_arc_summary.go`：current state 结构化基底 + 冲突 warning
10. `internal/tools/save_foundation.go`：world_rules 条数软限 + 字节硬限（保存前校验）
11. `internal/store/characters.go` / `renderForeshadow`：snapshot 兼容、retired 展示
12. prompt：writer.md 分流决策树 + evidence/horizon 要求；architect-long.md 台账职责（长线 only + horizon）+ world_rules 维护 + 消费 [world_rule] 提案；editor 一致性

### 7.3 不做（本期）
reopen、history[] 内嵌、自动 resolve、resolution_candidate（P3 后置）、target_window 状态机、foreshadow_events.jsonl 事件日志（后置）、writer 直改 world_rules、story_clock（作品不依赖精确日历时）、world_rule_proposal 新 schema（复用 feedback）

## 8. 验收标准与测试

- 所有 active ledger 项是真正长线承诺（含 horizon）；无 advanced+resolved_at 矛盾；无空 description；hk-lactation-ring-01 不再活跃；retired 排除于召回/计数/完结判定
- character_state 唯一键无近义膨胀；commit 派生 state_changes 无重复（幂等）；preserve 路径不写 character_state
- preflight 失败时 pending/终稿/摘要/ledger/character_state 全部零变化（~~failpoint 覆盖每个写入阶段失败与重放~~ **B4：写入边界 failpoint 测试延后**；恢复收敛性由 store 幂等 + 调整后写入顺序保证，见 §已接受偏差 B3）
- 旧数据无 last_touched_at/horizon 时诊断/召回正确回退
- 测试清单：Foreshadow 全状态转换表测试；retire/horizon/evidence 条件测试；world_rules 30 条 warning + 24576 bytes 拒绝测试；character_state 唯一键/命名空间/幂等派生测试；stale 配额四边界测试；注入矩阵（writer/editor/architect）测试；rewrite preserve 不写 character_state 测试；迁移工具 dry-run/重复执行幂等测试；**迁移不得覆盖原有 world rule**（孕月数字）测试；新旧 horizon/last_touched 兼容测试；迁移前后 context diff 验证；go test 全绿；重建 exe

## 9. 待作者决策（迁移决策清单 D1-D7）

- D1 semen-stock-01：继续承诺（重写描述+horizon）或 retire
- D2 hk-dark-web-broadcast-01：resolve 或另立 successor（观看者身份）
- D3 呼吸配额/氨水惩罚/灌肠循环：已随阶段结束（resolve）或仍全局机制（迁 world_rules）
- D4 奶税：迁 world_rules（boundary=仅密室内）或留剧情状态
- D5 climax-degradation-01 / drug-confusion-pathway-01：保留 ledger 或迁角色状态/world_rules
- D6 deep-probe-01/02：核对 ch185-186 正文；deep-probe-02 空描述处理
- D7 铺垫类 11 条 + arc3 状态类：批量原则"目标章已发生→resolve；长期状态→迁角色状态；未发生→保留重写"

## 10. 已接受偏差（终审后登记）

- **B3：PendingCommit 不做 mutation payload**。§6.1/§7.2 原计划的"PendingCommit 保存
  mutation payload（含 character_state_updates）"已简化取消：恢复完全依赖
  store 层幂等（`UpsertCharacterState` 同值跳过派生、`stateChangeKey` 去重）+ 调整后的
  写入顺序（character_state.go：**先写派生 state_changes，再写 character_state.json**）。
  收敛性：流水写失败 → 状态文件未动 → 重试全量重做；状态写失败 → 流水已写（重试
  key 去重不重复）+ 状态重写。任何单点失败后重试可收敛。
- **B4：failpoint 写入边界测试延后**。§6.1/§8 原计划的"failpoint 覆盖每个写入阶段
  失败与重放"未实施（写入边界注入依赖测试基建，本期不做）；写入失败恢复路径由
  B3 的幂等 + 顺序保证，单点失败收敛性已通过现有幂等/重放测试覆盖。
- **CharacterStateApplied 字段已移除**。原 PendingCommit.CharacterStateApplied（写入
  但全代码从未读取，暗示不存在的恢复能力）已从 domain.PendingCommit 与
  commit_chapter 写入点删除——恢复能力由 B3 的 store 幂等路径承担，无需标记字段。
