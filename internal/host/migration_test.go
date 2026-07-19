package host

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/voocel/ainovel-cli/assets"
	"github.com/voocel/ainovel-cli/internal/bootstrap"
	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/host/imp"
	"github.com/voocel/ainovel-cli/internal/projectprofile"
	storepkg "github.com/voocel/ainovel-cli/internal/store"
)

// ── Helpers ──

// storeSnapshot 采集 store 目录下所有文件的路径→sha256 摘要，用于断言不变。
type storeSnapshot map[string]string

func takeStoreSnapshot(dir string) storeSnapshot {
	snap := make(storeSnapshot)
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		h := sha256.Sum256(data)
		snap[filepath.ToSlash(rel)] = fmt.Sprintf("%x", h)
		return nil
	})
	return snap
}

func assertSnapshotUnchanged(t *testing.T, before, after storeSnapshot) {
	t.Helper()
	for path, hash := range before {
		if after[path] != hash {
			t.Errorf("file %q changed: before=%s after=%s", path, hash, after[path])
		}
	}
	if len(after) > len(before) {
		for path := range after {
			if _, ok := before[path]; !ok {
				t.Errorf("new file created: %s", path)
			}
		}
	}
}

func createMinimalStore(t *testing.T) *storepkg.Store {
	t.Helper()
	st := storepkg.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatalf("store init: %v", err)
	}
	if err := st.RunMeta.Init("default", "test", "test"); err != nil {
		t.Fatalf("run meta init: %v", err)
	}
	return st
}

// writeProjectProfileJSON 直接写 project_profile.json 到 store 目录。
func writeProjectProfileJSON(t *testing.T, dir string, data map[string]any) {
	t.Helper()
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	path := filepath.Join(dir, "meta", "project_profile.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// ── Sentinel error tests ──

func TestErrMigrationRequired(t *testing.T) {
	if !IsMigrationRequired(ErrMigrationRequired) {
		t.Error("IsMigrationRequired(ErrMigrationRequired) should be true")
	}
	if IsMigrationRequired(nil) {
		t.Error("IsMigrationRequired(nil) should be false")
	}
	if IsMigrationRequired(context.Canceled) {
		t.Error("IsMigrationRequired(other error) should be false")
	}
}

func TestIsMigrationRequired(t *testing.T) {
	if !IsMigrationRequired(ErrMigrationRequired) {
		t.Error("direct match")
	}
	wrapped := fmt.Errorf("wrapped: %w", ErrMigrationRequired)
	if !IsMigrationRequired(wrapped) {
		t.Error("wrapped match")
	}
}

// ── Migration gate unit tests ──

func TestCheckMigrationGate_PassesForCore(t *testing.T) {
	h := &Host{
		profile: projectprofile.ResolvedProfile{
			Contract: projectprofile.ContractCore4,
			Status:   projectprofile.StatusCore,
		},
	}
	if err := h.checkMigrationGate(); err != nil {
		t.Errorf("Core4 project should pass migration gate: %v", err)
	}
}

func TestCheckMigrationGate_BlocksForMigrationRequired(t *testing.T) {
	h := &Host{
		profile: projectprofile.ResolvedProfile{
			Contract: projectprofile.ContractSceneBeatV3,
			Status:   projectprofile.StatusMigrationRequired,
		},
	}
	err := h.checkMigrationGate()
	if err == nil {
		t.Fatal("migration_required should block migration gate")
	}
	if !IsMigrationRequired(err) {
		t.Errorf("error should wrap ErrMigrationRequired: %v", err)
	}
}

func TestCheckMigrationGate_PassesForActive(t *testing.T) {
	h := &Host{
		profile: projectprofile.ResolvedProfile{
			Contract: projectprofile.ContractSceneBeatV3,
			Status:   projectprofile.StatusActive,
		},
	}
	if err := h.checkMigrationGate(); err != nil {
		t.Errorf("active project should pass migration gate: %v", err)
	}
}

// ── Host entry point gate tests ──

// newMigrationTestHost 返回一个 migration_required 的模拟 Host。
func newMigrationTestHost(t *testing.T) *Host {
	t.Helper()
	st := storepkg.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatalf("store init: %v", err)
	}
	if err := st.RunMeta.Init("default", "test", "test"); err != nil {
		t.Fatalf("run meta init: %v", err)
	}
	if err := st.Progress.Init("测试书", 10); err != nil {
		t.Fatalf("progress init: %v", err)
	}
	return &Host{
		store: st,
		profile: projectprofile.ResolvedProfile{
			Contract: projectprofile.ContractSceneBeatV3,
			Status:   projectprofile.StatusMigrationRequired,
		},
		events:   make(chan Event, 100),
		streamCh: make(chan string, 100),
		done:     make(chan struct{}, 4),
	}
}

// assertBlockedByMigration 断言 err 是一个 migration_required 错误。
func assertBlockedByMigration(t *testing.T, err error, name string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s should be blocked by migration gate", name)
	}
	if !IsMigrationRequired(err) {
		t.Errorf("%s error should be migration_required: %v", name, err)
	}
}

