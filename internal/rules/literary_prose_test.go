package rules

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// ── 模式 7：抽象主语+状态动词 正反例（规格要求 5-6 例）──────────────────

func TestLiteraryProseAbstractSubjectPattern(t *testing.T) {
	positives := []string{
		"黑暗厚厚地压着",
		"时间缓缓地漫过山岗",
		"夜色一层层涌来",
		"寂静沉沉地覆着",
		"静默一寸寸沉入",
		"夜色一层层地涌来", // 一层层/一寸寸 的地-插入变体（与厚厚地 同构）
		"黑暗一寸寸地沉入",
	}
	for _, s := range positives {
		// 重复 2 次达到阈值（≥2），应产出 error 违例
		vs := CheckLiteraryProse(s + "，" + s + "。")
		v := findLiteraryViolation(vs, "抽象主语+状态动词")
		if v == nil {
			t.Errorf("应命中抽象主语模式: %q", s)
			continue
		}
		if v.Actual != 2 {
			t.Errorf("%q 应计数 2，got %v", s, v.Actual)
		}
		if v.Severity != SeverityError {
			t.Errorf("%q 应为 error 级，got %s", s, v.Severity)
		}
	}

	negatives := []string{
		"黑暗里什么也没变",
		"黑暗里什么也没变，时间还在走",
		"他缓缓地站起身",
		"夜色笼罩着小镇",
	}
	for _, s := range negatives {
		vs := CheckLiteraryProse(s + "。" + s + "。")
		if v := findLiteraryViolation(vs, "抽象主语+状态动词"); v != nil {
			t.Errorf("不应命中抽象主语模式: %q，got %+v", s, v)
		}
	}
}

// ── 8 类模式：超阈值 / 未超阈值 全表 ───────────────────────────────────

func TestLiteraryProseAllPatternsThresholds(t *testing.T) {
	cases := []struct {
		label     string
		limit     int
		over      string // 命中数 ≥ 阈值，应触发
		under     string // 命中数 < 阈值，不应触发
		exceedMsg string // 期望的错误信息片段（可选）
	}{
		{
			label: "否定修正句",
			limit: LitGateNegationLimit,
			over:  "他不是怕死，而是怕疼。他不是退缩，而是等待。他不是沉默，而是蓄力。",
			under: "这不是他的错，他只是在等待时机。",
		},
		{
			label: "抽象情绪概括",
			limit: LitGateAbstractEmoLimit,
			over:  "一种说不出的快感。一种难以言喻的战栗。一种无尽的渴望。",
			// "一种X的"是正常中文表达（916 误报修复后不再命中），
			// 仅 无尽/难以言喻/说不出的 计入。
			under: "转化成了一种酸胀，发出一种低呜。",
		},
		{
			label: "明喻滥用",
			limit: LitGateSimileLimit,
			over:  "她仿佛一朵花，他宛如一柄剑，你如同一条河。他像风一样轻，她像水一样柔，他像山一样稳，她像月一样明，他像雾一样迷。",
			under: "他像父亲，也像兄长。",
		},
		{
			label: "升华收尾",
			limit: LitGateSublimationLimit,
			over:  "这便是宿命，便已足够。这大概就是结局，或许这就是答案。",
			under: "这便好了，或许明日再来。",
		},
		{
			label: "伪停顿",
			limit: LitGatePseudoPauseLimit,
			over:  "他，终于，开口了。她，忽然，停住。众人，竟，沉默了。",
			under: "他终于开口了，她忽然停住。",
		},
		{
			label: "物化句式",
			limit: LitGateReificationLimit,
			over:  "她把身体交给对方，把自己封进黑暗。",
			under: "他把书交给对方，把钱放进抽屉。",
		},
		{
			label: "抽象主语+状态动词",
			limit: LitGateAbstractSubjectLimit,
			over:  "黑暗厚厚地压着，时间缓缓地漫过。",
			under: "黑暗里什么也没变，他缓缓地站起身。",
		},
		{
			label: "破折号伪深刻",
			limit: LitGateDashLimit,
			over:  strings.Repeat("——", LitGateDashLimit+1),
			under: strings.Repeat("——", LitGateDashLimit),
		},
	}

	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			// 超阈值：恰好在阈值上（破折号除外，它是严格大于）
			vs := CheckLiteraryProse(tc.over)
			v := findLiteraryViolation(vs, tc.label)
			if v == nil {
				t.Fatalf("超阈值应触发违例，over=%q", tc.over)
			}
			if v.Limit != tc.limit {
				t.Errorf("Limit = %v, want %d", v.Limit, tc.limit)
			}
			if v.Severity != SeverityError {
				t.Errorf("Severity = %s, want error", v.Severity)
			}
			wantActual := strings.Count(tc.over, "——")
			if tc.label != "破折号伪深刻" {
				wantActual = -1 // 模式各异，不在此处校验精确计数
			}
			if wantActual >= 0 && v.Actual != wantActual {
				t.Errorf("Actual = %v, want %d", v.Actual, wantActual)
			}
			// Target 必须含模式名与命中片段
			if !strings.HasPrefix(v.Target, tc.label+"：") {
				t.Errorf("Target 应以模式名开头: %q", v.Target)
			}
			snippets := strings.TrimPrefix(v.Target, tc.label+"：")
			if snippets == "" {
				t.Error("Target 应包含命中原文片段")
			}

			// 未超阈值：不应触发
			if vs := CheckLiteraryProse(tc.under); findLiteraryViolation(vs, tc.label) != nil {
				t.Errorf("未超阈值不应触发违例，under=%q", tc.under)
			}
		})
	}
}

