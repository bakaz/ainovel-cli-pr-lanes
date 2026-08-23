# Polisher 候选工具协议 Schema（冻结版 v1.1）

> 状态：**已冻结（v1.1，含 oracle 审查修订）**。实现目标：包 2（`feat/polisher-candidate-tools`）。
>
> 依据：[handoff-polisher-style-token-plan.md](./handoff-polisher-style-token-plan.md) §4；
> 背景文档 §6 冻结决策 8–12。本文件是三个候选工具的**唯一权威契约**，
> 包 2/4/6 与 prompt（包 1）均以本文件为准。任何修改必须先解冻并更新本文件。

## 1. 冻结范围

- 三个工具：`submit_polish_plan`、`submit_edit_batch`、`finish_polish`。
- 运行生命周期与内存 accumulator 语义。
- 确定性预检规则、错误码、返回格式。
- 审计映射（accumulator → `PolishCheckpointMeta`）。

不在本文件范围（由其他包决定）：prompt 文案（包 1）、runner 注册与 token/turn 预算
（包 4）、`polish_draft.go` 编排与最终 CAS 落盘（包 6）、agentcore recovery（包 5）。

已裁定解冻项（相对 handoff plan §4 原文的偏离，实现以本文件为准）：

- batch 与 finish 新增 `baseline_digest` 回显字段（防御加深：plan §5 CAS 已校
  baseline_digest，模型回显可更早失败）。
- `new_string` 上限 2000 runes（比现有 edit_list 路径更严；现有
  `ApplyPolishEditPlanDetailed` 不校验 new_string 长度，包 6 集成时注意一致性）。
- plan 的 `anchor` 不做存在性校验（意图声明，由 batch old_string 校验兜底）。

## 2. 工具注册

三个工具注册在 polisher runner 的工具集（当前 `polisherTools` 为空，包 4 接入）。
工具名与描述（agentcore 注册用）：

| 工具 | 描述（注册用，英文） |
|---|---|
| `submit_polish_plan` | Submit the full-chapter review plan before any edit batches. Must be called exactly once, first. Declares issues (existing findings + self-discovered) and routes fact/structural issues to the writer. |
| `submit_edit_batch` | Submit a batch of candidate edits (max 8 per batch, max 4 batches, max 32 total). All old_string values must come verbatim from the original baseline draft. |
| `finish_polish` | End the polish run: declare status, covered issues and unresolved items. After this call no further plan/batch calls are accepted. |

`finish_polish` 是 runner 的停止工具（StopAfterTools，包 4 配置），
**不能**复用 Writer StopGuard（计划 §6）。

## 3. 运行生命周期与 accumulator

### 3.1 operation_id 与 baseline_digest

- `operation_id` 由 **host 生成**（`polish_draft` 启动 run 时），格式
  `pol-<chapter>-<unixnano>-<4位随机hex>`，随任务文本注入（包 6 的
  `buildPolishTask` 追加）。模型在三个工具调用中**必须原样回显**。
- `baseline_digest` 取自 `store.PolishBaseline.InputDigest`（`sha256:` 前缀），
  同样注入任务文本并回显。
- 任一调用中 operation_id 或 baseline_digest 与 accumulator 不符 → 整批拒绝
  （错误码 `op_mismatch` / `baseline_mismatch`），不进入任何预检。

### 3.2 accumulator 状态机

accumulator 由 `PolishDraftTool` 持有（每次 Execute 开始时重置），
通过指针共享给三个工具。`polish_draft` 非 ConcurrencySafe，但 accumulator
内部仍加互斥锁（防御性，防未来误用）。

```
empty ──submit_polish_plan(通过)──▶ planned ──finish_polish(通过)──▶ finished
  │                                    │                                │
  │ 任何 batch/finish → not_planned     │ batch 0..4 次（严格递增；0 次 =   │ 任何调用 → already_finished
  │ 第二次 plan → plan_exists           │ plan 后直接 finish，合法 no_op） │
```

