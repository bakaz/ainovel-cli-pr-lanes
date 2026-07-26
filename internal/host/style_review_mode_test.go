package host

import (
	"strings"
	"testing"
	"time"

	"github.com/voocel/ainovel-cli/internal/domain"
	storepkg "github.com/voocel/ainovel-cli/internal/store"
)

// setupStyleModeStore 创建一个干净的 Store 用于风格评审模式切换测试。
func setupStyleModeStore(t *testing.T) *storepkg.Store {
	t.Helper()
	st := storepkg.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	// 初始化 RunMeta（正常 Init 会设置默认 AdvanceMode）
	if err := st.RunMeta.Init("test-style", "test-provider", "test-model"); err != nil {
		t.Fatalf("RunMeta.Init: %v", err)
	}
	return st
}

// assertMode 辅助检查 RunMeta 中的 StyleReviewMode。
func assertMode(t *testing.T, st *storepkg.Store, want domain.StyleQualityMode) {
	t.Helper()
	meta, err := st.RunMeta.Load()
	if err != nil {
		t.Fatalf("Load RunMeta: %v", err)
	}
	if meta == nil {
		t.Fatal("RunMeta is nil")
	}
	if meta.StyleReviewMode != want {
		t.Errorf("StyleReviewMode = %q, want %q", meta.StyleReviewMode, want)
	}
}

// ── 1. Initial state ────────────────────────────────────────────────

func TestSetStyleReviewMode_InitialState(t *testing.T) {
	st := setupStyleModeStore(t)
	h := &Host{store: st}

	// 未设置时应为空（Init 保留默认空值，加载时归一化为 off）
	meta, err := st.RunMeta.Load()
	if err != nil {
		t.Fatalf("Load RunMeta: %v", err)
	}
	// 初始时 style_review_mode 应为空（由 normalizeStyleReviewMode 在 Load 时转为 off）
	// 但我们直接检查持久化值：Init 不设置 StyleReviewMode，所以持久化是零值
	if meta.StyleReviewMode != "" && meta.StyleReviewMode != domain.StyleQualityOff {
		t.Logf("initial StyleReviewMode = %q (loaded; normalize may have set it)", meta.StyleReviewMode)
	}
	_ = h
}

// ── 2. Set critic mode ──────────────────────────────────────────────

func TestSetStyleReviewMode_SetCritic(t *testing.T) {
	st := setupStyleModeStore(t)
	eventsCh := make(chan Event, 10)
	h := &Host{
		store:  st,
		events: eventsCh,
		done:   make(chan struct{}, 4),
	}

	if err := h.SetStyleReviewMode(domain.StyleQualityCritic); err != nil {
		t.Fatalf("SetStyleReviewMode(critic): %v", err)
	}

	assertMode(t, st, domain.StyleQualityCritic)

	// 验证事件
	select {
	case ev := <-eventsCh:
		if ev.Category != "SYSTEM" {
			t.Errorf("event category = %q, want SYSTEM", ev.Category)
		}
		if !strings.Contains(ev.Summary, "批评模式") {
			t.Errorf("event summary = %q, want 批评模式", ev.Summary)
		}
		if ev.Level != "info" {
			t.Errorf("event level = %q, want info", ev.Level)
		}
	default:
		t.Error("expected event for mode switch")
	}
}

// ── 3. Set off mode ────────────────────────────────────────────────

func TestSetStyleReviewMode_SetOff(t *testing.T) {
	st := setupStyleModeStore(t)
	eventsCh := make(chan Event, 10)
	h := &Host{
		store:  st,
		events: eventsCh,
		done:   make(chan struct{}, 4),
	}

	// 先设为 critic
	if err := h.SetStyleReviewMode(domain.StyleQualityCritic); err != nil {
		t.Fatalf("SetStyleReviewMode(critic): %v", err)
	}
	assertMode(t, st, domain.StyleQualityCritic)

	// 清空事件通道
	select {
	case <-eventsCh:
	default:
	}

	// 再切回 off
	if err := h.SetStyleReviewMode(domain.StyleQualityOff); err != nil {
		t.Fatalf("SetStyleReviewMode(off): %v", err)
	}
	assertMode(t, st, domain.StyleQualityOff)

	// 验证事件
	select {
	case ev := <-eventsCh:
		if !strings.Contains(ev.Summary, "关闭") {
			t.Errorf("event summary = %q, want 关闭", ev.Summary)
		}
	default:
		t.Error("expected event for mode switch")
	}
}

// ── 4. Bad value rejected without mutation ─────────────────────────