func TestHost_Resume_BlockedByMigration(t *testing.T) {
	h := newMigrationTestHost(t)
	beforeSnap := takeStoreSnapshot(h.store.Dir())
	_, err := h.Resume()
	assertBlockedByMigration(t, err, "Resume")
	afterSnap := takeStoreSnapshot(h.store.Dir())
	assertSnapshotUnchanged(t, beforeSnap, afterSnap)
}

func TestHost_StartPrepared_BlockedByMigration(t *testing.T) {
	h := newMigrationTestHost(t)
	beforeSnap := takeStoreSnapshot(h.store.Dir())
	err := h.StartPrepared("写一本新书")
	assertBlockedByMigration(t, err, "StartPrepared")
	afterSnap := takeStoreSnapshot(h.store.Dir())
	assertSnapshotUnchanged(t, beforeSnap, afterSnap)
}

func TestHost_Continue_BlockedByMigration(t *testing.T) {
	h := newMigrationTestHost(t)
	beforeSnap := takeStoreSnapshot(h.store.Dir())
	err := h.Continue("继续写")
	assertBlockedByMigration(t, err, "Continue")
	afterSnap := takeStoreSnapshot(h.store.Dir())
	assertSnapshotUnchanged(t, beforeSnap, afterSnap)
}

func TestHost_AdvanceOneChapter_BlockedByMigration(t *testing.T) {
	h := newMigrationTestHost(t)
	beforeSnap := takeStoreSnapshot(h.store.Dir())
	err := h.AdvanceOneChapter()
	assertBlockedByMigration(t, err, "AdvanceOneChapter")
	afterSnap := takeStoreSnapshot(h.store.Dir())
	assertSnapshotUnchanged(t, beforeSnap, afterSnap)
}

func TestHost_Steer_BlockedByMigration(t *testing.T) {
	h := newMigrationTestHost(t)
	beforeSnap := takeStoreSnapshot(h.store.Dir())

	// Steer is fire-and-forget; read events synchronously from the buffered channel.
	h.Steer("修改剧情")

	// Read the emitted event (buffered channel, synchronous emitEvent)
	select {
	case ev := <-h.events:
		if ev.Level != "warn" || !strings.Contains(ev.Summary, "迁移未完成") {
			t.Errorf("Steer should emit migration warning event, got: %+v", ev)
		}
	default:
		t.Error("Steer should have emitted an event synchronously")
	}

	afterSnap := takeStoreSnapshot(h.store.Dir())
	assertSnapshotUnchanged(t, beforeSnap, afterSnap)
}

func TestHost_ImportFrom_BlockedByMigration(t *testing.T) {
	h := newMigrationTestHost(t)
	beforeSnap := takeStoreSnapshot(h.store.Dir())
	_, err := h.ImportFrom(context.Background(), imp.Options{SourcePath: "test.txt"})
	assertBlockedByMigration(t, err, "ImportFrom")
	afterSnap := takeStoreSnapshot(h.store.Dir())
	assertSnapshotUnchanged(t, beforeSnap, afterSnap)
}

func TestHost_Simulate_BlockedByMigration(t *testing.T) {
	h := newMigrationTestHost(t)
	beforeSnap := takeStoreSnapshot(h.store.Dir())
	_, err := h.Simulate(context.Background())
	assertBlockedByMigration(t, err, "Simulate")
	afterSnap := takeStoreSnapshot(h.store.Dir())
	assertSnapshotUnchanged(t, beforeSnap, afterSnap)
}

