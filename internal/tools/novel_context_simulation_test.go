package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/store"
)

func TestContextToolInjectsCompactSimulationProfile(t *testing.T) {
	dir := t.TempDir()
	st := store.NewStore(dir)
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	profile := domain.SimulationProfile{
		Version: domain.SimulationProfileVersion,
		Corpus: domain.SimulationCorpusManifest{
			Sources: []domain.SimulationSource{{
				RelativePath: "a.txt",
				SHA256:       "sha-a",
				Fingerprint:  domain.SimulationSourceFingerprint("a.txt", "sha-a"),
			}},
		},
		SourceReports: []domain.SimulationSourceReport{{
			RelativePath: "a.txt",
			SHA256:       "sha-a",
			Fingerprint:  domain.SimulationSourceFingerprint("a.txt", "sha-a"),
			Summary:      "full report should not be injected",
		}},
		Synthesis: domain.SimulationSynthesis{
			Style: domain.SimulationStyle{
				NarrativeVoice: []string{"close third"},
			},
			RoleGuidance: domain.SimulationRoleGuidance{
				Architect: []string{"escalate costs"},
				Writer:    []string{"borrow technique only"},
				Editor:    []string{"check non-copying"},
			},
		},
	}
	if err := st.Simulation.Save(profile); err != nil {
		t.Fatal(err)
	}
	if err := st.Outline.SaveOutline([]domain.OutlineEntry{
		{Chapter: 1, Title: "Start", CoreEvent: "Begin"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.Init("test", 1); err != nil {
		t.Fatal(err)
	}

	tool := NewContextToolForRole(st, References{}, "default", "writer")
	architectRaw, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("architect Execute: %v", err)
	}
	var architect map[string]any
	if err := json.Unmarshal(architectRaw, &architect); err != nil {
		t.Fatal(err)
	}
	assertCompactSimulationProfile(t, architect, "planning_memory")

	chapterRaw, err := tool.Execute(context.Background(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatalf("chapter Execute: %v", err)
	}
	var chapter map[string]any
	if err := json.Unmarshal(chapterRaw, &chapter); err != nil {
		t.Fatal(err)
	}
	assertCompactSimulationProfile(t, chapter, "working_memory")
}

func TestContextToolInjectsRoleProjectedSimulationProfile(t *testing.T) {
	dir := t.TempDir()
	st := store.NewStore(dir)
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	profile := domain.SimulationProfile{
		Version: domain.SimulationProfileVersion,
		Corpus: domain.SimulationCorpusManifest{
			Sources: []domain.SimulationSource{
				{RelativePath: "source-a.txt", SHA256: "sha-a"},
				{RelativePath: "source-b.txt", SHA256: "sha-b"},
			},
		},
		SourceReports: []domain.SimulationSourceReport{{Summary: "do not inject this report"}},
		Synthesis: domain.SimulationSynthesis{
			Style: domain.SimulationStyle{
				NarrativeVoice: []string{"abstract voice"},
				SentenceRhythm: []string{"abstract rhythm"},
				ProseTexture:   []string{"abstract detail"},
				Perspective:    []string{"close distance"},
				Mood:           []string{"rising dread"},
				DoNotCopy:      []string{"do not copy source wording"},
			},
			PlotDesign: domain.SimulationPlotDesign{
				EscalationPatterns: []string{"raise the cost"},
			},
			HookDesign: domain.SimulationHookDesign{
				HookTypes: []string{"unanswered choice"},
			},
			PacingDensity: domain.SimulationPacingDensity{
				SceneDensity:        []string{"goal obstacle turn"},
				InformationRelease:  []string{"delay explanation"},
				DialogueActionRatio: []string{"dialogue changes leverage"},
				CompressionRules:    []string{"compress transit"},
			},
			ReaderEngagement: domain.SimulationReaderEngagement{
				EmotionalDrivers:   []string{"fear of loss"},
				ProgressionRewards: []string{"visible clue gain"},
			},
			RoleGuidance: domain.SimulationRoleGuidance{
				Architect: []string{"architect guidance"},
				Writer:    []string{"writer guidance"},
				Editor:    []string{"editor anti-copy guidance"},
			},
		},
	}
	if err := st.Simulation.Save(profile); err != nil {
		t.Fatal(err)
	}
	if err := st.Outline.SaveOutline([]domain.OutlineEntry{
		{Chapter: 1, Title: "Start", CoreEvent: "Begin"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.Init("test", 1); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name     string
		role     string
		args     string
		section  string
		included []string
		excluded []string
	}{
		{
			name:    "architect",
			role:    "architect",
			args:    `{}`,
			section: "planning_memory",
			included: []string{
				"role_guidance", "macro_pacing", "emotional_progression",
				"conflict_escalation", "hooks",
			},
			excluded: []string{
				"narrative_distance", "sentence_rhythm", "detail_strategy", "dialogue_voice",
				"do_not_copy", "style_drift", "rhythm_similarity_risk", "anti_similarity_checks",
			},
		},
		{
			name:    "writer",
			role:    "writer",
			args:    `{"chapter":1}`,
			section: "working_memory",
			included: []string{
				"role_guidance", "narrative_distance", "sentence_rhythm", "detail_strategy",
				"dialogue_voice", "do_not_copy",
			},
			excluded: []string{
				"macro_pacing", "emotional_progression", "conflict_escalation", "hooks",
				"style_drift", "rhythm_similarity_risk", "anti_similarity_checks",
			},
		},
		{
			name:    "editor",
			role:    "editor",
			args:    `{"chapter":1}`,
			section: "working_memory",
			included: []string{
				"role_guidance", "style_drift", "rhythm_similarity_risk", "anti_similarity_checks",
			},
			excluded: []string{
				"macro_pacing", "emotional_progression", "conflict_escalation", "hooks",
				"narrative_distance", "sentence_rhythm", "detail_strategy", "dialogue_voice", "do_not_copy",
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tool := NewContextToolForRole(st, References{}, "default", tc.role)
			raw, err := tool.Execute(context.Background(), json.RawMessage(tc.args))
			if err != nil {
				t.Fatal(err)
			}
			var payload map[string]any
			if err := json.Unmarshal(raw, &payload); err != nil {
				t.Fatal(err)
			}
			assertRoleProjectedSimulationProfile(t, payload, tc.section, tc.role, tc.included, tc.excluded)
		})
	}
}

func assertRoleProjectedSimulationProfile(t *testing.T, payload map[string]any, section, role string, included, excluded []string) {
	t.Helper()
	if got := payload["simulation_profile"]; got != true {
		t.Fatalf("expected top-level simulation_profile marker, got %#v", got)
	}
	sectionMap, ok := payload[section].(map[string]any)
	if !ok {
		t.Fatalf("expected %s", section)
	}
	view, ok := sectionMap["simulation_profile"].(map[string]any)
	if !ok {
		t.Fatalf("expected role view under %s", section)
	}
	if got := view["role"]; got != role {
		t.Fatalf("role = %v, want %s", got, role)
	}
	if got := view["source_count"]; got != float64(2) {
		t.Fatalf("source_count = %v, want 2", got)
	}
	if _, exists := view["source_reports"]; exists {
		t.Fatal("role view must not include source_reports")
	}
	encoded, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "do not inject this report") {
		t.Fatal("role view leaked source report content")
	}
	for _, key := range included {
		if _, exists := view[key]; !exists {
			t.Fatalf("%s role view missing %q: %#v", role, key, view)
		}
	}
	for _, key := range excluded {
		if _, exists := view[key]; exists {
			t.Fatalf("%s role view leaked %q: %#v", role, key, view)
		}
	}
}

func assertCompactSimulationProfile(t *testing.T, payload map[string]any, section string) {
	t.Helper()
	if got := payload["simulation_profile"]; got != true {
		t.Fatalf("expected top-level simulation_profile marker, got %#v", got)
	}
	sectionMap, ok := payload[section].(map[string]any)
	if !ok {
		t.Fatalf("expected %s", section)
	}
	compact, ok := sectionMap["simulation_profile"].(map[string]any)
	if !ok {
		t.Fatalf("expected simulation_profile under %s", section)
	}
	if _, exists := compact["source_reports"]; exists {
		t.Fatal("compact simulation_profile must not include source_reports")
	}
	if got := compact["source_count"]; got != float64(1) {
		t.Fatalf("source_count = %v, want 1", got)
	}
}
