// Command migrate-state 一次性幂等状态迁移工具（feat/four-layer-state）：
// 按内置 manifest 将 foreshadow_ledger.json 的条目拆分为
//
//	A. resolve（回收） / B. 迁 world_rules / C. 迁 character_state /
//	D. 保留重写 / E. 不动，
//
// 并新建 meta/character_state.json。执行前自动备份，重复执行幂等。
//
// 用法：
//
//	go run ./cmd/migrate-state -dir workspace/output/novel -dry-run   # 只打印分类 diff（默认）
//	go run ./cmd/migrate-state -dir workspace/output/novel -apply     # 备份后执行迁移
//
// 收尾校验不通过时报错退出（dry-run 与 apply 都会校验）。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/store"
)

// resolveProtagonistEntity 从 <dir>/characters.json 读取角色清单，取 role 含"主角"的
// 条目的 name 作为规范实体名（entity 须与正式角色名一致，否则 Writer/Editor/
// check_consistency/save_arc_summary 按正式名/别名匹配全部失效）。
// 角色清单全部无 role 字段（遗留结构）时回退取第一条的 name；找不到主角角色时报错。
func resolveProtagonistEntity(dir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(dir, "characters.json"))
	if err != nil {
		return "", fmt.Errorf("读取 characters.json 失败（用于确定主角实体名）: %w", err)
	}
	var chars []domain.Character
	if err := json.Unmarshal(data, &chars); err != nil {
		return "", fmt.Errorf("解析 characters.json 失败: %w", err)
	}
	if len(chars) == 0 {
		return "", fmt.Errorf("characters.json 为空，无法确定主角实体名")
	}
	anyRole := false
	for _, c := range chars {
		if strings.TrimSpace(c.Role) != "" {
			anyRole = true
		}
		if strings.Contains(c.Role, "主角") {
			if strings.TrimSpace(c.Name) == "" {
				return "", fmt.Errorf("主角条目 name 为空，无法确定主角实体名")
			}
			return c.Name, nil
		}
	}
	// role 字段整体缺失（所有条目 role 均为空）：回退取第一条的 name
	if !anyRole {
		if strings.TrimSpace(chars[0].Name) == "" {
			return "", fmt.Errorf("characters.json 第一条 name 为空，无法确定主角实体名")
		}
		return chars[0].Name, nil
	}
	return "", fmt.Errorf("characters.json 中未找到 role 含「主角」的角色，无法确定主角实体名")
}

// ── manifest ──

type resolveSpec struct {
	id       string
	chapter  int
	evidence string
}

type worldRuleSpec struct {
	id          string
	chapter     int // ledger retire 的 closed_at
	category    string
	rule        string
	boundary    string
	closeReason string
}

type charStateSpec struct {
	id       string // 展示名（ledger id 或投影名，如「双乳状态」）
	sourceID string // 对应 ledger 条目 id（retire 用）
	chapter  int
	field    string
	value    string
	evidence string
}

type keepSpec struct {
	id            string
	description   string // 非空则重写
	horizon       string // 非空则设置
	retire        bool
	retireChapter int
	retireReason  string
}

// A. resolve（20 条）：status=resolved, resolved_at=<章节>, resolution_evidence=<引文>, last_touched_at=<章节>
var resolveManifest = []resolveSpec{
	{"hk-lactation-ring-01", 234, "右环。已解除"},
	{"device-reconfigure-75", 78, "装置重新配置，移出密室3"},
	{"hk-ballet-training-01", 39, "密室1 VR芭蕾训练与评分电击已执行完毕"},
	{"hk-uterus-doll-01", 35, "树脂球通过宫颈口时把假阳具从阴道里推了出去"},
	{"hk-hearing-recovery-01", 190, "评审确认听觉恢复与提示音用途已兑现"},
	{"review-75-whip-system", 90, "评审确认身体地图落点校准完成"},
	{"hk-collar-01", 82, "评审确认项圈威胁与电击能力已兑现"},
	{"hk-抽插机-01", 35, "抽插机的导线被扯得绷直，从连接处断开"},
	{"hk-dark-web-broadcast-01", 34, "芭蕾表演360度录像上传暗网已发生（ch29-34）"},
	{"hk-breast-asymmetry-01", 238, "双乳封锁侧逻辑于ch231-238转型"},
	{"hk-breathing-quota-01", 20, "呼吸配额仅早期密室执行（ch20）"},
	{"hk-ammonia-punishment-01", 20, "氨水惩罚早期密室执行后未再现"},
	{"wax-drop-01", 69, "一滴热蜡落在上面"},
	{"chain-beat-01", 71, "两片金属隔着乳胶夹住阴蒂根部"},
	{"window-compress-84", 85, "落下来的是一副吊索"},
	{"auto-cycle-order-91", 92, "要抢，抢着选挨得轻，选慢了两样一起咬"},
	{"no-choice-all-five-92", 93, "五处同时落下来，每处都在要她"},
	{"submit-open-83", 83, "手指分开阴唇，往两边掰开"},
	{"new-device-awaiting", 60, "一声极低的闷响，液压的"},
	{"补差循环", 51, "把她放回底噪"},
}

