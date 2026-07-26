package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/voocel/agentcore/schema"
	agentcoretools "github.com/voocel/agentcore/tools"
	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/errs"
	"github.com/voocel/ainovel-cli/internal/store"
)

// editToolExecutor abstracts the inner edit tool to allow test injection.
type editToolExecutor interface {
	Execute(ctx context.Context, args json.RawMessage) (json.RawMessage, error)
}

// EditChapterTool 对章节草稿做定点字符串替换，适用于打磨场景。
// 相比 draft_chapter 整章重写，token 节省 10x+。
//
// 落盘契约：只改 drafts/{ch:02d}.draft.md，禁止直接改 chapters/（终稿由 commit_chapter 独占）。
// Seed 语义：drafts 不存在但 chapters 有 → 自动把 chapters 复制到 drafts 作为起点。
// 归属检查：章节已完成时必须在 PendingRewrites 队列中，否则拒绝。
//
// 本工具是 agentcore.EditTool 的薄封装，找-换逻辑（多级容错匹配、diff 输出、行尾/BOM 保留）
// 全部复用上游实现。
type EditChapterTool struct {
	store *store.Store
	edit  editToolExecutor
}

func NewEditChapterTool(s *store.Store) *EditChapterTool {
	return &EditChapterTool{
		store: s,
		edit:  agentcoretools.NewEdit(s.Dir(), nil),
	}
}

func (t *EditChapterTool) Name() string  { return "edit_chapter" }
func (t *EditChapterTool) Label() string { return "编辑章节" }

// ReadOnly 明确声明写工具（配合 ConcurrencySafeTool 防止被并发调度）。
func (t *EditChapterTool) ReadOnly(_ json.RawMessage) bool { return false }

// ConcurrencySafe 显式禁止并发：同章节多次 edit_chapter 并行会读-改-写竞态，
// 即使不同章节并行也会穿插 checkpoint 顺序。统一串行最稳。
func (t *EditChapterTool) ConcurrencySafe(_ json.RawMessage) bool { return false }

// ActivityDescription 供 UI/日志展示当前工具的活动描述。
func (t *EditChapterTool) ActivityDescription(_ json.RawMessage) string { return "编辑章节草稿" }

func (t *EditChapterTool) Description() string {
	return "对章节草稿做定点字符串替换（打磨场景首选，比 draft_chapter 整章重写省 token）。" +
		"找到 old_string 并替换为 new_string，要求精确匹配且唯一（多处匹配需 replace_all=true）。" +
		"写入 drafts/{ch}.draft.md；drafts 不存在时自动从 chapters 播种。" +
		"章节已完成且不在 PendingRewrites 队列中时拒绝执行。每次调用只改一处，多处修改请多次调用。"
}

func (t *EditChapterTool) Schema() map[string]any {
	return schema.Object(
		schema.Property("chapter", schema.Int("章节号")).Required(),
		schema.Property("old_string", schema.String("要替换的原文精确片段，多行需包含换行；不加 replace_all 时必须在草稿中唯一出现")).Required(),
		schema.Property("new_string", schema.String("替换后的新文本")).Required(),
		schema.Property("replace_all", schema.Bool("替换所有匹配（默认 false）")),
	)
}