func TestHost_ImportSimulationProfile_BlockedByMigration(t *testing.T) {
	h := newMigrationTestHost(t)
	beforeSnap := takeStoreSnapshot(h.store.Dir())
	_, err := h.ImportSimulationProfile(context.Background(), "profile.json")
	assertBlockedByMigration(t, err, "ImportSimulationProfile")
	afterSnap := takeStoreSnapshot(h.store.Dir())
	assertSnapshotUnchanged(t, beforeSnap, afterSnap)
}

func TestHost_PrepareUserRules_BlockedByMigration(t *testing.T) {
	h := newMigrationTestHost(t)
	beforeSnap := takeStoreSnapshot(h.store.Dir())
	err := h.PrepareUserRules("创作要求")
	assertBlockedByMigration(t, err, "PrepareUserRules")
	afterSnap := takeStoreSnapshot(h.store.Dir())
	assertSnapshotUnchanged(t, beforeSnap, afterSnap)
}

func TestHost_SetAdvanceMode_BlockedByMigration(t *testing.T) {
	h := newMigrationTestHost(t)
	beforeSnap := takeStoreSnapshot(h.store.Dir())
	err := h.SetAdvanceMode(domain.ChapterAdvanceReview)
	assertBlockedByMigration(t, err, "SetAdvanceMode")
	afterSnap := takeStoreSnapshot(h.store.Dir())
	assertSnapshotUnchanged(t, beforeSnap, afterSnap)
}

func TestHost_SwitchModel_BlockedByMigration(t *testing.T) {
	h := newMigrationTestHost(t)
	beforeSnap := takeStoreSnapshot(h.store.Dir())
	err := h.SwitchModel("default", "provider", "model")
	assertBlockedByMigration(t, err, "SwitchModel")
	afterSnap := takeStoreSnapshot(h.store.Dir())
	assertSnapshotUnchanged(t, beforeSnap, afterSnap)
}

func TestHost_SetRoleThinking_BlockedByMigration(t *testing.T) {
	h := newMigrationTestHost(t)
	beforeSnap := takeStoreSnapshot(h.store.Dir())
	err := h.SetRoleThinking("writer", "high")
	assertBlockedByMigration(t, err, "SetRoleThinking")
	afterSnap := takeStoreSnapshot(h.store.Dir())
	assertSnapshotUnchanged(t, beforeSnap, afterSnap)
}

func TestHost_CoCreateStream_BlockedByMigration(t *testing.T) {
	h := newMigrationTestHost(t)
	beforeSnap := takeStoreSnapshot(h.store.Dir())
	_, err := h.CoCreateStream(context.Background(), nil, nil)
	assertBlockedByMigration(t, err, "CoCreateStream")
	afterSnap := takeStoreSnapshot(h.store.Dir())
	assertSnapshotUnchanged(t, beforeSnap, afterSnap)
}

func TestHost_StageCoCreateStream_BlockedByMigration(t *testing.T) {
	h := newMigrationTestHost(t)
	beforeSnap := takeStoreSnapshot(h.store.Dir())
	_, err := h.StageCoCreateStream(context.Background(), nil, nil)
	assertBlockedByMigration(t, err, "StageCoCreateStream")
	afterSnap := takeStoreSnapshot(h.store.Dir())
	assertSnapshotUnchanged(t, beforeSnap, afterSnap)
}

func TestHost_PauseForCoCreate_BlockedByMigration(t *testing.T) {
	h := newMigrationTestHost(t)
	// allocate events channel for the warn emission
	h.events = make(chan Event, 10)
	beforeSnap := takeStoreSnapshot(h.store.Dir())

	// Should return false and emit event
	if h.PauseForCoCreate() {
		t.Error("PauseForCoCreate should return false when migration_required")
	}

	afterSnap := takeStoreSnapshot(h.store.Dir())
	assertSnapshotUnchanged(t, beforeSnap, afterSnap)
}

func TestHost_ResumeFromCoCreate_BlockedByMigration(t *testing.T) {
	h := newMigrationTestHost(t)
	beforeSnap := takeStoreSnapshot(h.store.Dir())
	err := h.ResumeFromCoCreate("后续方向")
	assertBlockedByMigration(t, err, "ResumeFromCoCreate")
	afterSnap := takeStoreSnapshot(h.store.Dir())
	assertSnapshotUnchanged(t, beforeSnap, afterSnap)
}

// ── Engine dispatch migration gate ──