// ── 模式 6：物化句式 变体覆盖（"交出去/交给了"排比盲区修复）──────────────

func TestLiteraryProseReificationVariants(t *testing.T) {
	cases := []struct {
		name string
		text string
		want int // 期望命中数；-1 表示不应触发（计数 < 阈值）
	}{
		// a) 原形态 + 新增动词变体（了/结果补语/钉死）
		{"原形态-把身体交给+把自己封进", "她把身体交给对方，把自己封进黑暗。", 2},
		{"变体-交出去了", "她把自己交出去了，把身体交到他手里。", 2},
		{"变体-交到", "她把自己交到他手里，把身体交到对方怀中。", 2},
		{"变体-钉死", "她把自己钉死在原地，把身体钉在墙上。", 2},
		// b) 排比形态（模型真实绕行例句 + 变体）
		{"排比-真实例句", "她把自己交出去了。交给左乳的空痒，交给右乳的堵涨……交给了六拍，交给了新嗡鸣，交给了等", 3},
		{"排比-又交给", "交给了六拍，又交给了新嗡鸣，又交给了等。交给了白昼，又交给了黑夜。", 2},
		{"排比-纯四连", "交给了六拍，交给了新嗡鸣，交给了等，交给了呼吸。", 2},
		// 反例：单处不计、正常叙述不误报
		{"单处-把身体交给了", "她把身体交给了医生。", -1},
		{"单处-把自己交出去", "她把自己交出去了。", -1},
		{"单处-纯三连排比", "交给了六拍，交给了新嗡鸣，交给了等。", -1},
		{"正常句-交给了钥匙", "她把钥匙交给了钥匙保管人。", -1},
		{"正常句-把书交给对方", "他把书交给对方，把钱放进抽屉。", -1},
		{"正常句-把钱交给他", "他把钱交给了他，把信交给了邮差。", -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vs := CheckLiteraryProse(tc.text)
			v := findLiteraryViolation(vs, "物化句式")
			if tc.want < 0 {
				if v != nil {
					t.Fatalf("不应触发违例，got %+v", v)
				}
				return
			}
			if v == nil {
				t.Fatalf("应触发违例，want Actual=%d", tc.want)
			}
			if v.Actual != tc.want {
				t.Errorf("Actual = %d, want %d", v.Actual, tc.want)
			}
			if v.Severity != SeverityError {
				t.Errorf("应为 error 级，got %s", v.Severity)
			}
		})
	}
}

