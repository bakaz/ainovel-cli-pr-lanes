package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/voocel/ainovel-cli/assets"
	"github.com/voocel/ainovel-cli/internal/bootstrap"
	"github.com/voocel/ainovel-cli/internal/projectprofile"
	storepkg "github.com/voocel/ainovel-cli/internal/store"
	"github.com/voocel/ainovel-cli/internal/tools"
)

// ── Sentinel errors ──

// ErrMigrationRequired 是 migration_required 状态下阻止所有可变操作的
// typed sentinel error。调用者据此向用户显示迁移提示。
var ErrMigrationRequired = errors.New("migration required: this workspace needs SceneBeat v3 migration before writing or generating content")

// IsMigrationRequired 检查 error 是否为 ErrMigrationRequired。
func IsMigrationRequired(err error) bool {
	return errors.Is(err, ErrMigrationRequired)
}

// ── Early profile resolution (before any store/model/background init) ──

// resolveProjectProfileEarly 在 Host.New 的最早期解析项目档案。
// 使用直接从磁盘读取的方式，不依赖 store.Init / RunMeta / model / usage。
// migration-required 时返回 ErrMigrationRequired 阻止全量初始化。
//
// enrolledOverride 可选：为 nil 时使用内建 const 常量进行 enrolled 判定；
// 非 nil 时指纹匹配此值即视为已 enrolled（测试注入预期值验证真实磁盘路径）。
func resolveProjectProfileEarly(outputDir string, enrolledOverride ...*projectprofile.Fingerprint) (projectprofile.ResolvedProfile, error) {
	loadMarker := func() (*projectprofile.ProfileMarker, error) {
		return readProfileMarkerFromDisk(outputDir)
	}

	fp := projectprofile.NewStoreFingerprinter(outputDir)

	var expected *projectprofile.Fingerprint
	if len(enrolledOverride) > 0 {
		expected = enrolledOverride[0]
	}

	reg := projectprofile.NewRegistry(loadMarker, fp, expected)
	profile, err := reg.Resolve()
	if err != nil {
		return projectprofile.ResolvedProfile{}, fmt.Errorf("resolve project profile: %w", err)
	}

	// 如果是 migration_required，直接返回 sentinel error 阻止后续 init。
	if profile.Status == projectprofile.StatusMigrationRequired {
		return profile, fmt.Errorf("%w: contract=%s", ErrMigrationRequired, profile.Contract)
	}

	return profile, nil
}

// readProfileMarkerFromDisk 直接从磁盘读取 project_profile.json，无需 store 初始化。
// 零字节 marker → error（拒绝而非视为缺失）。
// 每个 JSON 对象解码后验证 EOF，拒绝尾随的第二文档。
func readProfileMarkerFromDisk(outputDir string) (*projectprofile.ProfileMarker, error) {
	path := filepath.Join(outputDir, "meta/project_profile.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read project_profile.json: %w", err)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("project_profile.json is empty (zero bytes)")
	}

	var raw struct {
		Version               string `json:"version"`
		Contract              string `json:"contract"`
		Status                string `json:"status"`
		ProfileID             string `json:"profile_id,omitempty"`
		EnrollmentFingerprint *struct {
			PremiseHash      string `json:"premise_hash"`
			ChaptersHash     string `json:"chapters_hash"`
			CompletedThrough int    `json:"completed_through"`
		} `json:"enrollment_fingerprint,omitempty"`
		ApprovedManifestSHA256 string `json:"approved_manifest_sha256,omitempty"`
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("parse project_profile.json: %w", err)
	}
	// 第二次 Decode 必须返回 io.EOF——拒绝所有非 EOF 的残余内容：
	// garbage, 孤立 ]/}, 第二个 JSON 文档。
	var trailing json.RawMessage
	if err := dec.Decode(&trailing); err == nil {
		return nil, fmt.Errorf("project_profile.json: trailing JSON document after first object")
	} else if !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("project_profile.json: unexpected content after first JSON object: %v", err)
	}

	if raw.Version == "" {
		return nil, fmt.Errorf("project_profile.json: missing version")
	}

	var fp *projectprofile.Fingerprint
	if raw.EnrollmentFingerprint != nil {
		fp = &projectprofile.Fingerprint{
			PremiseHash:      raw.EnrollmentFingerprint.PremiseHash,
			ChaptersHash:     raw.EnrollmentFingerprint.ChaptersHash,
			CompletedThrough: raw.EnrollmentFingerprint.CompletedThrough,
		}
	}

	return &projectprofile.ProfileMarker{
		Version:                raw.Version,
		Contract:               raw.Contract,
		Status:                 raw.Status,
		ProfileID:              raw.ProfileID,
		EnrollmentFingerprint:  fp,
		ApprovedManifestSHA256: raw.ApprovedManifestSHA256,
	}, nil
}

