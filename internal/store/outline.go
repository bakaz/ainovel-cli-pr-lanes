package store

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"time"

	"github.com/voocel/ainovel-cli/internal/domain"
)

// OutlineStore 管理故事前提、大纲（扁平/分层）和指南针。
type OutlineStore struct{ io *IO }

func NewOutlineStore(io *IO) *OutlineStore { return &OutlineStore{io: io} }

// SavePremise 保存故事前提到 premise.md。
func (s *OutlineStore) SavePremise(content string) error {
	return s.io.WriteMarkdown("premise.md", content)
}

// LoadPremise 读取 premise.md。不存在时返回空字符串。
func (s *OutlineStore) LoadPremise() (string, error) {
	data, err := s.io.ReadFile("premise.md")
	if os.IsNotExist(err) {
		return "", nil
	}
	return string(data), err
}

// SaveOutline 同时保存 outline.json 和 outline.md（原子写入）。
func (s *OutlineStore) SaveOutline(entries []domain.OutlineEntry) error {
	return s.io.WithWriteLock(func() error {
		if err := s.io.WriteJSONUnlocked("outline.json", entries); err != nil {
			return err
		}
		return s.io.WriteMarkdownUnlocked("outline.md", renderOutline(entries))
	})
}

// LoadOutline 从 outline.json 读取结构化大纲。
func (s *OutlineStore) LoadOutline() ([]domain.OutlineEntry, error) {
	var entries []domain.OutlineEntry
	if err := s.io.ReadJSON("outline.json", &entries); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return entries, nil
}

// GetChapterOutline 获取指定章节的大纲条目。
func (s *OutlineStore) GetChapterOutline(chapter int) (*domain.OutlineEntry, error) {
	entries, err := s.LoadOutline()
	if err != nil {
		return nil, err
	}
	for i := range entries {
		if entries[i].Chapter == chapter {
			return &entries[i], nil
		}
	}
	return nil, fmt.Errorf("chapter %d not found in outline", chapter)
}

// SaveLayeredOutline 保存分层大纲（长篇模式，原子写入）。
func (s *OutlineStore) SaveLayeredOutline(volumes []domain.VolumeOutline) error {
	return s.io.WithWriteLock(func() error {
		if err := validateAndNormalizeLayeredOutline(volumes); err != nil {
			return err
		}
		if err := s.io.WriteJSONUnlocked("layered_outline.json", volumes); err != nil {
			return err
		}
		return s.io.WriteMarkdownUnlocked("layered_outline.md", renderLayeredOutline(volumes))
	})
}

// LoadLayeredOutline 读取分层大纲，并在内存中校验编号/拓扑契约后返回。
// 持久化的 Chapter==0 会在内存补齐（不写盘）；非零错号和非法拓扑 fail-closed。
func (s *OutlineStore) LoadLayeredOutline() ([]domain.VolumeOutline, error) {
	var volumes []domain.VolumeOutline
	if err := s.io.ReadJSON("layered_outline.json", &volumes); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if err := checkLayeredOutline(volumes); err != nil {
		return nil, fmt.Errorf("layered_outline 数据损坏：%w", err)
	}
	// 内存中补齐 Chapter==0（不下沉到磁盘）
	ch := 1
	for vi := range volumes {
		for ai := range volumes[vi].Arcs {
			if !volumes[vi].Arcs[ai].IsExpanded() {
				continue
			}
			for ci := range volumes[vi].Arcs[ai].Chapters {
				if volumes[vi].Arcs[ai].Chapters[ci].Chapter == 0 {
					volumes[vi].Arcs[ai].Chapters[ci].Chapter = ch
				}
				ch++
			}
		}
	}
	return volumes, nil
}

// ValidateLayeredOutline 公开只读校验入口，供 save_foundation Phase 1 等外部路径
// 在写入任何状态之前完成编号/拓扑验证。不修改传入数据。
func (s *OutlineStore) ValidateLayeredOutline(volumes []domain.VolumeOutline) error {
	return checkLayeredOutline(volumes)
}

// ValidateVolumesLayeredOutline 无需 Store 对象的全局拓扑/编号校验，复用同一规则集。
// 供 migratev3、host/imp 等无 Store 实例的路径在持久化/计算前直接调用。
func ValidateVolumesLayeredOutline(volumes []domain.VolumeOutline) error {
	return checkLayeredOutline(volumes)
}