// ── 阈值边界：恰好在阈值上触发（≥ 语义）────────────────────────────────

func TestLiteraryProseThresholdBoundary(t *testing.T) {
	// 升华收尾：阈值 2
	vs := CheckLiteraryProse("这便是宿命。这便是轮回。") // 恰好 2
	if v := findLiteraryViolation(vs, "升华收尾"); v == nil {
		t.Fatal("恰好在阈值上（2 处）应触发")
	} else if v.Actual != 2 {
		t.Errorf("Actual = %v, want 2", v.Actual)
	}
	vs = CheckLiteraryProse("这便是宿命。") // 1 处
	if v := findLiteraryViolation(vs, "升华收尾"); v != nil {
		t.Fatalf("低于阈值（1 处）不应触发，got %+v", v)
	}

	// 破折号：严格大于 25
	vs = CheckLiteraryProse(strings.Repeat("——", LitGateDashLimit))
	if v := findLiteraryViolation(vs, "破折号伪深刻"); v != nil {
		t.Fatalf("恰好 25 处不应触发（严格大于），got %+v", v)
	}
	vs = CheckLiteraryProse(strings.Repeat("——", LitGateDashLimit+1))
	if v := findLiteraryViolation(vs, "破折号伪深刻"); v == nil {
		t.Fatal("26 处应触发（>25）")
	}
}

// ── 命中片段：40 字内截断（防御性）+ 去重 ──────────────────────────────

func TestLiteraryProseSnippetCapture(t *testing.T) {
	// 命中片段按原文记录：不是 + 25 字 + 而是（匹配 = 29 runes，在 40 字内，不截断）
	long := "不是" + strings.Repeat("啊", 25) + "而是如此。"
	text := strings.Repeat(long, LitGateNegationLimit)
	vs := CheckLiteraryProse(text)
	v := findLiteraryViolation(vs, "否定修正句")
	if v == nil {
		t.Fatal("应触发否定修正句违例")
	}
	snippet := strings.TrimPrefix(v.Target, "否定修正句：")
	want := "不是" + strings.Repeat("啊", 25) + "而是"
	if snippet != want {
		t.Errorf("片段应保留完整命中原文，got %q want %q", snippet, want)
	}
	if n := utf8.RuneCountInString(snippet); n > maxSnippetRunes {
		t.Errorf("片段超过 40 字上限: %q (%d)", snippet, n)
	}

	// 防御性截断：clipRunes 按 rune 截到 40 字并加省略号（当前 8 类模式最长匹配
	// 约 34 runes，不会实际触发；此处直接验证工具函数行为）
	clipped := clipRunes(strings.Repeat("啊", 50), maxSnippetRunes)
	if n := utf8.RuneCountInString(clipped); n != maxSnippetRunes+1 {
		t.Errorf("clipRunes 应截到 40 字 + 省略号，got %d runes", n)
	}
	if !strings.HasSuffix(clipped, "…") {
		t.Errorf("clipRunes 应加省略号，got %q", clipped)
	}

	// 去重：同样的命中片段重复多次，Target 只保留一条
	dup := strings.Repeat("这便是宿命。", 3)
	vs = CheckLiteraryProse(dup)
	v = findLiteraryViolation(vs, "升华收尾")
	if v == nil {
		t.Fatal("应触发升华收尾违例")
	}
	snippet = strings.TrimPrefix(v.Target, "升华收尾：")
	if strings.Contains(snippet, " / ") {
		t.Errorf("相同片段应去重，got %q", snippet)
	}
}

// ── —— 破折号与 fatigue_words 的取代关系（只保留本闸）──────────────────