- `empty → planned`：plan 校验通过后原子转换；plan 内容存入 accumulator。
- `planned → finished`：finish 校验通过后原子转换；finish 内容存入 accumulator。
- 状态转换后不可回退。进程中断后 accumulator 丢失，重新运行完整审阅
  （计划 §5：暂不建设持久化 mini-runner）。

### 3.3 accumulator 存储字段（Go 类型目标，包 2 实现）

```go
type polishAccState int

const (
    polishAccEmpty polishAccState = iota
    polishAccPlanned
    polishAccFinished
)

// PolishAccumulator 是单次 polish run 的候选累积器（内存态）。
type PolishAccumulator struct {
    mu            sync.Mutex
    operationID   string
    baselineDigest string
    chapter       int
    state         polishAccState

    plan          *PolishPlanRecord      // plan 校验通过后非 nil
    accepted      []PolishAcceptedEdit   // 已通过预检的候选（含 byte range）
    rejected      []PolishRejectedEdit   // 被预检拒绝的候选（审计计数用）
    usedIssueIDs  map[string]bool        // 已接受 edit 用过的 issue_id（防跨批复用）
    nextBatch     int                    // 期望的下一个 batch_index（1 起）
    finish        *PolishFinishRecord    // finish 校验通过后非 nil
}

type PolishPlanRecord struct {
    OverallAssessment string
    PlannedEditCount  int
    Issues            []PolishPlanIssue   // 按提交顺序
    Digest            string              // sha256(规范化 plan JSON)，审计用
}

type PolishAcceptedEdit struct {
    IssueID   string
    OldString string
    NewString string
    Reason    string
    Category  string
    Start     int // baseline 中的 byte range（定位时填充）
    End       int
    Mode      PolishEditMatchMode // exact | normalized（复用现有枚举）
}

type PolishRejectedEdit struct {
    IssueID string
    Code    string // 错误码（§8）
    Index   int    // 批内下标
}

type PolishFinishRecord struct {
    Status             string
    SubmittedEditCount int
    CoveredIssueIDs    []string
    Unresolved         []PolishUnresolved
    Summary            string
}
```

## 4. `submit_polish_plan`

### 4.1 请求 JSON（严格模式：未知字段拒绝）

```json
{
  "operation_id": "pol-123-1728000000000000-a1b2",
  "baseline_digest": "sha256:...",
  "overall_assessment": "全章风格判断",
  "planned_edit_count": 20,
  "issues": [
    {
      "issue_id": "p-001",
      "source_finding_ids": ["f-12"],
      "origin": "existing_finding",
      "category": "rhythm",
      "priority": "high",
      "anchor": "精确连续原文",
      "problem": "问题描述",
      "edit_goal": "修改目标",
      "fact_risk": "none",
      "action": "edit"
    }
  ]
}
```

### 4.2 字段约束

| 字段 | 约束 |
|---|---|
| `operation_id` | 必填，1–64 字符，必须等于 accumulator |
| `baseline_digest` | 必填，`^sha256:[0-9a-f]{64}$`，必须等于 accumulator |
| `overall_assessment` | 必填，1–2000 runes |
| `planned_edit_count` | 必填，0–32（软目标，不与其他字段交叉校验；越界 → `value_out_of_range`） |
| `issues` | 必填，0–32 项（**硬上限 32**） |
| `issue_id` | 必填，`^p-[0-9]{3}$`，批内唯一 |
| `source_finding_ids` | 可选，≤16 项，每项 1–32 字符 |
| `origin` | 必填，`existing_finding` \| `polisher_discovered` |
| `category` | 必填，`rhythm` \| `repetition` \| `voice` \| `density` \| `clarity` \| `imagery` \| `transition` |
| `priority` | 必填，`high` \| `medium` \| `low` |
| `anchor` | 必填，1–2000 runes（**不做存在性校验**——plan 是意图声明，锚点存在性由 batch 的 old_string 校验兜底） |
| `problem` | 必填，1–1000 runes |
| `edit_goal` | 必填，1–1000 runes |
| `fact_risk` | 必填，`none` \| `low` \| `high` |
| `action` | 必填，`edit` \| `defer_to_writer` \| `no_op` |

