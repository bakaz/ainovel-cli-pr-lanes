# 规划合约变更记录

> 分支：`fix/v3-core4-planning-contracts`
> 本文件记录该分支已实现的全部改动，不包含未实现计划。

---

## 背景与目标

`save_foundation` 工具的三条规划路径（`layered_outline`、`append_volume`、`expand_arc`）长期依赖 `domain.VolumeOutline` / `domain.ArcOutline` 作为统一解码目标，导致骨架弧（无 `chapters`）与详细弧（有 `chapters`）的 Schema 描述混用，V3 与 Core4 的 scene 字段严格度也无法分离。

本次变更引入 **PlanningInput 层**（工具输入层类型），从 `save_foundation.go` 的 Phase 1 解码起与 domain 模型解耦，使：

- 骨架弧的 JSON 形态（`estimated_chapters` ≥ 1，无 `chapters`）可被 Schema 准确描述并合法通过；
- 详细弧的 `estimated_chapters` 在转换到 domain 前归零；
- V3 与 Core4 各自使用独立 Schema 分支（anyOf Draft-07），不再共用单个 object 类型；
- flat outline 的 `chapter` 字段在 Schema 层面与 planning 路径分离（前者必填，后者可省略/0）。

---

## 三条规划路径

### 1. `layered_outline`（初始全局长篇规划）

- 输入：`[]PlanningVolumeInput`（JSON 数组）
- 必须通过 Schema 验证，然后进入 Phase 1 语义校验
- V3：空数组在 Schema 层 **拒绝**（`minItems=1`）；每卷至少 1 弧；全书首弧必须为详细弧
- Core4：外层 Schema 含宽松 `anyOf`/generic array/object 分支，空数组**可能通过 Schema**；`isV3` 守卫使 Core4 跳过 Phase 1 空数组拒绝，空卷进入 domain 转换和 `ValidateLayeredOutline`（拓扑校验兜底，非聚合错误）

### 2. `append_volume`（追加新卷）

- 输入：单 `PlanningVolumeInput`（JSON 对象）
- 卷 index 必须 > 0；必须带 `reason` 参数
- 新卷首弧必须为详细弧（`IsDetailed()` 为 true）
- 后续弧可为骨架弧或详细弧

### 3. `expand_arc`（展开已有骨架弧）

- 输入：`domain.ArcExpansion`（JSON 对象，含 `title`、`goal`、`chapters`）
- 使用 **fail-fast** 校验（场景错误不聚合），因为依赖已存在的目标弧
- 必须先确认目标弧存在于已落盘的分层大纲中，且未以不同内容展开过
- 展开后做前瞻性拓扑/编号校验

---

## 详细弧与骨架弧

### 输入形态（`ArcInput`，仅 PlanningInput 层）

| 特征 | 详细弧 | 骨架弧 |
|------|--------|--------|
| `chapters` | 非空数组（≥1） | 省略 / `null` / `[]` |
| `estimated_chapters` | 可为任意值（转换前归零） | ≥ 1 |
| `IsDetailed()` | `true` | `false` |
| `IsSkeleton()` | `false` | `true` |

### 转换到 domain

`ArcInput.toDomain()`：

- 详细弧：`EstimatedChapters` 强制置 0（无论输入值）
- 骨架弧：`EstimatedChapters` 保持输入值；`Chapters` 为空切片

### 编号规则

- **卷 index**：`layered_outline` 每卷 index > 0（聚合为 `INVALID_INDEX`）；`append_volume` 卷 index > 0（fail-fast）
- **弧 index**：卷内弧序号 > 0（聚合为 `INVALID_INDEX`）
- **章节 chapter**：flat outline 下 V3 要求 > 0（fail-fast）；planning 路径接受省略/0/显式值（系统自动编号）
- **无卷/弧连续性约束**（不会校验 index 是否从 1 开始连续递增）

---

## V3 与 Core4 差异

### Scene 字段严格度

| 层面 | V3 | Core4 |
|------|----|-------|
| 必填字段 | goal / action / conflict / outcome / body_reaction / emotion_reaction / erotic_charge（7 个） | goal / action / conflict / outcome（4 个） |
| 可选字段 | sensory_anchor | sensory_anchor、erotic_charge |
| body_reaction / emotion_reaction | 必填且 minLength=1 | 可选 |
| legacy string 场景 | 拒绝 | 接受（`anyOf` string / object） |
| `scenes` 数组 | minItems=1（非空） | 无 minItems |
| `erotic_charge` | `scenePropRequiredString` | `scenePropString`（可选） |

### Schema 严格度

| 维度 | V3 | Core4 |
|------|----|-------|
| 顶层结构 | 独立 `anyOf` 每条命令为独立 branch | `schema.Object` + content `anyOf`（含 generic object/array 分支，不是所有非法 payload 都在 Schema 层被拒绝） |
| arc 结构 | `anyOf` detailed + skeleton 双分支 | `anyOf` detailed + skeleton 双分支（本次新增） |
| 空数组拒绝 | 所有 planning 路径均拒绝 | flat outline 空数组**接受**（兼容旧行为）；分层空数组可能通过 Schema，依赖拓扑校验兜底 |
| flat outline `chapter` | Schema 层 **required** + 运行时 > 0 校验 | Schema 层 **required**（`chReq`） |

