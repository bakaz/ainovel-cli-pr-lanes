---
# 项目内置默认规则。Phase 1 保持极轻，不迁移 writer.md / editor.md 里现有的偏好条款；
# 待后续阶段再讨论 prompt 瘦身。
#
# 支持的 front matter 字段（Phase 1）：
#   genre              — 题材；非空时触发加载 assets/rules/genres/<genre>.md
#   chapter_words      — 章节字数范围，格式 "min-max"
#   forbidden_chars    — 字符/符号黑名单（出现 ≥1 次即 error）
#   forbidden_phrases  — 短语黑名单（出现 ≥1 次即 error）
#   fatigue_words      — 疲劳词 map：词→每章阈值（超过才 warning）；也兼容 list 形式（默认阈值 1）
---