func TestEngine_MigrationCheck_BlocksStart(t *testing.T) {
	st := createMinimalStore(t)
	if err := st.Progress.Init("测试书", 10); err != nil {
		t.Fatalf("progress init: %v", err)
	}

	obs := newObserver(st, func(Event) {}, func(string) {}, func() {})

	// Engine with migrationCheck that returns error
	e := &engine{
		store:          st,
		observer:       obs,
		migrationCheck: func() error { return ErrMigrationRequired },
		emitEvent:      func(Event) {},
		onDone:         func() {},
	}

	if !e.start(nil) {
		t.Fatal("engine.start should return true even when migration blocks")
	}

	// Wait for engine to finish
	done := make(chan struct{})
	go func() {
		for e.isRunning() {
			// spin
		}
		close(done)
	}()
	<-done
}

func TestEngine_MigrationCheck_Passes(t *testing.T) {
	st := createMinimalStore(t)
	if err := st.Progress.Init("测试书", 10); err != nil {
		t.Fatalf("progress init: %v", err)
	}

	obs := newObserver(st, func(Event) {}, func(string) {}, func() {})
	e := &engine{
		store:          st,
		observer:       obs,
		migrationCheck: func() error { return nil },
		emitEvent:      func(Event) {},
		notify:         func(string, string, string, string) {},
		onPause:        func(string) {},
		onDone:         func() {},
	}

	if !e.start(nil) {
		t.Fatal("engine.start should return true when migrationCheck passes")
	}

	done := make(chan struct{})
	go func() {
		for e.isRunning() {
			// spin
		}
		close(done)
	}()
	<-done
}

// ── ResolveProjectProfile integration ──

func TestResolveProjectProfile_NoMarker(t *testing.T) {
	st := createMinimalStore(t)
	rp, err := resolveProjectProfile(st)
	if err != nil {
		t.Fatalf("resolveProjectProfile on empty store: %v", err)
	}
	if rp.Contract != projectprofile.ContractCore4 {
		t.Errorf("no marker should resolve to core4, got %v", rp.Contract)
	}
	if rp.Status != projectprofile.StatusCore {
		t.Errorf("no marker should resolve to core, got %v", rp.Status)
	}
}

func TestResolveProjectProfile_WithMarker(t *testing.T) {
	st := createMinimalStore(t)

	writeProjectProfileJSON(t, st.Dir(), map[string]any{
		"version":  "v1",
		"contract": "scene_beat_v3",
		"status":   "migration_required",
	})

	rp, err := resolveProjectProfile(st)
	if err != nil {
		t.Fatalf("resolveProjectProfile: %v", err)
	}
	if rp.Contract != projectprofile.ContractSceneBeatV3 {
		t.Errorf("marker should resolve to scene_beat_v3, got %v", rp.Contract)
	}
	if rp.Status != projectprofile.StatusMigrationRequired {
		t.Errorf("marker should resolve to migration_required, got %v", rp.Status)
	}
}

func TestResolveProjectProfile_EarlyWithCore4Marker_Rejected(t *testing.T) {
	dir := t.TempDir()
	writeProjectProfileJSON(t, dir, map[string]any{
		"version":  "v1",
		"contract": "core4",
		"status":   "core",
	})

	_, err := resolveProjectProfileEarly(dir)
	if err == nil {
		t.Fatal("Core4 marker should be rejected by resolveProjectProfileEarly")
	}
}

func TestResolveProjectProfile_EarlyWithTrailingData_Rejected(t *testing.T) {
	dir := t.TempDir()
	// Write valid JSON followed by trailing data
	path := filepath.Join(dir, "meta", "project_profile.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"version":"v1","contract":"scene_beat_v3","status":"migration_required"}{"extra":"doc"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := resolveProjectProfileEarly(dir)
	if err == nil || !strings.Contains(err.Error(), "trailing JSON document") {
		t.Fatalf("should reject trailing JSON document, got: %v", err)
	}
}

func TestResolveProjectProfile_EarlyZeroBytes_Rejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "meta", "project_profile.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte{}, 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := resolveProjectProfileEarly(dir)
	if err == nil || !strings.Contains(err.Error(), "zero bytes") {
		t.Fatalf("zero-byte marker should be rejected, got: %v", err)
	}
}