> **注意**：Core4 content 的 `anyOf` 包含 `{"type": "object"}`（用于 update_compass/complete_book）等宽松分支。匹配到错误分支的 payload 可能通过 Schema 验证，但在 Phase 1 解码或后续拓扑校验中被拒绝。V3 无此问题，因其 `anyOf` 每条命令都是独立且属性完备的 branch。

### Scale 兼容

- **V3**：`append_volume` / `expand_arc` **不传递** `scale` 参数（由 `validateV3ArgumentKeys` 拒绝不明字段）
- **Core4**：`append_volume` / `expand_arc` **仍传递** `scale` 参数且正常落盘（当前兼容，未禁止）
- 该差异已在 Prompt 的版本说明中明确标注，不构成对 Core4 的禁止

---

## Prompt / Reference 同步

### `assets/prompts/architect-long.md`

| 改动 | 说明 |
|------|------|
| 初始卷描述 | 卷 1：首弧为详细弧（含 chapters），后续为骨架弧；卷 2：全骨架弧 |
| estimated_chapters 约束 | 仅对骨架弧要求 ≥ 8 |
| V3 scene 字段 | 七必填字段列出（goal/action/conflict/outcome/body_reaction/emotion_reaction/erotic_charge），sensory_anchor 可选 |
| append_volume 示例 | 首弧 `chapters` 数组、后续弧 `estimated_chapters`（无 `chapters`） |
| expand_arc 章节说明 | 同上场景字段描述 |
| 版本说明 | 新增脚注：V3 不传 scale，Core4 按自身契约传——不构成对 Core4 的禁止 |

### `assets/references/outline-template.md`

| 改动 | 说明 |
|------|------|
| 扁平大纲 scenes | 移除 V3 专属字段，仅保留 Core4 四字段 + 版本说明 blockquote（移出代码块） |
| 分层大纲文字 | "前 2 卷有弧骨架，第一弧为详细弧（含章节），其余为骨架弧" |
| 卷 1 弧 2 | 骨架弧示例无 `chapters: []` |
| 卷 2 弧 | 骨架弧示例无 `chapters: []` |
| 第三卷 | **移除**（初始规划仅 2 卷，与 Prompt 一致） |

---

## 错误聚合边界

### 聚合范围（`planValidationErrors`）

| 错误码 | 触发条件 | 路径格式 |
|--------|----------|----------|
| `INVALID_INDEX` | volume / arc index ≤ 0 | `volumes[N]` / `volumes[N].arcs[M]` |
| `EMPTY_VOLUME` | Core4 volume 无 arcs | `volumes[N]` |
| `EMPTY_ARC` | arc 既非 detailed 也非 skeleton | `volumes[N].arcs[M]` |
| `INVALID_SCENE` | scene 字段缺失或空值 | `volumes[N].arcs[M].chapters` |

### 不纳入聚合（直接 fail-fast）

- Schema 验证失败（由工具框架层处理）
- JSON 解码失败（`decodeFoundationJSON` 返回）
- Store / I/O 错误（如 `store.Progress.Load()`、`store.Outline.LoadLayeredOutline()`）
- 状态前置条件（如 `Phase=Complete` 拒绝 append_volume）
- 拓扑/编号校验失败（`ValidateLayeredOutline`）
- `expand_arc` 的所有校验（始终 fail-fast，因依赖已存在的目标）

### expand_arc fail-fast 理由

`expand_arc` 必须先读已落盘的 `layered_outline.json` 确认目标弧存在，并在内存中构造 prospective 做拓扑校验。这些操作依赖 Store 可读且数据完整，若在此过程中发现任何不一致，应当在首次写入前立即拒绝，不应聚类到语义错误列表。

---

## 保持不变的下游行为

以下行为不受本次变更影响（不回归确认）：

- **`domain.TotalChapters`**：仍按详细弧的 `len(Chapters)` + 骨架弧的 `EstimatedChapters` 计算
- **`domain.FlattenOutline`**：仍按全局连续章节号展平分层大纲
- **`outline.json` 写入**：`layered_outline` 仍然在 Phase 2 写两份（`layered_outline.json` + 展平的 `outline.json`）
- **前缀规则**：展平时的 `"前缀-卷号-弧号-节号"` 格式不受影响（由 `FlattenOutline` 内部实现）
- **`Store.AppendVolume` / `Store.ExpandArc`**：提交路径签名和语义不变
- **`Progress.TotalChapters`**、`UpdateVolumeArc`、`SetLayered` 等 Progress 字段更新逻辑不变
- **V3 `complete_book` 拒绝条件**：仍要求 Phase=Writing、无 pending rewrites、有已完成章节、大纲内无未写章节

---

## 非目标

以下明确**不在本分支范围内**：

