package projectprofile

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// MarkerLoader 是注册表读取 marker 的抽象接口。
type MarkerLoader func() (*ProfileMarker, error)

// Fingerprinter 是注册表计算指纹的抽象接口。
type Fingerprinter func() (Fingerprint, error)

// Registry 是项目档案注册表，负责：
//   - 根据 workspace 存储状态解析 Contract
//   - 优先使用 marker（meta/project_profile.json），
//     不存在时回退至指纹识别
//   - marker 存在时始终校验 fingerprint——已 enrolled 项目绝不允许 Core4 marker
//   - 空/损坏/未知 profile 必须 fail closed（绝不回落 Core4）
//   - marker 不得包含 policy/required-field 等可降级开关
type Registry struct {
	loadMarker       MarkerLoader
	fingerprinter    Fingerprinter
	expectedEnrolled *Fingerprint // 非 nil 时替换 IsV3Enrolled 的内建常量检查
}

// NewRegistry 创建注册表。
// expectedEnrolled 可选：为 nil 时使用内建 v3 const 常量进行 enrolled 判定；
// 非 nil 时 fingerprint 匹配此值即视为已 enrolled（用于测试真实磁盘路径）。
func NewRegistry(loadMarker MarkerLoader, fingerprinter Fingerprinter, expectedEnrolled ...*Fingerprint) *Registry {
	r := &Registry{
		loadMarker:    loadMarker,
		fingerprinter: fingerprinter,
	}
	if len(expectedEnrolled) > 0 {
		r.expectedEnrolled = expectedEnrolled[0]
	}
	return r
}

// ResolvedProfile 是注册表解析的完整结果。
type ResolvedProfile struct {
	Contract Contract `json:"contract"`
	Status   Status   `json:"status"`
}

// Resolve 解析当前 workspace 的契约和状态。
// 流程：
//  1. 尝试读取 marker
//  2. 存在 marker → 解析 marker，同时计算 fingerprint 做降级检测
//  3. 无 marker → fingerprint 识别
//  4. 如果有 fingerprint 证明已 enrolled，任何 Core4 marker 均拒绝
func (r *Registry) Resolve() (ResolvedProfile, error) {
	// 计算 fingerprint（用于降级检测和无 marker 识别）
	var fp Fingerprint
	hasFP := false
	if r.fingerprinter != nil {
		var err error
		fp, err = r.fingerprinter()
		if err != nil {
			return ResolvedProfile{}, fmt.Errorf("projectprofile: fingerprint error: %w", err)
		}
		hasFP = true
	}

	// 1. 尝试读取 marker
	if r.loadMarker != nil {
		marker, err := r.loadMarker()
		if err != nil {
			return ResolvedProfile{}, fmt.Errorf("projectprofile: cannot read marker: %w", err)
		}
		if marker != nil {
			return r.resolveMarker(*marker, fp, hasFP)
		}
	}

	// 2. 无 marker → 指纹识别
	return r.resolveFingerprint(fp, hasFP)
}

// resolveMarker 处理已存在的 marker。
// Core4 marker 一律拒绝——无 marker 才是普通项目的正确途径。
// marker 存在时，如果 fingerprint 证明已 enrolled，拒绝任何非 v3 降级。
func (r *Registry) resolveMarker(profile ProfileMarker, fp Fingerprint, hasFP bool) (ResolvedProfile, error) {
	if profile.Version != ProfileVersion {
		return ResolvedProfile{}, fmt.Errorf(
			"projectprofile: marker version mismatch: got %q, want %q",
			profile.Version, ProfileVersion)
	}

	contract, err := ParseContract(profile.Contract)
	if err != nil {
		return ResolvedProfile{}, fmt.Errorf("projectprofile: invalid marker contract: %w", err)
	}

	status, err := ParseStatus(profile.Status)
	if err != nil {
		return ResolvedProfile{}, fmt.Errorf("projectprofile: invalid marker status: %w", err)
	}

	// Core4 marker 一律拒绝——无 marker 才是 Core4 项目的正确途径。
	// marker 的存在意味着 v3 enrollment 意图或已迁移结果。
	if contract == ContractCore4 {
		return ResolvedProfile{}, fmt.Errorf(
			"projectprofile: Core4 marker is not allowed (marker implies v3 enrollment; use no marker for Core4 projects)")
	}

	// 已 enrolled 但 marker 不是 scene_beat_v3 → 降级拒绝
	if hasFP && r.isEnrolled(fp) && contract != ContractSceneBeatV3 {
		return ResolvedProfile{}, fmt.Errorf(
			"projectprofile: enrolled project cannot downgrade to contract %q", profile.Contract)
	}

	// Active markers are accepted only as complete Phase 3 audit receipts. The
	// marker must bind the reviewed manifest and the enrolled source fingerprint
	// to the exact canonical source currently on disk.
	if status == StatusActive {
		if contract != ContractSceneBeatV3 || !hasFP || !r.isEnrolled(fp) {
			return ResolvedProfile{}, fmt.Errorf("projectprofile: active marker requires the enrolled scene_beat_v3 source")
		}
		if profile.ProfileID != SceneBeatV3ProfileID || profile.EnrollmentFingerprint == nil || !sameFingerprint(*profile.EnrollmentFingerprint, fp) {
			return ResolvedProfile{}, fmt.Errorf("projectprofile: active marker fingerprint/profile audit mismatch")
		}
		if !manifestSHA256Pattern.MatchString(profile.ApprovedManifestSHA256) {
			return ResolvedProfile{}, fmt.Errorf("projectprofile: active marker requires a lowercase approved manifest SHA-256")
		}
		return ResolvedProfile{Contract: contract, Status: status}, nil
	}

	// StatusMigrationRequired 必须搭配 ContractSceneBeatV3
	if status == StatusMigrationRequired && contract != ContractSceneBeatV3 {
		return ResolvedProfile{}, fmt.Errorf(
			"projectprofile: marker status %q with contract %q is invalid: migration_required requires scene_beat_v3",
			profile.Status, profile.Contract)
	}

	// StatusCore 与 scene_beat_v3 不兼容
	if status == StatusCore && contract == ContractSceneBeatV3 {
		return ResolvedProfile{}, fmt.Errorf(
			"projectprofile: marker status 'core' with contract 'scene_beat_v3' is invalid")
	}

	return ResolvedProfile{
		Contract: contract,
		Status:   status,
	}, nil
}

