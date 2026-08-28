package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"unicode/utf8"

	"github.com/voocel/agentcore/schema"
	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/errs"
	"github.com/voocel/ainovel-cli/internal/projectprofile"
	"github.com/voocel/ainovel-cli/internal/store"
)

// DraftChapterTool 写入整章草稿，替代旧的 write_scene + polish_chapter 流水线。
// Agent 自主决定一次写完还是分批续写。
type DraftChapterTool struct {
	store    *store.Store
	contract *projectprofile.SceneBeatContract
	// fsmConfig 是章节流水线强制状态机配置（BuildWorkers 注入）；Enabled 时
	// Execute 入口调用 RequireChapterAction 强制顺序（draft/edit→check→polish→…）。
	fsmConfig ChapterFSMConfig
}

func NewDraftChapterTool(store *store.Store, contract *projectprofile.SceneBeatContract) *DraftChapterTool {
	return &DraftChapterTool{store: store, contract: contract}
}

// SetChapterFSMConfig 注入章节流水线强制状态机配置（BuildWorkers 调用）。
func (t *DraftChapterTool) SetChapterFSMConfig(cfg ChapterFSMConfig) { t.fsmConfig = cfg }

// FSMConfig 返回注入的章节流水线配置（构建/测试诊断用）。
func (t *DraftChapterTool) FSMConfig() ChapterFSMConfig { return t.fsmConfig }

func (t *DraftChapterTool) Name() string { return "draft_chapter" }
func (t *DraftChapterTool) Description() string {
	return "写入章节正文。mode=write 覆盖写入整章，mode=append 追加到现有草稿（续写/修改）"
}
func (t *DraftChapterTool) Label() string { return "写入章节" }

func (t *DraftChapterTool) ReadOnly(_ json.RawMessage) bool        { return false }
func (t *DraftChapterTool) ConcurrencySafe(_ json.RawMessage) bool { return false }

func (t *DraftChapterTool) Schema() map[string]any {
	return schema.Object(
		schema.Property("chapter", schema.Int("章节号")).Required(),
		schema.Property("content", schema.String("章节正文")).Required(),
		schema.Property("mode", schema.Enum("写入模式", "write", "append")).Required(),
	)
}

func (t *DraftChapterTool) StrictSchema() bool { return true }

