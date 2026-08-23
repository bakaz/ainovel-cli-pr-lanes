package bootstrap

import (
	"errors"
	"testing"
	"time"

	"github.com/voocel/ainovel-cli/internal/errs"
	"github.com/voocel/ainovel-cli/internal/notify"
)

// TestConfigFlagsAccessors 验证灰度 flags（计划 §12）的访问器：默认全关，
// 开启后对应访问器为 true。
func TestConfigFlagsAccessors(t *testing.T) {
	cfg := Config{}
	if cfg.FullContextPolisherV3Enabled() || cfg.PolisherCandidateToolsV3Enabled() ||
		cfg.AgentOutputBudgetV2Enabled() || cfg.StyleContentBudgetV2Enabled() ||
		cfg.LegacyPolisherHighOutputEnabled() {
		t.Fatal("all grayscale flags must default to false (rollback-safe)")
	}
	cfg.Flags = Flags{
		FullContextPolisherV3:   true,
		PolisherCandidateToolsV3: true,
		AgentOutputBudgetV2:      true,
		StyleContentBudgetV2:     true,
		LegacyPolisherHighOutput: true,
	}
	if !cfg.FullContextPolisherV3Enabled() || !cfg.PolisherCandidateToolsV3Enabled() ||
		!cfg.AgentOutputBudgetV2Enabled() || !cfg.StyleContentBudgetV2Enabled() ||
		!cfg.LegacyPolisherHighOutputEnabled() {
		t.Error("enabled flags must report true via accessors")
	}
}

// TestMergeConfigFlags 验证 flags 逐字段合并：任一配置层开启即开启（opt-in），
// 低层配置不能关闭高层已开启的 flag。
func TestMergeConfigFlags(t *testing.T) {
	base := Config{Flags: Flags{FullContextPolisherV3: true}}
	overlay := Config{Flags: Flags{LegacyPolisherHighOutput: true}}
	merged := mergeConfig(base, overlay)
	if !merged.FullContextPolisherV3Enabled() {
		t.Error("base flag must survive overlay merge")
	}
	if !merged.LegacyPolisherHighOutputEnabled() {
		t.Error("overlay flag must be merged in")
	}
	// 未开启的 flag 保持关闭
	if merged.PolisherCandidateToolsV3Enabled() || merged.AgentOutputBudgetV2Enabled() {
		t.Error("unset flags must remain false")
	}
}

func TestConfigResolveReasoningEffort(t *testing.T) {
	cfg := Config{
		ReasoningEffort: "low", // 顶层默认
		Roles: map[string]RoleConfig{
			"writer":    {Provider: "p", Model: "m", ReasoningEffort: "high"}, // 角色覆盖
			"architect": {Provider: "p", Model: "m"},                          // 无 reasoning_effort，应回落默认
		},
	}

	cases := []struct {
		role string
		want string
	}{
		{"writer", "high"},   // 角色覆盖优先
		{"architect", "low"}, // 角色未配 → 回落顶层默认
		{"editor", "low"},    // 角色不存在 → 顶层默认
		{"", "low"},          // 空 → 顶层默认
		{"default", "low"},   // default → 顶层默认
		{"arbiter", "low"},   // 非配置角色（裁定恒随顶层默认）
	}
	for _, c := range cases {
		if got := cfg.ResolveReasoningEffort(c.role); got != c.want {
			t.Errorf("ResolveReasoningEffort(%q) = %q, want %q", c.role, got, c.want)
		}
	}

	// 顶层默认也为空时，未覆盖角色返回 ""（不覆盖）。
	empty := Config{Roles: map[string]RoleConfig{"writer": {ReasoningEffort: "xhigh"}}}
	if got := empty.ResolveReasoningEffort("editor"); got != "" {
		t.Errorf("空默认下 editor 应返回 \"\"，得 %q", got)
	}
	if got := empty.ResolveReasoningEffort("writer"); got != "xhigh" {
		t.Errorf("空默认下 writer 覆盖应生效，得 %q", got)
	}
}

func TestValidateBaseRejectsNonConfigurableRoles(t *testing.T) {
	for _, role := range []string{"coordinator", "arbiter", "invalid_role"} {
		t.Run(role, func(t *testing.T) {
			cfg := Config{
				Provider:  "openrouter",
				ModelName: "test-model",
				Providers: map[string]ProviderConfig{
					"openrouter": {APIKey: "sk-test-123456"},
				},
				Roles: map[string]RoleConfig{
					role: {Provider: "openrouter", Model: "test-model"},
				},
			}

			err := cfg.ValidateBase()
			if err == nil {
				t.Fatalf("roles.%s 应被拒绝", role)
			}
			if !errors.Is(err, errs.ErrConfig) {
				t.Fatalf("应包装 errs.ErrConfig，得到: %v", err)
			}
		})
	}
}

