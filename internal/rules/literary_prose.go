package rules

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// ── 文学腔句式硬闸（literary prose gate）──────────────────────────────
//
// 目标：拦截 ds 系模型写中文情色网文时常见的"散文腔"——抒情咏叹、象征升华、
// 静态清点；以及四类 commit 级形态检查：段落碎片化（单句段超标）、自评/
// 素材口吻不足、节拍账本（身体节奏被写成抽象计数系统）、禁词清零（成熟
// 质检体系的硬性禁词出现即打回）。纯正则机械计数，不经 LLM judge；
// 超阈值即 error 级违例，由 commit_chapter 的前置硬闸（tools.CheckLiteraryProseGate）
// 阻止提交。
//
// 契约：
//   - 只返事实（Violation 列表），是否阻断由调用方（commit gate）裁定
//   - check_consistency / draft 阶段同样运行，作为可见事实，但不在该阶段阻断
//   - 与 fatigue_words / forbidden_phrases 机制互不干扰（唯一交集是 —— 破折号，
//     取代关系见 CheckLiteraryGate 注释）

// LiteraryProseRule 描述一条文学腔句式规则。
type LiteraryProseRule struct {
	// Name 规则短名（诊断用；Violation.Rule 统一为 literary_prose）。
	Name string
	// Label 模式中文名，进入 Violation.Target 便于 editor 定位。
	Label string
	// Pattern 正则（Go RE2；中文按 rune 匹配，. 不跨行）。
	Pattern string
	// Limit 阈值：count >= Limit 即触发；ExceedOnly 时 count > Limit 才触发。
	Limit int
	// ExceedOnly 严格大于语义（用于"—— 每章 > 25"这类表达）。
	ExceedOnly bool

	re *regexp.Regexp
}

// 阈值集中定义，便于调参。
const (
	LitGateNegationLimit        = 3  // 否定修正句 ≥3
	LitGateAbstractEmoLimit     = 3  // 抽象情绪概括 ≥3
	LitGateSimileLimit          = 8  // 明喻滥用 ≥8
	LitGateSublimationLimit     = 2  // 升华 closer ≥2
	LitGatePseudoPauseLimit     = 3  // 伪停顿 ≥3
	LitGateReificationLimit     = 2  // 物化句式 ≥2
	LitGateAbstractSubjectLimit = 2  // 抽象主语+状态动词 ≥2
	LitGateDashLimit            = 25 // 破折号伪深刻 每章 >25（error 级，取代 fatigue_words 的 ——≤30 warning）

	// LitGateSingleSentenceLimit 第 9 类：段落碎片化——"单句段"（按空行切分、
	// 整段只析出 1 个句子的段落）计数 ≥6 触发 error（规则要求 ≤5）。
	LitGateSingleSentenceLimit = 6
	// LitGateSelfReviewMin 第 10 类：自评/素材口吻不足——自评关键词命中
	// <2 触发 error（规则要求 ≥2）。
	LitGateSelfReviewMin = 2

	// LitGateBeatLedgerALimit 第 11 类组合 A：计数单位+账本关系词结构
	// ≥2 触发 error（"维持器转一圈也是六拍"这类单句即含多个关系结构）。
	LitGateBeatLedgerALimit = 2
	// LitGateBeatLedgerBLimit 第 11 类组合 B：序号计数"第N+单位" ≥3 触发 error。
	LitGateBeatLedgerBLimit = 3

	// LitGateBannedHitLimit 第 12 类禁词清零：硬性禁词出现 1 次即 error
	// （清零级，不计数）。
	LitGateBannedHitLimit = 1
)