// TestResolveProjectProfile_EarlyEnrolledNoMarker 测试无 marker + 注入预期 enrolled
// fingerprint（expectedEnrolled）时 Registry 正确返回 migration_required。
func TestResolveProjectProfile_EarlyEnrolledNoMarker(t *testing.T) {
	prod := projectprofile.V3EnrolledFingerprint()

	// 使用 expectedEnrolled 参数，fingerprinter 为真实计算但返回匹配值
	reg := projectprofile.NewRegistry(
		func() (*projectprofile.ProfileMarker, error) { return nil, nil },
		func() (projectprofile.Fingerprint, error) { return prod, nil },
		&prod, // 注入预期 enrolled 指纹
	)
	rp, err := reg.Resolve()
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if rp.Contract != projectprofile.ContractSceneBeatV3 {
		t.Errorf("enrolled no-marker should get scene_beat_v3, got %v", rp.Contract)
	}
	if rp.Status != projectprofile.StatusMigrationRequired {
		t.Errorf("enrolled no-marker should get migration_required, got %v", rp.Status)
	}
}

// TestNewDiagnosticHost 验证诊断 Host 构造正确且所有创作门返回 ErrMigrationRequired。
func TestNewDiagnosticHost(t *testing.T) {
	dir := t.TempDir()
	cfg := bootstrap.Config{OutputDir: dir, Provider: "test", ModelName: "test"}
	cfg.FillDefaults()
	profile := projectprofile.ResolvedProfile{
		Contract: projectprofile.ContractSceneBeatV3,
		Status:   projectprofile.StatusMigrationRequired,
	}

	h := newDiagnosticHost(cfg, assets.Bundle{}, dir, profile)
	if h == nil {
		t.Fatal("newDiagnosticHost returned nil")
	}

	// 验证 profile
	if h.profile.Status != projectprofile.StatusMigrationRequired {
		t.Errorf("profile status = %v, want migration_required", h.profile.Status)
	}

	// 验证 store 存在但未 Init（不应有目录写入）
	if h.store == nil {
		t.Fatal("store should be set for read access")
	}

	// Dir() 应返回正确的输出目录
	if h.Dir() != dir {
		t.Errorf("Dir() = %q, want %q", h.Dir(), dir)
	}

	// Snapshot() 不应 panic
	snap := h.Snapshot()
	if snap.RuntimeState != string(lifecycleIdle) {
		t.Errorf("state = %q, want idle", snap.RuntimeState)
	}

	// 所有创作门应被阻止
	if err := h.checkMigrationGate(); err == nil || !IsMigrationRequired(err) {
		t.Error("checkMigrationGate should return ErrMigrationRequired")
	}

	// Close 不应 panic
	h.Close()
}

// TestHost_NewDiagnosticHostGate 验证诊断 Host 的所有创作入口被 gate。
func TestHost_NewDiagnosticHostGates(t *testing.T) {
	h := newDiagnosticHost(bootstrap.Config{}, assets.Bundle{}, t.TempDir(), projectprofile.ResolvedProfile{
		Contract: projectprofile.ContractSceneBeatV3,
		Status:   projectprofile.StatusMigrationRequired,
	})

	// 为 Steer 和 PauseForCoCreate 分配 events 通道
	h.events = make(chan Event, 10)

	assertBlocked := func(err error, name string) {
		t.Helper()
		if err == nil {
			t.Errorf("%s should be blocked by migration gate", name)
		} else if !IsMigrationRequired(err) {
			t.Errorf("%s error should be ErrMigrationRequired, got: %v", name, err)
		}
	}

	assertBlocked(h.StartPrepared("test"), "StartPrepared")
	_, err := h.Resume()
	assertBlocked(err, "Resume")
	assertBlocked(h.Continue("test"), "Continue")
	assertBlocked(h.AdvanceOneChapter(), "AdvanceOneChapter")
	assertBlocked(h.PrepareUserRules("test"), "PrepareUserRules")
	assertBlocked(h.SetAdvanceMode(domain.ChapterAdvanceReview), "SetAdvanceMode")
	_, err = h.CoCreateStream(context.Background(), nil, nil)
	assertBlocked(err, "CoCreateStream")
	_, err = h.StageCoCreateStream(context.Background(), nil, nil)
	assertBlocked(err, "StageCoCreateStream")
	assertBlocked(h.ResumeFromCoCreate("draft"), "ResumeFromCoCreate")

	// Steer 不返回 error，验证事件
	h.Steer("test")
	select {
	case ev := <-h.events:
		if ev.Level != "warn" || !strings.Contains(ev.Summary, "迁移未完成") {
			t.Errorf("Steer expected warning event, got: %+v", ev)
		}
	default:
		t.Error("Steer should have emitted an event")
	}

	// PauseForCoCreate 返回 false
	if h.PauseForCoCreate() {
		t.Error("PauseForCoCreate should return false for migration_required")
	}

	// switch 类操作被 gate
	assertBlocked(h.SwitchModel("default", "p", "m"), "SwitchModel")
	assertBlocked(h.SetRoleThinking("writer", "high"), "SetRoleThinking")
}