// B. 迁 world_rules（5 条）：ledger retire + 向 world_rules.json 追加（保留现有条目）
var ruleManifest = []worldRuleSpec{
	{"hk-milk-tax-01", 238, "society",
		"奶水为地牢通货，奶税随时间增长，需持续产奶补差入账",
		"仅密室内有效；密室7右环解除后转型（两边同抽同伤），救出后另论",
		"moved-to-world-rules"},
	{"hk-urethral-catheter-01", 15, "technology",
		"膀胱括约肌与导尿管融合失灵，排尿完全由装置控制并需支付",
		"密室内持续；外界救出后需医疗处理",
		"moved-to-world-rules+character-state"},
	{"hk-power-source-01", 12, "technology",
		"全身装置由背后电源供电，需定时充电，充电与高潮控制联动",
		"密室内持续",
		"moved-to-world-rules+character-state"},
	{"hk-milk-production-01", 8, "technology",
		"激素催乳无法回奶，乳汁持续积累需排出",
		"密室内持续",
		"moved-to-world-rules"},
	{"hk-enema-cycle-01", 21, "technology",
		"奶水高压注入直肠的日常循环，肠道已适应",
		"密室内持续",
		"moved-to-world-rules"},
}

// C. 迁 character_state（12 条）：新建/追加 meta/character_state.json
// （entity=主角正式名，运行时从 characters.json 解析，见 resolveProtagonistEntity）；
// ledger retire（已由 B 组 retire 或已在 A 组 resolve 的条目不重复处理）。
var stateManifest = []charStateSpec{
	{"hk-permanent-milker-01", "hk-permanent-milker-01", 35, "body_device.左乳榨乳器",
		"左乳永久榨乳器固定，持续榨乳", "左乳的榨乳器还挂在原位"},
	{"hk-urethral-catheter-01", "hk-urethral-catheter-01", 15, "body_device.导尿管",
		"导尿管与尿道融合，排尿由装置控制", "导尿管从尿道口延伸出来"},
	{"hk-foot-binding-01", "hk-foot-binding-01", 28, "body_device.缠足",
		"脚骨液压折断后缠足缩小，永久改变行走方式", "一双精钢打造的马蹄形高跟鞋被套在了女孩的脚上"},
	{"hk-leg-folding-01", "hk-leg-folding-01", 32, "body_device.折叠腿",
		"双腿被大腿环和钢杆永久折叠固定，只能爬行移动", "双腿被大腿环和钢杆永久折叠固定"},
	{"hk-power-source-01", "hk-power-source-01", 126, "body_device.背后电源",
		"背后电源为全身装置供电，需定时充电", "背后电源，后腰的那块皮肤温温的"},
	{"climax-degradation-01", "climax-degradation-01", 139, "health.高潮能力",
		"频繁寸止磨损高潮能力，完整释放越来越难", "第六次高潮衰减暗示完整释放正变得越来越难"},
	{"drug-confusion-pathway-01", "drug-confusion-pathway-01", 99, "health.痛快混淆通路",
		"痛快混淆药建立的痛觉-快感交叉通路；效果逐渐衰减，不会一直持续", "痛快混淆药在脊髓背角建立痛觉-快感交叉通路"},
	{"arc3-gag-deprivation", "arc3-gag-deprivation", 80, "body_device.口塞",
		"口塞与通气口收窄（ch44 安装），ch80 起由项圈链接管", "口塞安装完成。通气口收窄"},
	{"arc3-surrender-lesson", "arc3-surrender-lesson", 59, "status.驯顺因果",
		"已内化「挣扎只会让黑暗更长」因果，驯顺加深", "挣扎，加时"},
	{"arc3-phantom-touch-01", "arc3-phantom-touch-01", 58, "status.幻觉触碰",
		"密室3建立的对装置幻触的湿回应，后续持续引用", "被幻觉碰了太多次"},
	{"arc3-balance-posture", "arc3-balance-posture", 123, "status.默认姿态",
		"默认屈从姿态已成形（提示音未响身体先动）", "默认姿态验收"},
	{"双乳状态", "hk-breast-asymmetry-01", 238, "body_device.双乳状态",
		"左空右堵逻辑已转型：左榨右环仍在，右乳卸环侧红肿淤紫，两边同出同伤", "右环解除后双乳状态转型"},
}