// TestCriticRole_ValidateAndFallback 验证 critic 角色可通过校验且无配置时回落默认模型。
func TestCriticRole_ValidateAndFallback(t *testing.T) {
	// critic 显式配置应通过校验
	cfg := Config{
		Provider:  "openrouter",
		ModelName: "default-model",
		Providers: map[string]ProviderConfig{
			"openrouter": {APIKey: "sk-test-123456"},
		},
		Roles: map[string]RoleConfig{
			"critic": {Provider: "openrouter", Model: "critic-model"},
		},
	}
	if err := cfg.ValidateBase(); err != nil {
		t.Fatalf("critic 角色配置应通过校验: %v", err)
	}

	// critic 未配置时回落到默认 —— ModelSet 的 ForRole 逻辑已覆盖
	// （ForRole("critic") 返回 ms.Default），此测试仅验证配置校验不拒绝 critic。
	// 不在 roles 中声明 critic 时的默认回落属于 ModelSet 行为。
}

// TestCriticRole_UnconfiguredFallsBack 验证未配 critic 角色时 ModelSet.ForRole 返回默认模型。
func TestCriticRole_UnconfiguredFallsBack(t *testing.T) {
	cfg := Config{
		Provider:  "openrouter",
		ModelName: "default-model",
		Providers: map[string]ProviderConfig{
			"openrouter": {Type: "openai", APIKey: "sk-test-123456"},
		},
		// Roles 明确缺失 critic —— 应回落默认
		Roles: map[string]RoleConfig{
			"writer": {Provider: "openrouter", Model: "writer-model"},
		},
	}
	ms, err := NewModelSet(cfg)
	if err != nil {
		t.Fatalf("NewModelSet: %v", err)
	}

	// critic 未配置 -> ForRole 应返回默认模型
	got := ms.ForRole("critic")
	if got == nil {
		t.Fatal("未配置 critic 时 ForRole 不应返回 nil")
	}
	if got != ms.Default {
		t.Fatal("critic 未配置时应返回默认模型的同一实例")
	}
}

// TestPolisherRole_ValidateAndFallback 验证 polisher 角色可通过校验且未配置时回落默认模型。
func TestPolisherRole_ValidateAndFallback(t *testing.T) {
	// polisher 显式配置应通过校验
	cfg := Config{
		Provider:  "openrouter",
		ModelName: "default-model",
		Providers: map[string]ProviderConfig{
			"openrouter": {APIKey: "sk-test-123456"},
		},
		Roles: map[string]RoleConfig{
			"polisher": {Provider: "openrouter", Model: "mimo-polisher"},
		},
	}
	if err := cfg.ValidateBase(); err != nil {
		t.Fatalf("polisher 角色配置应通过校验: %v", err)
	}

	// polisher 显式配置 → ModelSet.ForRole 返回该角色模型（非默认）
	ms, err := NewModelSet(cfg)
	if err != nil {
		t.Fatalf("NewModelSet: %v", err)
	}
	if ms.ForRole("polisher") == ms.Default {
		t.Fatal("显式配置 polisher 时 ForRole 不应返回默认模型")
	}

	// polisher 未配置时回落到默认 —— ModelSet 的 ForRole 逻辑已覆盖
	unconfigured := Config{
		Provider:  "openrouter",
		ModelName: "default-model",
		Providers: map[string]ProviderConfig{
			"openrouter": {Type: "openai", APIKey: "sk-test-123456"},
		},
		Roles: map[string]RoleConfig{},
	}
	ms2, err := NewModelSet(unconfigured)
	if err != nil {
		t.Fatalf("NewModelSet: %v", err)
	}
	if ms2.ForRole("polisher") != ms2.Default {
		t.Fatal("polisher 未配置时应返回默认模型的同一实例")
	}
}

