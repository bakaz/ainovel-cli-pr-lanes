package domain

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

func TestCompactSimulationProfileCapsInjectedArrays(t *testing.T) {
	profile := &SimulationProfile{
		Version: SimulationProfileVersion,
		Corpus: SimulationCorpusManifest{
			Sources: make([]SimulationSource, 25),
		},
		Synthesis: SimulationSynthesis{
			Style: SimulationStyle{
				NarrativeVoice: longSimulationList("voice", maxCompactSimulationItems+5),
				DoNotCopy:      longSimulationList("copy", maxCompactSimulationItems+5),
			},
			Lexicon: SimulationLexicon{
				CommonWords: longSimulationList("word", maxCompactSimulationItems+5),
			},
			PlotDesign: SimulationPlotDesign{
				OpeningPatterns: longSimulationList("opening", maxCompactSimulationItems+5),
			},
			HookDesign: SimulationHookDesign{
				HookTypes: longSimulationList("hook", maxCompactSimulationItems+5),
			},
			PacingDensity: SimulationPacingDensity{
				SceneDensity: longSimulationList("density", maxCompactSimulationItems+5),
			},
			ReaderEngagement: SimulationReaderEngagement{
				Methods: longSimulationList("method", maxCompactSimulationItems+5),
			},
			RoleGuidance: SimulationRoleGuidance{
				Writer: longSimulationList("writer", maxCompactSimulationItems+5),
			},
		},
	}
	for i := range profile.Corpus.Sources {
		profile.Corpus.Sources[i] = SimulationSource{RelativePath: "source-" + strconv.Itoa(i)}
	}

	compact := CompactSimulationProfile(profile)
	if compact == nil {
		t.Fatal("compact profile is nil")
	}
	if got := len(compact.SourceFiles); got != maxCompactSimulationSourceFiles {
		t.Fatalf("SourceFiles len = %d, want %d", got, maxCompactSimulationSourceFiles)
	}
	assertCompactLen(t, "Style.NarrativeVoice", compact.Style.NarrativeVoice)
	assertCompactLen(t, "Style.DoNotCopy", compact.Style.DoNotCopy)
	assertCompactLen(t, "Lexicon.CommonWords", compact.Lexicon.CommonWords)
	assertCompactLen(t, "PlotDesign.OpeningPatterns", compact.PlotDesign.OpeningPatterns)
	assertCompactLen(t, "HookDesign.HookTypes", compact.HookDesign.HookTypes)
	assertCompactLen(t, "PacingDensity.SceneDensity", compact.PacingDensity.SceneDensity)
	assertCompactLen(t, "ReaderEngagement.Methods", compact.ReaderEngagement.Methods)
	assertCompactLen(t, "RoleGuidance.Writer", compact.RoleGuidance.Writer)
	if got := len(profile.Synthesis.Style.NarrativeVoice); got != maxCompactSimulationItems+5 {
		t.Fatalf("CompactSimulationProfile mutated source profile, len = %d", got)
	}
}

