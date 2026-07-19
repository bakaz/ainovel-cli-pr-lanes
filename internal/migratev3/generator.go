package migratev3

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/projectprofile"
)

func requiredSceneFieldNames() []string {
	return []string{"goal", "action", "conflict", "outcome", "body_reaction", "emotion_reaction", "erotic_charge"}
}

type FakeGenerator struct{}

func (FakeGenerator) Descriptor() GeneratorDescriptor {
	return GeneratorDescriptor{
		Provider: "offline-fake", Model: "deterministic-review-fixture-v1",
		Pricing: Pricing{Known: true, InputUSDPerMillion: 0, OutputUSDPerMillion: 0, MaxOutputTokens: 8192},
	}
}

func (FakeGenerator) Prepare(req ChapterRequest) (PreparedRequest, error) {
	data, err := json.Marshal(req)
	if err != nil {
		return PreparedRequest{}, err
	}
	return PreparedRequest{OutboundBody: string(data)}, nil
}

func (FakeGenerator) Generate(_ context.Context, req ChapterRequest, prepared PreparedRequest, _ int) (GenerateResult, error) {
	want, err := (FakeGenerator{}).Prepare(req)
	if err != nil {
		return GenerateResult{}, err
	}
	if prepared.OutboundBody != want.OutboundBody {
		return GenerateResult{Usage: Usage{Known: true}}, fmt.Errorf("fake prepared request mismatch")
	}
	items := make([]ProposalItem, 0, len(req.Scenes))
	for _, scene := range req.Scenes {
		fields := make(map[string]string, len(scene.MissingFields))
		anchor := scene.LegacyAction
		if anchor == "" {
			anchor = scene.Source.Action
		}
		for _, field := range scene.MissingFields {
			switch field {
			case "goal":
				fields[field] = "迁移草案：明确推进场景——" + anchor
			case "action":
				fields[field] = "迁移草案：执行场景动作——" + anchor
			case "conflict":
				fields[field] = "迁移草案：补充阻碍该场景推进的具体冲突"
			case "outcome":
				fields[field] = "迁移草案：补充该场景结束后的明确状态变化"
			case "body_reaction":
				fields[field] = "迁移草案：补充角色可观察的身体反应"
			case "emotion_reaction":
				fields[field] = "迁移草案：补充角色当下的心理与情绪变化"
			case "erotic_charge":
				fields[field] = "迁移草案：标注本场景的色气或性张力变化；若无则明确为无"
			}
		}
		items = append(items, ProposalItem{ID: scene.ID, Fields: fields})
	}
	raw, err := json.Marshal(proposalEnvelope{Items: items})
	if err != nil {
		return GenerateResult{}, err
	}
	return GenerateResult{RawResponse: raw, Usage: Usage{Known: true, InputTokens: 0, OutputTokens: 0}, FinishReason: "stop"}, nil
}

func buildChapterRequest(ch domain.OutlineEntry) ChapterRequest {
	req := ChapterRequest{Chapter: ch.Chapter, Title: ch.Title}
	for i, scene := range ch.Scenes {
		id := fmt.Sprintf("ch-%02d/s-%02d", ch.Chapter, i+1)
		allFields := sceneFieldMap(scene)
		requiredFields := requiredSceneFieldNames()
		missing := make([]string, 0, len(requiredFields))
		for _, field := range requiredFields {
			if strings.TrimSpace(allFields[field]) == "" {
				missing = append(missing, field)
			}
		}
		preserved := nonemptySceneFields(scene)
		item := SceneRequest{ID: id, Source: scene, MissingFields: missing, PreservedFields: preserved}
		if scene.IsLegacy() {
			item.LegacyAction = scene.Action
		}
		req.Scenes = append(req.Scenes, item)
	}
	return req
}

func nonemptySceneFields(scene domain.SceneBeat) map[string]string {
	all := sceneFieldMap(scene)
	out := map[string]string{}
	for key, value := range all {
		if strings.TrimSpace(value) != "" {
			out[key] = value
		}
	}
	return out
}

func sceneFieldMap(scene domain.SceneBeat) map[string]string {
	return map[string]string{
		"goal": scene.Goal, "action": scene.Action, "conflict": scene.Conflict, "outcome": scene.Outcome,
		"sensory_anchor": scene.SensoryAnchor, "body_reaction": scene.BodyReaction,
		"emotion_reaction": scene.EmotionReaction, "erotic_charge": scene.EroticCharge,
	}
}