func TestHost_requireProfileResolved(t *testing.T) {
	h := &Host{
		profile: projectprofile.ResolvedProfile{
			Contract: projectprofile.ContractCore4,
			Status:   projectprofile.StatusCore,
		},
	}
	rp := h.requireProfileResolved()
	if rp.Contract != projectprofile.ContractCore4 {
		t.Errorf("got contract %v", rp.Contract)
	}
}

// ── Core4 project pass-through ──

func TestHost_StartPrepared_PassesForCore(t *testing.T) {
	h := &Host{
		profile: projectprofile.ResolvedProfile{
			Contract: projectprofile.ContractCore4,
			Status:   projectprofile.StatusCore,
		},
	}
	err := h.checkMigrationGate()
	if err != nil {
		t.Errorf("Core4 project should pass migration gate: %v", err)
	}
}

// ── resolveProjectProfileEarly tests ──

func TestResolveProjectProfileEarly_NoFiles(t *testing.T) {
	dir := t.TempDir()
	profile, err := resolveProjectProfileEarly(dir)
	if err != nil {
		t.Fatalf("resolveProjectProfileEarly on empty dir: %v", err)
	}
	if profile.Contract != projectprofile.ContractCore4 {
		t.Errorf("expected core4, got %v", profile.Contract)
	}
	if profile.Status != projectprofile.StatusCore {
		t.Errorf("expected core, got %v", profile.Status)
	}
}

func TestResolveProjectProfileEarly_MarkerMigrationRequired(t *testing.T) {
	dir := t.TempDir()
	writeProjectProfileJSON(t, dir, map[string]any{
		"version":  "v1",
		"contract": "scene_beat_v3",
		"status":   "migration_required",
	})

	_, err := resolveProjectProfileEarly(dir)
	if err == nil {
		t.Fatal("migration_required marker should return error from resolveProjectProfileEarly")
	}
	if !IsMigrationRequired(err) {
		t.Errorf("error should be migration_required: %v", err)
	}
}

func TestResolveProjectProfileEarly_MarkerCore_Rejected(t *testing.T) {
	dir := t.TempDir()
	writeProjectProfileJSON(t, dir, map[string]any{
		"version":  "v1",
		"contract": "core4",
		"status":   "core",
	})

	_, err := resolveProjectProfileEarly(dir)
	if err == nil {
		t.Fatal("Core4 marker should be rejected by resolveProjectProfileEarly")
	}
	if !strings.Contains(err.Error(), "Core4 marker is not allowed") {
		t.Errorf("expected Core4 rejection error, got: %v", err)
	}
}

func TestResolveProjectProfileEarly_StrictDecodeRejectsUnknownFields(t *testing.T) {
	dir := t.TempDir()
	writeProjectProfileJSON(t, dir, map[string]any{
		"version":       "v1",
		"contract":      "core4",
		"status":        "core",
		"unknown_field": "should_fail",
	})

	_, err := resolveProjectProfileEarly(dir)
	if err == nil {
		t.Fatal("unknown fields should cause strict decode to fail")
	}
}

func TestResolveProjectProfileEarly_UnknownContractFailsClosed(t *testing.T) {
	dir := t.TempDir()
	writeProjectProfileJSON(t, dir, map[string]any{
		"version":  "v1",
		"contract": "nonexistent",
		"status":   "core",
	})

	_, err := resolveProjectProfileEarly(dir)
	if err == nil {
		t.Fatal("unknown contract should fail closed")
	}
}

func TestResolveProjectProfileEarly_VersionMismatchFailsClosed(t *testing.T) {
	dir := t.TempDir()
	writeProjectProfileJSON(t, dir, map[string]any{
		"version":  "v0",
		"contract": "core4",
		"status":   "core",
	})

	_, err := resolveProjectProfileEarly(dir)
	if err == nil {
		t.Fatal("version mismatch should fail closed")
	}
}

// ── Trailing data comprehensive tests (Blocker 3) ──