// ClearLayeredOutline 清理分层大纲文件。
func (s *OutlineStore) ClearLayeredOutline() error {
	return s.io.WithWriteLock(func() error {
		if err := s.io.RemoveFileUnlocked("layered_outline.json"); err != nil {
			return err
		}
		return s.io.RemoveFileUnlocked("layered_outline.md")
	})
}

// GetChapterFromLayered 从分层大纲中按全局章节号查找。
func (s *OutlineStore) GetChapterFromLayered(chapter int) (*domain.OutlineEntry, error) {
	volumes, err := s.LoadLayeredOutline()
	if err != nil {
		return nil, err
	}
	ch := 1
	for _, v := range volumes {
		for _, a := range v.Arcs {
			for i := range a.Chapters {
				if ch == chapter {
					e := a.Chapters[i]
					e.Chapter = ch
					return &e, nil
				}
				ch++
			}
		}
	}
	return nil, fmt.Errorf("chapter %d not found in layered outline", chapter)
}

// LocateChapter 根据全局章节号定位所在的卷和弧。
func (s *OutlineStore) LocateChapter(chapter int) (volume, arc int, err error) {
	volumes, err := s.LoadLayeredOutline()
	if err != nil {
		return 0, 0, err
	}
	ch := 1
	for _, v := range volumes {
		for _, a := range v.Arcs {
			for range a.Chapters {
				if ch == chapter {
					return v.Index, a.Index, nil
				}
				ch++
			}
		}
	}
	return 0, 0, fmt.Errorf("chapter %d not found in layered outline", chapter)
}

// ArcBoundary 弧边界信息。
type ArcBoundary struct {
	IsArcEnd       bool
	IsVolumeEnd    bool
	Volume         int
	Arc            int
	NextVolume     int
	NextArc        int
	NeedsExpansion bool
	NeedsNewVolume bool // 卷末且当前 layered_outline 没有下一卷
}

// HasNextArc 是否还有后续弧。
func (b *ArcBoundary) HasNextArc() bool {
	return b.NextVolume > 0 || b.NextArc > 0
}

// CheckArcBoundary 检查某章是否为弧/卷的最后一章。
func (s *OutlineStore) CheckArcBoundary(chapter int) (*ArcBoundary, error) {
	volumes, err := s.LoadLayeredOutline()
	if err != nil || len(volumes) == 0 {
		return nil, err
	}

	type arcPos struct {
		volIdx, arcIdx int
		volume, arc    int
		chInArc        int
		arcLen         int
	}

	ch := 1
	var cur *arcPos
	for vi, v := range volumes {
		for ai, a := range v.Arcs {
			for ci := range a.Chapters {
				if ch == chapter {
					cur = &arcPos{
						volIdx:  vi,
						arcIdx:  ai,
						volume:  v.Index,
						arc:     a.Index,
						chInArc: ci,
						arcLen:  len(a.Chapters),
					}
				}
				ch++
			}
		}
	}
	if cur == nil {
		return nil, nil
	}

	b := &ArcBoundary{
		Volume: cur.volume,
		Arc:    cur.arc,
	}

	isLastChInArc := cur.chInArc == cur.arcLen-1
	isLastArcInVol := cur.arcIdx == len(volumes[cur.volIdx].Arcs)-1

	// Next*/NeedsExpansion/NeedsNewVolume 只在弧末才有意义，否则会让协调者误以为要提前展开下一弧。
	if !isLastChInArc {
		return b, nil
	}

	b.IsArcEnd = true
	if isLastArcInVol {
		b.IsVolumeEnd = true
	}

	found := false
	for vi := cur.volIdx; vi < len(volumes); vi++ {
		startArc := 0
		if vi == cur.volIdx {
			startArc = cur.arcIdx + 1
		}
		for ai := startArc; ai < len(volumes[vi].Arcs); ai++ {
			b.NextVolume = volumes[vi].Index
			b.NextArc = volumes[vi].Arcs[ai].Index
			b.NeedsExpansion = !volumes[vi].Arcs[ai].IsExpanded()
			found = true
			break
		}
		if found {
			break
		}
	}

	if b.IsVolumeEnd && !found {
		b.NeedsNewVolume = true
	}

	return b, nil
}

