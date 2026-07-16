package store

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/voocel/ainovel-cli/internal/domain"
)

// ── 文件状态枚举 ──

type ManualFileStatus int

const (
	StatusNotExist    ManualFileStatus = iota
	StatusEmptyValid
	StatusValid
	StatusCorrupted
	StatusLegacyFormat
)

type StyleAnchorsStore struct{ io *IO }

func NewStyleAnchorsStore(io *IO) *StyleAnchorsStore { return &StyleAnchorsStore{io: io} }

const styleAnchorsPath = "meta/style_anchors.json"
const maxStyleAnchorsFileBytes = 64 * 1024

type LoadManualResult struct {
	Anchors  *domain.StyleAnchorsV1
	Warnings []string
	Status   ManualFileStatus
}

// LoadManual 读取并校验 meta/style_anchors.json。
//
// 新格式（version=1 顶层）：
//   - 任意未知字段 → StatusCorrupted（fail closed）
//   - chapter_ranges 每个元素必须是严格 [int,int]
//
// 旧格式（顶层含 purpose/usage）：
//   - 未知字段产生 warning 但仍可加载（迁移兼容）
//   - 绝不写回文件
func (s *StyleAnchorsStore) LoadManual() LoadManualResult {
	s.io.mu.RLock()
	defer s.io.mu.RUnlock()

	data, err := s.io.ReadFileUnlocked(styleAnchorsPath)
	if err != nil {
		if os.IsNotExist(err) {
			return LoadManualResult{Status: StatusNotExist}
		}
		return LoadManualResult{
			Warnings: []string{fmt.Sprintf("style_anchors: 读取失败: %v", err)},
			Status:   StatusCorrupted,
		}
	}

	if len(data) > maxStyleAnchorsFileBytes {
		return LoadManualResult{
			Warnings: []string{fmt.Sprintf("style_anchors: 文件大小 %d 字节超过上限 %d 字节", len(data), maxStyleAnchorsFileBytes)},
			Status:   StatusCorrupted,
		}
	}

	var rawMap map[string]any
	if err := json.Unmarshal(data, &rawMap); err != nil {
		return LoadManualResult{
			Warnings: []string{fmt.Sprintf("style_anchors: JSON 语法错误: %v", err)},
			Status:   StatusCorrupted,
		}
	}

	// 旧格式检测（顶层有 purpose/usage）
	if _, hasPurpose := rawMap["purpose"]; hasPurpose {
		return s.loadLegacyFormat(rawMap, data)
	}
	if _, hasUsage := rawMap["usage"]; hasUsage {
		return s.loadLegacyFormat(rawMap, data)
	}

	// ── 新格式（version=1）──
	// 新格式中任何未知字段 → corrupted
	if unknown := checkNewFormatUnknownFields(rawMap); len(unknown) > 0 {
		return LoadManualResult{
			Warnings: unknown,
			Status:   StatusCorrupted,
		}
	}

	// chapter_ranges 逐元素严格校验
	if errStr := validateChapterRangesRaw(rawMap); errStr != "" {
		return LoadManualResult{
			Warnings: []string{fmt.Sprintf("style_anchors: %s", errStr)},
			Status:   StatusCorrupted,
		}
	}

	var v1 domain.StyleAnchorsV1
	if err := json.Unmarshal(data, &v1); err != nil {
		return LoadManualResult{
			Warnings: []string{fmt.Sprintf("style_anchors: 反序列化失败: %v", err)},
			Status:   StatusCorrupted,
		}
	}

	if errs := v1.Validate(); len(errs) > 0 {
		var warns []string
		for _, e := range errs {
			warns = append(warns, e.Error())
		}
		return LoadManualResult{Warnings: warns, Status: StatusCorrupted}
	}

	if len(v1.Anchors) == 0 {
		return LoadManualResult{Anchors: &v1, Status: StatusEmptyValid}
	}
	return LoadManualResult{Anchors: &v1, Status: StatusValid}
}