// ── Profile resolution (after store init, for non-migration-required paths) ──

// resolveProjectProfile 解析当前 workspace 的项目档案（store 已初始化后）。
func resolveProjectProfile(store *storepkg.Store) (projectprofile.ResolvedProfile, error) {
	loadMarker := func() (*projectprofile.ProfileMarker, error) {
		raw, err := store.ProjectProfile.LoadRaw()
		if err != nil {
			return nil, err
		}
		if raw == nil {
			return nil, nil
		}
		fp := raw.EnrollmentFingerprint
		var fingerprint *projectprofile.Fingerprint
		if fp != nil {
			fingerprint = &projectprofile.Fingerprint{
				PremiseHash:      fp.PremiseHash,
				ChaptersHash:     fp.ChaptersHash,
				CompletedThrough: fp.CompletedThrough,
			}
		}
		return &projectprofile.ProfileMarker{
			Version:                raw.Version,
			Contract:               raw.Contract,
			Status:                 raw.Status,
			ProfileID:              raw.ProfileID,
			EnrollmentFingerprint:  fingerprint,
			ApprovedManifestSHA256: raw.ApprovedManifestSHA256,
		}, nil
	}

	reg := projectprofile.NewRegistry(
		loadMarker,
		projectprofile.NewStoreFingerprinter(store.Dir()),
	)
	return reg.Resolve()
}

// ── Migration gate ──

// checkMigrationGate 是统一迁移门：migration_required 状态下返回
// ErrMigrationRequired，阻止所有 Provider 请求和状态写入。
// 允许只读诊断。
func (h *Host) checkMigrationGate() error {
	h.mu.Lock()
	profile := h.profile
	h.mu.Unlock()

	if profile.Status == projectprofile.StatusMigrationRequired {
		return fmt.Errorf("%w: contract=%s", ErrMigrationRequired, profile.Contract)
	}
	return nil
}

// requireProfileResolved 确保 profile 已解析；未解析时尝试解析。
// profile 字段在 Host 构造时解析一次后不可变。
func (h *Host) requireProfileResolved() projectprofile.ResolvedProfile {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.profile
}

// ── 只读诊断 Host（migration_required 时返回）──

// newDiagnosticHost 构造一个 migration_required 状态的受限只读 Host。
// 适用于 TUI/诊断场景，支持读取已有文件、展示 Snapshot、显示诊断信息。
// 所有创作/写入/Provider 方法受 checkMigrationGate 保护并返回 ErrMigrationRequired。
// 不初始化 Store.Init / RunMeta / model / worker / usage background / engine。
func newDiagnosticHost(cfg bootstrap.Config, bundle assets.Bundle, outputDir string, profile projectprofile.ResolvedProfile) *Host {
	store := storepkg.NewStore(outputDir) // 只读 store，不调用 Init()
	usage := NewUsageTracker(nil, store)  // nil modelSet → Totals() 等返回零值
	h := &Host{
		cfg:            cfg,
		bundle:         bundle,
		store:          store,
		profile:        profile,
		diagnosticOnly: true,
		usage:          usage,
		askUser:        tools.NewAskUserTool(), // 无副作用 AskUser（Execute 在 handler 未设置时自动降级）
		events:         make(chan Event, 100),
		streamCh:       make(chan string, 256),
		done:           make(chan struct{}, 4),
		lifecycle:      lifecycleIdle,
	}
	h.runCtx, h.runCancel = context.WithCancel(context.Background())
	h.observer = newObserver(store, h.emitEvent, h.emitDelta, h.emitClear)
	return h
}
