# 文风对齐草案 v2（手动对照用）

生成时间：2026-07-25
基准版本：基于已完成 ch1–286（v11/a3 进行中）的 workspace 现行 `user_rules.json` / `style_rules.json`
**未写入** `workspace/output/novel/meta/`。以下所有文件仅位于 `.pr-drafts/style-align-manual/`，workspace 未被修改。

## 文件

| 草案 | 对应线上 |
|------|----------|
| `user_rules.new.json` | `workspace/output/novel/meta/user_rules.json` |
| `style_rules.new.json` | `workspace/output/novel/meta/style_rules.json` |
| `user_rules.diff` | 相对线上 user_rules.json 的 unified diff（Python difflib，UTF-8） |
| `style_rules.diff` | 相对线上 style_rules.json 的 unified diff（Python difflib，UTF-8） |
| `COMPLETE.diff` | 上述两个 diff 合并 |

**锚点不替换**：`style_anchors.json` 保留当前锚点。建议后续手动加入 1–2 条已接受的 v11/a3 节选（附真实来源 chapter/digest），而非整体替换。

## 175–284 审计证据摘要

ch1–174 阶段文风相关问题已基本通过负面控制减少命名违规（`forbidden_phrases` / `fatigue_words` 命中率下降）。**剩余风险**集中为以下四类模式（本条草案的审阅尺度与焦点契约直接针对这些模式）：

| 风险模式 | 表现 | 本条草案的对应措施 |
|----------|------|-------------------|
| 因果链替代过程 | 「制造/传导/意味着」把动作链写成逻辑推导 | editor drift rule 明确跨章 pattern 判 aesthetic error |
| 固定顺序枚举 | 每段按同一顺序复述部位/信号 | focal contract：必须选一个焦点，禁止固定顺序盘点 |
| 临床全知 | 替角色观察体内毫米位移、神经路径、组织变化 | writer drift rule + focal contract：内部生理只写可观察/可推断 |
| 名词清单式短句 | 堆叠名词替代中句推进，段落被打碎 | editor drift rule 判 pattern；focal contract：短句只标记转折点 |

## 本草案变更摘要

### `user_rules.new.json`（相对当前线上）

1. **保留前一草案的去重成果**：
   - Writer `rule_style_drift_guard_v2`：两条重复 → 一条正面规则
   - Editor `rule_editor_style_drift_guard_v2`：两条重复 → 一条
   - `rule_c70bfc6b8861`：已无重复段落
   - `rule_2bf211376bd4`：已改进（无「优先使用阿拉伯数字」）
2. **替换重复的动作链/清单式强调为焦点变化契约**：
   - `rule_writer_process_style_v1`（7 项清单）→ `rule_writer_focal_contract_v1`：选最强感官焦点、已建立状态只用一个细节携带、大多数篇幅花在动作失败/注意力/预期/判断转变或身体后果、内部生理只写可观察/可推断、中句推过程短句标转折
   - `rule_writer_attention_shift_v1`（新增）：每个主要场景至少一次女孩注意力/预期/尝试性应对的可见变化，一句口语化内心足够
   - `rule_050d45a65912`（简化）：移除固定动作链公式，改为按焦点契约写
   - `rule_writer_erotic_fulfillment`（简化）：移除动作链 checklist 重复
   - `rule_d6e4f8a2c0b1`（简化）：移除「关键场景以动作链推进」程式化表述
3. **`rule_erotic_sensory_priority_v3` 修改**：从全身清单 → 选一个动作相关的焦点部位 + 至多一个因果相连的远端余响；禁止逐一点名全身
4. **Editor `rule_editor_style_drift_guard_v2` 升级**：明确判跨章模板模式——因果「制造/传导/意味着」链、固定顺序枚举、同构动作序列、名词清单碎片；判整体 pattern，不追单次
5. **`structured` 字节级不变**。无新增 forbidden_phrases 或 fatigue_words
6. 其他 default / architect 规则完整保留。sources 补充 `proposal:focal-contract-v1`

### `style_rules.new.json`（相对当前线上）

1. **`long.prose` 压缩**：7 条 → 5 条焦点变化契约：选最强感官焦点 / 已建立状态只用一个细节 / 大多数篇幅花在新动作失败/注意力/预期/判断转变或身体后果 / 内部生理可观察才写 / 中句推过程短句标转折；消除全身清单压力与强制动作链公式
2. **`long.taboos` / `long.outline` 保留**，无文字重复需清理
3. **`long.dialogue` 系统规则**：已包含呈现形式 defer 到当前弧
4. **`current` 更新为 v11/a3**：volume 保持 11，arc 改为 3。`current.prose` 从 v11/a2 自动化/排出指令替换为 v11/a3 体腔物质操控/多系统并行/四足姿态下的内压交互指令：
   - 压迫重心从外部装置转向体腔内物质（气体/液体/凝胶）的持续存在与内部位移
   - 多个独立系统同时运行为底色，节奏差异制造体腔干涉区，复合体感优先于逐条盘点
   - 四足姿态下身体移动（爬行/呼吸/姿势微调）与腔体内含物交互作为过程承载
   - 体感以不可控、不固定、无法通过姿势消除的内部触感为主
   - AI指令不超8字，系统变更通过机械声体现
5. **`current.dialogue` / `current.taboos` 更新为 v11/a3 对应约束**：系统通过屏幕文字/声音信号传达；女孩无主动对白（兽化生理反应）；taboos 禁止静态体腔内容物、多系统逐条罗列、运输式四足移动、工程参数替代体感

### 锚点

`style_anchors.json` 不替换。建议后续手动加入 1–2 条已接受的 v11/a3 节选，附真实来源 chapter 和 digest，而非整体替换。

## 审阅建议

1. 打开 `COMPLETE.diff` 查看完整 unified diff（或分别查看 `user_rules.diff` + `style_rules.diff`）
2. 确认 `structured` 段未变更
3. 对比旧 `rule_writer_process_style_v1`（7 项清单）与新 `rule_writer_focal_contract_v1` + `rule_writer_attention_shift_v1`
4. 确认 editor drift rule 新增的跨章 template pattern 判据
5. 确认 `current` 标识 v11/a3，`current.prose` 仅含 v11/a3 体腔操控/多系统/四足内压交互指令，无 generic duplicate

## 注意

- 运行中书以 `workspace/output/novel/meta/user_rules.json` 为权威；手改后重启/下章 `novel_context` 才会吃到
- 本草案仅作手动对照用，**不自动写入 workspace**
- 不新增 `forbidden_phrases` 或 `fatigue_words`；structured 段与线上字节级一致
