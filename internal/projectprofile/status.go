package projectprofile

import "fmt"

// Status 表示项目档案的当前状态。
type Status int

const (
	// StatusCore 是 Core4 契约项目：无迁移需求。
	StatusCore Status = iota

	// StatusMigrationRequired 表示 enrolled v3 项目尚需迁移
	// 才能达到 v3 契约要求。此时所有状态写入/Provider 调用均被阻塞，
	// 仅允许只读诊断和迁移草案生成。
	StatusMigrationRequired

	// StatusActive 表示 enrolled v3 项目已完成迁移，
	// marker 已写入且校验通过。
	StatusActive
)

// String 返回状态的可读名称。
func (s Status) String() string {
	switch s {
	case StatusCore:
		return "core"
	case StatusMigrationRequired:
		return "migration_required"
	case StatusActive:
		return "active"
	default:
		return fmt.Sprintf("status(%d)", s)
	}
}

// ParseStatus 从字符串解析状态。
func ParseStatus(s string) (Status, error) {
	switch s {
	case "core":
		return StatusCore, nil
	case "migration_required":
		return StatusMigrationRequired, nil
	case "active":
		return StatusActive, nil
	default:
		return StatusCore, fmt.Errorf("projectprofile: unknown status %q", s)
	}
}
