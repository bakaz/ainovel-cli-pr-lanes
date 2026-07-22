package tools

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

// ── Parser ──

// roomMarkerRe 匹配末尾的 [room:<id>]；id 匹配到第一个 ] 为止。
// 后续由 ParseOpenThreadMarkers 严格校验：id 不含空格/控制字符/unicode whitespace，
// 且 marker 必须位于末尾（只匹配末尾连续的 marker，$ 锚定）。
var roomMarkerRe = regexp.MustCompile(`\[room:([^\]]*)\]$`)

// unclosedMarkerRe 检测字符串中是否有未闭合的 [room:...（缺少 ]）。
var unclosedMarkerRe = regexp.MustCompile(`\[room:[^\]]*$`)

// hasRoomBracketPrefix 检测是否包含 [room: 模式（无论闭合）。
var hasRoomBracketPrefix = regexp.MustCompile(`\[room:`)

// ParsedOpenThread 是对一条 compass.long.open_threads 条目的解析结果。
type ParsedOpenThread struct {
	// NaturalSummary 是去除末尾 [room:<id>] 标记后的自然语言摘要。
	// 解析失败时等于原始文本（保留兼容，由调用方决定如何处理）。
	NaturalSummary string `json:"natural_summary"`
	// RoomIDs 是从末尾连续提取的 [room:<id>] ID 列表，去重保留顺序。
	RoomIDs []string `json:"room_ids,omitempty"`
	// ParseError 非空表示该条目解析失败，NaturalSummary 为原始文本。
	// 调用方可据此决定是拒绝整次写入，还是保留旧文本不阻断无关更新。
	ParseError string `json:"parse_error,omitempty"`
}

// ThreadParseError 记录一条线程的解析错误。
type ThreadParseError struct {
	// Index 是线程在原始切片中的位置。
	Index int `json:"index"`
	// Text 是原始线程文本。
	Text string `json:"text"`
	// Err 是解析错误描述。
	Err string `json:"error"`
}

func (e ThreadParseError) Error() string {
	return fmt.Sprintf("thread[%d]: %s (text: %q)", e.Index, e.Err, e.Text)
}

// ParseOpenThreadMarkers 严格解析 open_threads 条目末尾的 [room:<id>] marker(s)。
//
	// 规则：
	//   - 只匹配末尾连续的 [room:<id>] suffix；marker 之间允许普通空格（如 "线索 [room:a] [room:b]"）。
	//   - 非末尾处（中间）出现 marker 视为畸形并拒绝。
	//   - 未闭合的 [room:...（缺少 ]）视为畸形并拒绝。
	//   - marker 后不允许任何尾随 whitespace/非 marker 文本（最后一个 ] 后必须直接结束）。
	//   - ID 中的任何 Unicode 空白字符或控制字符视为畸形并拒绝。
	//   - ID 大小写敏感，不 trim/不转换/不规范化。
	//   - 重复 id 去重保留首次出现顺序。
	//   - 无 marker：返回原始文本（不 trim），RoomIDs 为空，ParseError 为空。
	//   - 解析失败时（畸形/中间/未闭合/空/空白/trailing whitespace id）：返回 err != nil，
	//     同时 ParsedOpenThread 的 NaturalSummary 设为原始文本、ParseError 设错误描述，
	//     调用方可通过它决定是拒绝写入还是保留旧值。
	//   - 特别注意：不做全局 TrimRight / TrimSpace 预处理；最后一个 ] 后的任何尾随
	//     空白会导致解析失败，因为 marker 必须精确位于字符串最末（$ 锚定）。
	//     marker 间的空格在逐个剥离时会被适当清除。
	func ParseOpenThreadMarkers(entry string) (ParsedOpenThread, error) {
	orig := entry
	rest := entry // 不 Trim，保持原样

	// 检测未闭合的 [room:...（省略了 ]）
	if loc := unclosedMarkerRe.FindStringIndex(rest); loc != nil {
		p := ParsedOpenThread{NaturalSummary: orig, ParseError: "未闭合的 room marker（缺少 ]）"}
		return p, &parseError{"未闭合的 room marker"}
	}

	// 第一个 marker 在 orig 中的起始位置（用于计算 NaturalSummary）
	firstMarkerStart := -1

	roomIDs := make([]string, 0, 2)
	seen := make(map[string]bool)

	for {
		loc := roomMarkerRe.FindStringSubmatchIndex(rest)
		if loc == nil {
			break
		}
		matchStart := loc[0]
		if matchStart+len(rest)-loc[0] != len(rest) {
			break
		}

		idRaw := rest[loc[2]:loc[3]]

		if idRaw == "" {
			p := ParsedOpenThread{NaturalSummary: orig, ParseError: "空 room id"}
			return p, &parseError{"空 room id"}
		}
		if strings.IndexFunc(idRaw, unicode.IsSpace) >= 0 {
			p := ParsedOpenThread{NaturalSummary: orig, ParseError: fmt.Sprintf("room id 含空白字符: %q", idRaw)}
			return p, &parseError{fmt.Sprintf("room id 含空白字符: %q", idRaw)}
		}
		if strings.IndexFunc(idRaw, func(r rune) bool { return r < 0x20 }) >= 0 {
			p := ParsedOpenThread{NaturalSummary: orig, ParseError: fmt.Sprintf("room id 含控制字符: %q", idRaw)}
			return p, &parseError{fmt.Sprintf("room id 含控制字符: %q", idRaw)}
		}
		if strings.IndexFunc(idRaw, unicode.IsControl) >= 0 {
			p := ParsedOpenThread{NaturalSummary: orig, ParseError: fmt.Sprintf("room id 含控制字符: %q", idRaw)}
			return p, &parseError{fmt.Sprintf("room id 含控制字符: %q", idRaw)}
		}

		if seen[idRaw] {
			p := ParsedOpenThread{NaturalSummary: orig, ParseError: "重复 room id: " + idRaw}
			return p, &parseError{"重复 room id: " + idRaw}
		}
		seen[idRaw] = true
		roomIDs = append(roomIDs, idRaw)

		// 追踪最左侧 marker 在 orig 中的位置：markers 从右往左解析，
		// 后续迭代找到的 marker 更靠左，因此总是覆盖。
		firstMarkerStart = matchStart

		// 去掉匹配到的 marker，再清除 marker 间空格，继续解析前一个
		rest = strings.TrimRight(rest[:matchStart], " \t\r\n")
	}

	// 检查是否有 [room:...] 遗留在前面（中间 marker）
	if suspectMidMarker.MatchString(rest) {
		p := ParsedOpenThread{NaturalSummary: orig, ParseError: "中间位置出现 room marker"}
		return p, &parseError{"中间位置出现 room marker"}
	}
	if hasRoomBracketPrefix.MatchString(rest) {
		p := ParsedOpenThread{NaturalSummary: orig, ParseError: "未闭合或残缺的 room marker"}
		return p, &parseError{"未闭合或残缺的 room marker"}
	}

	// 计算 NaturalSummary：使用第一个 marker 的原始位置，保留原文不 trim
	var naturalSummary string
	if firstMarkerStart >= 0 {
		naturalSummary = orig[:firstMarkerStart]
	} else {
		naturalSummary = orig
	}

	// 反转 roomIDs（从右往左解析，roomIDs 是逆序的）
	for i, j := 0, len(roomIDs)-1; i < j; i, j = i+1, j-1 {
		roomIDs[i], roomIDs[j] = roomIDs[j], roomIDs[i]
	}

	return ParsedOpenThread{
		NaturalSummary: naturalSummary,
		RoomIDs:        roomIDs,
	}, nil
}