func TestCheckLiteraryGateDashSubsumption(t *testing.T) {
	dashText := strings.Repeat("——", 30) // >25：触发硬闸 error；>30：原 fatigue warning 也会触发

	// 用户配置 ——≤30（warning）→ 被硬闸取代：只出 literary_prose error，不出 fatigue warning
	s30 := Structured{FatigueWords: map[string]int{"——": 30}}
	vs := CheckLiteraryGate(dashText, utf8.RuneCountInString(dashText), s30)
	if v := findLiteraryViolation(vs, "破折号伪深刻"); v == nil {
		t.Fatal("破折号硬闸应触发 error")
	}
	if v := findViolation(vs, "fatigue_words", "——"); v != nil {
		t.Fatalf("——≤30 的 warning 应被硬闸取代（只保留本闸），got %+v", v)
	}

	// 用户配置 ——≤10（warning 先于 error 触发）→ 保留原机制
	s10 := Structured{FatigueWords: map[string]int{"——": 10}}
	vs = CheckLiteraryGate(dashText, utf8.RuneCountInString(dashText), s10)
	if v := findViolation(vs, "fatigue_words", "——"); v == nil {
		t.Fatal("——≤10 时 warning 先于 error 触发，应保留")
	} else if v.Severity != SeverityWarning {
		t.Errorf("fatigue_words 应为 warning 级，got %s", v.Severity)
	}
	if v := findLiteraryViolation(vs, "破折号伪深刻"); v == nil {
		t.Fatal("破折号硬闸 error 应同时触发")
	}

	// 其他疲劳词机制不受影响：不禁 阈值 1、出现 3 次 → warning 仍触发
	sMixed := Structured{FatigueWords: map[string]int{"——": 30, "不禁": 1}}
	vs = CheckLiteraryGate(dashText+"不禁不禁不禁", utf8.RuneCountInString(dashText+"不禁不禁不禁"), sMixed)
	if v := findViolation(vs, "fatigue_words", "不禁"); v == nil {
		t.Fatal("其他 fatigue_words 机制不应被改变")
	}
	if v := findViolation(vs, "fatigue_words", "——"); v != nil {
		t.Fatalf("——≤30 warning 应被取代，got %+v", v)
	}
}

// ── 第 9 类：段落形态（碎片化）单句段计数 ──────────────────────────────

func TestLiteraryProseParagraphShape(t *testing.T) {
	// 正例：段落正常（每段 3 句），单句段 0 → 不触发
	normal := strings.Join([]string{
		"他握住剑柄，指节发白。她后退一步，撞在墙上。他慢慢开口，声音沙哑。",
		"风从窗缝钻进来，带着湿气。他打了个寒颤，裹紧衣服。她低下头，没有说话。",
		"脚步声由远及近。他屏住呼吸，贴着墙根。她攥紧拳头，指甲陷进掌心。",
		"灯忽然灭了。黑暗漫上来，淹过脚踝。他摸索着前行，撞翻了凳子。",
		"她终于开口，声音发颤。他站在原地，没有动。窗外雨声渐密，敲打着屋檐。",
	}, "\n\n")
	if v := findLiteraryViolation(CheckLiteraryProse(normal), "段落碎片化"); v != nil {
		t.Fatalf("正常段落（每段 3 句）不应触发单句段违例: %+v", v)
	}

	// 边界：恰 5 个单句段（≤5 允许）→ 不触发
	five := strings.Join([]string{
		"他停住了。",
		"她笑了。",
		"他皱眉。",
		"她点头。",
		"他转身。",
	}, "\n\n")
	if v := findLiteraryViolation(CheckLiteraryProse(five), "段落碎片化"); v != nil {
		t.Fatalf("恰 5 个单句段不应触发（≥6 才触发）: %+v", v)
	}

	// 反例：6 个单句段（≥6）→ error 违例
	six := strings.Join([]string{
		"他停住了。",
		"她笑了。",
		"他皱眉。",
		"她点头。",
		"他转身。",
		"她哭了。",
	}, "\n\n")
	vs := CheckLiteraryProse(six)
	v := findLiteraryViolation(vs, "段落碎片化")
	if v == nil {
		t.Fatal("6 个单句段应触发违例")
	}
	if v.Limit != LitGateSingleSentenceLimit {
		t.Errorf("Limit = %v, want %d", v.Limit, LitGateSingleSentenceLimit)
	}
	if v.Actual != 6 {
		t.Errorf("Actual = %v, want 6", v.Actual)
	}
	if v.Severity != SeverityError {
		t.Errorf("应为 error 级，got %s", v.Severity)
	}
	if !strings.HasPrefix(v.Target, "段落碎片化：") {
		t.Errorf("Target 应以模式名开头: %q", v.Target)
	}

	// 多句段不计数：6 段中仅 3 段是单句段 → 不触发
	mixed := strings.Join([]string{
		"他停住了。她也停住。",
		"她笑了。他也笑了。",
		"他皱眉。她叹气。",
		"她点头。他退后。",
		"他转身。她跟上。",
		"他停住了。",
		"她哭了。",
		"他沉默。",
	}, "\n\n")
	if v := findLiteraryViolation(CheckLiteraryProse(mixed), "段落碎片化"); v != nil {
		t.Fatalf("单句段 3 处不应触发: %+v", v)
	}

	// 纯对白行不误报：对白单句段不计入（含三种对白形态）
	dialogue := strings.Join([]string{
		"「别走。」",
		"「别停。」",
		"「别哭。」",
		"「别怕。」",
		"「别动。」",
		"「别说了。」",
		"「别这样。」",
		"「别走，」她低声道。",   // 对白 + 说话人尾注
		"她低声道：「别走。」",   // 前置引导 + 对白（以 」 结尾）
		"他大声喊：\"别走！\"", // ASCII 引号包裹的纯对白（以 " 结尾）
	}, "\n\n")
	if v := findLiteraryViolation(CheckLiteraryProse(dialogue), "段落碎片化"); v != nil {
		t.Fatalf("纯对白行不应计入单句段: %+v", v)
	}
}