func TestImportRoles_Validate(t *testing.T) {
	cfg := Config{
		Provider:  "openrouter",
		ModelName: "default-model",
		Providers: map[string]ProviderConfig{
			"openrouter": {APIKey: "sk-test-123456"},
		},
		Roles: map[string]RoleConfig{
			"import_segment":    {Provider: "openrouter", Model: "segment-model"},
			"import_analyze":    {Provider: "openrouter", Model: "analyze-model"},
			"import_synthesize": {Provider: "openrouter", Model: "synthesize-model"},
		},
	}
	if err := cfg.ValidateBase(); err != nil {
		t.Fatalf("三个导入角色配置应通过校验: %v", err)
	}
}

// TestChapterPipeline_ValidationAndDetection 验证 chapter_pipeline 开关的校验与启用判定。
func TestChapterPipeline_ValidationAndDetection(t *testing.T) {
	base := func() Config {
		return Config{
			Provider:  "openrouter",
			ModelName: "test-model",
			Providers: map[string]ProviderConfig{
				"openrouter": {APIKey: "sk-test-123456"},
			},
		}
	}

	// 1. 非法 pipeline 值拒绝
	bad := base()
	bad.ChapterPipeline = "unknown_pipeline"
	if err := bad.ValidateBase(); err == nil {
		t.Fatal("非法 chapter_pipeline 应被拒绝")
	}

	// 2. 合法值通过校验
	ok := base()
	ok.ChapterPipeline = "ds_mimo_critic"
	if err := ok.ValidateBase(); err != nil {
		t.Fatalf("chapter_pipeline=ds_mimo_critic 应通过校验: %v", err)
	}

	// 3. 启用判定：显式开关
	if !ok.ChapterPipelineEnabled() {
		t.Fatal("chapter_pipeline=ds_mimo_critic 应视为启用精修流水线")
	}

	// 4. 启用判定：roles.polisher 显式配置（无显式开关）
	byRole := base()
	byRole.Roles = map[string]RoleConfig{
		"polisher": {Provider: "openrouter", Model: "mimo-polisher"},
	}
	if !byRole.ChapterPipelineEnabled() {
		t.Fatal("显式配置 roles.polisher 应视为启用精修流水线")
	}
	if err := byRole.ValidateBase(); err != nil {
		t.Fatalf("roles.polisher 配置应通过校验: %v", err)
	}

	// 5. 默认关闭：无开关、无 polisher 角色 → 不启用（旧项目行为不变）
	if base().ChapterPipelineEnabled() {
		t.Fatal("默认配置不应启用精修流水线")
	}
}

func TestValidateBaseNotifyEventsMatchRuntimeContract(t *testing.T) {
	validConfig := func(events []string) Config {
		return Config{
			Provider:  "openrouter",
			ModelName: "test-model",
			Providers: map[string]ProviderConfig{
				"openrouter": {APIKey: "sk-test-123456"},
			},
			Notify: NotifyConfig{Events: events},
		}
	}

	cfg := validConfig(notify.Kinds())
	if err := cfg.ValidateBase(); err != nil {
		t.Fatalf("当前通知事件契约应全部通过配置校验: %v", err)
	}

	cfg = validConfig([]string{"repeat"})
	if err := cfg.ValidateBase(); !errors.Is(err, errs.ErrConfig) {
		t.Fatalf("旧 repeat 事件应被拒绝，得到: %v", err)
	}
}

func TestProviderStreamIdleTimeoutValue(t *testing.T) {
	cases := []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{"", defaultStreamIdleTimeout, false},
		{"900s", 15 * time.Minute, false},
		{"15m", 15 * time.Minute, false},
		{"abc", 0, true},
		{"-5s", 0, true},
		{"0", 0, true}, // 不提供"关闭看门狗"——真死流需要有限界
	}
	for _, c := range cases {
		got, err := ProviderConfig{StreamIdleTimeout: c.in}.StreamIdleTimeoutValue()
		if c.wantErr {
			if err == nil {
				t.Errorf("%q 应报错", c.in)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("%q = (%v, %v), want %v", c.in, got, err, c.want)
		}
	}
}

func TestValidateBaseRejectsBadStreamIdleTimeout(t *testing.T) {
	cfg := Config{
		Provider:  "openrouter",
		ModelName: "test-model",
		Providers: map[string]ProviderConfig{
			"openrouter": {APIKey: "sk-test-123456", StreamIdleTimeout: "fast"},
		},
	}
	if err := cfg.ValidateBase(); !errors.Is(err, errs.ErrConfig) {
		t.Fatalf("非法 stream_idle_timeout 应拒绝并包装 ErrConfig，得到: %v", err)
	}
}