// literaryProseRules 是 8 类文学腔句式的完整清单。
//
// 注意：变长模式（1/2/3）使用懒惰量词（.{0,N}?）而非贪婪——贪婪会让相邻的
// 同类句式折叠进同一次匹配（如"不是A而是B。不是C而是D"只算 1 次），
// 低估"散文腔"密度；懒惰量词按"每一次句式构造"计数，与闸门意图一致。
var literaryProseRules = []LiteraryProseRule{
	{
		Name:    "negation_correction",
		Label:   "否定修正句",
		Pattern: `不是.{0,30}?(?:而是|，是|。是)`,
		Limit:   LitGateNegationLimit,
	},
	{
		Name:    "abstract_emotion",
		Label:   "抽象情绪概括",
		Pattern: `一种.{0,8}?的|无尽|难以言喻|说不出的`,
		Limit:   LitGateAbstractEmoLimit,
	},
	{
		Name:    "simile_abuse",
		Label:   "明喻滥用",
		Pattern: `仿佛|宛如|如同|像.{0,20}?(?:一般|一样|似的)`,
		Limit:   LitGateSimileLimit,
	},
	{
		Name:    "sublimation_closer",
		Label:   "升华收尾",
		Pattern: `这便是|便已足够|这大概就是|或许这就是`,
		Limit:   LitGateSublimationLimit,
	},
	{
		Name:    "pseudo_pause",
		Label:   "伪停顿",
		Pattern: `，(?:终于|忽然|竟|却|便)，`,
		Limit:   LitGatePseudoPauseLimit,
	},
	{
		Name:  "reification",
		Label: "物化句式",
		// 两类形态合并计数（阈值 2，累计触发）：
		//   a) 把 + 身体/自己/整个人/这具身体 + 动词。动词覆盖"了"变体（交给了）、
		//      结果补语变体（交出去/交到）与"钉死"（对"钉在"的同构扩展）。
		//   b) "交给了X，交给了Y" 排比链：交给(了) + 1..12 字 + 标点? + 交给(了)/又交给(了)。
		//      覆盖不带"把"的"交给了X，交给了Y"纯排比形态（模型绕行"把自己交出去"
		//      前缀后的直接排比）。X 排除 。，防贪婪吞并后续排比项导致折叠；
		//      懒惰量词与 1/2/3 模式同理——按"每一次句式构造"计数，不低估密度。
		//      单处"交给了钥匙/把书交给"不计（X 需配对一个后续 交给(了)），
		//      只有排比/物化形态累计 ≥2 才触发。
		Pattern: `把(?:身体|自己|整个人|这具身体)(?:交给|交给了|交到|交出去|放进|封进|钉在|钉死|按进|埋进)|交给了?[^。，]{1,12}?[，。]?(?:交给了?|又交给了?)`,
		Limit:   LitGateReificationLimit,
	},
	{
		Name:    "abstract_subject",
		Label:   "抽象主语+状态动词",
		Pattern: `(?:黑暗|时间|寂静|静默|夜色)(?:厚厚地|沉沉地|缓缓地|一层层地?|一寸寸地?)(?:压|漫|沉|涌|覆)`,
		Limit:   LitGateAbstractSubjectLimit,
	},
	{
		Name:       "dash_pseudo_depth",
		Label:      "破折号伪深刻",
		Pattern:    `——`,
		Limit:      LitGateDashLimit,
		ExceedOnly: true,
	},
}

// litGateSentenceSplit 句子断句：句号/叹号/问号/分号。与 stylestat 的
// [。！？\n]+ 语义一致并补分号；段内换行不参与断句（段落已按空行切分）。
var litGateSentenceSplit = regexp.MustCompile(`[。！？；]+`)

// litGateSelfReviewRe 第 10 类自评/自我对话关键词（保守词表）。
//
// 误报控制：词表按"明确自评"口径收词，命中计数仅统计本表、不做语境消歧
// （RE2 无词边界、无句法，"别走/别停"在装置指令、对白、第三人称叙述里
// 都可能出现）。接受的代价是漏判方向：装置/对白语境的关键词会如实计入、
// 拉高命中数，可能掩盖真正缺自评的章——editor 可在违例 Target 的"命中词"
// 明细里核对。宁漏勿滥，先只收明确自评短语。
var litGateSelfReviewRe = regexp.MustCompile(`骂自己|她骂|心里骂|别湿|别走|别停|恨自己|丢人|不要脸|真贱|真不要脸|算了吧`)

