package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/voocel/agentcore/schema"
	"github.com/voocel/ainovel-cli/internal/errs"
	"github.com/voocel/ainovel-cli/internal/store"
)

const maxPlanningVolumesPerRead = 6

// ReadPlanningReferenceTool 让长篇 Architect 按需补读首轮视野外的规划资料。
// 首轮 novel_context 只带当前相邻卷的章节细纲和精简 Compass；详细长期参考
// 与远处卷章节细纲由本工具批量读取，避免每次规划都支付整包上下文成本。
type ReadPlanningReferenceTool struct {
	store *store.Store
}

func NewReadPlanningReferenceTool(store *store.Store) *ReadPlanningReferenceTool {
	return &ReadPlanningReferenceTool{store: store}
}

func (t *ReadPlanningReferenceTool) Name() string { return "read_planning_reference" }
func (t *ReadPlanningReferenceTool) Description() string {
	return "Architect 按需读取长期规划参考、远处卷完整章节细纲或指定卷完整摘要。一次最多涉及 6 卷；同一规划任务最多调用两次。"
}
func (t *ReadPlanningReferenceTool) Label() string { return "读取规划参考" }

func (t *ReadPlanningReferenceTool) ReadOnly(_ json.RawMessage) bool        { return true }
func (t *ReadPlanningReferenceTool) ConcurrencySafe(_ json.RawMessage) bool { return true }

func (t *ReadPlanningReferenceTool) Schema() map[string]any {
	return schema.Object(
		schema.Property("include_long_reference", schema.Bool("是否读取 compass.long.reference 的详细长期方案")),
		schema.Property("volumes", schema.Array("要补读完整章节细纲的卷号；一次最多 6 卷", schema.Int("卷号"))),
		schema.Property("summary_volumes", schema.Array("要补读完整 volume_summaries 的卷号；可与 volumes 合并请求", schema.Int("卷号"))),
	)
}

func (t *ReadPlanningReferenceTool) Execute(_ context.Context, args json.RawMessage) (json.RawMessage, error) {
	var a struct {
		IncludeLongReference bool  `json:"include_long_reference"`
		Volumes              []int `json:"volumes"`
		SummaryVolumes       []int `json:"summary_volumes"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("invalid args: %w", err)
	}

	volumes := uniquePositiveInts(a.Volumes)
	summaryVolumes := uniquePositiveInts(a.SummaryVolumes)
	allRequestedVolumes := uniquePositiveInts(append(slices.Clone(volumes), summaryVolumes...))
	if !a.IncludeLongReference && len(allRequestedVolumes) == 0 {
		return nil, fmt.Errorf("至少请求 long reference 或一个卷号: %w", errs.ErrToolArgs)
	}
	if len(allRequestedVolumes) > maxPlanningVolumesPerRead {
		return nil, fmt.Errorf("一次最多涉及 %d 卷，当前请求 %d 卷: %w", maxPlanningVolumesPerRead, len(allRequestedVolumes), errs.ErrToolArgs)
	}

	result := map[string]any{
		"_usage": "这是首轮视野外的按需资料；同一规划任务最多调用 read_planning_reference 两次，章节细纲和卷摘要的卷号尽量一次合并请求。",
	}
	if a.IncludeLongReference {
		compass, err := t.store.Outline.LoadCompass()
		if err != nil {
			return nil, fmt.Errorf("load compass: %w", err)
		}
		if compass == nil || len(compass.Long.Reference) == 0 {
			result["long_reference"] = nil
			result["long_reference_hint"] = "本书没有额外 Long Reference；使用 novel_context 中的 long/current 即可"
		} else {
			result["long_reference"] = compass.Long.Reference
		}
	}

	if len(volumes) > 0 {
		layered, err := t.store.Outline.LoadLayeredOutline()
		if err != nil {
			return nil, fmt.Errorf("load layered outline: %w", err)
		}
		requested := make(map[int]struct{}, len(volumes))
		for _, volume := range volumes {
			requested[volume] = struct{}{}
		}
		found := make([]any, 0, len(volumes))
		foundIndexes := make(map[int]struct{}, len(volumes))
		for _, volume := range layered {
			if _, ok := requested[volume.Index]; !ok {
				continue
			}
			found = append(found, volume)
			foundIndexes[volume.Index] = struct{}{}
		}
		var missing []int
		for _, volume := range volumes {
			if _, ok := foundIndexes[volume]; !ok {
				missing = append(missing, volume)
			}
		}
		result["volume_outlines"] = found
		result["requested_volumes"] = volumes
		if len(missing) > 0 {
			result["missing_volumes"] = missing
		}
	}

	if len(summaryVolumes) > 0 {
		summaries, err := t.store.Summaries.LoadAllVolumeSummaries()
		if err != nil {
			return nil, fmt.Errorf("load volume summaries: %w", err)
		}
		requested := make(map[int]struct{}, len(summaryVolumes))
		for _, volume := range summaryVolumes {
			requested[volume] = struct{}{}
		}
		found := make([]any, 0, len(summaryVolumes))
		foundIndexes := make(map[int]struct{}, len(summaryVolumes))
		for _, summary := range summaries {
			if _, ok := requested[summary.Volume]; !ok {
				continue
			}
			found = append(found, summary)
			foundIndexes[summary.Volume] = struct{}{}
		}
		var missing []int
		for _, volume := range summaryVolumes {
			if _, ok := foundIndexes[volume]; !ok {
				missing = append(missing, volume)
			}
		}
		result["volume_summaries"] = found
		result["requested_summary_volumes"] = summaryVolumes
		if len(missing) > 0 {
			result["missing_summary_volumes"] = missing
		}
	}
	return json.Marshal(result)
}

func uniquePositiveInts(values []int) []int {
	result := make([]int, 0, len(values))
	for _, value := range values {
		if value <= 0 || slices.Contains(result, value) {
			continue
		}
		result = append(result, value)
	}
	return result
}