// suspectMidMarker 检测字符串中是否还有未被末尾解析消耗的 [room:...] 片段。
var suspectMidMarker = regexp.MustCompile(`\[room:[^\]]*\]`)

// ── Ref 类型 ──

// PlanningRef 标识一个 planning archive 条目引用。
type PlanningRef struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// ExtractPlanningRefs 从多条 open_threads 中提取所有唯一 ref，保留首次出现顺序。
//
// 返回的 parsed 中每条条目均有 ParseError 字段：
//   - 空字符串表示解析成功，RoomIDs 有效
//   - 非空字符串表示解析失败，RoomIDs 为空，NaturalSummary 为原始文本
// 调用方可以遍历 parsed 收集 warnings 给 LLM，同时不影响 refs 的提取。
func ExtractPlanningRefs(openThreads []string) ([]PlanningRef, []ParsedOpenThread) {
	refs := make([]PlanningRef, 0, 8)
	seen := make(map[string]bool)
	parsed := make([]ParsedOpenThread, 0, len(openThreads))

	for _, entry := range openThreads {
		p, err := ParseOpenThreadMarkers(entry)
		if err != nil {
			// 解析失败的条目：NaturalSummary 保留原文，ParseError 由 ParseOpenThreadMarkers 已设
			parsed = append(parsed, p)
			continue
		}
		for _, roomID := range p.RoomIDs {
			key := "room/" + roomID
			if seen[key] {
				continue
			}
			seen[key] = true
			refs = append(refs, PlanningRef{Kind: "room", ID: roomID})
		}
		parsed = append(parsed, p)
	}

	return refs, parsed
}

// ExtractPlanningRefsWithErrors 与 ExtractPlanningRefs 相同，但额外返回解析错误列表，
// 供需要精确 error 信息的调用方使用（如 context builder 写 warnings）。
func ExtractPlanningRefsWithErrors(openThreads []string) (refs []PlanningRef, parsed []ParsedOpenThread, errs []ThreadParseError) {
	refs = make([]PlanningRef, 0, 8)
	seen := make(map[string]bool)
	parsed = make([]ParsedOpenThread, 0, len(openThreads))

	for idx, entry := range openThreads {
		p, err := ParseOpenThreadMarkers(entry)
		if err != nil {
			parsed = append(parsed, p)
			errs = append(errs, ThreadParseError{
				Index: idx,
				Text:  entry,
				Err:   err.Error(),
			})
			continue
		}
		for _, roomID := range p.RoomIDs {
			key := "room/" + roomID
			if seen[key] {
				continue
			}
			seen[key] = true
			refs = append(refs, PlanningRef{Kind: "room", ID: roomID})
		}
		parsed = append(parsed, p)
	}

	return refs, parsed, errs
}

// parseError 是解析过程中的错误类型。
type parseError struct{ msg string }

func (e *parseError) Error() string { return "planning archive refs 解析: " + e.msg }