未列专属错误码的长度/数值范围违规（如 `overall_assessment` 超 2000、`problem`/
`edit_goal` 超 1000）统一 → `value_out_of_range`。

### 4.3 一致性规则（确定性）

- `fact_risk=high` ⇒ `action` 必须为 `defer_to_writer`（`edit` 或 `no_op` → 拒绝，
  错误码 `fact_risk_edit_conflict`）。事实/结构/时间线/人物状态/因果主体问题
  必须显式转 Writer（计划 §4.1）；high 风险 issue 不得以 no_op 静默不处理。
- `fact_risk∈{none,low}` 时 `action` 三值均可。
- `action=edit` 的 issue 是 batch 可引用集合；`defer_to_writer`/`no_op` 的 issue
  被 batch 引用 → 拒绝（`issue_not_editable`）。

### 4.4 返回

```json
{"accepted": 1, "rejected": 0, "errors": []}
```

- `accepted` ∈ {0, 1}。拒绝时 `errors` 含 `{"index": -1, "code": "..."}`。
- 不回显 plan 内容（审计只存 digest 与计数，计划 §10）。

## 5. `submit_edit_batch`

### 5.1 请求 JSON（严格模式）

> 解冻裁定：相对 handoff plan §4.2，本工具新增 `baseline_digest` 回显字段（见 §1）。