- **不重构 typed 顶层**：`typedCommand` 及 Phase 1/2 的分派结构保持不变
- **不新增 Core4 scale 拒绝**：Core4 的 `append_volume` / `expand_arc` 仍可传 `scale` 参数并正常落盘
- **不添加卷/弧连续性约束**：不会校验卷 index 是否从 1 起始、弧 index 是否连续递增
- **不修改 Store / domain 层**：`domain.VolumeOutline`、`domain.ArcOutline`、`domain.OutlineEntry` 等模型结构未改动
- **新增独立测试文件**（`planning_skeleton_test.go`），不修改 `save_foundation_test.go` 等已有测试
- **不修改下游消费者**：`plan_chapter`、`commit_chapter`、`draft_chapter`、`novel_context_builders` 等读取大纲的工具未感知 PlanningInput 层
- **不做事务/幂等**：Phase 1 校验与 Phase 2 写入之间没有事务保护；重复提交相同 payload 可能产生重复写入

### 重试边界

- 若 Phase 1 聚合错误返回，Phase 2 **不会执行**（无写入）
- 若 Phase 2 写入中途失败（如 `SaveLayeredOutline` 成功但 `SaveOutline` 失败），已写入的部分不会回滚
- 调用方在收到错误后可修复 payload 后重试，但不会自动检测"已部分写入"状态

---

## 文件改动地图

| 文件 | 状态 | 改动概要 |
|------|------|----------|
| `internal/tools/save_foundation.go` | 已修改 | 新增 `chapterOutlineSchema` 参数；`arcOutlineSchema` V3/Core4 均用 anyOf；Phase 1 聚合错误；`validateChapterScenesAggregated`；移除死代码 `validateV3Content`/`validateCore4Content` |
| `internal/tools/planning_input.go` | **未跟踪**（新文件） | `ChapterInput`、`ArcInput`、`PlanningVolumeInput` + `planValidationErrors` |
| `internal/tools/planning_skeleton_test.go` | **未跟踪**（新文件） | 骨架弧/详细弧/聚合错误的完整测试套件 |
| `assets/prompts/architect-long.md` | 已修改 | 场景字段、骨架弧描述、版本说明 |
| `assets/references/outline-template.md` | 已修改 | 移除第三卷、修正场景字段、版本说明移出代码块 |

---

## 新增测试（`planning_skeleton_test.go`）

| 测试函数 | 验证内容 |
|----------|----------|
| `TestPlanV3_InitialSkeletonArc` | V3 layered_outline 首弧详细 + 后续骨架通过 |
| `TestPlanV3_InitialFirstArcSkeletonRejected` | V3 首弧为骨架→拒绝 |
| `TestPlanV3_SkeletonChaptersNullOrEmpty` | 骨架弧 chapters omitted / null / [] 均通过 |
| `TestPlanV3_AppendSkeletonArc` | V3 append_volume 带后续骨架弧通过 |
| `TestPlanV3_AppendFirstArcSkeletonRejected` | append_volume 首弧为骨架→拒绝 |
| `TestPlanV3_SkeletonEstimateMissingOrZero` | Schema 层拒绝无 estimated_chapters 或估计=0 |
| `TestPlanV3_ChapterOmittedOrZero` | planning 路径接受 chapter 省略或 0 |
| `TestPlanV3_ChapterExplicitCorrect` | 显式 chapter 接受，0 值被标准化 |
| `TestPlanV3_ChapterWrongExplicitRejected` | 错号 chapter 被编号校验拒绝 |
| `TestPlanV3_DetailedArcEstimateZeroed` | 详细弧 estimated_chapters 在转换前归零 |
| `TestPlanCore4_SkeletonArcCompat` | Core4 append_volume 骨架弧兼容 |
| `TestPlanV3_MultiSemanticError` | 多个 EMPTY_ARC 聚合为一个错误响应 |
| `TestPlanCore4_AppendExpandScaleCompat` | Core4 仍可传递 scale 参数 |
| `TestPlanCore4_ExpandArcScaleCompat` | Core4 expand_arc 仍可传递 scale 参数 |
| `TestPlanV3_Scene7FieldStrictInLayered` | V3 scene 缺 erotic_charge→Schema 拒绝 |
| `TestPlanCore4_Scene4FieldAcceptsLegacy` | Core4 接受遗留 string 场景 |
| `TestPlanV3_FlatOutlineChapterRequired` | V3 flat outline 缺 chapter→Schema required + 运行时 chapter>0 均拒绝 |

---

## 已执行验证命令

```powershell
# 编译
go build ./internal/tools/

# 格式化
gofmt -w internal\tools\save_foundation.go

# planning 路径 + save_foundation 全量测试
go test ./internal/tools/ -run "TestPlan|TestSaveFoundation" -count=1 -v

# tools 包全量测试
go test ./internal/tools/ -count=1

# 上下游相关包
go test ./internal/flow/ ./internal/host/ ./internal/agents/ ./internal/store/ ./internal/domain/ ./internal/projectprofile/ -count=1

# 全量测试（预存失败 4 项：migrate import、ctxpack strategy、notify encoding、version mode）
go test ./... -count=1
```
