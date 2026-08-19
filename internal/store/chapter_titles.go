package store

import (
	"fmt"
	"os"
)

const chapterTitlesPath = "meta/chapter_titles.json"

// ChapterTitleStore 持久化正文完成后确定的章节标题。
//
// 它与 OutlineStore 分离：outline.json 中的标题仍是规划/审计事实，最终标题
// 只写入 meta/chapter_titles.json，供提交结果和读者导出使用。
type ChapterTitleStore struct{ io *IO }

func NewChapterTitleStore(io *IO) *ChapterTitleStore { return &ChapterTitleStore{io: io} }

// Save 保存指定章节的最终标题。标题由工具层负责校验；Store 只负责事实持久化。
func (s *ChapterTitleStore) Save(chapter int, title string) error {
	if chapter <= 0 {
		return fmt.Errorf("chapter must be > 0")
	}
	return s.io.WithWriteLock(func() error {
		titles, err := s.loadAllUnlocked()
		if err != nil {
			return err
		}
		titles[chapter] = title
		return s.io.WriteJSONUnlocked(chapterTitlesPath, titles)
	})
}

// Load 读取指定章节的最终标题。文件或章节缺失时返回空事实。
func (s *ChapterTitleStore) Load(chapter int) (string, error) {
	if chapter <= 0 {
		return "", fmt.Errorf("chapter must be > 0")
	}
	s.io.mu.RLock()
	defer s.io.mu.RUnlock()
	titles, err := s.loadAllUnlocked()
	if err != nil {
		return "", err
	}
	return titles[chapter], nil
}

// LoadAll 读取全部最终标题。文件缺失时返回空 map 和 nil error。
func (s *ChapterTitleStore) LoadAll() (map[int]string, error) {
	s.io.mu.RLock()
	defer s.io.mu.RUnlock()
	return s.loadAllUnlocked()
}

func (s *ChapterTitleStore) loadAllUnlocked() (map[int]string, error) {
	titles := make(map[int]string)
	if err := s.io.ReadJSONUnlocked(chapterTitlesPath, &titles); err != nil {
		if os.IsNotExist(err) {
			return titles, nil
		}
		return nil, err
	}
	if titles == nil {
		titles = make(map[int]string)
	}
	return titles, nil
}
