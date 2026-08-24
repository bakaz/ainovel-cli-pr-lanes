package tools

import "testing"

func TestProgressLogHint(t *testing.T) {
	tests := []struct {
		name    string
		field   string
		value   string
		wantHit bool
	}{
		// 命中：历史上被误提交的进度日志形态
		{"阶段后缀", "status.密室14第一枚阀植入完成阶段", "第一枚乳导管阀植入完成", true},
		{"完成结尾", "status.夜班第三班笑腔之夜完成", "第三班完成", true},
		{"落地结尾", "status.弧3锯影落地", "锯齿咬进左腿凉线正中", true},
		{"首轮字样", "status.密室15自由时段首轮完成", "授权牌全亮", true},
		{"第N班字样", "status.夜班第四班粗嗓之夜完成", "夜里报感受规矩落地", true},
		{"值以完成叙述开头", "status.某装置", "收束：四路接口同亮", true},
		// 不命中：合法的持续状态
		{"永久装置", "status.驯顺因果", "已内化挣扎只会让黑暗更长因果", false},
		{"心理状态", "status.默认姿态", "默认屈从姿态已成形", false},
		{"无进度词", "status.阀门方向", "四路阀门接线方向确认", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := progressLogHint(tt.field, tt.value)
			if (got != "") != tt.wantHit {
				t.Fatalf("progressLogHint(%q, %q) hit=%v want %v (hint=%q)", tt.field, tt.value, got != "", tt.wantHit, got)
			}
		})
	}
}

// TestPreflightRejectsProgressLogStatus 验证 preflightCommitArgs 对进度日志式
// status.* 新增条目的拦截（需要完整 store fixture，此处验证纯函数层；
// 端到端拦截由 TestPreflightCommitArgs_* 系列覆盖）。
func TestProgressLogHintDoesNotTouchOtherNamespaces(t *testing.T) {
	// 非 status.* 前缀的字段即使含进度词也不归本启发式管（由调用方前缀判断）
	if got := progressLogHint("body_device.双腿缺失", "左腿锯断烙封"); got != "" {
		t.Fatalf("body_device 字段不应被标记, got %q", got)
	}
}