func (t *EditChapterTool) Execute(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	args = normalizeIntegerStringFields(args, "chapter")
	var a struct {
		Chapter    int    `json:"chapter"`
		OldString  string `json:"old_string"`
		NewString  string `json:"new_string"`
		ReplaceAll bool   `json:"replace_all"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("invalid args: %w: %w", errs.ErrToolArgs, err)
	}
	if a.Chapter <= 0 {
		return nil, fmt.Errorf("chapter must be > 0: %w", errs.ErrToolArgs)
	}
	if a.OldString == "" {
		return nil, fmt.Errorf("old_string 不能为空: %w", errs.ErrToolArgs)
	}
	if a.OldString == a.NewString {
		return nil, fmt.Errorf("old_string 与 new_string 相同，无需修改: %w", errs.ErrToolArgs)
	}

	// critic 模式变异守卫
	if err := CheckStyleReviewMutationGuard(t.store, a.Chapter); err != nil {
		return nil, fmt.Errorf("edit_chapter: %w", err)
	}

	// 归属检查：已完成章节必须在重写队列中，避免污染终稿
	if t.store.Progress.IsChapterCompleted(a.Chapter) {
		progress, _ := t.store.Progress.Load()
		inRewriteQueue := progress != nil && slices.Contains(progress.PendingRewrites, a.Chapter)
		if !inRewriteQueue {
			return nil, fmt.Errorf("第 %d 章已完成且不在 PendingRewrites 队列中，不能编辑；需修改请先由 editor 评审触发重写/打磨: %w", a.Chapter, errs.ErrToolPrecondition)
		}
	}

	// Seed：drafts 不存在时从 chapters 复制一份作为起点
	if err := t.ensureDraft(a.Chapter); err != nil {
		return nil, err
	}

	// 读替换前草稿，用于字数差计算和变更范围检测
	oldDraft, err := t.store.Drafts.LoadDraft(a.Chapter)
	if err != nil {
		return nil, fmt.Errorf("read draft before edit: %w: %w", errs.ErrStoreRead, err)
	}
	oldWordCount := domain.WordCount(oldDraft)

	// 委托 agentcore.EditTool 完成找-换
	subArgs, _ := json.Marshal(map[string]any{
		"path":        fmt.Sprintf("drafts/%02d.draft.md", a.Chapter),
		"file_path":   fmt.Sprintf("drafts/%02d.draft.md", a.Chapter),
		"old_text":    a.OldString,
		"old_string":  a.OldString,
		"new_text":    a.NewString,
		"new_string":  a.NewString,
		"replace_all": a.ReplaceAll,
	})
	result, err := t.edit.Execute(ctx, subArgs)
	if err != nil {
		return nil, fmt.Errorf("apply edit: %w: %w", errs.ErrToolPrecondition, err)
	}

	if _, err := t.store.Checkpoints.AppendArtifact(
		domain.ChapterScope(a.Chapter), "edit",
		fmt.Sprintf("drafts/%02d.draft.md", a.Chapter),
	); err != nil {
		return nil, fmt.Errorf("checkpoint edit: %w: %w", errs.ErrStoreWrite, err)
	}

	// 读替换后草稿，计算结构化反馈
	newDraft, err := t.store.Drafts.LoadDraft(a.Chapter)
	if err != nil {
		return nil, fmt.Errorf("read draft after edit: %w: %w", errs.ErrStoreRead, err)
	}
	chapterWordCount := domain.WordCount(newDraft)
	wordCountDelta := chapterWordCount - oldWordCount
	hash := sha256.Sum256([]byte(newDraft))
	draftDigest := "sha256:" + hex.EncodeToString(hash[:])

	// 通过比较新旧草稿确定实际变更字节范围（不受上游模糊匹配/CRLF/缩进影响）
	changeStart, changeEnd, changeOK := findChangedRange(oldDraft, newDraft)

	// 计算受影响上下文
	var firstCtx string
	if changeOK {
		firstCtx = extractContextAt(newDraft, changeStart, changeEnd-changeStart)
	}
	// 回退：仅当 newText 非空时才使用字符串搜索（空字符串会误定位到文件开头）
	if firstCtx == "" && a.NewString != "" {
		firstCtx = extractAffectedContext(newDraft, a.NewString)
	}

	// 构造公共响应字段（始终存在）
	commonResp := map[string]any{
		"chapter":                      a.Chapter,
		"affected_context":             firstCtx,
		"draft_digest":                 draftDigest,
		"chapter_word_count":           chapterWordCount,
		"word_count_delta":             wordCountDelta,
		"requires_consistency_recheck": true,
		"next_step":                    "edit 已落盘。每次 edit 后**必须**调 check_consistency 重新核验；通过后按 mode 执行（off 模式 check→commit，critic 模式需 review_style→terminal→commit）",
	}
	// replace_all 时变更范围是整体包络，只能提供单一保守上下文
	if a.ReplaceAll {
		commonResp["affected_contexts_complete"] = false
	}

	// 合并上游结果并附加结构化字段
	var passthrough map[string]any
	if err := json.Unmarshal(result, &passthrough); err != nil {
		// 非 JSON 上游结果：文件已改变且 checkpoint 成功 → 视为编辑成功
		// 设置标记而非回传原始文本（避免泄露），不返回 error
		commonResp["upstream_result_unparsed"] = true
		resp, marshalErr := json.Marshal(commonResp)
		if marshalErr != nil {
			return nil, fmt.Errorf("marshal response: %w: %w", errs.ErrStoreWrite, marshalErr)
		}
		return resp, nil
	}
	for k, v := range commonResp {
		passthrough[k] = v
	}
	return json.Marshal(passthrough)
}

// ensureDraft 保证 drafts/{ch}.draft.md 存在：
//   - 已有草稿 → 直接返回
//   - 无草稿但有终稿 → 把终稿复制到 drafts 作为修改起点（常见于打磨场景）
//   - 都没有 → 报错，提示先用 draft_chapter 创建初稿
func (t *EditChapterTool) ensureDraft(chapter int) error {
	draft, err := t.store.Drafts.LoadDraft(chapter)
	if err != nil {
		return fmt.Errorf("load draft: %w: %w", errs.ErrStoreRead, err)
	}
	if draft != "" {
		return nil
	}
	text, err := t.store.Drafts.LoadChapterText(chapter)
	if err != nil {
		return fmt.Errorf("load chapter: %w: %w", errs.ErrStoreRead, err)
	}
	if text == "" {
		return fmt.Errorf("第 %d 章无草稿也无终稿，请先调 draft_chapter(mode=write, chapter=%d) 创建初稿: %w", chapter, chapter, errs.ErrToolPrecondition)
	}
	if err := t.store.Drafts.SaveDraft(chapter, text); err != nil {
		return fmt.Errorf("seed draft from chapter: %w: %w", errs.ErrStoreWrite, err)
	}
	return nil
}

// maxContextRunes 是 affected_context 的安全截断上限。
const maxContextRunes = 500

// findChangedRange 通过比较新旧草稿找到 newDraft 中实际发生变更的字节范围。
//
// 使用最长公共前缀/后缀扫描确定差异区间，不依赖 oldString 的文字匹配，
// 因此不受上游模糊/CRLF/block-anchor/indent-aware 匹配的影响。
// 对单次替换返回精确替换范围；对 replace_all 返回覆盖所有变更的整体包络。
// 当两草稿相同时返回 ok=false。
func findChangedRange(oldDraft, newDraft string) (start, end int, ok bool) {
	if oldDraft == newDraft {
		return 0, 0, false
	}
	n, m := len(oldDraft), len(newDraft)

	// 最长公共前缀
	prefix := 0
	for prefix < n && prefix < m && oldDraft[prefix] == newDraft[prefix] {
		prefix++
	}

	// 最长公共后缀（不与前缀重叠）
	sufO, sufN := n, m
	for sufO > prefix && sufN > prefix && oldDraft[sufO-1] == newDraft[sufN-1] {
		sufO--
		sufN--
	}

	start = prefix
	end = sufN
	if end < start {
		end = start
	}
	return start, end, true
}

// extractContextAt 在草稿的指定字节偏移处提取段落上下文。
// context 以 [pos, pos+textLen) 为中心，截断至多 maxContextRunes 个 rune。
func extractContextAt(draft string, pos, textLen int) string {
	if pos < 0 || pos+textLen > len(draft) {
		return ""
	}

	// 向左找到段落起始（\n\n 或文本开头）
	before := draft[:pos]
	paraStart := strings.LastIndex(before, "\n\n")
	if paraStart < 0 {
		paraStart = 0
	} else {
		paraStart += 2 // 跳过 \n\n
	}

	// 向右找到段落结束（\n\n 或文本结尾）
	after := draft[pos+textLen:]
	paraEndRel := strings.Index(after, "\n\n")
	var paraEnd int
	if paraEndRel < 0 {
		paraEnd = len(draft)
	} else {
		paraEnd = pos + textLen + paraEndRel
	}

	paragraph := draft[paraStart:paraEnd]
	runes := []rune(paragraph)
	if len(runes) <= maxContextRunes {
		return paragraph
	}

	// 段落超长时以目标位置为中心截取
	relOffset := utf8.RuneCountInString(draft[:pos]) - utf8.RuneCountInString(draft[:paraStart])
	half := maxContextRunes / 2
	start := relOffset - half
	if start < 0 {
		start = 0
	}
	end := start + maxContextRunes
	if end > len(runes) {
		end = len(runes)
		start = end - maxContextRunes
		if start < 0 {
			start = 0
		}
	}
	return string(runes[start:end])
}

// extractAffectedContext 从新草稿中搜索 newText 提取上下文（回退用）。
//
// 警告：newText=="" 时 strings.Index 返回 0 导致错误定位到文件开头，
// 调用方必须保证 newText 非空才使用此函数。
func extractAffectedContext(draft, newText string) string {
	idx := strings.Index(draft, newText)
	if idx < 0 {
		return ""
	}
	return extractContextAt(draft, idx, len(newText))
}