func mergeProposal(ch domain.OutlineEntry, req ChapterRequest, raw []byte) (domain.OutlineEntry, []ReviewDiffEntry, int, error) {
	if err := validateJSONLexically(raw); err != nil {
		return ch, nil, 0, fmt.Errorf("proposal JSON: %w", err)
	}
	var env proposalEnvelope
	if err := strictJSON(raw, &env); err != nil {
		return ch, nil, 0, fmt.Errorf("proposal decode: %w", err)
	}
	expected := make(map[string]SceneRequest, len(req.Scenes))
	for _, scene := range req.Scenes {
		expected[scene.ID] = scene
	}
	seen := map[string]bool{}
	byID := map[string]ProposalItem{}
	for _, item := range env.Items {
		if seen[item.ID] {
			return ch, nil, 0, fmt.Errorf("duplicate proposal id %q", item.ID)
		}
		seen[item.ID] = true
		if _, ok := expected[item.ID]; !ok {
			return ch, nil, 0, fmt.Errorf("unknown proposal id %q", item.ID)
		}
		byID[item.ID] = item
	}
	if len(byID) != len(expected) {
		return ch, nil, 0, fmt.Errorf("proposal omitted ids: got %d want %d", len(byID), len(expected))
	}

	out := ch
	out.Scenes = append(domain.SceneList(nil), ch.Scenes...)
	contract := newV3Contract()
	var diffs []ReviewDiffEntry
	filled := 0
	for i, src := range req.Scenes {
		item := byID[src.ID]
		missing := map[string]bool{}
		for _, field := range src.MissingFields {
			missing[field] = true
		}
		for field, value := range item.Fields {
			if !isRequiredSceneField(field) {
				return ch, nil, 0, fmt.Errorf("id %s returned unknown/non-migratable field %q", src.ID, field)
			}
			if !missing[field] {
				return ch, nil, 0, fmt.Errorf("id %s attempted to rewrite preserved field %q", src.ID, field)
			}
			if strings.TrimSpace(value) == "" {
				return ch, nil, 0, fmt.Errorf("id %s field %q is empty", src.ID, field)
			}
			if strings.Contains(value, providerFillSentinel) {
				return ch, nil, 0, fmt.Errorf("id %s field %q retained Provider template sentinel", src.ID, field)
			}
		}
		if len(item.Fields) != len(missing) {
			return ch, nil, 0, fmt.Errorf("id %s omitted missing fields: got %d want %d", src.ID, len(item.Fields), len(missing))
		}
		beat := out.Scenes[i]
		added := map[string]string{}
		for _, field := range src.MissingFields {
			value, ok := item.Fields[field]
			if !ok {
				return ch, nil, 0, fmt.Errorf("id %s omitted field %q", src.ID, field)
			}
			setSceneField(&beat, field, value)
			added[field] = value
			filled++
		}
		// Reconstructing an object intentionally clears the legacy provenance.
		encoded, err := json.Marshal(beat)
		if err != nil {
			return ch, nil, 0, err
		}
		var migrated domain.SceneBeat
		if err := json.Unmarshal(encoded, &migrated); err != nil {
			return ch, nil, 0, err
		}
		beat = migrated
		if err := contract.Validate(beat); err != nil {
			return ch, nil, 0, fmt.Errorf("id %s merged scene invalid: %w", src.ID, err)
		}
		out.Scenes[i] = beat
		diffs = append(diffs, ReviewDiffEntry{ID: src.ID, Preserved: src.PreservedFields, Added: added})
	}
	return out, diffs, filled, nil
}

func isRequiredSceneField(field string) bool {
	for _, allowed := range requiredSceneFieldNames() {
		if field == allowed {
			return true
		}
	}
	return false
}

func setSceneField(scene *domain.SceneBeat, field, value string) {
	switch field {
	case "goal":
		scene.Goal = value
	case "action":
		scene.Action = value
	case "conflict":
		scene.Conflict = value
	case "outcome":
		scene.Outcome = value
	case "body_reaction":
		scene.BodyReaction = value
	case "emotion_reaction":
		scene.EmotionReaction = value
	case "erotic_charge":
		scene.EroticCharge = value
	}
}

func newV3Contract() interface{ Validate(domain.SceneBeat) error } {
	return projectprofile.NewSceneBeatV3Contract()
}
