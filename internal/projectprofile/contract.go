// Package projectprofile 提供内建 SceneBeat 项目档案识别与契约校验。
//
// Phase 1 提供：
//   - 内建 Core4 和当前项目的 v3 契约定义
//   - Status（core / migration_required / active）
//   - SceneBeatContract 的严格校验 API（供 Phase 2 使用）
//   - 注册表：根据 workspace 指纹（premise hash + chapters/01-34 有序 hash）
//     解析为对应契约；其他项目回落 Core4
//   - 从不从 workspace config/rules/prompt 读取契约
package projectprofile

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// Contract 标识项目应遵循的场景节拍契约版本。
type Contract int

const (
	// ContractCore4 是全局默认：Goal / Action / Conflict / Outcome 四字段。
	// BodyReaction、EmotionReaction、EroticCharge 均为可选（omitempty）。
	// ValidateRequired 只校验四字段。旧 string 格式（legacy）跳过校验。
	ContractCore4 Contract = iota

	// ContractSceneBeatV3 是当前 enrolled 项目的 SceneBeat v3 契约：
	// Goal / Action / Conflict / Outcome / BodyReaction /
	// EmotionReaction / EroticCharge 七个字段在结构化场景中均为必填。
	// 旧 string 格式（legacy）仍然跳过校验。
	ContractSceneBeatV3
)

// String 返回契约的人类可读名称。
func (c Contract) String() string {
	switch c {
	case ContractCore4:
		return "core4"
	case ContractSceneBeatV3:
		return "scene_beat_v3"
	default:
		return fmt.Sprintf("unknown(%d)", c)
	}
}

// ParseContract 从字符串解析契约。
func ParseContract(s string) (Contract, error) {
	switch s {
	case "core4":
		return ContractCore4, nil
	case "scene_beat_v3":
		return ContractSceneBeatV3, nil
	default:
		return ContractCore4, fmt.Errorf("projectprofile: unknown contract %q", s)
	}
}

// sceneBeatV3Fields 是 v3 契约要求的全部七个字段名（内部不可变源）。
// 外部通过 SceneBeatContract.RequiredFields() 获取副本。
var sceneBeatV3Fields = []string{
	"goal",
	"action",
	"conflict",
	"outcome",
	"body_reaction",
	"emotion_reaction",
	"erotic_charge",
}

// ── Fingerprint ──

// Fingerprint 是 workspace 的稳定标识，用于在不依赖 marker 时识别 enrolled 项目。
type Fingerprint struct {
	// PremiseHash 是 premise.md 内容的 SHA256 摘要。
	PremiseHash string `json:"premise_hash"`
	// ChaptersHash 是 chapters/01.md 到 chapters/34.md 按文件名排序后的
	// 内容 SHA256 摘要（用 "\x00" 连接各文件摘要后再取 SHA256）。
	ChaptersHash string `json:"chapters_hash"`
	// CompletedThrough 预期值为 34（chapters/01-34 全部完成）。
	CompletedThrough int `json:"completed_through"`
}

// NewFingerprint 根据 premise 内容和章节内容构建指纹。
// chapters 的 key 为章节文件名（如 "01.md"），value 为文件内容。
func NewFingerprint(premise string, chapters map[string]string) Fingerprint {
	ph := sha256.Sum256([]byte(premise))

	// 收集文件名并排序
	names := make([]string, 0, len(chapters))
	for name := range chapters {
		names = append(names, name)
	}
	sort.Strings(names)

	var chHashes []string
	for _, name := range names {
		h := sha256.Sum256([]byte(chapters[name]))
		chHashes = append(chHashes, hex.EncodeToString(h[:]))
	}
	joined := strings.Join(chHashes, "\x00")
	ch := sha256.Sum256([]byte(joined))

	return Fingerprint{
		PremiseHash:      hex.EncodeToString(ph[:]),
		ChaptersHash:     hex.EncodeToString(ch[:]),
		CompletedThrough: 34,
	}
}

// IsV3Enrolled 判断指纹是否匹配当前 enrolled 项目（v3 契约）。
// 当前 enrolled 指纹硬编码为已知值；其他指纹返回 false（回落 Core4）。
func (f Fingerprint) IsV3Enrolled() bool {
	// 当前项目已确认的认证指纹。
	// 这是 Phase 1 内建的单项目 enrolled 值。
	return f.PremiseHash == v3PremiseHash &&
		f.ChaptersHash == v3ChaptersHash &&
		f.CompletedThrough == 34
}

// ── 内建 enrolled 指纹（不可变常量）──
// Go 支持 typed string const，编译器保证这些值在链接后不可修改。
// 测试不得依赖或修改这些值；通过 Registry.Fingerprinter 注入测试指纹。

const v3PremiseHash = "14942836f0097dc68019c85321ee130def69d89a603863c470214f6d194b18cb"
const v3ChaptersHash = "30c4812865ba3f1e5227c7a1af55933ff070e5af58cc959c1eff639ca2f07c93"

// V3EnrolledFingerprint 返回当前生产环境的 enrolled 不可变 Fingerprint。
// 每次调用构造新值，但值本身由 const 保证不变。
// 测试通过 Registry.Fingerprinter 注入模拟指纹来验证 enrolled 路径。
func V3EnrolledFingerprint() Fingerprint {
	return Fingerprint{
		PremiseHash:      v3PremiseHash,
		ChaptersHash:     v3ChaptersHash,
		CompletedThrough: 34,
	}
}