func sameFingerprint(a, b Fingerprint) bool {
	return a.PremiseHash == b.PremiseHash && a.ChaptersHash == b.ChaptersHash && a.CompletedThrough == b.CompletedThrough
}

// isEnrolled 判断指纹是否匹配 enrolled 项目。
// 优先使用注入的 expectedEnrolled（测试时可通过此路径验证真实的磁盘指纹），
// 未注入时回退至内建 IsV3Enrolled const 常量。
func (r *Registry) isEnrolled(fp Fingerprint) bool {
	if r.expectedEnrolled != nil {
		return fp.PremiseHash == r.expectedEnrolled.PremiseHash &&
			fp.ChaptersHash == r.expectedEnrolled.ChaptersHash &&
			fp.CompletedThrough == r.expectedEnrolled.CompletedThrough
	}
	return fp.IsV3Enrolled()
}

// resolveFingerprint 无 marker 时通过指纹识别。
func (r *Registry) resolveFingerprint(fp Fingerprint, hasFP bool) (ResolvedProfile, error) {
	if !hasFP {
		return ResolvedProfile{Contract: ContractCore4, Status: StatusCore}, nil
	}

	if r.isEnrolled(fp) {
		return ResolvedProfile{
			Contract: ContractSceneBeatV3,
			Status:   StatusMigrationRequired,
		}, nil
	}

	return ResolvedProfile{
		Contract: ContractCore4,
		Status:   StatusCore,
	}, nil
}

// NewStoreFingerprinter 基于 store 目录创建指纹计算器。
// 不直接引用 store 包避免循环依赖；调用方从 store.Dir() 传入输出目录。
func NewStoreFingerprinter(outputDir string) Fingerprinter {
	return func() (Fingerprint, error) {
		return computeFingerprintFromDir(outputDir)
	}
}

// computeFingerprintFromDir 从 workspace 输出目录计算指纹。
func computeFingerprintFromDir(dir string) (Fingerprint, error) {
	premisePath := filepath.Join(dir, "premise.md")
	premiseData, err := os.ReadFile(premisePath)
	if err != nil {
		if os.IsNotExist(err) {
			return Fingerprint{CompletedThrough: 0}, nil
		}
		return Fingerprint{}, fmt.Errorf("read premise: %w", err)
	}
	premise := strings.TrimSpace(string(premiseData))

	chapters := make(map[string]string)
	for i := 1; i <= 34; i++ {
		path := filepath.Join(dir, fmt.Sprintf("chapters/%02d.md", i))
		data, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return Fingerprint{}, fmt.Errorf("read chapter %02d: %w", i, err)
		}
		chapters[fmt.Sprintf("%02d.md", i)] = strings.TrimSpace(string(data))
	}

	// 要求 01-34 全部存在
	if len(chapters) < 34 {
		return Fingerprint{CompletedThrough: len(chapters)}, nil
	}

	return NewFingerprint(premise, chapters), nil
}

// ── File helpers ──

// listChapterFiles 列出 chapters/ 目录下已完成的章节文件并排序。
func listChapterFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(dir, "chapters"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)
	return files, nil
}