// expandArcUnlocked 内部方法，在 Store.ExpandArc 跨域协调中调用。
func (s *OutlineStore) expandArcUnlocked(volumeIdx, arcIdx int, expansion domain.ArcExpansion) ([]domain.VolumeOutline, error) {
	if strings.TrimSpace(expansion.Title) == "" {
		return nil, fmt.Errorf("弧标题不能为空")
	}
	if strings.TrimSpace(expansion.Goal) == "" {
		return nil, fmt.Errorf("弧目标不能为空")
	}
	if len(expansion.Chapters) == 0 {
		return nil, fmt.Errorf("展开弧必须至少包含一章")
	}

	var volumes []domain.VolumeOutline
	if err := s.io.ReadJSONUnlocked("layered_outline.json", &volumes); err != nil {
		return nil, fmt.Errorf("load layered_outline: %w", err)
	}
	found := false
	for vi := range volumes {
		if volumes[vi].Index != volumeIdx {
			continue
		}
		for ai := range volumes[vi].Arcs {
			if volumes[vi].Arcs[ai].Index != arcIdx {
				continue
			}
			if volumes[vi].Arcs[ai].IsExpanded() {
				current := domain.ArcExpansion{
					Title:    volumes[vi].Arcs[ai].Title,
					Goal:     volumes[vi].Arcs[ai].Goal,
					Chapters: volumes[vi].Arcs[ai].Chapters,
				}
				if reflect.DeepEqual(current, expansion) {
					return volumes, nil
				}
				return nil, fmt.Errorf("arc already expanded: volume=%d, arc=%d", volumeIdx, arcIdx)
			}
			volumes[vi].Arcs[ai].Title = expansion.Title
			volumes[vi].Arcs[ai].Goal = expansion.Goal
			volumes[vi].Arcs[ai].Chapters = expansion.Chapters
			volumes[vi].Arcs[ai].EstimatedChapters = 0
			found = true
			break
		}
		if found {
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("arc not found: volume=%d, arc=%d", volumeIdx, arcIdx)
	}
	if err := validateAndNormalizeLayeredOutline(volumes); err != nil {
		return nil, err
	}
	if err := s.io.WriteJSONUnlocked("layered_outline.json", volumes); err != nil {
		return nil, err
	}
	if err := s.io.WriteMarkdownUnlocked("layered_outline.md", renderLayeredOutline(volumes)); err != nil {
		return nil, err
	}
	flat := domain.FlattenOutline(volumes)
	if err := s.io.WriteJSONUnlocked("outline.json", flat); err != nil {
		return nil, err
	}
	if err := s.io.WriteMarkdownUnlocked("outline.md", renderOutline(flat)); err != nil {
		return nil, err
	}
	return volumes, nil
}

// appendVolumeUnlocked 内部方法，在 Store.AppendVolume 跨域协调中调用。
func (s *OutlineStore) appendVolumeUnlocked(vol domain.VolumeOutline) ([]domain.VolumeOutline, error) {
	var volumes []domain.VolumeOutline
	if err := s.io.ReadJSONUnlocked("layered_outline.json", &volumes); err != nil {
		return nil, fmt.Errorf("load layered_outline: %w", err)
	}
	if err := validateAppendVolume(volumes, vol); err != nil {
		return nil, err
	}
	volumes = append(volumes, vol)
	if err := validateAndNormalizeLayeredOutline(volumes); err != nil {
		return nil, err
	}
	if err := s.io.WriteJSONUnlocked("layered_outline.json", volumes); err != nil {
		return nil, err
	}
	if err := s.io.WriteMarkdownUnlocked("layered_outline.md", renderLayeredOutline(volumes)); err != nil {
		return nil, err
	}
	flat := domain.FlattenOutline(volumes)
	if err := s.io.WriteJSONUnlocked("outline.json", flat); err != nil {
		return nil, err
	}
	if err := s.io.WriteMarkdownUnlocked("outline.md", renderOutline(flat)); err != nil {
		return nil, err
	}
	return volumes, nil
}

// validateAndNormalizeLayeredOutline 校验全书分层大纲拓扑与章节编号契约，
// 并自动补齐零号章节。全书单一规划前沿：一旦出现骨架弧，之后任意卷的展开弧均拒绝。
// 契约：
//   - 已展开弧只能是前缀，按全书 volume→arc 顺序维持 sawSkeleton。
//   - Chapter == 0 时自动补入按卷→弧→章顺序派生的结构位置（从 1 连续）。
//   - Chapter != 0 且与预期结构位置不一致时拒绝整个保存，零写入。
func validateAndNormalizeLayeredOutline(volumes []domain.VolumeOutline) error {
	ch := 1
	sawSkeleton := false
	for vi := range volumes {
		for ai := range volumes[vi].Arcs {
			if !volumes[vi].Arcs[ai].IsExpanded() {
				sawSkeleton = true
				continue
			}
			if sawSkeleton {
				return fmt.Errorf("卷 %d（索引 %d）弧 %d 已展开，但全书范围已有骨架弧在之前出现：已展开弧必须是全书前缀",
					volumes[vi].Index, vi, volumes[vi].Arcs[ai].Index)
			}
			for ci := range volumes[vi].Arcs[ai].Chapters {
				got := volumes[vi].Arcs[ai].Chapters[ci].Chapter
				if got == 0 {
					volumes[vi].Arcs[ai].Chapters[ci].Chapter = ch
				} else if got != ch {
					return fmt.Errorf("卷 %d（索引 %d）弧 %d 第 %d 章编号 %d 与预期结构位置 %d 不一致，已拒绝保存",
						volumes[vi].Index, vi, volumes[vi].Arcs[ai].Index, ci+1, got, ch)
				}
				ch++
			}
		}
	}
	return nil
}

// checkLayeredOutline 不修改数据的只读校验：验证全书拓扑合法性与章节编号正确性。
// Chapter==0 接受（调用方自行决定写入前 Normalize）；非零错号和拓扑非法则返回错误。
// 全书单一规划前沿：一旦出现骨架弧，之后任意卷的展开弧均拒绝。
// 供读取路径和 save_foundation Phase 1 使用。
func checkLayeredOutline(volumes []domain.VolumeOutline) error {
	ch := 1
	sawSkeleton := false
	for vi := range volumes {
		for ai := range volumes[vi].Arcs {
			if !volumes[vi].Arcs[ai].IsExpanded() {
				sawSkeleton = true
				continue
			}
			if sawSkeleton {
				return fmt.Errorf("卷 %d（索引 %d）弧 %d 已展开，但全书范围已有骨架弧在之前出现",
					volumes[vi].Index, vi, volumes[vi].Arcs[ai].Index)
			}
			for ci := range volumes[vi].Arcs[ai].Chapters {
				got := volumes[vi].Arcs[ai].Chapters[ci].Chapter
				if got != 0 && got != ch {
					return fmt.Errorf("卷 %d（索引 %d）弧 %d 第 %d 章编号 %d 与预期结构位置 %d 不一致",
						volumes[vi].Index, vi, volumes[vi].Arcs[ai].Index, ci+1, got, ch)
				}
				ch++
			}
		}
	}
	return nil
}

func validateAppendVolume(existing []domain.VolumeOutline, vol domain.VolumeOutline) error {
	if len(vol.Arcs) == 0 {
		return fmt.Errorf("新卷必须至少包含一个弧")
	}
	if !vol.Arcs[0].IsExpanded() {
		return fmt.Errorf("新卷的首弧必须包含详细章节")
	}
	if len(existing) > 0 {
		maxIdx := existing[len(existing)-1].Index
		if vol.Index <= maxIdx {
			return fmt.Errorf("卷 Index %d 必须大于现有最大值 %d", vol.Index, maxIdx)
		}
		// 全书单一规划前沿：存在骨架弧时只能追加骨架卷，不可追加展开卷
		for _, v := range existing {
			for _, a := range v.Arcs {
				if !a.IsExpanded() {
					return fmt.Errorf("已有骨架弧（卷 %d 弧 %d），不可追加包含展开弧的新卷。必须先展开所有骨架弧或追加骨架卷",
						v.Index, a.Index)
				}
			}
		}
	}
	return nil
}

// SaveCompass 保存终局方向指南针。
func (s *OutlineStore) SaveCompass(compass domain.StoryCompass) error {
	if strings.TrimSpace(compass.Long.EndingDirection) == "" {
		return fmt.Errorf("long.ending_direction 不能为空")
	}
	return s.io.WriteJSON("meta/compass.json", compass)
}

// LoadCompass 读取终局方向指南针。
func (s *OutlineStore) LoadCompass() (*domain.StoryCompass, error) {
	s.io.mu.RLock()
	defer s.io.mu.RUnlock()
	return s.loadCompassUnlocked()
}

func (s *OutlineStore) loadCompassUnlocked() (*domain.StoryCompass, error) {
	var c domain.StoryCompass
	if err := s.io.ReadJSONUnlocked("meta/compass.json", &c); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return &c, nil
}

// SaveCompassUnlocked 在已持有 io.mu 写锁时保存 compass。
func (s *OutlineStore) SaveCompassUnlocked(compass domain.StoryCompass) error {
	if strings.TrimSpace(compass.Long.EndingDirection) == "" {
		return fmt.Errorf("long.ending_direction 不能为空")
	}
	return s.io.WriteJSONUnlocked("meta/compass.json", compass)
}

func renderLayeredOutline(volumes []domain.VolumeOutline) string {
	var b strings.Builder
	b.WriteString("# 分层大纲\n\n")
	ch := 1
	for _, v := range volumes {
		fmt.Fprintf(&b, "## 第 %d 卷：%s\n\n", v.Index, v.Title)
		fmt.Fprintf(&b, "**主题**：%s\n\n", v.Theme)
		for _, a := range v.Arcs {
			fmt.Fprintf(&b, "### 第 %d 弧：%s\n\n", a.Index, a.Title)
			fmt.Fprintf(&b, "**目标**：%s\n\n", a.Goal)
			if !a.IsExpanded() {
				fmt.Fprintf(&b, "*（待展开，预估 %d 章）*\n\n", a.EstimatedChapters)
				continue
			}
			for _, e := range a.Chapters {
				fmt.Fprintf(&b, "#### 第 %d 章：%s\n\n", ch, e.Title)
				fmt.Fprintf(&b, "**核心事件**：%s\n\n", e.CoreEvent)
				if e.Hook != "" {
					fmt.Fprintf(&b, "**钩子**：%s\n\n", e.Hook)
				}
				ch++
			}
		}
	}
	return b.String()
}

func renderOutline(entries []domain.OutlineEntry) string {
	var b strings.Builder
	b.WriteString("# 大纲\n\n")
	for _, e := range entries {
		fmt.Fprintf(&b, "## 第 %d 章：%s\n\n", e.Chapter, e.Title)
		fmt.Fprintf(&b, "**核心事件**：%s\n\n", e.CoreEvent)
		if e.Hook != "" {
			fmt.Fprintf(&b, "**钩子**：%s\n\n", e.Hook)
		}
		if len(e.Scenes) > 0 {
			b.WriteString("**场景**：\n")
			for i, sc := range e.Scenes {
				fmt.Fprintf(&b, "%d. %s\n", i+1, sc.Text())
			}
			b.WriteString("\n")
		}
	}
	return b.String()
}

// ── Writer 大纲反馈池 ──
//
// commit_chapter 的 feedback(偏离/建议)持久化于此,architect 下次结构操作
// (expand_arc / append_volume / update_compass)经 novel_context 消费后清空。
// 事实闭环:工具落盘 → 上下文注入 → 结构操作即消费(docs/engine-arbiter.md 阻断1)。

// ChapterFeedback 一条带章节号的大纲反馈。
type ChapterFeedback struct {
	Chapter    int    `json:"chapter"`
	Deviation  string `json:"deviation,omitempty"`
	Suggestion string `json:"suggestion,omitempty"`
	At         string `json:"at"`
}

const outlineFeedbackFile = "meta/outline_feedback.jsonl"

// AppendOutlineFeedback 追加一条 writer 反馈(best-effort 附属事实,不参与 commit 原子性)。
func (s *OutlineStore) AppendOutlineFeedback(fb ChapterFeedback) error {
	if fb.At == "" {
		fb.At = time.Now().Format(time.RFC3339)
	}
	data, err := json.Marshal(fb)
	if err != nil {
		return err
	}
	return s.io.AppendLine(outlineFeedbackFile, append(data, '\n'))
}

// LoadPendingOutlineFeedback 读取未消费的反馈(旧→新);损坏行跳过。
func (s *OutlineStore) LoadPendingOutlineFeedback() []ChapterFeedback {
	s.io.mu.RLock()
	defer s.io.mu.RUnlock()
	data, err := os.ReadFile(s.io.path(outlineFeedbackFile))
	if err != nil {
		return nil
	}
	var out []ChapterFeedback
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var fb ChapterFeedback
		if json.Unmarshal([]byte(line), &fb) == nil {
			out = append(out, fb)
		}
	}
	return out
}

// ClearOutlineFeedback 清空反馈池(architect 结构操作成功 = 反馈已被参考)。
// 缺口 3：改用 RemoveFileUnlocked（统一写守卫）替换直接 os.Remove——只读/未
// ready/Close 后的 Store 不得绕过守卫修改 workspace。
func (s *OutlineStore) ClearOutlineFeedback() error {
	s.io.mu.Lock()
	defer s.io.mu.Unlock()
	return s.io.RemoveFileUnlocked(outlineFeedbackFile)
}