// D. 保留重写（10 条：7 keep + 3 retire）：ledger 保留、status 不变（advanced），按需改 description/horizon；
// deep-probe-02 例外：retire。
var keepManifest = []keepSpec{
	{id: "semen-stock-01", description: "种猪精液储备是否被用于后续生育/身体改造剧情（ch1 埋设，长期未回收）", horizon: "cross_arc"},
	{id: "hk-buried-letter-01", horizon: "cross_arc"},
	{id: "充气栓膨胀", description: "充气栓注液膨胀与直肠灌排-穴内对冲双路湿——ch64 铺垫未按计划在 ch65 兑现，后续如需使用需重新铺设", horizon: "cross_arc"},
	{id: "film-breath-skin-129", description: "透明薄膜贴敷译呼吸为幻触——ch129 铺垫未兑现（ch130 为热风/感官放大），后续如需使用需重新铺设", horizon: "cross_arc"},
	{id: "deep-probe-01", description: "三十厘米金属探针（三关节九段螺旋）——ch185 亮相与 ch186 首次接触未按计划写入；探针剧情是否保留待定", horizon: "cross_arc"},
	{id: "knee-wear-01", horizon: "cross_arc"},
	{id: "arc3-collapse-penalty", horizon: "cross_arc"},
	{id: "deep-probe-02", retire: true, retireChapter: 191, retireReason: "merged-into-deep-probe-01（空描述，无独立承诺）"},
	{id: "arc3-observer-light-01", retire: true, retireChapter: 41, retireReason: "密室3一次性观察者事件（身份未揭示），非长线承诺"},
	{id: "hk-urethra-flowmeter-01", retire: true, retireChapter: 15, retireReason: "moved-to-world-rules（并入排尿控制机制）"},
}

// E. 不动（4 条）：已是 resolved，原样保留。
var untouchedManifest = []string{
	"hk-breast-net-01",
	"hk-clown-cart-01",
	"penalty-in-beat-01",
	"auto-start-89",
}

// ── 工具函数 ──

func manifestIDSet() map[string]struct{} {
	set := make(map[string]struct{})
	for _, m := range resolveManifest {
		set[m.id] = struct{}{}
	}
	for _, m := range ruleManifest {
		set[m.id] = struct{}{}
	}
	for _, m := range stateManifest {
		set[m.sourceID] = struct{}{}
	}
	for _, m := range keepManifest {
		set[m.id] = struct{}{}
	}
	for _, id := range untouchedManifest {
		set[id] = struct{}{}
	}
	return set
}

func statusText(e domain.ForeshadowEntry) string {
	switch {
	case e.Status == "resolved" && e.ResolvedAt > 0:
		return fmt.Sprintf("resolved(第%d章)", e.ResolvedAt)
	case e.Status == "retired" && e.ClosedAt > 0:
		return fmt.Sprintf("retired(第%d章)", e.ClosedAt)
	}
	return e.Status
}

func ruleExists(rules []domain.WorldRule, rule string) bool {
	for _, r := range rules {
		if r.Rule == rule {
			return true
		}
	}
	return false
}