// ── 第 9 类：段落形态（碎片化）────────────────────────────────────────
//
// 按空行切分段落（兼容 \r\n；连续空行合并；空白段忽略），句子按
// [。！？；] 断句，"整段只析出 1 个句子"的段落计为单句段。单句段
// ≥ LitGateSingleSentenceLimit(6) 触发 error（规则要求 ≤5）。
//
// 对白行排除：以「 或 " 开头、或以 」 或 " 结尾的段落视为对白行——单句对白段
// （「别走。」）、带说话人尾注的对白（「别走，」她低声道。）、带前置引导的
// 对白（她低声道：「别走。」）都是正常对话节奏，不是碎片化，一律不计入。
//
// 段落数 vs 句子数的比率 warning（段落数 > 句子数*0.5 且段落数 > 40）暂缓
// 实现——先只做单句段 error，避免引入新的误报面。
func checkParagraphShape(text string) *Violation {
	paragraphs := splitParagraphs(text)
	if len(paragraphs) < LitGateSingleSentenceLimit {
		return nil
	}
	var singles []string
	for _, p := range paragraphs {
		if isDialogueParagraph(p) {
			continue
		}
		if countSentences(p) == 1 {
			singles = append(singles, p)
		}
	}
	if len(singles) < LitGateSingleSentenceLimit {
		return nil
	}
	return &Violation{
		Rule: "literary_prose",
		Target: fmt.Sprintf("段落碎片化：单句段 %d 处（共 %d 段）：%s",
			len(singles), len(paragraphs), uniqueSnippets(singles)),
		Limit:    LitGateSingleSentenceLimit,
		Actual:   len(singles),
		Severity: SeverityError,
	}
}

// splitParagraphs 按空行切段（兼容 \r\n），连续多空行合并，忽略空白段；
// 段内单个换行保留（并入段内容）。与 host/exp 的 splitParagraphs 语义一致。
func splitParagraphs(text string) []string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	parts := strings.Split(text, "\n\n")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		out = append(out, p)
	}
	return out
}

// isDialogueParagraph 判定对白行：以「 或 " 开头、或以 」 或 " 结尾。
// 覆盖纯对白、带说话人尾注、带前置引导三种形态（见 checkParagraphShape 注释）。
func isDialogueParagraph(p string) bool {
	if strings.HasPrefix(p, "「") || strings.HasPrefix(p, "\"") {
		return true
	}
	return strings.HasSuffix(p, "」") || strings.HasSuffix(p, "\"")
}

// countSentences 按 [。！？；] 统计段落内的句子数（空片段不计）。
func countSentences(p string) int {
	parts := litGateSentenceSplit.Split(p, -1)
	n := 0
	for _, s := range parts {
		if strings.TrimSpace(s) != "" {
			n++
		}
	}
	return n
}

// ── 第 10 类：自评/素材口吻不足 ────────────────────────────────────────
//
// 统计 litGateSelfReviewRe 关键词命中数，命中 < LitGateSelfReviewMin(2)
// 触发 error（规则要求 ≥2）。Target 附带去重命中词明细，供 editor 核对
// 装置/对白语境的误计。
func checkSelfReviewTone(text string) *Violation {
	matches := litGateSelfReviewRe.FindAllString(text, -1)
	if len(matches) >= LitGateSelfReviewMin {
		return nil
	}
	detail := ""
	if words := uniqueSnippets(matches); words != "" {
		detail = "；命中词：" + words
	}
	return &Violation{
		Rule: "literary_prose",
		Target: fmt.Sprintf("自评口吻不足：命中 %d 处（要求 ≥%d）%s",
			len(matches), LitGateSelfReviewMin, detail),
		Limit:    LitGateSelfReviewMin,
		Actual:   len(matches),
		Severity: SeverityError,
	}
}

