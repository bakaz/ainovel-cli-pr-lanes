# 完整草案使用说明

**未写入**线上 `workspace/output/novel/meta/`。你确认后手动复制覆盖。

## 文件

| 文件 | 说明 |
|------|------|
| `user_rules.new.json` | 完整 user_rules（structured 字节级不变；writer 焦点契约 + 去重 + 新增注意力规则；editor 合并 + 跨章 pattern 判据；default v3 修改） |
| `style_rules.new.json` | 完整 style_rules（long.prose 5 条焦点契约；current 标识 v11/a3，含 5 条体腔操控/多系统/四足内压交互指令） |
| `user_rules.diff` | 相对当前 `workspace/output/novel/meta/user_rules.json` 的 Python difflib unified diff（UTF-8） |
| `style_rules.diff` | 相对当前 `workspace/output/novel/meta/style_rules.json` 的 Python difflib unified diff（UTF-8） |
| `COMPLETE.diff` | 上述两个 diff 合并 |

`style_anchors.json` 本轮不替换；当前锚点保留。

## 对照命令

```powershell
cd G:\opencode\ainovel-cli_0.6.3_Windows_x86_64
# 查看完整统一 diff（推荐）：
code .pr-drafts\style-align-manual\COMPLETE.diff
# 或分别查看：
git diff --no-index -- workspace/output/novel/meta/user_rules.json .pr-drafts/style-align-manual/user_rules.new.json
git diff --no-index -- workspace/output/novel/meta/style_rules.json .pr-drafts/style-align-manual/style_rules.new.json
```

## 应用（确认后）

```powershell
# 建议先备份：
Copy-Item workspace\output\novel\meta\user_rules.json workspace\output\novel\meta\user_rules.json.bak
Copy-Item workspace\output\novel\meta\style_rules.json workspace\output\novel\meta\style_rules.json.bak

# 覆盖：
Copy-Item .pr-drafts\style-align-manual\user_rules.new.json workspace\output\novel\meta\user_rules.json -Force
Copy-Item .pr-drafts\style-align-manual\style_rules.new.json workspace\output\novel\meta\style_rules.json -Force
```

重启或下章 `novel_context` 后生效。

## 变更摘要

### `user_rules.new.json` → `user_rules.json`

| 变更 | 说明 |
|------|------|
| structured | 未改（字节级不变，无新增 forbidden/fatigue） |
| default | `rule_erotic_sensory_priority_v3` 改造（全身清单 → 焦点+至多一个回响）；其余保留 |
| architect | 全部保留，未改 |
| writer | `rule_writer_process_style_v1` → `rule_writer_focal_contract_v1`；新增 `rule_writer_attention_shift_v1`；`rule_050d45a65912` / `rule_d6e4f8a2c0b1` / `rule_writer_erotic_fulfillment` 简化（移除动作链公式）；`rule_style_drift_guard_v2` 去重（2→1） |
| editor | `rule_editor_style_drift_guard_v2` 去重（2→1）并追加跨章 template pattern 判据；其余保留 |
| sources | 补充 `proposal:focal-contract-v1` |

### `style_rules.new.json` → `style_rules.json`

| 变更 | 说明 |
|------|------|
| long.prose | 7→5 条，焦点变化契约（消除全身清单压力与强制动作链公式） |
| long.taboos | 保留 |
| long.outline | 保留 |
| long.dialogue | 已包含呈现形式 defer 到当前弧 |
| current | volume 11, arc: 2→3；prose 全部替换为 v11/a3 体腔物质操控/多系统并行/四足内压交互指令（5条）；dialogue 更新为屏幕/声音信号约束；taboos 更新为静态体腔/多系统罗列/运输式移动/工程参数禁令 |

## 审阅要点

1. 确认 `structured`（forbidden_phrases / fatigue_words）字节级未变
2. 焦点契约是否覆盖了因果链/枚举/临床全知/名词清单四类风险
3. editor drift rule 的跨章 pattern 判据是否可操作
4. `current` 是否标识 v11/a3，prose/dialogue/taboos 是否与体腔操控/多系统并行/四足内压交互一致