// ── 第 10 类：自评/素材口吻不足 ────────────────────────────────────────

func TestLiteraryProseSelfReviewTone(t *testing.T) {
	// 正例：自评关键词 ≥1 → 不触发
	positives := []string{
		"她心里骂自己丢人，真不要脸。",
		"她恨自己软弱，算了吧。",
		"他骂自己没用，丢人现眼。",
		"她心里骂自己：别走，别停！", // 心里骂 + 骂自己 + 别走 + 别停 = 4
	}
	for _, s := range positives {
		if v := findLiteraryViolation(CheckLiteraryProse(s), "自评口吻不足"); v != nil {
			t.Errorf("自评 ≥1 不应触发违例: %q, got %+v", s, v)
		}
	}

	// 反例：0 命中 → warning 违例（文学偏好，非硬底线）
	vs := CheckLiteraryProse("他站在窗前，看着远处的灯火。")
	v := findLiteraryViolation(vs, "自评口吻不足")
	if v == nil {
		t.Fatal("自评 0 命中应触发违例")
	}
	if v.Limit != LitGateSelfReviewMin {
		t.Errorf("Limit = %v, want %d", v.Limit, LitGateSelfReviewMin)
	}
	if v.Actual != 0 {
		t.Errorf("Actual = %v, want 0", v.Actual)
	}
	if v.Severity != SeverityWarning {
		t.Errorf("应为 warning 级，got %s", v.Severity)
	}

	// 边界：恰好 1 命中 → 通过（阈值 <1，916 误报修复：1 处"她骂自己贱"即达标）
	vs = CheckLiteraryProse("装置发出指令：别走，任务尚未完成。")
	if v := findLiteraryViolation(vs, "自评口吻不足"); v != nil {
		t.Fatalf("自评 1 命中应通过（阈值 <1）: %+v", v)
	}

	// 含"别走"的装置语境：单处装置指令"别走"计 1 命中，≥1 即满足最低
	// 自评要求（保守词表只收词表词、不做语境消歧；1 处命中即达标）。
	vs = CheckLiteraryProse("装置显示屏亮起：别走，任务尚未完成。")
	if v := findLiteraryViolation(vs, "自评口吻不足"); v != nil {
		t.Fatalf("单处装置语境 别走（1 命中）应通过: %+v", v)
	}
}

// ── 第 11 类：节拍账本（计数单位+账本关系 / 序号计数）────────────────────