// ── 第 11 类：节拍账本（计数单位+账本关系 / 序号计数）────────────────────
//
// 拦截模型把身体节奏写成"抽象计数系统"的关系结构（ch62 实测："维持器转一圈
// 也是六拍""两套节拍永远差一拍""六拍和七拍永远差半拍"）。只匹配"拍"字会被
// 换成轮/圈/回/次绕过，故按关系结构计数：计数单位 + 账本关系词，并要求章内
// 重复出现才触发（单次"差一拍"是正常表达，不算账本）。
//
//   - 组合 A（计数单位+账本关系，命中 ≥ LitGateBeatLedgerALimit 触发）：
//     正向 = 计数单位 + 0..15 字 + 账本关系词（差/也是/对应/维持/累计/归零/
//     倍数/等于/永远）；反向 = 账本关系词（差/永远/也是/每）+ 0..15 字 +
//     数字? + 计数单位。反向的数字? 可空——"半拍"的"半"不在数字类，由
//     [^。！？；] 吞掉后单位直接命中（"永远差半拍"成立）。
//     "下"易误报（"等一下"），仅以"一下/几下"形态参与。
//   - 组合 B（序号计数，命中 ≥ LitGateBeatLedgerBLimit 触发）：
//     "第N+(拍|轮|圈|回|遍|轮次|次|号)"，"号"覆盖"第147号"形态。
//
// 豁免（宁漏勿滥）：命中起点前 10 字内出现测量动词（数着/她数/数到/心里数/
// 咬着指节/数了）时剔除该命中——"数到第六十遍"是计数行为本身，不是账本化
// 表达。"跳了一拍/漏跳了一拍"类成语化表达缺账本关系词，天然不匹配。
var (
	// litGateBeatLedgerAFwd 组合 A 正向：计数单位 + 账本关系词。
	litGateBeatLedgerAFwd = regexp.MustCompile(
		`(?:(?:[一二三四五六七八九十\d]+|每|几)(?:拍|轮|圈|回|次|遍|轮次)|(?:一|几)下)[^。！？；]{0,15}(?:差|也是|对应|维持|累计|归零|倍数|等于|永远)`)
	// litGateBeatLedgerARev 组合 A 反向：账本关系词 + 数字? + 计数单位。
	litGateBeatLedgerARev = regexp.MustCompile(
		`(?:差|永远|也是|每)[^。！？；]{0,15}[一二三四五六七八九十\d]?(?:拍|轮|圈|回|遍|轮次)`)
	// litGateBeatLedgerB 组合 B：序号计数"第N+单位"。
	litGateBeatLedgerB = regexp.MustCompile(
		`第[一二三四五六七八九十百\d]+(?:拍|轮|圈|回|遍|轮次|次|号)`)
	// litGateMeasureVerb 测量动词：命中起点前 10 字内出现则豁免。
	litGateMeasureVerb = regexp.MustCompile(`数着|她数|数到|心里数|咬着指节|数了`)
)

// litGateBeatLedgerExempt 豁免判定：命中起点（byte 偏移）前 10 个 rune 内
// 出现测量动词即豁免该命中（宁漏勿滥）。
func litGateBeatLedgerExempt(text string, start int) bool {
	if start <= 0 {
		return false
	}
	rs := []rune(text[:start])
	if len(rs) > 10 {
		rs = rs[len(rs)-10:]
	}
	return litGateMeasureVerb.MatchString(string(rs))
}

// checkBeatLedger 第 11 类检查：组合 A 命中 ≥2 或组合 B 命中 ≥3 时产出
// error 级违例；Actual 为两组合并命中数（与 10 类合并计数口径一致）。
func checkBeatLedger(text string) *Violation {
	var aHits, bHits []string
	for _, re := range []*regexp.Regexp{litGateBeatLedgerAFwd, litGateBeatLedgerARev} {
		for _, loc := range re.FindAllStringIndex(text, -1) {
			if litGateBeatLedgerExempt(text, loc[0]) {
				continue
			}
			aHits = append(aHits, text[loc[0]:loc[1]])
		}
	}
	for _, loc := range litGateBeatLedgerB.FindAllStringIndex(text, -1) {
		if litGateBeatLedgerExempt(text, loc[0]) {
			continue
		}
		bHits = append(bHits, text[loc[0]:loc[1]])
	}
	if len(aHits) < LitGateBeatLedgerALimit && len(bHits) < LitGateBeatLedgerBLimit {
		return nil
	}
	limit := LitGateBeatLedgerALimit
	detail := []string{fmt.Sprintf("组合A(计数+账本) %d处：%s", len(aHits), uniqueSnippets(aHits))}
	if len(bHits) >= LitGateBeatLedgerBLimit {
		limit = LitGateBeatLedgerBLimit
		detail = append(detail, fmt.Sprintf("组合B(序号计数) %d处：%s", len(bHits), uniqueSnippets(bHits)))
	}
	return &Violation{
		Rule:     "literary_prose",
		Target:   "节拍账本：" + strings.Join(detail, "；"),
		Limit:    limit,
		Actual:   len(aHits) + len(bHits),
		Severity: SeverityError,
	}
}