// validateFinal 收尾校验（迁移后状态）。返回问题列表，空 = 通过。
func validateFinal(ledger []domain.ForeshadowEntry, rules []domain.WorldRule, states []domain.CharacterStateEntry) []string {
	var problems []string

	// 1. 无 status=advanced 且 resolved_at>0 的矛盾条目
	for _, e := range ledger {
		if e.Status == "advanced" && e.ResolvedAt > 0 {
			problems = append(problems, fmt.Sprintf("%s: status=advanced 且 resolved_at=%d 矛盾", e.ID, e.ResolvedAt))
		}
	}

	// 2. 无空 description（retired 允许——deep-probe-02 迁移为 retired）
	for _, e := range ledger {
		if strings.TrimSpace(e.Description) == "" && e.Status != "retired" {
			problems = append(problems, fmt.Sprintf("%s: description 为空（非 retired）", e.ID))
		}
	}

	// 3. hk-lactation-ring-01 不再活跃
	for _, e := range ledger {
		if e.ID == "hk-lactation-ring-01" && e.Status != "resolved" && e.Status != "retired" {
			problems = append(problems, "hk-lactation-ring-01 仍活跃")
		}
	}

	// 4. 所有保留 active 条目都有 horizon
	for _, e := range ledger {
		if (e.Status == "advanced" || e.Status == "planted") && e.Horizon == "" {
			problems = append(problems, fmt.Sprintf("%s: active 但无 horizon", e.ID))
		}
	}

	// 5. world_rules.json 仍含「孕月数字」条目
	hasPregnancy := false
	for _, r := range rules {
		if strings.Contains(r.Rule, "孕月数字") {
			hasPregnancy = true
			break
		}
	}
	if !hasPregnancy {
		problems = append(problems, "world_rules 缺少「孕月数字」条目（迁移不得覆盖原有规则）")
	}

	// 6. character_state.json 无重复 (entity, field)
	seen := make(map[string]struct{}, len(states))
	for _, s := range states {
		key := s.Entity + "\x00" + s.Field
		if _, ok := seen[key]; ok {
			problems = append(problems, fmt.Sprintf("character_state 重复 (entity,field): %s / %s", s.Entity, s.Field))
		}
		seen[key] = struct{}{}
	}

	return problems
}

// backup 备份将被迁移工具改写的文件到 <dir>/meta/migrate-state-backup-<timestamp>/。
// world_rules.md 虽非 manifest 指定，但 SaveWorldRules 会重写它，一并备份以便完整恢复。
func backup(dir string) (string, error) {
	backupDir := filepath.Join(dir, "meta", "migrate-state-backup-"+time.Now().Format("20060102_150405"))
	if err := os.MkdirAll(backupDir, 0o755); err != nil {
		return "", err
	}
	for _, rel := range []string{
		"foreshadow_ledger.json",
		"foreshadow_ledger.md",
		"world_rules.json",
		"world_rules.md",
		filepath.Join("meta", "character_state.json"),
	} {
		src := filepath.Join(dir, rel)
		data, err := os.ReadFile(src)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return "", err
		}
		if err := os.WriteFile(filepath.Join(backupDir, filepath.Base(rel)), data, 0o644); err != nil {
			return "", err
		}
	}
	return backupDir, nil
}

// ── 主流程 ──

func main() {
	dir := flag.String("dir", "workspace/output/novel", "小说输出根目录（foreshadow_ledger.json 所在目录）")
	dryRun := flag.Bool("dry-run", false, "只打印分类 diff 不写盘（默认行为；与 -apply 同时给出时优先）")
	apply := flag.Bool("apply", false, "执行迁移（执行前自动备份）；不传则默认 dry-run")
	flag.Parse()

	if *apply && *dryRun {
		*apply = false
	}
	if err := run(*dir, *apply); err != nil {
		fmt.Fprintln(os.Stderr, "migrate-state:", err)
		os.Exit(1)
	}
}