func TestLiteraryProseBeatLedger(t *testing.T) {
	// 组合 A 正例：任务指定真实例句合段，计数单位+账本关系 ≥2 触发
	aPos := "维持器转一圈也是六拍。两套节拍永远差一拍。六拍和七拍永远差半拍。三圈对应三拍。"
	vs := CheckLiteraryProse(aPos)
	v := findLiteraryViolation(vs, "节拍账本")
	if v == nil {
		t.Fatalf("组合A ≥2 应触发违例: %q", aPos)
	}
	if v.Severity != SeverityError {
		t.Errorf("应为 error 级，got %s", v.Severity)
	}
	if v.Actual != 6 {
		t.Errorf("Actual = %v, want 6（一圈也是/也是六拍/永远差一拍/六拍和七拍永远差/永远差半拍/三圈对应）", v.Actual)
	}
	if !strings.Contains(v.Target, "组合A") {
		t.Errorf("Target 应含组合A明细: %q", v.Target)
	}

	// 单句密集账本也触发：一句含 2 个关系结构（六拍和七拍永远差 / 永远差半拍）
	single := "六拍和七拍永远差半拍。"
	vs = CheckLiteraryProse(single)
	v = findLiteraryViolation(vs, "节拍账本")
	if v == nil {
		t.Fatalf("单句含 2 个关系结构应触发: %q", single)
	}
	if v.Actual != 2 {
		t.Errorf("Actual = %v, want 2", v.Actual)
	}

	// 组合 B 正例：序号计数"第147号" ≥3 触发
	bPos := "第147号。第147号。第147号。"
	vs = CheckLiteraryProse(bPos)
	v = findLiteraryViolation(vs, "节拍账本")
	if v == nil {
		t.Fatalf("组合B ≥3 应触发违例: %q", bPos)
	}
	if v.Limit != LitGateBeatLedgerBLimit {
		t.Errorf("Limit = %v, want %d", v.Limit, LitGateBeatLedgerBLimit)
	}
	if v.Actual != 3 {
		t.Errorf("Actual = %v, want 3", v.Actual)
	}
	if !strings.Contains(v.Target, "组合B") {
		t.Errorf("Target 应含组合B明细: %q", v.Target)
	}
	bUnder := "第147号。第147号。"
	if v := findLiteraryViolation(CheckLiteraryProse(bUnder), "节拍账本"); v != nil {
		t.Fatalf("组合B 2 处不应触发（≥3 才触发）: %q, got %+v", bUnder, v)
	}

	// 反例：测量动词豁免（计数行为本身不算账本）
	exempt := "她数着呼吸，一，二，三。数到第六十遍门锁响了。"
	if v := findLiteraryViolation(CheckLiteraryProse(exempt), "节拍账本"); v != nil {
		t.Fatalf("测量动词豁免不应触发: %q, got %+v", exempt, v)
	}

	// 反例："维持器"装置名豁免（916 误报修复）："把维持器裹得更紧"是装置
	// 动作，不是账本关系——AFwd 匹配止于"维持"（"器"在匹配之外），豁免
	// 判定覆盖命中片段 + 其后 1 字。两处装置动作合段也不触发。
	device := "把维持器裹得更紧。把维持器绞得死紧。"
	if v := findLiteraryViolation(CheckLiteraryProse(device), "节拍账本"); v != nil {
		t.Fatalf("维持器装置名应豁免: %q, got %+v", device, v)
	}
	deviceOnce := "她听着那声音，听一声，那口就缩一下，把维持器裹得更紧。"
	if v := findLiteraryViolation(CheckLiteraryProse(deviceOnce), "节拍账本"); v != nil {
		t.Fatalf("维持器装置名单处应豁免: %q, got %+v", deviceOnce, v)
	}

	// 正例："维持"作为账本关系词仍生效（非装置名场景）：计数单位 + 维持
	// 结构照常命中，≥2 触发。
	keep := "六拍维持着节奏。七拍维持着节拍。"
	vs = CheckLiteraryProse(keep)
	v = findLiteraryViolation(vs, "节拍账本")
	if v == nil {
		t.Fatalf("维持 作为账本关系词应触发: %q", keep)
	}
	if v.Actual != 2 {
		t.Errorf("Actual = %v, want 2（六拍维持/七拍维持）", v.Actual)
	}

	// 反例：成语化表达"跳了一拍"天然不匹配（缺账本关系词）
	idiom := "心跳漏跳了一拍，他扶着墙慢慢站稳。"
	if v := findLiteraryViolation(CheckLiteraryProse(idiom), "节拍账本"); v != nil {
		t.Fatalf("漏跳了一拍不应触发: %q, got %+v", idiom, v)
	}

	// 边界：单独 1 次「差一拍」不触发
	once := "差一拍。"
	if v := findLiteraryViolation(CheckLiteraryProse(once), "节拍账本"); v != nil {
		t.Fatalf("单独 1 次差一拍不应触发: %q, got %+v", once, v)
	}

	// 换词变体：轮/圈/回/次 绕过"拍"字仍触发
	variants := "转三圈也是六轮。永远差一回。每次也是六拍。"
	vs = CheckLiteraryProse(variants)
	v = findLiteraryViolation(vs, "节拍账本")
	if v == nil {
		t.Fatalf("换词变体（轮/圈/回/次）应触发: %q", variants)
	}
	if v.Actual != 5 {
		t.Errorf("Actual = %v, want 5（三圈也是/也是六轮/永远差一回/每次也是）", v.Actual)
	}

	// 组合 A+B 混合：同时超阈时 Target 含两组合计，Actual 合并计数
	mixed := "维持器转一圈也是六拍。第147号。第147号。第147号。两套节拍永远差一拍。"
	vs = CheckLiteraryProse(mixed)
	v = findLiteraryViolation(vs, "节拍账本")
	if v == nil {
		t.Fatal("组合A+B 混合应触发违例")
	}
	if !strings.Contains(v.Target, "组合A") || !strings.Contains(v.Target, "组合B") {
		t.Errorf("Target 应含组合A与组合B明细: %q", v.Target)
	}
	if v.Actual != 6 {
		t.Errorf("Actual = %v, want 6（组合A 3 + 组合B 3）", v.Actual)
	}
}

