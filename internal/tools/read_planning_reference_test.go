package tools

import (
	"encoding/json"
	"testing"

	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/store"
)

func TestReadPlanningReferenceBatchesLongReferenceAndVolumes(t *testing.T) {
	st := store.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	if err := st.Outline.SaveCompass(domain.StoryCompass{Long: domain.LongCompass{
		EndingDirection: "终局",
		Reference:       json.RawMessage(`{"rooms":[1,2,3]}`),
	}}); err != nil {
		t.Fatal(err)
	}
	if err := st.Outline.SaveLayeredOutline([]domain.VolumeOutline{
		{Index: 1, Title: "第一卷", Arcs: []domain.ArcOutline{{Index: 1, Chapters: []domain.OutlineEntry{{Chapter: 1}}}}},
		{Index: 2, Title: "第二卷", Arcs: []domain.ArcOutline{{Index: 1, Chapters: []domain.OutlineEntry{{Chapter: 2}}}}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Summaries.SaveVolumeSummary(domain.VolumeSummary{
		Volume: 1, Title: "第一卷", Summary: "完整卷摘要", KeyEvents: []string{"完整事件"},
	}); err != nil {
		t.Fatal(err)
	}

	out, err := NewReadPlanningReferenceTool(st).Execute(t.Context(), json.RawMessage(`{
		"include_long_reference":true,
		"volumes":[2,2,9],
		"summary_volumes":[1,9]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		LongReference      map[string]any         `json:"long_reference"`
		VolumeOutlines     []domain.VolumeOutline `json:"volume_outlines"`
		Requested          []int                  `json:"requested_volumes"`
		Missing            []int                  `json:"missing_volumes"`
		VolumeSummaries    []domain.VolumeSummary `json:"volume_summaries"`
		RequestedSummaries []int                  `json:"requested_summary_volumes"`
		MissingSummaries   []int                  `json:"missing_summary_volumes"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.LongReference) == 0 || len(got.VolumeOutlines) != 1 || got.VolumeOutlines[0].Index != 2 {
		t.Fatalf("unexpected planning bundle: %s", out)
	}
	if len(got.Requested) != 2 || len(got.Missing) != 1 || got.Missing[0] != 9 {
		t.Fatalf("unexpected requested/missing volumes: %s", out)
	}
	if len(got.VolumeSummaries) != 1 || got.VolumeSummaries[0].Summary != "完整卷摘要" || len(got.RequestedSummaries) != 2 || len(got.MissingSummaries) != 1 || got.MissingSummaries[0] != 9 {
		t.Fatalf("unexpected requested/missing volume summaries: %s", out)
	}
}

func TestReadPlanningReferenceRejectsEmptyRequest(t *testing.T) {
	_, err := NewReadPlanningReferenceTool(store.NewStore(t.TempDir())).Execute(t.Context(), json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("expected empty request error")
	}
}

func TestReadPlanningReferenceCapsBatchAtSixVolumes(t *testing.T) {
	_, err := NewReadPlanningReferenceTool(store.NewStore(t.TempDir())).Execute(t.Context(), json.RawMessage(`{
		"volumes":[1,2,3,4,5,6,7]
	}`))
	if err == nil {
		t.Fatal("expected oversized volume batch error")
	}
}