// checkNewFormatUnknownFields 检查新格式顶层和所有嵌套的未知字段。
// 未知字段 → 返回描述列表（非空即为 corrupted）。
func checkNewFormatUnknownFields(raw map[string]any) []string {
	knownTop := map[string]bool{"version": true, "anchors": true, "include_auto": true}
	knownAnchor := map[string]bool{"id": true, "excerpt": true, "applies_to": true, "provenance": true}
	knownAppliesTo := map[string]bool{"chapter_ranges": true}
	knownProvenance := map[string]bool{"source_chapter": true, "source_digest": true}

	var errs []string
	for k := range raw {
		if !knownTop[k] {
			errs = append(errs, fmt.Sprintf("style_anchors: 未知顶层字段 %q", k))
		}
	}
	if anchorsRaw, ok := raw["anchors"].([]any); ok {
		for i, item := range anchorsRaw {
			itemMap, ok := item.(map[string]any)
			if !ok {
				continue
			}
			prefix := fmt.Sprintf("style_anchors.anchors[%d]", i)
			for k := range itemMap {
				if !knownAnchor[k] {
					errs = append(errs, fmt.Sprintf("%s: 未知字段 %q", prefix, k))
				}
			}
			if at, ok := itemMap["applies_to"].(map[string]any); ok {
				for k := range at {
					if !knownAppliesTo[k] {
						errs = append(errs, fmt.Sprintf("%s.applies_to: 未知字段 %q", prefix, k))
					}
				}
			}
			if pv, ok := itemMap["provenance"].(map[string]any); ok {
				for k := range pv {
					if !knownProvenance[k] {
						errs = append(errs, fmt.Sprintf("%s.provenance: 未知字段 %q", prefix, k))
					}
				}
			}
		}
	}
	return errs
}

// validateChapterRangesRaw 检查 chapter_ranges 每个元素是严格 [int,int]。
func validateChapterRangesRaw(raw map[string]any) string {
	anchorsRaw, ok := raw["anchors"].([]any)
	if !ok {
		return ""
	}
	for i, item := range anchorsRaw {
		itemMap, ok := item.(map[string]any)
		if !ok {
			continue
		}
		atRaw, ok := itemMap["applies_to"].(map[string]any)
		if !ok {
			continue
		}
		rangesRaw, ok := atRaw["chapter_ranges"].([]any)
		if !ok {
			continue
		}
		for j, rRaw := range rangesRaw {
			rArr, ok := rRaw.([]any)
			if !ok || len(rArr) != 2 {
				return fmt.Sprintf("anchors[%d].applies_to.chapter_ranges[%d] 必须为 [int,int]，当前类型 %T 长度 %d", i, j, rRaw, len(rArr))
			}
			// 每个元素必须是 float64（JSON 数字解码为 float64）且无小数部分
			for k := 0; k < 2; k++ {
				f, ok := rArr[k].(float64)
				if !ok || f != float64(int(f)) {
					return fmt.Sprintf("anchors[%d].applies_to.chapter_ranges[%d][%d] 不是合法整数", i, j, k)
				}
			}
		}
	}
	return ""
}

// ── 旧格式兼容 ──

func (s *StyleAnchorsStore) loadLegacyFormat(rawMap map[string]any, rawJSON []byte) LoadManualResult {
	warnings := []string{"style_anchors: 检测到旧格式（purpose/usage），已自动转换为新格式，建议迁移到 version=1 + anchors[].excerpt"}

	// 旧格式中未知顶层字段仅产生 warning（不阻断加载）
	legacyTop := map[string]bool{"version": true, "purpose": true, "usage": true, "anchors": true}
	for k := range rawMap {
		if !legacyTop[k] {
			warnings = append(warnings, fmt.Sprintf("style_anchors: 旧格式中未知顶层字段 %q", k))
		}
	}

	type legacyItem struct {
		ID    string `json:"id"`
		Label string `json:"label"`
		Text  string `json:"text"`
	}
	type legacyRoot struct {
		Version int          `json:"version"`
		Anchors []legacyItem `json:"anchors"`
	}
	var old legacyRoot
	if err := json.Unmarshal(rawJSON, &old); err != nil {
		return LoadManualResult{
			Warnings: append(warnings, fmt.Sprintf("style_anchors: 旧格式解析失败: %v", err)),
			Status:   StatusCorrupted,
		}
	}

	// id 优先，label 仅当 id 为空时回退
	var items []domain.StyleAnchorItem
	for _, oa := range old.Anchors {
		id := strings.TrimSpace(oa.ID)
		if id == "" {
			id = strings.TrimSpace(oa.Label)
		}
		items = append(items, domain.StyleAnchorItem{
			ID:      id,
			Excerpt: strings.TrimSpace(oa.Text),
		})
	}

	v1 := &domain.StyleAnchorsV1{Version: 1, Anchors: items}
	if errs := v1.Validate(); len(errs) > 0 {
		for _, e := range errs {
			warnings = append(warnings, e.Error())
		}
		return LoadManualResult{Warnings: warnings, Status: StatusCorrupted}
	}
	if len(items) == 0 {
		return LoadManualResult{Anchors: v1, Warnings: warnings, Status: StatusEmptyValid}
	}
	return LoadManualResult{Anchors: v1, Warnings: warnings, Status: StatusLegacyFormat}
}