// ── 第 12 类：禁词清零（硬性禁词出现即 error）────────────────────────────

func TestLiteraryProseBannedWords(t *testing.T) {
	positives := []struct {
		name         string
		text         string
		wantInTarget string
		wantActual   int
	}{
		{"浑身发抖", "她浑身发抖，靠在墙上。", "浑身发抖", 1},
		{"瑟瑟发抖", "他瑟瑟发抖地退到墙角。", "瑟瑟发抖", 1},
		{"眼泪无声地流下来", "眼泪无声地流下来，砸在衣襟上。", "眼泪无声地流下来", 1},
		{"身体背叛了她", "身体背叛了她，腿软得站不住。", "身体背叛了她", 1},
		{"淫荡", "淫荡的身躯在灯光下晃动。", "淫荡", 1},
		{"销魂蚀骨", "销魂蚀骨的快感涌上来。", "销魂", 2}, // 销魂+蚀骨 两处命中
	}
	for _, tc := range positives {
		t.Run(tc.name, func(t *testing.T) {
			vs := CheckLiteraryProse(tc.text)
			v := findLiteraryViolation(vs, "禁词清零")
			if v == nil {
				t.Fatalf("禁词应触发清零违例: %q", tc.text)
			}
			if v.Severity != SeverityError {
				t.Errorf("应为 error 级，got %s", v.Severity)
			}
			if v.Limit != LitGateBannedHitLimit {
				t.Errorf("Limit = %v, want %d", v.Limit, LitGateBannedHitLimit)
			}
			if !strings.Contains(v.Target, tc.wantInTarget) {
				t.Errorf("Target 应含命中词 %q: %q", tc.wantInTarget, v.Target)
			}
			if v.Actual != tc.wantActual {
				t.Errorf("Actual = %v, want %d", v.Actual, tc.wantActual)
			}
		})
	}

	// 多词混合：合并为一条违例，Actual 为总命中数
	mixed := "她浑身发抖。淫荡的笑声传来。销魂的滋味漫上来。"
	vs := CheckLiteraryProse(mixed)
	v := findLiteraryViolation(vs, "禁词清零")
	if v == nil {
		t.Fatal("多词混合应触发合并违例")
	}
	if v.Limit != LitGateBannedHitLimit {
		t.Errorf("Limit = %v, want %d", v.Limit, LitGateBannedHitLimit)
	}
	if v.Actual != 3 {
		t.Errorf("Actual = %v, want 3（浑身发抖+淫荡+销魂）", v.Actual)
	}

	// 反例：黑暗题材核心体验词不触发（绝望/痛苦 已移出禁词表，交由
	// taboos 反散文规则与 style-critic 处理，第 12 类只留机械可判标签词）
	emotionNeg := []string{
		"绝望地意识到自己走不出去。",
		"一声痛苦的闷哼从喉间溢出。",
		"他痛苦地皱眉，额角青筋跳动。",
		"她的情绪彻底崩溃了，瘫坐在地上。",
	}
	for _, s := range emotionNeg {
		if v := findLiteraryViolation(CheckLiteraryProse(s), "禁词清零"); v != nil {
			t.Errorf("绝望/痛苦/崩溃 不应触发禁词违例: %q, got %+v", s, v)
		}
	}

	// 反例：正常正文无禁词不触发
	clean := "她咬着嘴唇，指尖掐进掌心。他慢慢起身，把药丸收进袖中。"
	if v := findLiteraryViolation(CheckLiteraryProse(clean), "禁词清零"); v != nil {
		t.Fatalf("正常正文不应触发禁词违例: %+v", v)
	}
}