// ── 第 12 类：禁词清零（硬性禁词出现即 error）────────────────────────────
//
// 来自成熟情色网文质检体系的硬性禁词（6 项）：出现即 error（清零级，不是
// 计数）——浑身发抖/瑟瑟发抖/眼泪无声地流下来/身体背叛了她（完整短语，
// 误报低）、淫荡（情绪标签）、销魂|蚀骨（作者贴标签）。命中 Target 附
// "命中词+上下文片段（≤40 字）"，沿用 Violation 结构（Rule=literary_prose、
// Severity=error），与 1-11 类合并计数口径一致（每类一条违例）。
//
// 有意排除 绝望/崩溃/痛苦 等情绪词：在黑暗 BDSM 调教题材里它们是核心体验
// 本身（「痛苦的闷哼」「绝望地意识到自己走不出去」合法且必要），机械禁掉
// 会误伤正文。这些情绪词交由既有反散文规则（taboos）与 style-critic 处理，
// 第 12 类只保留真正机械可判的 AI 腔标签词。

// litGateBannedWords 第 12 类硬性禁词表（全部出现即 error，Limit=1）。
var litGateBannedWords = []struct {
	Label   string
	Pattern string
	re      *regexp.Regexp
}{
	{Label: "浑身发抖", Pattern: `浑身发抖`},
	{Label: "瑟瑟发抖", Pattern: `瑟瑟发抖`},
	{Label: "眼泪无声地流下来", Pattern: `眼泪无声地流下来`},
	{Label: "身体背叛了她", Pattern: `身体背叛了她`},
	{Label: "淫荡", Pattern: `淫荡`},
	{Label: "销魂/蚀骨", Pattern: `销魂|蚀骨`},
}

// checkBannedWords 第 12 类检查：硬性禁词出现即 error（清零级）。合并为
// 一条 Violation：Actual=总命中数，Limit=1（任一禁词出现即触发）。
func checkBannedWords(text string) *Violation {
	var groups []string
	total := 0
	for i := range litGateBannedWords {
		w := &litGateBannedWords[i]
		var hits []string
		for _, loc := range w.re.FindAllStringIndex(text, -1) {
			hits = append(hits, litGateBannedContext(text, loc[0], loc[1]))
		}
		if len(hits) == 0 {
			continue
		}
		total += len(hits)
		groups = append(groups, fmt.Sprintf("%s %d处：%s", w.Label, len(hits), uniqueSnippets(hits)))
	}
	if len(groups) == 0 {
		return nil
	}
	return &Violation{
		Rule:     "literary_prose",
		Target:   "禁词清零：" + strings.Join(groups, "；"),
		Limit:    LitGateBannedHitLimit,
		Actual:   total,
		Severity: SeverityError,
	}
}

// litGateBannedContext 命中词上下文片段：不跨行（段内），命中词居中，
// 总长（含两侧省略号）≤ maxSnippetRunes(40)。start/end 为 byte 偏移
// （FindAllStringIndex 语义），内部统一换算 rune 偏移。
func litGateBannedContext(text string, start, end int) string {
	lineStart := strings.LastIndexByte(text[:start], '\n') + 1
	lineEnd := len(text)
	if i := strings.IndexByte(text[end:], '\n'); i >= 0 {
		lineEnd = end + i
	}
	rs := []rune(text[lineStart:lineEnd])
	hs := utf8.RuneCountInString(text[lineStart:start])
	he := utf8.RuneCountInString(text[lineStart:end])
	hitLen := he - hs
	pad := (maxSnippetRunes - hitLen - 2) / 2 // 预留两侧省略号
	if pad < 0 {
		pad = 0
	}
	cs := hs - pad
	if cs < 0 {
		cs = 0
	}
	ce := cs + hitLen + pad*2
	if ce > len(rs) {
		ce = len(rs)
	}
	if ce < he {
		ce = he
	}
	if ce > len(rs) {
		ce = len(rs)
	}
	if ce <= cs {
		return text[start:end]
	}
	out := string(rs[cs:ce])
	if cs > 0 {
		out = "…" + out
	}
	if ce < len(rs) {
		out += "…"
	}
	return out
}

// maxSnippetRunes 每条命中片段记录进 Target 的最大长度（40 字内）。
const maxSnippetRunes = 40

// maxSnippetSamples 每个模式写入 Target 的去重片段上限。
const maxSnippetSamples = 5