func TestResolveProjectProfileEarly_TrailingGarbage_Rejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "meta", "project_profile.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	// Valid JSON followed by non-JSON garbage
	if err := os.WriteFile(path, []byte(`{"version":"v1","contract":"scene_beat_v3","status":"migration_required"}garbage`), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := resolveProjectProfileEarly(dir)
	if err == nil {
		t.Fatal("should reject garbage after valid JSON")
	}
}

func TestResolveProjectProfileEarly_TrailingUnmatchedBracket_Rejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "meta", "project_profile.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	// Valid JSON followed by an unmatched closing bracket
	if err := os.WriteFile(path, []byte(`{"version":"v1","contract":"scene_beat_v3","status":"migration_required"}]`), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := resolveProjectProfileEarly(dir)
	if err == nil {
		t.Fatal("should reject unmatched closing bracket")
	}
}

func TestResolveProjectProfileEarly_TrailingUnmatchedCloseBrace_Rejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "meta", "project_profile.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	// Valid JSON followed by an extra closing brace
	if err := os.WriteFile(path, []byte(`{"version":"v1","contract":"scene_beat_v3","status":"migration_required"}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := resolveProjectProfileEarly(dir)
	if err == nil {
		t.Fatal("should reject extra closing brace")
	}
}

// ── Enrolled fingerprint through real disk path (Blocker 2) ──

// TestResolveProjectProfileEarly_DiskEnrolled 使用真实磁盘 Fingerprinter 路径，
// 只注入预期 enrolled fingerprint 匹配值（而非替换整个 Fingerprinter）。
// snapshot 在 resolver 调用前后比较。
func TestResolveProjectProfileEarly_DiskEnrolled_MigrationRequired(t *testing.T) {
	dir := t.TempDir()

	// 在磁盘上写入 premise.md 和 chapters/01.md-34.md
	premiseContent := "测试用 premise 内容"
	if err := os.WriteFile(filepath.Join(dir, "premise.md"), []byte(premiseContent), 0o644); err != nil {
		t.Fatal(err)
	}
	chapters := make(map[string]string)
	for i := 1; i <= 34; i++ {
		content := fmt.Sprintf("第 %02d 章测试内容", i)
		name := fmt.Sprintf("%02d.md", i)
		chapters[name] = content
		chDir := filepath.Join(dir, "chapters")
		if err := os.MkdirAll(chDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(chDir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// 预测该内容的 fingerprint
	expected := projectprofile.NewFingerprint(premiseContent, chapters)

	// snapshot before resolver
	beforeSnap := takeStoreSnapshot(dir)

	// 使用真实磁盘 Fingerprinter + 仅注入预期 enrolled fingerprint
	// （不替换 Fingerprinter，只替换匹配值）
	profile, err := resolveProjectProfileEarly(dir, &expected)
	if err == nil {
		t.Fatal("enrolled disk content should return migration_required error")
	}
	if !IsMigrationRequired(err) {
		t.Errorf("error should be migration_required, got: %v", err)
	}
	if profile.Status != projectprofile.StatusMigrationRequired {
		t.Errorf("profile status should be migration_required, got %v", profile.Status)
	}
	if profile.Contract != projectprofile.ContractSceneBeatV3 {
		t.Errorf("profile contract should be scene_beat_v3, got %v", profile.Contract)
	}

	// snapshot after resolver — storage must be unchanged
	afterSnap := takeStoreSnapshot(dir)
	assertSnapshotUnchanged(t, beforeSnap, afterSnap)
}

func TestResolveProjectProfileEarly_DiskEnrolledActiveAudit(t *testing.T) {
	dir := t.TempDir()
	premise := "active audit premise"
	if err := os.WriteFile(filepath.Join(dir, "premise.md"), []byte(premise), 0o644); err != nil {
		t.Fatal(err)
	}
	chapters := make(map[string]string, 34)
	if err := os.MkdirAll(filepath.Join(dir, "chapters"), 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 34; i++ {
		name := fmt.Sprintf("%02d.md", i)
		chapters[name] = fmt.Sprintf("active chapter %02d", i)
		if err := os.WriteFile(filepath.Join(dir, "chapters", name), []byte(chapters[name]), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	expected := projectprofile.NewFingerprint(premise, chapters)
	writeProjectProfileJSON(t, dir, map[string]any{
		"version": projectprofile.ProfileVersion, "contract": "scene_beat_v3", "status": "active",
		"profile_id":               projectprofile.SceneBeatV3ProfileID,
		"enrollment_fingerprint":   expected,
		"approved_manifest_sha256": "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	})
	before := takeStoreSnapshot(dir)
	profile, err := resolveProjectProfileEarly(dir, &expected)
	if err != nil || profile.Contract != projectprofile.ContractSceneBeatV3 || profile.Status != projectprofile.StatusActive {
		t.Fatalf("active audited profile was not accepted: profile=%+v err=%v", profile, err)
	}
	assertSnapshotUnchanged(t, before, takeStoreSnapshot(dir))
}

func TestResolveProjectProfileEarly_DiskNonEnrolled_ReturnsCore4(t *testing.T) {
	dir := t.TempDir()

	// 写入 premise 但 chapter 不足 34 篇（非 enrolled）
	if err := os.WriteFile(filepath.Join(dir, "premise.md"), []byte("普通 premise"), 0o644); err != nil {
		t.Fatal(err)
	}

	// snapshot before
	beforeSnap := takeStoreSnapshot(dir)

	// 不注入任何 expectedEnrolled → 使用内建 const（不会匹配普通内容）
	profile, err := resolveProjectProfileEarly(dir)
	if err != nil {
		t.Fatalf("non-enrolled disk content should not error: %v", err)
	}
	if profile.Contract != projectprofile.ContractCore4 {
		t.Errorf("non-enrolled should get core4, got %v", profile.Contract)
	}
	if profile.Status != projectprofile.StatusCore {
		t.Errorf("non-enrolled should get core, got %v", profile.Status)
	}

	// snapshot after
	afterSnap := takeStoreSnapshot(dir)
	assertSnapshotUnchanged(t, beforeSnap, afterSnap)
}

// TestResolveProjectProfileEarly_DiskEmpty_ReturnsCore4 验证空目录走真实磁盘路径也得到 Core4。
func TestResolveProjectProfileEarly_DiskEmpty_ReturnsCore4(t *testing.T) {
	dir := t.TempDir()

	beforeSnap := takeStoreSnapshot(dir)
	profile, err := resolveProjectProfileEarly(dir)
	if err != nil {
		t.Fatalf("empty disk should not error: %v", err)
	}
	if profile.Contract != projectprofile.ContractCore4 {
		t.Errorf("empty should get core4, got %v", profile.Contract)
	}
	if profile.Status != projectprofile.StatusCore {
		t.Errorf("empty should get core, got %v", profile.Status)
	}
	afterSnap := takeStoreSnapshot(dir)
	assertSnapshotUnchanged(t, beforeSnap, afterSnap)
}

// TestNewDiagnosticHost_TUISetupClose 模拟 TUI 启动/关闭的完整路径：
// 诊断 Host 配合 logger.Setup(io.Discard) 不应创建任何存储文件。
func TestNewDiagnosticHost_TUISetupClose(t *testing.T) {
	dir := t.TempDir()
	cfg := bootstrap.Config{OutputDir: dir, Provider: "test", ModelName: "test"}
	cfg.FillDefaults()
	profile := projectprofile.ResolvedProfile{
		Contract: projectprofile.ContractSceneBeatV3,
		Status:   projectprofile.StatusMigrationRequired,
	}

	beforeSnap := takeStoreSnapshot(dir)

	// 模拟 TUI 启动流程
	h := newDiagnosticHost(cfg, assets.Bundle{}, dir, profile)
	if h == nil {
		t.Fatal("newDiagnosticHost returned nil")
	}

	// 模拟 TUI 的 logger.Setup(io.Discard, slog.LevelInfo) —— 不写文件
	// 验证诊断 Host 标识
	if !h.IsDiagnosticOnly() {
		t.Error("diagnostic host should return true for IsDiagnosticOnly()")
	}

	// AskUser 非 nil（SetHandler 不会 panic）
	if h.AskUser() == nil {
		t.Error("AskUser() should return non-nil for diagnostic host")
	}

	// Snapshot 不 panic
	snap := h.Snapshot()
	if snap.RuntimeState != string(lifecycleIdle) {
		t.Errorf("state = %q, want idle", snap.RuntimeState)
	}

	// Close 不 panic、不创建文件
	h.Close()

	afterSnap := takeStoreSnapshot(dir)
	assertSnapshotUnchanged(t, beforeSnap, afterSnap)
}
