package store

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/voocel/ainovel-cli/internal/domain"
)

// PlanningArchiveStore 管理 meta/planning_archive.json。
// 提供原子、锁内的 Load / UpsertEntry / DeleteEntry 基础 API。
type PlanningArchiveStore struct{ io *IO }

func NewPlanningArchiveStore(io *IO) *PlanningArchiveStore { return &PlanningArchiveStore{io: io} }

const planningArchivePath = "meta/planning_archive.json"

// Load 读取 archive。文件不存在时返回 (nil, nil)。
func (s *PlanningArchiveStore) Load() (*domain.PlanningArchiveV1, error) {
	s.io.mu.RLock()
	defer s.io.mu.RUnlock()
	return s.loadUnlocked()
}

func (s *PlanningArchiveStore) loadUnlocked() (*domain.PlanningArchiveV1, error) {
	var archive domain.PlanningArchiveV1
	if err := s.io.ReadJSONUnlocked(planningArchivePath, &archive); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("planning archive: load: %w", err)
	}
	if err := archive.Validate(); err != nil {
		return nil, fmt.Errorf("planning archive: load: invalid archive: %w", err)
	}
	return &archive, nil
}

func (s *PlanningArchiveStore) saveUnlocked(archive *domain.PlanningArchiveV1) error {
	if err := archive.Validate(); err != nil {
		return fmt.Errorf("planning archive: save: invalid archive: %w", err)
	}
	return s.io.WriteJSONUnlocked(planningArchivePath, archive)
}

// UpsertEntry 添加或替换一个 (kind, id) 条目（summary 为空）。
// kind 和 id 均不可为空。新创建的 archive 会自动设置 schema 与 version。
func (s *PlanningArchiveStore) UpsertEntry(kind, id string, data json.RawMessage) error {
	return s.UpsertEntryWithSummary(kind, id, "", data)
}

// UpsertEntryWithSummary 添加或替换一个 (kind, id) 条目，同时持久化摘要。
// 允许 summary 为空以清除旧 summary。写入前完整 Validate，invalid/v2/duplicate
// 均 fail closed 且字节不变。
func (s *PlanningArchiveStore) UpsertEntryWithSummary(kind, id, summary string, data json.RawMessage) error {
	if kind == "" {
		return fmt.Errorf("planning archive: kind must not be empty")
	}
	if id == "" {
		return fmt.Errorf("planning archive: id must not be empty")
	}

	s.io.mu.Lock()
	defer s.io.mu.Unlock()

	archive, err := s.loadUnlocked()
	if err != nil {
		return err
	}
	if archive == nil {
		archive = &domain.PlanningArchiveV1{
			Schema:  "ainovel.planning-archive",
			Version: 1,
		}
	}

	found := false
	for i := range archive.Entries {
		if archive.Entries[i].Kind == kind && archive.Entries[i].ID == id {
			archive.Entries[i].Data = data
			archive.Entries[i].Summary = summary // 允许 summary 为空以清除旧摘要
			found = true
			break
		}
	}
	if !found {
		archive.Entries = append(archive.Entries, domain.PlanningArchiveEntry{
			Kind:    kind,
			ID:      id,
			Summary: summary,
			Data:    data,
		})
	}

	// 写入前完整 Validate：invalid/v2/duplicate/empty key → fail closed
	if err := archive.Validate(); err != nil {
		return fmt.Errorf("planning archive: upsert would create invalid archive: %w", err)
	}

	return s.saveUnlocked(archive)
}

// deleteEntry 删除匹配 (kind, id) 的条目。条目不存在时返回错误。
// ⚠️ 内部 primitive——业务删除应使用 Store.DeleteArchiveEntrySafe 以执行
// open_threads 安全检查。直接调用此方法会绕过保护。
func (s *PlanningArchiveStore) deleteEntry(kind, id string) error {
	if kind == "" {
		return fmt.Errorf("planning archive: kind must not be empty")
	}
	if id == "" {
		return fmt.Errorf("planning archive: id must not be empty")
	}

	s.io.mu.Lock()
	defer s.io.mu.Unlock()

	return s.deleteEntryUnlocked(kind, id)
}

func (s *PlanningArchiveStore) deleteEntryUnlocked(kind, id string) error {
	archive, err := s.loadUnlocked()
	if err != nil {
		return err
	}
	if archive == nil || len(archive.Entries) == 0 {
		return fmt.Errorf("planning archive: entry (kind=%q, id=%q) not found", kind, id)
	}

	before := len(archive.Entries)
	kept := make([]domain.PlanningArchiveEntry, 0, before)
	for _, e := range archive.Entries {
		if e.Kind == kind && e.ID == id {
			continue
		}
		kept = append(kept, e)
	}
	if len(kept) == before {
		return fmt.Errorf("planning archive: entry (kind=%q, id=%q) not found", kind, id)
	}
	archive.Entries = kept
	return s.saveUnlocked(archive)
}