// CheckLiteraryProse 统计正文中 8 类文学腔句式命中数 + 4 类形态检查
// （段落碎片化 / 自评口吻不足 / 节拍账本 / 禁词清零），超阈值即产生
// error 级违例。
//
// 每类超阈值规则产出一条 Violation：
//   - Rule    = "literary_prose"
//   - Target  = "模式名：命中片段1 / 命中片段2 …"（每片段 ≤40 runes，去重，最多 5 条）
//   - Limit   = 阈值
//   - Actual  = 命中总数
//   - Severity= error
//
// 不阻断任何流程；是否阻断由 commit 硬闸（tools.CheckLiteraryProseGate）决定。
func CheckLiteraryProse(text string) []Violation {
	var vs []Violation
	for i := range literaryProseRules {
		r := &literaryProseRules[i]
		matches := r.re.FindAllString(text, -1)
		if len(matches) == 0 {
			continue
		}
		triggered := len(matches) > r.Limit
		if !r.ExceedOnly {
			triggered = len(matches) >= r.Limit
		}
		if !triggered {
			continue
		}
		vs = append(vs, Violation{
			Rule:     "literary_prose",
			Target:   r.Label + "：" + uniqueSnippets(matches),
			Limit:    r.Limit,
			Actual:   len(matches),
			Severity: SeverityError,
		})
	}
	// 第 9 类：段落碎片化（单句段计数，对白行排除）
	if v := checkParagraphShape(text); v != nil {
		vs = append(vs, *v)
	}
	// 第 10 类：自评/素材口吻不足（命中 < 2 触发）
	if v := checkSelfReviewTone(text); v != nil {
		vs = append(vs, *v)
	}
	// 第 11 类：节拍账本（计数单位+账本关系 ≥2 / 序号计数 ≥3 触发）
	if v := checkBeatLedger(text); v != nil {
		vs = append(vs, *v)
	}
	// 第 12 类：禁词清零（硬性禁词出现即 error）
	if v := checkBannedWords(text); v != nil {
		vs = append(vs, *v)
	}
	return vs
}

// CheckLiteraryGate 是 commit/check 两阶段共用入口：用户规则机械检查（Lint 之外）
// + 文学腔句式硬闸。
//
// ——（破折号）规则冲突处理：用户若在 fatigue_words 里配置 —— 且阈值 ≥
// LitGateDashLimit(25)，其 warning 完全被破折号硬闸的 error 级（>25）取代——
// warning 永远不可能比 error 先触发，重复报告只会制造噪音，故该配置下剔除
// fatigue_words 的 —— 条目（只保留本闸）。阈值 < 25（如 ——≤10）时 warning 先于
// error 触发、仍有独立价值，保留原机制。其余 fatigue_words / forbidden_* /
// chapter_words 机制完全不变。
func CheckLiteraryGate(text string, wordCount int, s Structured) []Violation {
	vs := Check(text, wordCount, withoutSubsumedDashFatigue(s))
	return append(vs, CheckLiteraryProse(text)...)
}

// withoutSubsumedDashFatigue 返回剔除"被破折号硬闸取代"的 —— fatigue_words
// 条目的结构化副本；无取代关系时原样返回（不复制）。
func withoutSubsumedDashFatigue(s Structured) Structured {
	if len(s.FatigueWords) == 0 {
		return s
	}
	limit, ok := s.FatigueWords["——"]
	if !ok || limit < LitGateDashLimit {
		return s
	}
	out := s
	out.FatigueWords = make(map[string]int, len(s.FatigueWords))
	for w, l := range s.FatigueWords {
		if w == "——" {
			continue
		}
		out.FatigueWords[w] = l
	}
	return out
}

// uniqueSnippets 提取去重后的命中片段，每条 ≤40 runes，最多 5 条，用 " / " 连接。
func uniqueSnippets(matches []string) string {
	seen := make(map[string]struct{}, len(matches))
	parts := make([]string, 0, len(matches))
	for _, m := range matches {
		if _, ok := seen[m]; ok {
			continue
		}
		seen[m] = struct{}{}
		parts = append(parts, clipRunes(m, maxSnippetRunes))
		if len(parts) >= maxSnippetSamples {
			break
		}
	}
	return strings.Join(parts, " / ")
}

// clipRunes 按 rune 截断到 max 字（超出加省略号），绝不按 byte 切坏 UTF-8。
func clipRunes(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max]) + "…"
}

// compile 在包初始化时编译全部正则，编译失败直接 panic（模式是代码常量，必须合法）。
func init() {
	for i := range literaryProseRules {
		r := &literaryProseRules[i]
		r.re = regexp.MustCompile(r.Pattern)
	}
	for i := range litGateBannedWords {
		w := &litGateBannedWords[i]
		w.re = regexp.MustCompile(w.Pattern)
	}
}