func run(dir string, apply bool) error {
	mode := "dry-run"
	if apply {
		mode = "apply"
	}

	// 规范实体名：从 characters.json 读取主角正式名（不硬编码——硬编码"主角"会让
	// Writer/Editor/check_consistency/save_arc_summary 的正式名/别名匹配全部失效）。
	charEntity, err := resolveProtagonistEntity(dir)
	if err != nil {
		return err
	}

	st := store.NewStore(dir)

	ledger, err := st.World.LoadForeshadowLedger()
	if err != nil {
		return fmt.Errorf("load foreshadow ledger: %w", err)
	}
	if len(ledger) == 0 {
		return fmt.Errorf("foreshadow_ledger.json 不存在或为空（dir=%s）", dir)
	}
	rules, err := st.World.LoadWorldRules()
	if err != nil {
		return fmt.Errorf("load world rules: %w", err)
	}
	charState, err := st.World.LoadCharacterState()
	if err != nil {
		return fmt.Errorf("load character state: %w", err)
	}

	idx := make(map[string]*domain.ForeshadowEntry, len(ledger))
	for i := range ledger {
		idx[ledger[i].ID] = &ledger[i]
	}

	// manifest 存在性校验：manifest 里的每个 id 必须存在于 ledger
	set := manifestIDSet()
	var missing []string
	for id := range set {
		if _, ok := idx[id]; !ok {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("manifest 条目不在 ledger 中：%v", missing)
	}

	// 工作副本
	newRules := append([]domain.WorldRule(nil), rules...)
	newStates := append([]domain.CharacterStateEntry(nil), charState...)
	stateIdx := make(map[string]int, len(newStates))
	for i, s := range newStates {
		stateIdx[s.Entity+"\x00"+s.Field] = i
	}

	var out strings.Builder
	fmt.Fprintf(&out, "== migrate-state %s ==\n数据目录：%s\n\n", mode, dir)

	stats := map[string]int{}
	applied := 0

	// ── A. resolve ──
	fmt.Fprintf(&out, "=== A. resolve（%d 条）===\n", len(resolveManifest))
	for _, m := range resolveManifest {
		e := idx[m.id]
		before := *e
		if e.Status == "resolved" && e.ResolutionEvidence != "" {
			fmt.Fprintf(&out, "%s: 已迁移（status=resolved 且已有 resolution_evidence），跳过\n", m.id)
			continue
		}
		e.Status = "resolved"
		e.ResolvedAt = m.chapter
		e.ResolutionEvidence = m.evidence
		e.LastTouchedAt = m.chapter
		e.ClosedAt = 0
		e.CloseReason = ""
		stats["resolve"]++
		applied++
		note := ""
		if before.ResolvedAt > 0 && before.Status != "resolved" {
			note = "（修复 advanced+resolved_at 矛盾）"
		}
		fmt.Fprintf(&out, "%s: %s → resolved %s\n  resolved_at=%d, last_touched_at=%d\n  resolution_evidence=%q\n",
			m.id, statusText(before), note, m.chapter, m.chapter, m.evidence)
	}
	fmt.Fprintf(&out, "\n")

	// ── B. 迁 world_rules ──
	fmt.Fprintf(&out, "=== B. 迁 world_rules（%d 条）===\n", len(ruleManifest))
	for _, m := range ruleManifest {
		e := idx[m.id]
		before := *e

		ruleNote := ""
		if ruleExists(newRules, m.rule) {
			ruleNote = "world_rules 已含同 rule，跳过新增"
		} else {
			newRules = append(newRules, domain.WorldRule{Category: m.category, Rule: m.rule, Boundary: m.boundary})
			ruleNote = fmt.Sprintf("→ world_rules 新增 [%s]", m.category)
		}

		retireNote := ""
		switch {
		case e.Status == "retired":
			retireNote = "已迁移（ledger 已 retire），跳过"
		case e.Status == "resolved":
			return fmt.Errorf("manifest 冲突：%s 已 resolved，无法迁 world_rules", m.id)
		default:
			e.Status = "retired"
			e.ClosedAt = m.chapter
			e.CloseReason = m.closeReason
			e.ResolvedAt = 0
			stats["world_rules"]++
			applied++
			retireNote = fmt.Sprintf("%s → retired（closed_at=%d）", statusText(before), m.chapter)
		}

		fmt.Fprintf(&out, "%s: %s\n  %s\n  close_reason=%q\n  rule: %s\n  boundary: %s\n",
			m.id, retireNote, ruleNote, m.closeReason, m.rule, m.boundary)
	}
	fmt.Fprintf(&out, "\n")

	// ── C. 迁 character_state ──
	fmt.Fprintf(&out, "=== C. 迁 character_state（%d 条）===\n", len(stateManifest))
	for _, m := range stateManifest {
		key := charEntity + "\x00" + m.field
		stateNote := ""
		if i, ok := stateIdx[key]; ok {
			if newStates[i].Value == m.value {
				stateNote = "character_state 已存在同值，跳过"
			} else {
				newStates[i].Value = m.value
				newStates[i].Evidence = m.evidence
				newStates[i].UpdatedChapter = m.chapter
				stats["character_state"]++
				applied++
				stateNote = "character_state 更新（upsert 覆盖）"
			}
		} else {
			stateIdx[key] = len(newStates)
			newStates = append(newStates, domain.CharacterStateEntry{
				Entity:         charEntity,
				Field:          m.field,
				Value:          m.value,
				UpdatedChapter: m.chapter,
				Evidence:       m.evidence,
			})
			stats["character_state"]++
			applied++
			stateNote = "character_state 新增"
		}

		// ledger retire：已被 B 组 retire（双写条目）或已在 A 组 resolve（双乳状态）则跳过
		e := idx[m.sourceID]
		before := *e
		retireNote := ""
		switch {
		case e.Status == "retired":
			if e.CloseReason == "moved-to-world-rules+character-state" {
				retireNote = "ledger 已 retire（B 组 moved-to-world-rules+character-state，不重复处理）"
			} else {
				retireNote = "ledger 已 retire（不重复处理）"
			}
		case e.Status == "resolved":
			retireNote = "ledger 已在 A 组 resolve（resolve 与迁状态是不同投影，不重复 retire）"
		default:
			e.Status = "retired"
			e.ClosedAt = m.chapter
			e.CloseReason = "moved-to-character-state"
			e.ResolvedAt = 0
			applied++
			retireNote = fmt.Sprintf("ledger: %s → retired（closed_at=%d, close_reason=\"moved-to-character-state\"）", statusText(before), m.chapter)
		}

		fmt.Fprintf(&out, "%s: %s\n  %s\n  character_state 条目: %s | %s | updated_chapter=%d\n    value: %s\n    evidence: %s\n",
			m.id, stateNote, retireNote, charEntity, m.field, m.chapter, m.value, m.evidence)
	}
	fmt.Fprintf(&out, "\n")

	// ── D. 保留重写 ──
	fmt.Fprintf(&out, "=== D. 保留重写（%d 条）===\n", len(keepManifest))
	for _, m := range keepManifest {
		e := idx[m.id]
		before := *e
		if m.retire {
			if e.Status == "retired" {
				fmt.Fprintf(&out, "%s: 已迁移（已 retire），跳过\n", m.id)
				continue
			}
			e.Status = "retired"
			e.ClosedAt = m.retireChapter
			e.CloseReason = m.retireReason
			e.ResolvedAt = 0
			stats["retire"]++
			applied++
			fmt.Fprintf(&out, "%s: %s → retired\n  closed_at=%d\n  close_reason=%q\n",
				m.id, statusText(before), m.retireChapter, m.retireReason)
			continue
		}
		changed := false
		var notes []string
		if m.description != "" && e.Description != m.description {
			notes = append(notes, fmt.Sprintf("description 重写: %q", m.description))
			e.Description = m.description
			changed = true
		}
		if m.horizon != "" && e.Horizon != m.horizon {
			notes = append(notes, fmt.Sprintf("horizon=%s", m.horizon))
			e.Horizon = m.horizon
			changed = true
		}
		if !changed {
			fmt.Fprintf(&out, "%s: 已迁移（description/horizon 已就位），跳过\n", m.id)
			continue
		}
		stats["keep"]++
		applied++
		fmt.Fprintf(&out, "%s: 保留（status 不变 %s）\n", m.id, e.Status)
		for _, n := range notes {
			fmt.Fprintf(&out, "  %s\n", n)
		}
	}
	fmt.Fprintf(&out, "\n")

	// ── E. 不动 ──
	fmt.Fprintf(&out, "=== E. 不动（%d 条）===\n", len(untouchedManifest))
	for _, id := range untouchedManifest {
		e := idx[id]
		stats["untouched"]++
		fmt.Fprintf(&out, "%s: 不动（status=%s，原样保留）\n", id, e.Status)
	}
	fmt.Fprintf(&out, "\n")

	// ── 未分配条目（manifest 未覆盖）──
	// 当前 ledger 中 hk-urethra-flowmeter-01 / arc3-observer-light-01 不在 manifest 内，
	// 均为 active 且无 horizon。按设计文档 §7.1「保留的 active 条目补 horizon（默认 cross_arc）」处理。
	var unassigned []string
	for id := range idx {
		if _, ok := set[id]; !ok {
			unassigned = append(unassigned, id)
		}
	}
	sort.Strings(unassigned)
	if len(unassigned) > 0 {
		fmt.Fprintf(&out, "=== 未分配（manifest 未覆盖，%d 条）===\n", len(unassigned))
		for _, id := range unassigned {
			e := idx[id]
			if (e.Status == "advanced" || e.Status == "planted") && e.Horizon == "" {
				e.Horizon = "cross_arc"
				stats["unassigned"]++
				applied++
				fmt.Fprintf(&out, "%s: 保留 + 补 horizon=cross_arc（manifest 未覆盖；按设计文档 §7.1 默认，请人工确认）\n", id)
			} else {
				fmt.Fprintf(&out, "%s: 不动（status=%s，无需变更）\n", id, e.Status)
			}
		}
		fmt.Fprintf(&out, "\n")
	}

	// ── 统计 ──
	fmt.Fprintf(&out, "统计：resolve %d / world_rules %d / character_state %d / 保留 %d / retire %d / 不动 %d",
		stats["resolve"], stats["world_rules"], stats["character_state"], stats["keep"], stats["retire"], stats["untouched"])
	if stats["unassigned"] > 0 {
		fmt.Fprintf(&out, " / 未分配补 horizon %d", stats["unassigned"])
	}
	fmt.Fprintf(&out, "\n")
	if applied == 0 {
		fmt.Fprintf(&out, "已迁移：所有条目已处于目标状态，无变更需执行\n")
	}

	// ── 收尾校验 ──
	problems := validateFinal(ledger, newRules, newStates)
	fmt.Fprintf(&out, "\n校验（迁移后）：\n")
	if len(problems) == 0 {
		fmt.Fprintf(&out, "  全部通过（6 项）\n")
	} else {
		for _, p := range problems {
			fmt.Fprintf(&out, "  [失败] %s\n", p)
		}
	}

	if !apply {
		fmt.Fprintf(&out, "\n[dry-run] 未写盘。确认后执行：go run ./cmd/migrate-state -dir %s -apply\n", dir)
		fmt.Print(out.String())
		if len(problems) > 0 {
			return fmt.Errorf("收尾校验未通过（%d 项），不写盘", len(problems))
		}
		return nil
	}

	if len(problems) > 0 {
		fmt.Fprintf(&out, "\n[apply] 收尾校验未通过，拒绝写盘\n")
		fmt.Print(out.String())
		return fmt.Errorf("收尾校验未通过（%d 项），拒绝写盘", len(problems))
	}

	backupDir, err := backup(dir)
	if err != nil {
		fmt.Print(out.String())
		return fmt.Errorf("备份失败：%w", err)
	}

	if err := st.World.SaveForeshadowLedger(ledger); err != nil {
		fmt.Print(out.String())
		return fmt.Errorf("save foreshadow ledger: %w", err)
	}
	if err := st.World.SaveWorldRules(newRules); err != nil {
		fmt.Print(out.String())
		return fmt.Errorf("save world rules: %w", err)
	}
	if err := st.World.SaveCharacterState(newStates); err != nil {
		fmt.Print(out.String())
		return fmt.Errorf("save character state: %w", err)
	}

	fmt.Fprintf(&out, "\n[apply] 备份：%s\n已写入：\n", backupDir)
	for _, rel := range []string{"foreshadow_ledger.json", "foreshadow_ledger.md", "world_rules.json", "world_rules.md", filepath.Join("meta", "character_state.json")} {
		fmt.Fprintf(&out, "  %s\n", filepath.Join(dir, rel))
	}
	fmt.Print(out.String())
	return nil
}
