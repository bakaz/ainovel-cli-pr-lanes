# 文风修正主线：Rules、Compass 与自审

本轮改动以 v0.6.3 原工作流为基础，不替换 Coordinator 长循环，不新增 Phase/Flow，也不把 Editor 改成逐章评审。

## Rules

`meta/user_rules.json` 是唯一事实源。机械可检字段保持原样且全部可选：`genre`、`chapter_words`、`forbidden_chars`、`forbidden_phrases`、`fatigue_words`。代码内 `SystemDefaults` 始终作为最低优先级生效，不要求用户显式取消。

自然语言规则存为四个简单桶，每条带稳定 ID：

```json
"preferences": {
  "default": [{"id":"rule_x","text":"共同作品定位"}],
  "architect": [{"id":"rule_y","text":"规划偏好"}],
  "writer": [{"id":"rule_z","text":"正文笔法"}],
  "editor": [{"id":"rule_e","text":"审阅尺度"}]
}
```

Agent 实际仍收到原形状 `{structured, preferences}`，其中 preferences 是 Host 选择后拼成的 Markdown：

| Agent | 可见分区 |
|---|---|
| Coordinator | default |
| Architect | default + architect |
| Writer | default + writer |
| Editor | default + writer + editor；writer 只读审计 |

Coordinator 使用 `save_user_rules` 提交受限 patch：add/revise 会调用 Normalizer，scope 可省略让模型分类；remove/reclassify 按 rule_id 定点修改；rebuild 只规范化现有数据。工具不接受整份快照替换。

## Writer / Editor

Writer 仍执行 `novel_context → read_chapter → plan → draft → check_consistency → commit`。没有新增 style_memory 或大上下文包。`check_consistency` 在草稿阶段运行 `rules.Lint + rules.Check`，返回 digest、字数、违规事实与原有一致性数据，不重复返回整章正文，也不强制阻断提交。`commit_chapter` 保留同样的非阻断复核作为保险。

Editor 保持弧级评审。它读取 default、editor 和 Writer Rules，对 Writer Rules 只做是否遵守的审计；原有 style_stats、风格规则和 anti-ai-tone 判据继续生效。

## Compass

仍使用 `meta/compass.json` 与 `planning_memory.compass`：

```json
{
  "long": {
    "ending_direction": "稳定的全书终局方向",
    "open_threads": ["跨卷长线"],
    "estimated_scale": "预计 4-6 卷",
    "last_updated": 12
  },
  "current": {
    "direction": "近期自由方向 Markdown",
    "open_threads": ["当前开放短线"],
    "last_updated": 18
  }
}
```

`current` 整体可选，不含 volume/arc 等重复大纲字段。Architect/Coordinator 读完整 Compass；Editor 弧审只读；Writer 不读。日常滚动规划更新 current。修改 long 必须给 reason，只用于用户改变长期目标或创作出现实质长期变化。`update_compass` 合并字段而非覆盖整份对象；last_updated 由 Host 写入。旧根级 Compass 自动迁入 long。

## Pause Point

原 `save_pause_point` 支持 `chapter / arc / volume / rewrites_drained`。章在目标 commit 后、弧在目标 arc summary 后、卷在目标 volume summary 后触发。只保留一个活动点，新设置覆盖旧值；全书 complete 时消费但不额外暂停。它是 Host 边界策略，不是新状态机。

## Prompt 调试

外置资源优先级：内置 < `~/.ainovel/` < `./.ainovel/` < `--prompts-dir`。启动时加载，坏文件告警回退，并记录 key/path/SHA-256。小改可直接用外置 writer/editor prompt；重大修改继续使用 `ainovel-cli eval --variant ... --repeat 3` 做 baseline/variant A/B。