// ── 第 12 类：命中词+上下文片段（≤40 字，不跨行）────────────────────────

func TestLiteraryProseBannedContextSnippet(t *testing.T) {
	// 长行（62 字）中的 淫荡：上下文片段含省略号且总长 ≤40
	long := strings.Repeat("灯", 30) + "淫荡" + strings.Repeat("影", 30) + "。"
	snippet := litGateBannedContext(long, 30*3, 30*3+6)
	if n := utf8.RuneCountInString(snippet); n > maxSnippetRunes {
		t.Errorf("上下文片段应 ≤%d 字，got %d: %q", maxSnippetRunes, n, snippet)
	}
	if !strings.Contains(snippet, "淫荡") {
		t.Errorf("片段应含命中词: %q", snippet)
	}
	if !strings.HasPrefix(snippet, "…") || !strings.HasSuffix(snippet, "…") {
		t.Errorf("两侧截断应带省略号: %q", snippet)
	}
	// 端到端：违例 Target 含组计数明细与命中词
	vs := CheckLiteraryProse(long)
	v := findLiteraryViolation(vs, "禁词清零")
	if v == nil {
		t.Fatal("应触发禁词违例")
	}
	if !strings.Contains(v.Target, "淫荡 1处：") {
		t.Errorf("Target 应含组计数明细: %q", v.Target)
	}
	// 上下文不跨行：命中词前后均截断到行内
	twoLine := strings.Repeat("灯", 20) + "\n" + "淫荡" + strings.Repeat("影", 20) + "。"
	vs = CheckLiteraryProse(twoLine)
	v = findLiteraryViolation(vs, "禁词清零")
	if v == nil {
		t.Fatal("应触发禁词违例")
	}
	if strings.Contains(v.Target, "灯") {
		t.Errorf("上下文不应跨行带出上一行内容: %q", v.Target)
	}
}

// ── 辅助 ──────────────────────────────────────────────────────────────

func findLiteraryViolation(vs []Violation, label string) *Violation {
	for i := range vs {
		if vs[i].Rule == "literary_prose" && strings.HasPrefix(vs[i].Target, label+"：") {
			return &vs[i]
		}
	}
	return nil
}