```json
{
  "operation_id": "pol-123-1728000000000000-a1b2",
  "baseline_digest": "sha256:...",
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

### 5.2 字段约束

| 字段 | 约束 |
|---|---|
| `operation_id` / `baseline_digest` | 同 §4.2 |
| `batch_index` | 必填，1–4，**必须等于 accumulator.nextBatch**（严格递增，防重复/乱序批次） |
| `edits` | 必填，1–8 项（**每批硬上限 8**） |
| `issue_id` | 必填，`^p-[0-9]{3}$`，必须存在于 plan、`action=edit`，且**此前未被任何已接受 edit 使用过**（跨批复用 → `issue_reused`） |
| `old_string` | 必填，1–2000 runes，必须唯一命中 baseline（exact → normalized 两级，复用 `locatePolishEdit`） |
| `new_string` | 必填，1–2000 runes（本协议新增上限，见 §1 解冻裁定），与 old_string 归一化后不得相同（复用现有 no-op 判定） |
| `reason` | 必填，1–500 runes |
| `category` | 必填，同 §4.2 枚举 |
| `fact_check` | 必填，**仅允许 `unchanged`**；其他值 → 拒绝（`fact_check_invalid`） |

### 5.3 确定性预检（host，按序短路）

1. operation/baseline 一致（§3.1）。
2. 状态为 `planned`（否则 `not_planned`）。
3. `batch_index == nextBatch`（否则 `bad_batch_index`）。
4. 批内 edits ≤ 8（`batch_limit`）。
5. 累计 accepted+rejected ≤ 32（`total_limit`）。注：rejected 计入 32 上限是
   有意设计——anchor 失准消耗预算，激励模型提供准确锚点；不放宽。
6. 每条 edit：issue 存在且 `action=edit`（`issue_unknown` / `issue_not_editable`）。
7. `old_string` 非空、≤2000 runes（`empty_old` / `anchor_too_long`）。
8. `new_string` 非空、≤2000 runes（`empty_new` / `new_too_long`）。
9. old/new 归一化后不同（`noop`）。
10. `old_string` 唯一命中 baseline（`anchor_missing` / `anchor_ambiguous`，
    复用 `locatePolishEdit` 的 exact+normalized 两级与弱锚点门槛）。
11. 与已接受候选不重叠（`anchor_overlap`，byte range 判定）。
12. **数字保持**：`D(old_string) == D(new_string)`，其中 `D(s)` 定义为
    `s` 中所有"极大连续 ASCII 数字 run"按出现顺序组成的**字符串序列**
    （例：`"第3章有5只猫，2024年"` → `["3","5","2024"]`）。
    序列长度不同、任一对应 run 字符串不同、或顺序不同 → `fact_changed`
    （按 run 字符串逐项比较，**非**按 run 长度比较）。此检查覆盖所有 ASCII
    数字与时间数字（年月日时分秒均为数字 run）。
13. `fact_check == "unchanged"`（`fact_check_invalid`）。
14. 枚举/必填字段合法（`bad_enum` / `field_required`）。

**事实保护边界（冻结决策）**：host 确定性检查覆盖数字与时间数字（年月日时分秒
均为数字 run）；专名、称谓、因果主体**无法无实体表确定性判定**——由三层兜底：
(a) `fact_check=unchanged` 模型声明（强制）；(b) plan 级 `fact_risk` 路由
（high → defer_to_writer）；(c) 最终验证复用现有机械/覆盖检查（包 6）。
事实大纲是自由文本（`loadFactualOutline` 返回 string），不从中提取实体表；
未来若大纲提供结构化实体列表，可扩展本检查（需解冻）。

### 5.4 返回

```json
{"accepted": 2, "rejected": 1, "accepted_total": 5, "errors": [{"index": 2, "code": "anchor_missing"}]}
```

- `accepted` = 本批通过预检并存入 accumulator 的条数。
- `rejected` = 本批被拒条数；`errors` 逐条给 `index`（批内下标）+ `code`。
- `accepted_total` = 累计已接受数（帮助模型感知 32 上限）。
- **不回显任何 old_string/new_string 内容**（计划 §4.2/§10）。

## 6. `finish_polish`

### 6.1 请求 JSON（严格模式）

> 解冻裁定：相对 handoff plan §4.3，本工具新增 `baseline_digest` 回显字段（见 §1）。

```json
{
  "operation_id": "pol-123-1728000000000000-a1b2",
  "baseline_digest": "sha256:...",
  "status": "complete",
  "submitted_edit_count": 20,
  "covered_issue_ids": ["p-001"],
  "unresolved": [
    {"issue_id": "p-021", "reason": "requires_structural_rewrite", "recommended_owner": "writer"}
  ],
  "summary": "本次精修摘要"
}
```

### 6.2 字段约束

| 字段 | 约束 |
|---|---|
| `operation_id` / `baseline_digest` | 同 §4.2 |
| `status` | 必填，`complete` \| `partial` \| `no_op` \| `escalate` |
| `submitted_edit_count` | 必填，0–32，**必须等于 accumulator 已接受总数**（`count_mismatch`） |
| `covered_issue_ids` | 必填（可为空数组），每项 `^p-[0-9]{3}$`，必须是 plan 中存在的 issue_id，且满足下列之一：(a) 该 issue 在某已接受 batch 中提交过 edit；或 (b) 该 issue `action=no_op`。不满足 → `coverage_not_editable` |
| `unresolved` | 可选，≤32 项；`issue_id` 必须存在于 plan；`reason` 1–200 runes；`recommended_owner` 必填且**仅允许 `writer`**（`owner_invalid`） |
| `summary` | 必填，1–1000 runes |

### 6.3 语义

- `finish_polish` 是 **runner 停止工具**：调用成功后 runner 结束（StopAfterTools），
  但**不直接保存正文**（计划 §4.3）。
- 状态转换 `planned → finished`；此后任何 plan/batch/finish 调用 → `already_finished`。
- `no_op` 表示完整审阅后认为无需修改（合法终态，非错误）。

### 6.4 一致性规则（确定性）

- `status=no_op` ⇒ `submitted_edit_count` 必须为 0、`covered_issue_ids` 必须为空数组、
  `unresolved` 必须为空（违反 → `status_count_conflict`）。
- `status=complete` ⇒ `unresolved` 中不得包含 plan 里 `action=edit` 的 issue
  （已编辑的 issue 不能再列为未解决 → `unresolved_edited`）。
- `status=escalate` ⇒ `unresolved` 必须非空（`unresolved_empty`）。
- `covered_issue_ids` 中每个 issue 必须满足 §6.2 的可覆盖条件（`coverage_not_editable`）。
- `submitted_edit_count` 必须等于 accumulator 已接受总数（`count_mismatch`）。

### 6.5 返回

```json
{"accepted": 1, "rejected": 0, "errors": []}
```

## 7. JSON 解析容错（三个工具共用）

- 严格模式：`json.Decoder` + `DisallowUnknownFields`，顶层与嵌套均拒绝未知字段。
- 容错（计划 §7）：容忍 BOM 前缀；容忍**单层** Markdown fence 包裹
  （```json ... ``` 剥掉后解析）；格式修复只做 1 次（本地，不重试模型）。
- 解析失败 → 整批拒绝，错误码 `malformed_json`（不触发模型重试，由 runner
  技术恢复预算处理，包 4/5）。

## 8. 错误码（稳定枚举，包 2 实现为 string 常量）

| 错误码 | 含义 |
|---|---|
| `op_mismatch` | operation_id 与 accumulator 不符 |
| `baseline_mismatch` | baseline_digest 与 accumulator 不符 |
| `not_planned` | 未提交 plan 就调用 batch/finish |
| `plan_exists` | 重复提交 plan |
| `already_finished` | finish 后再次调用 |
| `bad_batch_index` | batch_index 不是期望的下一个 |
| `batch_limit` | 单批超过 8 条 |
| `total_limit` | 累计超过 32 条 |
| `issue_unknown` | issue_id 不在 plan 中 |
| `issue_not_editable` | issue 的 action 不是 edit |
| `fact_risk_edit_conflict` | fact_risk=high 但 action 不是 defer_to_writer |
| `issue_reused` | issue_id 跨批重复使用（同一 issue 已提交过 edit） |
| `coverage_not_editable` | covered_issue_ids 中的 issue 未提交过 edit 且 action≠no_op |
| `status_count_conflict` | status=no_op 但 submitted_edit_count/covered_issue_ids/unresolved 非空 |
| `unresolved_edited` | status=complete 但 unresolved 含 action=edit 的 issue |
| `unresolved_empty` | status=escalate 但 unresolved 为空 |
| `value_out_of_range` | 非空字段长度/数值越界（未列专属码时统一归此） |
| `anchor_missing` | old_string 在 baseline 中不存在（含弱锚点 normalized 拒绝） |
| `anchor_ambiguous` | old_string 在 baseline 中多次命中 |
| `anchor_overlap` | 与已接受候选重叠 |
| `anchor_too_long` | old_string 超过 2000 runes |
| `empty_old` | old_string 为空 |
| `empty_new` | new_string 为空 |
| `new_too_long` | new_string 超过 2000 runes |
| `noop` | old/new 归一化后相同 |
| `fact_changed` | 数字 run 序列不一致（D(old) != D(new)） |
| `fact_check_invalid` | fact_check 不是 `unchanged` |
| `bad_enum` | 枚举值非法 |
| `field_required` | 缺少必填字段 |
| `malformed_json` | JSON 解析失败（含未知字段/尾随内容） |
| `plan_limit` | issues 超过 32 |
| `issue_id_invalid` | issue_id 格式非法或重复 |
| `status_invalid` | finish status 非法 |
| `count_mismatch` | submitted_edit_count 与已接受总数不符 |
| `owner_invalid` | recommended_owner 不是 writer |
| `summary_required` | summary 为空 |

## 9. 审计映射（包 6 落盘时使用）

accumulator 终态 → `PolishCheckpointMeta`（现有字段 + 包 6 新增字段）：

| PolishCheckpointMeta 字段 | 来源 |
|---|---|
| `Method` | `"candidate_tools"`（新值，区别于 `edit_list`/`full_text`） |
| `EditCount` | 最终实际应用条数（包 6 复用 `ApplyPolishEditPlanDetailed` 后） |
| `ProposedEditCount` | accepted + rejected 总数 |
| `DroppedEditCount` | rejected 数 + 最终验证丢弃数 |
| `DropReasons` | **per-edit** 预检丢弃原因，复用并扩展 `PolishEditDropReason`：保留现有 `anchor_missing`/`anchor_ambiguous`/`anchor_overlap`/`noop`/`empty_old`/`anchor_too_long`/`coverage_limit`/`output_too_short`/`output_too_long`/`overlap_lower_priority`/`count_limit`/`mechanical`，新增 per-edit 码 `fact_changed`/`fact_check_invalid`/`issue_unknown`/`issue_not_editable`/`issue_reused`/`empty_new`/`new_too_long`/`total_limit`/`batch_limit`。**run 级拒绝**（`op_mismatch`/`baseline_mismatch`/`not_planned`/`plan_exists`/`already_finished`/`bad_batch_index`/`plan_limit`/`issue_id_invalid`/`bad_enum`/`field_required`/`malformed_json`/`status_invalid`/`count_mismatch`/`owner_invalid`/`summary_required`/`fact_risk_edit_conflict`/`coverage_not_editable`/`status_count_conflict`/`unresolved_edited`/`unresolved_empty`/`value_out_of_range`）**不进** `DropReasons`，由 `RunRejectionCode` 单独记录 |
| `NormalizedMatchCount` / `Partial` / `MatchModes` | 复用现有（最终验证结果） |
| **新增** `OperationID` | accumulator.operationID |
| **新增** `RunRejectionCode` | 整 run/整批被拒时的错误码（§8 中 run 级码），per-edit 全部通过时为空字符串 |
| **新增** `PlanIssueCount` | len(plan.Issues) |
| **新增** `BatchCount` | 实际提交批次数 |
| **新增** `FinishStatus` | finish.status |
| **新增** `UnresolvedCount` | len(finish.Unresolved) |
| **新增** `PlanDigest` | plan.Digest |

审计边界（计划 §10）：完整章节/大纲/findings 不写入 checkpoint 或 audit metadata；
原始候选只存在于 run 内存 accumulator 与最终章节文件；工具返回与 checkpoint
均不回显 edit 内容。

## 10. 与现有代码的复用关系（包 2 实现约束）

- **复用**：`locatePolishEdit`（exact+normalized 两级定位与弱锚点门槛）、
  `normalizePolishAnchor`（no-op 判定）、`normalizePolishAnchorMapped` 与
  `polishNormMap`（normalized 定位的 byte range 回映，batch 预检的
  `anchor_overlap` 判定依赖）、`PolishEditMatchMode`、
  `PolishEditDropReason`（扩展）、`maxPolishEditOldRunes`（2000）、
  `maxPolishEdits`（32）、`schema.Object/Property`（工具 Schema 声明）。
- **新增**：`polish_plan.go`、`polish_edit_batch.go`、`polish_finish.go`、
  `polish_accumulator.go`、数字保持检查（`D(old) == D(new)`）、错误码常量、
  纯单元测试（不依赖 store/runner）。
- **禁止触碰**：`polish_draft.go`、`internal/store`、`assets/prompts`、
  `internal/agents/build.go`（计划 §11 包 2 边界）。
- 最终应用（覆盖/输出/机械检查）仍由包 6 在 `polish_draft.go` 中调用现有
  `ApplyPolishEditPlanDetailed` 完成——accumulator 只做 §5.3 的确定性预检。

## 11. 与 prompt/runner 的接口约定（供包 1/4/6 引用）

- 任务文本（包 6 `buildPolishTask`）追加：`operation_id`、`baseline_digest`、
  协议说明（先 plan → 最多 4 批 → finish；每批 ≤8；总 ≤32；基于同一 baseline）。
- `polisher.md`（包 1）描述协议行为，不得与本文档冲突。
- runner（包 4）：polisher 工具集 = 三个工具；`StopAfterTools=["finish_polish"]`；
  MaxTurns=6（初始 + plan + 4 batch + finish）；单 run 实际 API 调用硬上限 8
  （含技术恢复，包 5）；plan 可见输出 ≤8k tokens、单 batch ≤24k tokens 且原始
  工具参数 ≤48 KiB、finish ≤4k tokens（计划 §6）。