// Execute 首先验证目标章的 outline entry 场景符合契约，
// 验证通过后才进行任何写入。
func (t *DraftChapterTool) Execute(_ context.Context, args json.RawMessage) (json.RawMessage, error) {
	args = normalizeIntegerStringFields(args, "chapter")
	var a struct {
		Chapter int    `json:"chapter"`
		Content string `json:"content"`
		Mode    string `json:"mode"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("invalid args: %w: %w", errs.ErrToolArgs, err)
	}
	if a.Chapter <= 0 {
		return nil, fmt.Errorf("chapter must be > 0: %w", errs.ErrToolArgs)
	}
	if a.Content == "" {
		return nil, fmt.Errorf("content must not be empty: %w", errs.ErrToolArgs)
	}
	// mode 显式三分支（数据安全）：只接受 write/append，未知值或直接调用一律拒绝，
	// 绝不静默走 write 覆盖已有草稿。
	if a.Mode != "write" && a.Mode != "append" {
		return nil, fmt.Errorf("draft_chapter: mode 必须是 write 或 append，收到 %q: %w", a.Mode, errs.ErrToolArgs)
	}

	// 预检：使用共享 ValidateOutlineEntry 验证目标章场景
	if t.contract != nil {
		if err := ValidateOutlineEntry(t.store, t.contract, a.Chapter); err != nil {
			return nil, fmt.Errorf("draft_chapter: %w", err)
		}
	}

	if err := t.store.Progress.ValidateChapterWork(a.Chapter); err != nil {
		return nil, err
	}
	if err := EnsureChapterExpanded(t.store, a.Chapter); err != nil {
		return nil, err
	}
	// 章节流水线强制状态机（Enabled 时）：needs_draft/draft_dirty/revision_open
	// 允许 draft；needs_polish/needs_review/needs_commit 等阶段拒绝越权修改。
	if err := RequireChapterAction(t.store, a.Chapter, ChapterActionDraft, t.fsmConfig); err != nil {
		return nil, fmt.Errorf("draft_chapter: %w", err)
	}
	if err := CheckStyleReviewMutationGuard(t.store, a.Chapter); err != nil {
		return nil, fmt.Errorf("draft_chapter: %w", err)
	}
	if t.store.Progress.IsChapterCompleted(a.Chapter) {
		progress, _ := t.store.Progress.Load()
		inRewriteQueue := progress != nil && slices.Contains(progress.PendingRewrites, a.Chapter)
		if !inRewriteQueue {
			return json.Marshal(map[string]any{
				"chapter":   a.Chapter,
				"skipped":   true,
				"completed": true,
				"reason":    fmt.Sprintf("第 %d 章已提交完成，不能覆盖", a.Chapter),
			})
		}
	}
	// Once the initial draft is known to be short for the sole mechanical
	// reason of the lower bound, reject another full overwrite before it can
	// create a checkpoint. This leaves rewrite/review/error paths untouched.
	if t.fsmConfig.Enabled && a.Mode == "write" {
		if existing, loadErr := t.store.Drafts.LoadDraft(a.Chapter); loadErr == nil && existing != "" {
			wordCount := utf8.RuneCountInString(existing)
			rng := chapterWordRange(t.store)
			if shouldForceAppendUnderMin(t.store, a.Chapter, existing, wordCount) {
				deficit := 0
				if rng != nil && rng.Min > wordCount {
					deficit = rng.Min - wordCount
				}
				return nil, fmt.Errorf(
					"draft_chapter: 第 %d 章当前草稿 %d 字，唯一 error 是低于最小字数（还差 %d 字）；请使用 mode=append 续写，不要再次 mode=write 整章覆盖: %w",
					a.Chapter, wordCount, deficit, errs.ErrToolPrecondition,
				)
			}
			// 现有草稿已达标但本次 write 的新内容不达标：拒绝覆盖，防止
			// 达标草稿被单次输出习惯偏短的整章重写打回不达标（826/850 类循环）。
			// 豁免条件：
			//   - 活跃评审 ledger（revision_open 等）：整章覆盖合法
			//   - 重写队列且现有草稿未达标：重写刚开始，允许整章覆盖
			//   - FSM 强制 draft（required=draft_chapter）：合法动作（827 死锁修复）
			// 不豁免：重写队列但现有草稿已达标 → 防倒退（850 循环）
			if !hasActiveReviewLedger(t.store, a.Chapter) {
				inRewrite := chapterInRewriteQueue(t.store, a.Chapter)
				// 进入守卫的条件：非重写队列，或重写队列但现有草稿已达标。
				// 重写队列 + 未达标草稿 = 重写刚开始，豁免整章覆盖（不得按
				// 字数误伤）；重写队列 + 已达标草稿 = 防倒退（850 循环）。
				if !inRewrite || rng == nil || wordCount >= rng.Min {
					// FSM 强制 draft 时（needs_edit/needs_draft 的 required=draft_chapter，
					// 如 error 级机械违规待修）write 是 FSM 要求的合法动作，不得按
					// 字数拒绝——否则 FSM 与守卫互相矛盾，模型无路可走（827 死锁）。
					// write 后 next_step 会引导 append 补字，不会形成倒退循环。
					fsmForcesDraft := false
					if decision, err := ResolveChapterStage(t.store, a.Chapter, t.fsmConfig); err == nil {
						fsmForcesDraft = decision.Required == ChapterActionDraft
					}
					if !fsmForcesDraft {
						if rng != nil && rng.Min > 0 &&
							wordCount >= rng.Min && utf8.RuneCountInString(a.Content) < rng.Min {
							return nil, fmt.Errorf(
								"draft_chapter: 第 %d 章现有草稿 %d 字已达标，本次 write 内容仅 %d 字（低于下限 %d）；请改用 mode=append 在现有草稿上续写，或先 check_consistency 确认结构问题后再决定是否整章重写: %w",
								a.Chapter, wordCount, utf8.RuneCountInString(a.Content), rng.Min, errs.ErrToolPrecondition,
							)
						}
					}
				}
			}
		}
	}

	if err := t.store.Progress.StartChapter(a.Chapter); err != nil {
		return nil, fmt.Errorf("mark chapter in progress: %w", err)
	}

	switch a.Mode {
	case "write":
		if err := t.store.Drafts.SaveDraft(a.Chapter, a.Content); err != nil {
			return nil, fmt.Errorf("save draft: %w", err)
		}
		if _, err := t.store.Checkpoints.AppendArtifact(
			domain.ChapterScope(a.Chapter), "draft",
			fmt.Sprintf("drafts/%02d.draft.md", a.Chapter),
		); err != nil {
			return nil, fmt.Errorf("checkpoint draft: %w", err)
		}
		wc := utf8.RuneCountInString(a.Content)
		return json.Marshal(map[string]any{
			"written":    true,
			"chapter":    a.Chapter,
			"mode":       "write",
			"word_count": wc,
			"next_step":  draftNextStep(t.store, a.Chapter, wc),
		})
	case "append":
		if err := t.store.Drafts.AppendDraft(a.Chapter, a.Content); err != nil {
			return nil, fmt.Errorf("append draft: %w", err)
		}
		full, err := t.store.Drafts.LoadDraft(a.Chapter)
		if err != nil {
			return nil, fmt.Errorf("load draft after append: %w", err)
		}
		if _, err := t.store.Checkpoints.AppendArtifact(
			domain.ChapterScope(a.Chapter), "draft",
			fmt.Sprintf("drafts/%02d.draft.md", a.Chapter),
		); err != nil {
			return nil, fmt.Errorf("checkpoint draft: %w", err)
		}
		wc := utf8.RuneCountInString(full)
		return json.Marshal(map[string]any{
			"written":    true,
			"chapter":    a.Chapter,
			"mode":       "append",
			"word_count": wc,
			"next_step":  draftNextStep(t.store, a.Chapter, wc),
		})
	default: // 理论不可达（Execute 入口已校验），纵深防御：新增 mode 不得静默降级为 write
		return nil, fmt.Errorf("draft_chapter: mode 必须是 write 或 append，收到 %q: %w", a.Mode, errs.ErrToolArgs)
	}
}

// hasActiveReviewLedger 报告章节是否存在活跃评审账本（CurrentStatus 非空）。
// 活跃评审周期（revision_open 等）时整章覆盖是合法操作（评审修订需要覆盖
// 旧草稿），不得被"达标草稿防倒退"守卫误伤。
func hasActiveReviewLedger(st *store.Store, chapter int) bool {
	ledger, err := st.StyleReview.Load(chapter)
	return err == nil && ledger != nil && ledger.CurrentStatus() != ""
}

// chapterInRewriteQueue 报告章节是否在 PendingRewrites 重写队列中。
func chapterInRewriteQueue(st *store.Store, chapter int) bool {
	progress, err := st.Progress.Load()
	return err == nil && progress != nil && slices.Contains(progress.PendingRewrites, chapter)
}

// draftNextStep 生成 draft_chapter 返回的 next_step 引导。字数低于下限时
// 直接提示缺口与 mode=append 续写，避免模型反复 mode=write 整章重写却
// 始终达不到字数（825 类死循环：模型单次输出习惯低于下限，且不主动
// check_consistency 就看不到 under_min 引导）。
func draftNextStep(st *store.Store, chapter, wordCount int) string {
	if rng := chapterWordRange(st); rng != nil && rng.Min > 0 && wordCount < rng.Min {
		return fmt.Sprintf(
			"当前草稿 %d 字，低于下限 %d（还差 %d 字）。请调用 draft_chapter(chapter=%d, mode=\"append\") 追加续写，不要用 mode=\"write\" 整章重写；追加后 read_chapter(source=\"draft\") 回读，再 check_consistency。",
			wordCount, rng.Min, rng.Min-wordCount, chapter,
		)
	}
	return fmt.Sprintf(
		"草稿已达标（%d 字）。请调用 check_consistency(chapter=%d) 自审结构/设定；若发现重复段落或结构问题，先看 check 返回的 rule_violations，再用 edit_chapter 定点修改或 draft_chapter(mode=write) 整章重写（整章重写必须一次输出达到字数下限）。禁止在 check_consistency 之前再次 draft_chapter 覆盖。",
		wordCount, chapter,
	)
}
