package store

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/voocel/ainovel-cli/internal/domain"
)

const styleReviewDir = "meta/style_review"

func styleReviewFile(chapter int) string {
	return filepath.Join(styleReviewDir, fmt.Sprintf("%02d.json", chapter))
}

// StyleReviewStore 管理每章风格评审账本的原子加载/保存/更新。
// Save 仅用于创建新账本（chapter 不存在时）；Update 是唯一的修改路径并强制
// append-only：必须保持历史前缀完全一致，仅追加恰好一个合法后续周期。
type StyleReviewStore struct {
	io *IO
}

func NewStyleReviewStore(io *IO) *StyleReviewStore {
	return &StyleReviewStore{io: io}
}

// Load 读取指定章节的风格评审账本。文件不存在时返回 (nil, nil)。
func (s *StyleReviewStore) Load(chapter int) (*domain.StyleReviewLedger, error) {
	if chapter <= 0 {
		return nil, fmt.Errorf("style review: chapter must be > 0, got %d", chapter)
	}
	s.io.mu.RLock()
	defer s.io.mu.RUnlock()
	return s.loadUnlocked(chapter)
}

func (s *StyleReviewStore) loadUnlocked(chapter int) (*domain.StyleReviewLedger, error) {
	var ledger domain.StyleReviewLedger
	if err := s.io.ReadJSONUnlocked(styleReviewFile(chapter), &ledger); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("style review: load chapter %d: %w", chapter, err)
	}
	if ledger.Chapter != chapter {
		return nil, fmt.Errorf("style review: chapter mismatch: path chapter %d, ledger chapter %d", chapter, ledger.Chapter)
	}
	if err := domain.ValidateLedger(&ledger); err != nil {
		return nil, fmt.Errorf("style review: chapter %d validation: %w", chapter, err)
	}
	return &ledger, nil
}

// Save 创建新账本。已存在时拒绝，强制 append-only 路径走 Update。
func (s *StyleReviewStore) Save(ledger domain.StyleReviewLedger) error {
	if err := domain.ValidateLedger(&ledger); err != nil {
		return err
	}
	s.io.mu.Lock()
	defer s.io.mu.Unlock()

	rel := styleReviewFile(ledger.Chapter)
	if _, err := os.Stat(s.io.path(rel)); err == nil {
		return fmt.Errorf("style review: chapter %d ledger already exists; use Update for modifications", ledger.Chapter)
	}

	return s.saveUnlocked(ledger)
}

func (s *StyleReviewStore) saveUnlocked(ledger domain.StyleReviewLedger) error {
	return s.io.WriteJSONUnlocked(styleReviewFile(ledger.Chapter), &ledger)
}

// Update 以 append-only 语义修改一章的账本。
// loader 收到当前账本的深度独立副本（可能为 nil），必须返回与现有历史完全一致
// 的前缀加上恰好一个合法后续周期，或返回 (nil, nil) 表示无操作。
// 修改/删除/替换任何已有周期、跳过编号、或添加超过一个周期都将被拒绝。
// 回调对传入副本的任意修改不会持久化，也不会影响 append-only 校验结果。
func (s *StyleReviewStore) Update(chapter int, loader func(ledger *domain.StyleReviewLedger) (*domain.StyleReviewLedger, error)) error {
	s.io.mu.Lock()
	defer s.io.mu.Unlock()

	current, err := s.loadUnlocked(chapter)
	if err != nil {
		return err
	}

	// 深克隆：一份作为不可变的原始快照用于 append-only 前缀校验，
	// 另一份传入回调（回调的任意修改不影响快照或持久化状态）。
	origSnapshot := current.DeepClone()
	callbackArg := current.DeepClone()

	updated, err := loader(callbackArg)
	if err != nil {
		return err
	}
	if updated == nil {
		return nil // 选择跳过写入
	}
	if updated.Chapter != chapter {
		return fmt.Errorf("style review: Update chapter mismatch: requested %d, returned %d", chapter, updated.Chapter)
	}
	if updated.Chapter <= 0 {
		updated.Chapter = chapter
	}

	// ── Append-only 强制校验 ──
	var origLen int
	if origSnapshot != nil {
		origLen = len(origSnapshot.Cycles)
	}
	wantNew := origLen + 1
	if len(updated.Cycles) != wantNew {
		return fmt.Errorf("style review: Update must append exactly one cycle: existing %d, returned %d", origLen, len(updated.Cycles))
	}
	for i := 0; i < origLen; i++ {
		if !domain.EntriesEqual(origSnapshot.Cycles[i], updated.Cycles[i]) {
			return fmt.Errorf("style review: Update changed cycle[%d]; append-only prohibits history modification", i)
		}
	}

	if err := domain.ValidateLedger(updated); err != nil {
		return err
	}
	return s.saveUnlocked(*updated)
}

// Exists 检查指定章节的账本是否存在。
func (s *StyleReviewStore) Exists(chapter int) bool {
	_, err := os.Stat(s.io.path(styleReviewFile(chapter)))
	return err == nil
}