func TestProjectSimulationProfileKeepsRoleBoundaries(t *testing.T) {
	profile := &SimulationProfile{
		Version: SimulationProfileVersion,
		Corpus: SimulationCorpusManifest{
			Sources: make([]SimulationSource, maxCompactSimulationSourceFiles+5),
		},
		SourceReports: []SimulationSourceReport{{Summary: "source prose must stay out of context"}},
		Synthesis: SimulationSynthesis{
			Style: SimulationStyle{
				NarrativeVoice: []string{"abstract voice"},
				SentenceRhythm: []string{"abstract rhythm"},
				ProseTexture:   []string{"abstract detail"},
				Perspective:    []string{"close distance"},
				Mood:           []string{"rising dread"},
				DoNotCopy:      []string{"never copy source wording"},
			},
			PlotDesign: SimulationPlotDesign{
				EscalationPatterns: []string{"raise the cost"},
			},
			HookDesign: SimulationHookDesign{
				HookTypes:           []string{"unanswered choice"},
				Placement:           []string{"end on changed stakes"},
				CliffhangerPatterns: []string{"decision before consequence"},
				PayoffRules:         []string{"answer one question"},
			},
			PacingDensity: SimulationPacingDensity{
				SceneDensity:        []string{"goal obstacle turn"},
				InformationRelease:  []string{"delay explanation"},
				DialogueActionRatio: []string{"dialogue changes leverage"},
				CompressionRules:    []string{"compress transit"},
			},
			ReaderEngagement: SimulationReaderEngagement{
				EmotionalDrivers:   []string{"fear of loss"},
				ProgressionRewards: []string{"visible clue gain"},
			},
			RoleGuidance: SimulationRoleGuidance{
				Architect: []string{"architect guidance"},
				Writer:    []string{"writer guidance"},
				Editor:    []string{"editor anti-copy guidance"},
			},
		},
	}
	for i := range profile.Corpus.Sources {
		profile.Corpus.Sources[i] = SimulationSource{RelativePath: "source-" + strconv.Itoa(i)}
	}

	tests := []struct {
		role  string
		check func(*SimulationRoleProjection)
	}{
		{
			role: "architect",
			check: func(view *SimulationRoleProjection) {
				assertSimulationItems(t, "architect guidance", view.RoleGuidance, "architect guidance")
				assertSimulationItems(t, "macro pacing", view.MacroPacing, "goal obstacle turn", "delay explanation", "compress transit")
				assertSimulationItems(t, "emotional progression", view.EmotionalProgression, "rising dread", "fear of loss", "visible clue gain")
				assertSimulationItems(t, "conflict escalation", view.ConflictEscalation, "raise the cost")
				if view.Hooks == nil || len(view.Hooks.HookTypes) != 1 {
					t.Fatalf("architect hooks = %+v", view.Hooks)
				}
				if view.NarrativeDistance != nil || view.DoNotCopy != nil || view.StyleDrift != nil {
					t.Fatal("architect view leaked writer/editor signals")
				}
			},
		},
		{
			role: "writer",
			check: func(view *SimulationRoleProjection) {
				assertSimulationItems(t, "writer guidance", view.RoleGuidance, "writer guidance")
				assertSimulationItems(t, "narrative distance", view.NarrativeDistance, "abstract voice", "close distance")
				assertSimulationItems(t, "sentence rhythm", view.SentenceRhythm, "abstract rhythm")
				assertSimulationItems(t, "detail strategy", view.DetailStrategy, "abstract detail", "dialogue changes leverage", "compress transit")
				assertSimulationItems(t, "dialogue voice", view.DialogueVoice, "abstract voice")
				assertSimulationItems(t, "do not copy", view.DoNotCopy, "never copy source wording")
				if view.MacroPacing != nil || view.ConflictEscalation != nil || view.StyleDrift != nil {
					t.Fatal("writer view leaked architect/editor signals")
				}
			},
		},
		{
			role: "editor",
			check: func(view *SimulationRoleProjection) {
				assertSimulationItems(t, "editor guidance", view.RoleGuidance, "editor anti-copy guidance")
				assertSimulationItems(t, "style drift", view.StyleDrift, "abstract voice", "close distance", "rising dread")
				assertSimulationItems(t, "rhythm similarity risk", view.RhythmSimilarityRisk, "abstract rhythm", "goal obstacle turn", "delay explanation")
				assertSimulationItems(t, "anti-similarity checks", view.AntiSimilarityChecks, "never copy source wording", "editor anti-copy guidance")
				if view.NarrativeDistance != nil || view.DoNotCopy != nil || view.MacroPacing != nil {
					t.Fatal("editor view leaked architect/writer-only signals")
				}
			},
		},
	}
	for _, tc := range tests {
		view := ProjectSimulationProfile(profile, tc.role)
		if view == nil {
			t.Fatalf("ProjectSimulationProfile(%q) returned nil", tc.role)
		}
		if view.Role != tc.role || view.SourceCount != len(profile.Corpus.Sources) {
			t.Fatalf("%s audit metadata = %+v", tc.role, view)
		}
		if got := len(view.SourceFiles); got != maxCompactSimulationSourceFiles {
			t.Fatalf("%s source_files len = %d, want %d", tc.role, got, maxCompactSimulationSourceFiles)
		}
		raw, err := json.Marshal(view)
		if err != nil {
			t.Fatalf("marshal %s role view: %v", tc.role, err)
		}
		if stringContainsAny(string(raw), "source_reports", "source prose must stay out of context") {
			t.Fatalf("%s role view leaked source report data: %s", tc.role, raw)
		}
		tc.check(view)
	}
}

func TestProjectSimulationProfileAcceptsLegacyShape(t *testing.T) {
	var profile SimulationProfile
	legacy := []byte(`{
  "version": "simulation_profile.v1",
  "corpus": {"sources": [{"relative_path": "legacy.txt", "sha256": "legacy-sha"}]},
  "source_reports": [{"summary": "legacy report"}],
  "synthesis": {"style": {"narrative_voice": ["legacy voice"], "do_not_copy": ["do not copy"]}, "role_guidance": {"writer": ["legacy guidance"]}}
}`)
	if err := json.Unmarshal(legacy, &profile); err != nil {
		t.Fatal(err)
	}
	if err := ValidateSimulationProfile(&profile); err != nil {
		t.Fatal(err)
	}
	view := ProjectSimulationProfile(&profile, "writer")
	if view == nil || view.SourceCount != 1 || len(view.DoNotCopy) != 1 {
		t.Fatalf("legacy profile projection = %+v", view)
	}
}

func assertSimulationItems(t *testing.T, name string, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s = %v, want %v", name, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s = %v, want %v", name, got, want)
		}
	}
}

func stringContainsAny(s string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
}

func assertCompactLen(t *testing.T, name string, got []string) {
	t.Helper()
	if len(got) != maxCompactSimulationItems {
		t.Fatalf("%s len = %d, want %d", name, len(got), maxCompactSimulationItems)
	}
}

func longSimulationList(prefix string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = prefix + "-" + strconv.Itoa(i)
	}
	return out
}