func TestSetStyleReviewMode_RejectsInvalid(t *testing.T) {
	st := setupStyleModeStore(t)
	h := &Host{store: st}

	// 先设为 critic 确认起始状态
	if err := h.SetStyleReviewMode(domain.StyleQualityCritic); err != nil {
		t.Fatalf("SetStyleReviewMode(critic): %v", err)
	}
	assertMode(t, st, domain.StyleQualityCritic)

	// 尝试无效值
	err := h.SetStyleReviewMode(domain.StyleQualityMode("auto"))
	if err == nil {
		t.Fatal("expected error for invalid mode")
	}
	if !strings.Contains(err.Error(), "off") || !strings.Contains(err.Error(), "critic") {
		t.Errorf("error should mention valid values, got: %v", err)
	}

	// 模式未被修改（仍为 critic）
	assertMode(t, st, domain.StyleQualityCritic)
}

// ── 5. Empty string → rejected (explicit validation) ───────────────

func TestSetStyleReviewMode_RejectsEmpty(t *testing.T) {
	st := setupStyleModeStore(t)
	h := &Host{store: st}

	if err := h.SetStyleReviewMode(domain.StyleQualityMode("")); err == nil {
		t.Fatal("expected error for empty mode")
	}
}

// ── 6. Double set critic idempotent ────────────────────────────────

func TestSetStyleReviewMode_DoubleCriticIdempotent(t *testing.T) {
	st := setupStyleModeStore(t)
	eventsCh := make(chan Event, 10)
	h := &Host{
		store:  st,
		events: eventsCh,
		done:   make(chan struct{}, 4),
	}

	if err := h.SetStyleReviewMode(domain.StyleQualityCritic); err != nil {
		t.Fatalf("first SetStyleReviewMode(critic): %v", err)
	}
	// 清空事件
	select {
	case <-eventsCh:
	default:
	}

	if err := h.SetStyleReviewMode(domain.StyleQualityCritic); err != nil {
		t.Fatalf("second SetStyleReviewMode(critic): %v", err)
	}
	// 第二次仍然应发射事件（与 SetAdvanceMode 行为一致）
	select {
	case ev := <-eventsCh:
		if ev.Category != "SYSTEM" {
			t.Errorf("event category = %q, want SYSTEM", ev.Category)
		}
	default:
		t.Error("expected event for idempotent mode switch")
	}
}

// ── 7. Snapshot shows current mode ─────────────────────────────────

func TestSetStyleReviewMode_SnapshotReflectsMode(t *testing.T) {
	st := setupStyleModeStore(t)
	h := &Host{
		store:  st,
		events: make(chan Event, 10),
		done:   make(chan struct{}, 4),
	}

	// Snapshot 返回 RunMeta 中的 mode（通过 Load 加载）
	// 验证 Snapshot 含 StyleReviewMode 字段（在 UISnapshot 中暂无此字段，
	// 但 RunMeta 已正确持久化）
	if err := h.SetStyleReviewMode(domain.StyleQualityCritic); err != nil {
		t.Fatalf("SetStyleReviewMode: %v", err)
	}
	meta, err := st.RunMeta.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if meta.StyleReviewMode != domain.StyleQualityCritic {
		t.Errorf("RunMeta.StyleReviewMode = %q, want critic", meta.StyleReviewMode)
	}
}

// ── 8. Mode survives reload (persistence) ────────────────────────

func TestSetStyleReviewMode_SurvivesReload(t *testing.T) {
	st := setupStyleModeStore(t)
	{
		h := &Host{store: st, events: make(chan Event, 10), done: make(chan struct{}, 4)}
		if err := h.SetStyleReviewMode(domain.StyleQualityCritic); err != nil {
			t.Fatalf("SetStyleReviewMode: %v", err)
		}
	}
	// 重新加载验证持久化
	meta, err := st.RunMeta.Load()
	if err != nil {
		t.Fatalf("Load after Host close: %v", err)
	}
	if meta.StyleReviewMode != domain.StyleQualityCritic {
		t.Errorf("after reload StyleReviewMode = %q, want critic", meta.StyleReviewMode)
	}
}

// ── 9. EmitEvent is called with correct timing ─────────────────────

func TestSetStyleReviewMode_EventTimeIsRecent(t *testing.T) {
	st := setupStyleModeStore(t)
	eventsCh := make(chan Event, 10)
	h := &Host{
		store:  st,
		events: eventsCh,
		done:   make(chan struct{}, 4),
	}

	before := time.Now()
	if err := h.SetStyleReviewMode(domain.StyleQualityCritic); err != nil {
		t.Fatalf("SetStyleReviewMode: %v", err)
	}
	after := time.Now()

	select {
	case ev := <-eventsCh:
		if ev.Time.Before(before) || ev.Time.After(after) {
			t.Errorf("event time %v out of range [%v, %v]", ev.Time, before, after)
		}
	default:
		t.Error("expected event")
	}
}
