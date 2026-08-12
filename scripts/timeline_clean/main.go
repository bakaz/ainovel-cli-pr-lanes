// Command timeline_clean 一次性 timeline 清理工具（handoff 批次 2）。
//
// 背景：旧版该书曾写到 518 章，7/29-7/31 三次硬回滚把正文砍到 94 章，
// 但回滚从不清理 timeline.json，导致 2607 条事件里混入大量旧分支（ch95-518
// 共 2058 条，ch40-94 旧分支约 405 条）。
//
// 目标：保留 ch1-39 全部（81 条）+ 最后一代 ch40-94（63 条）= 144 条。
//
// 默认只 dry-run（打印统计、代际识别依据、候选结果与抽查），绝不写文件；
// 加 -apply 才写入（先自动备份，再复用 store.World.SaveTimeline 语义，
// 同步重写 timeline.json + timeline.md，保证两文件一致）。
//
// 用法：
//
//	go run ./scripts/timeline_clean                  # dry-run（默认）
//	go run ./scripts/timeline_clean -apply           # 执行：备份 + 写入
//	go run ./scripts/timeline_clean -dir <novel目录>  # 指定数据目录
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/store"
)

// genEvidence 代际识别的证据与校验结果。
type genEvidence struct {
	startIdx    int   // 最后一代段在数组中的起始索引
	endIdx      int   // 最后一代段结束索引（含）
	prevChapter int   // 段前一个元素（分界点）的章节号；-1 表示段前无元素
	missing     []int // [genStart, genEnd] 中段内缺失的章
	ordered     bool  // 段内章节是否非递减
	oldCnt      int   // 旧分支 ch40-94 条数（ch40-94 总数 - 段条数）
	// 交叉验证
	progressHasWordCount []int // progress.chapter_word_counts 中覆盖的 [genStart,genEnd] 章
	missingMd            []int // chapters/ 目录缺失的章
	lowAfterBlock        bool  // chapter<genStart 的条目出现在段之后（异常）
	dupKeys              int   // 全数组按稳定 key 完全重复的冗余条数（仅提示，不做去重）
}

func main() {
	dir := flag.String("dir", "workspace/output/novel", "小说数据目录（含 timeline.json / meta/progress.json / chapters/）")
	apply := flag.Bool("apply", false, "执行模式：先自动备份，再以 store.SaveTimeline 语义写入 timeline.json + timeline.md；默认 dry-run 不写入")
	genStart := flag.Int("gen-start", 40, "最后一代起始章（含）；chapter < 该值的条目全部保留")
	genEndFlag := flag.Int("gen-end", 0, "最后一代结束章（含）；0 = 从 progress 最大完成章推导")
	wantCount := flag.Int("want-count", 144, "期望候选条数断言；-1 关闭")
	wantMax := flag.Int("want-max", 94, "期望候选最大章断言；-1 关闭")
	flag.Parse()

	st := store.NewStore(*dir)
	// 复核阻塞项 4：可写 Store 入口统一 fail-closed（workspace 锁失败 /
	// checkpoint 损坏时不写盘）。
	if err := st.Init(); err != nil {
		fail("store 不可用（workspace 锁或 checkpoint 校验失败）: %v", err)
	}
	defer st.Close()
	world := st.World

	events, err := world.LoadTimeline()
	if err != nil {
		fail("读取 timeline.json 失败: %v", err)
	}
	if len(events) == 0 {
		fail("timeline.json 为空或无数据")
	}

	progress, err := st.Progress.Load()
	if err != nil {
		fail("读取 meta/progress.json 失败: %v", err)
	}

	genEnd := *genEndFlag
	if genEnd == 0 {
		if progress == nil || progress.LatestCompleted() == 0 {
			fail("无法从 progress 推导最后一代结束章（progress 缺失或未完成任何章）；请用 -gen-end 显式指定")
		}
		genEnd = progress.LatestCompleted()
	}
	if *genStart >= genEnd {
		fail("gen-start (%d) 必须小于 gen-end (%d)", *genStart, genEnd)
	}

	// ── [1] 输入统计 ──
	maxCh := 0
	cLow, cGen, cHigh := 0, 0, 0
	for _, e := range events {
		if e.Chapter > maxCh {
			maxCh = e.Chapter
		}
		switch {
		case e.Chapter < *genStart:
			cLow++
		case e.Chapter <= genEnd:
			cGen++
		default:
			cHigh++
		}
	}

	// ── [2] 代际识别：从数组末尾向前找最后一个 "章节<=genEnd" 的连续段 ──
	ev, err := findLastGeneration(events, *genStart, genEnd, progress, *dir)
	if err != nil {
		fail("%v", err)
	}
	block := events[ev.startIdx : ev.endIdx+1]
	ev.oldCnt = cGen - len(block)

	// 候选 = ch<genStart 全部（原顺序）+ 最后一代段（原顺序）
	var candidate []domain.TimelineEvent
	for _, e := range events {
		if e.Chapter < *genStart {
			candidate = append(candidate, e)
		}
	}
	candidate = append(candidate, block...)
	candMax := 0
	for _, e := range candidate {
		if e.Chapter > candMax {
			candMax = e.Chapter
		}
	}

	// ── [3] 输出 ──
	printHeader(*dir, *apply)
	printInputStats(cLow, cGen, cHigh, maxCh, len(events))
	printGenerationEvidence(ev, *genStart, genEnd, block)
	printCandidateStats(len(candidate), candMax, len(events), *wantCount, *wantMax)
	printSpotChecks(candidate, *genStart, genEnd)

	// ── [4] 断言 ──
	countOK := *wantCount < 0 || len(candidate) == *wantCount
	maxOK := *wantMax < 0 || candMax == *wantMax
	if !countOK || !maxOK {
		fmt.Printf("\n✗ 断言失败: 候选 %d 条 / max %d", len(candidate), candMax)
		if *wantCount >= 0 {
			fmt.Printf("（期望 %d 条）", *wantCount)
		}
		if *wantMax >= 0 {
			fmt.Printf("（期望 max %d）", *wantMax)
		}
		fmt.Println()
		fmt.Println("  请勿在未确认代际识别正确的情况下写入；可用 -want-count -1 -want-max -1 跳过断言（不推荐）。")
		if *apply {
			fail("断言失败，拒绝写入")
		}
		os.Exit(1)
	}

	// ── [5] 执行模式 ──
	if *apply {
		backupDir := filepath.Join(os.TempDir(), fmt.Sprintf("timeline-clean-backup-%s", time.Now().Format("20060102-150405")))
		fmt.Println("\n============================================================")
		fmt.Println(" 执行模式 (-apply)：先备份，再写入")
		fmt.Println("============================================================")
		hashList, err := backupFiles(*dir, backupDir, []string{"timeline.json", "timeline.md", "meta/progress.json"})
		if err != nil {
			fail("备份失败: %v", err)
		}
		fmt.Printf(" 备份目录: %s\n", backupDir)
		for _, rel := range []string{"timeline.json", "timeline.md", "meta/progress.json"} {
			fmt.Printf("   %-18s sha256=%s\n", rel, hashList[rel])
		}
		fmt.Println("   （批次 0 备份亦已保留于 handoff-batch0-*，此处为执行前独立快照）")

		if err := world.SaveTimeline(candidate); err != nil {
			fail("写入 timeline 失败（已备份，可从备份恢复）: %v", err)
		}
		// 重载验证
		loaded, err := world.LoadTimeline()
		if err != nil {
			fail("写入后重载 timeline.json 失败: %v", err)
		}
		lMax := 0
		for _, e := range loaded {
			if e.Chapter > lMax {
				lMax = e.Chapter
			}
		}
		if len(loaded) != len(candidate) || lMax != candMax {
			fail("写入后重载校验不一致: loaded=%d/max%d, 期望 %d/max%d；请从备份恢复", len(loaded), lMax, len(candidate), candMax)
		}
		if fi, err := os.Stat(filepath.Join(*dir, "timeline.md")); err != nil || fi.Size() == 0 {
			fail("timeline.md 重渲染结果缺失或为空")
		}
		fmt.Printf("\n 写入完成: %d 条 → %d 条（移除 %d 条），max chapter %d\n", len(events), len(loaded), len(events)-len(loaded), lMax)
		fmt.Println(" timeline.json + timeline.md 已同步（store.SaveTimeline 语义）")
		fmt.Println(" 提醒: 请确认 TUI 进程已停止；源码修复需重启新二进制后生效。")
		return
	}

	fmt.Println("\n[dry-run] 未写入任何文件（timeline.json / timeline.md 原样保留）。")
}

// findLastGeneration 定位最后一代段并做多层校验。
//
// 定位规则（不硬编码索引）：从数组末尾向前扫描，只要元素 chapter <= genEnd
// 就继续，遇到第一个 chapter > genEnd 的元素即停止；该段即为"最后一代"。
// 依据：当前代事件由 AppendTimelineEvents 追加在数组末尾（ch40-94 按序写入、
// 章节非递减），其前方必然紧邻旧分支的 ch95+（>genEnd）条目作为分界。
//
// 校验：
//  1. 段内所有章节 ∈ [genStart, genEnd]；
//  2. 段覆盖 [genStart, genEnd] 几乎全部章节（缺失 ≤ 2）；
//  3. 密度：段条数不超过章节区间长度的 2 倍（旧分支每章多事件、条数庞大，会超限）；
//  4. 与 progress.chapter_word_counts 交叉验证：段内章节必须有正字数；
//  5. chapters/%02d.md 文件存在性；
//  6. chapter < genStart 的条目不得出现在段之后（无低章污染）。
func findLastGeneration(events []domain.TimelineEvent, genStart, genEnd int, progress *domain.Progress, dir string) (*genEvidence, error) {
	ev := &genEvidence{startIdx: -1, ordered: true}

	// 末尾回溯
	startIdx := len(events) - 1
	for startIdx > 0 && events[startIdx-1].Chapter <= genEnd {
		startIdx--
	}
	if events[startIdx].Chapter > genEnd {
		return nil, fmt.Errorf("数组末尾没有 chapter<=%d 的条目（末条 ch=%d）：TUI 可能仍在写入 ch>%d 的事件，请先停止进程再运行", genEnd, events[len(events)-1].Chapter, genEnd)
	}
	ev.startIdx = startIdx
	ev.endIdx = len(events) - 1
	ev.prevChapter = -1
	if startIdx > 0 {
		ev.prevChapter = events[startIdx-1].Chapter
	}

	block := events[startIdx:]
	covered := make(map[int]bool, len(block))
	for i, e := range block {
		if e.Chapter < genStart {
			return nil, fmt.Errorf("最后一代段内出现 chapter<%d 的条目（索引 %d, ch=%d）：代际定位失败", genStart, startIdx+i, e.Chapter)
		}
		if i > 0 && e.Chapter < block[i-1].Chapter {
			ev.ordered = false
		}
		covered[e.Chapter] = true
	}
	rangeLen := genEnd - genStart + 1
	for ch := genStart; ch <= genEnd; ch++ {
		if !covered[ch] {
			ev.missing = append(ev.missing, ch)
		}
	}
	if len(ev.missing) > 2 {
		return nil, fmt.Errorf("最后一代段缺失章节过多 %v（覆盖 %d/%d），疑似定位到旧分支；请人工检查", ev.missing, len(covered), rangeLen)
	}
	if len(block) < rangeLen-len(ev.missing) {
		return nil, fmt.Errorf("最后一代段条数异常少（%d 条覆盖 %d 章）", len(block), len(covered))
	}
	if len(block) > 2*rangeLen {
		return nil, fmt.Errorf("最后一代段密度异常（%d 条 / %d 章 > 2 倍区间长度）：疑似旧分支被误纳入", len(block), rangeLen)
	}

	// progress 交叉验证：段内章节必须有正字数
	if progress != nil {
		for ch := range covered {
			if wc, ok := progress.ChapterWordCounts[ch]; !ok || wc <= 0 {
				return nil, fmt.Errorf("最后一代段内章节 %d 在 progress.chapter_word_counts 中无正字数：交叉验证失败", ch)
			}
		}
		for ch := genStart; ch <= genEnd; ch++ {
			if wc, ok := progress.ChapterWordCounts[ch]; ok && wc > 0 {
				ev.progressHasWordCount = append(ev.progressHasWordCount, ch)
			}
		}
	}

	// chapters/ 目录完整性
	for ch := genStart; ch <= genEnd; ch++ {
		if _, err := os.Stat(filepath.Join(dir, "chapters", fmt.Sprintf("%02d.md", ch))); err != nil {
			ev.missingMd = append(ev.missingMd, ch)
		}
	}

	// 低章条目不得出现在段之后
	lastLow := -1
	for i, e := range events {
		if e.Chapter < genStart {
			lastLow = i
		}
	}
	ev.lowAfterBlock = lastLow >= startIdx

	// 全数组稳定 key 完全重复条数（仅提示：本工具不做去重）
	seen := make(map[string]int, len(events))
	for _, e := range events {
		seen[eventKey(e)]++
	}
	for _, n := range seen {
		if n > 1 {
			ev.dupKeys += n - 1
		}
	}
	return ev, nil
}

// eventKey 与 internal/store.world.go 的 timelineEventKey 同构的稳定 key。
func eventKey(e domain.TimelineEvent) string {
	chars := append([]string(nil), e.Characters...)
	slices.Sort(chars)
	return fmt.Sprintf("%d|%s|%s|%s", e.Chapter, e.Time, e.Event, strings.Join(chars, ","))
}

// backupFiles 把指定相对文件复制到 destDir 并生成 SHA256SUMS.txt。
func backupFiles(srcDir, destDir string, rels []string) (map[string]string, error) {
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return nil, err
	}
	hashes := make(map[string]string, len(rels))
	var b strings.Builder
	for _, rel := range rels {
		data, err := os.ReadFile(filepath.Join(srcDir, rel))
		if err != nil {
			return nil, fmt.Errorf("读取 %s: %w", rel, err)
		}
		base := filepath.Base(rel)
		if err := os.WriteFile(filepath.Join(destDir, base), data, 0o644); err != nil {
			return nil, fmt.Errorf("复制 %s: %w", rel, err)
		}
		sum := sha256.Sum256(data)
		h := hex.EncodeToString(sum[:])
		hashes[rel] = h
		fmt.Fprintf(&b, "%s  %s\n", h, base)
	}
	if err := os.WriteFile(filepath.Join(destDir, "SHA256SUMS.txt"), []byte(b.String()), 0o644); err != nil {
		return nil, fmt.Errorf("写 SHA256SUMS.txt: %w", err)
	}
	return hashes, nil
}

// ── 输出辅助 ──

func printHeader(dir string, apply bool) {
	mode := "dry-run 模式（未写入任何文件）"
	if apply {
		mode = "执行模式（-apply）"
	}
	fmt.Println("============================================================")
	fmt.Printf(" timeline 清理工具 — %s\n", mode)
	fmt.Printf(" 数据目录: %s\n", dir)
	fmt.Println("============================================================")
}

func printInputStats(cLow, cGen, cHigh, maxCh, total int) {
	fmt.Println("\n[1] 输入统计")
	fmt.Printf("  timeline.json 总条数: %d\n", total)
	fmt.Printf("  最大章节: %d\n", maxCh)
	fmt.Printf("  ch1-39 条数（保留全部）: %d\n", cLow)
	fmt.Printf("  ch40-94 条数: %d（其中最后一代 + 旧分支，见 [2]）\n", cGen)
	fmt.Printf("  ch95+ 条数（将丢弃）: %d\n", cHigh)
}

func printGenerationEvidence(ev *genEvidence, genStart, genEnd int, block []domain.TimelineEvent) {
	fmt.Println("\n[2] 代际识别")
	fmt.Printf("  最后一代段 = 数组尾部连续段 [%d..%d]，共 %d 条\n", ev.startIdx, ev.endIdx, len(block))
	fmt.Printf("  向前回溯: 从末尾元素 (ch%d) 向前扫描，只要 chapter<=%d 就继续；\n", block[len(block)-1].Chapter, genEnd)
	if ev.prevChapter >= 0 {
		fmt.Printf("             在索引 %d 处遇到 ch=%d（>%d）停止 → 段起点 = %d\n", ev.startIdx-1, ev.prevChapter, genEnd, ev.startIdx)
	} else {
		fmt.Printf("             回溯到数组头（无分界元素）→ 段起点 = %d\n", ev.startIdx)
	}
	if ev.ordered {
		fmt.Println("  段内顺序: 章节非递减 = true（追加式写入特征）")
	} else {
		fmt.Println("  段内顺序: 章节非递减 = false ⚠（异常，请人工复核）")
	}
	missingStr := "无"
	if len(ev.missing) > 0 {
		missingStr = intList(ev.missing)
	}
	fmt.Printf("  覆盖章节: %d..%d（%d 章），缺失: %s\n", genStart, genEnd, genEnd-genStart+1, missingStr)
	rangeLen := genEnd - genStart + 1
	fmt.Printf("  密度: %.2f 条/章（%d 条 / %d 章）", float64(len(block))/float64(rangeLen), len(block), rangeLen)
	fmt.Printf("；旧分支 ch40-94 共 %d 条（≈%.2f 条/章）\n", ev.oldCnt, float64(ev.oldCnt)/float64(rangeLen))
	fmt.Println("  与 progress.json / 正文交叉验证:")
	fmt.Printf("    - chapter_word_counts 覆盖 [%d..%d] 共 %d 章，段内所有章节均有正字数 ✓\n", genStart, genEnd, len(ev.progressHasWordCount))
	md := "全部存在 ✓"
	if len(ev.missingMd) > 0 {
		md = fmt.Sprintf("缺失: %s ⚠", intList(ev.missingMd))
	}
	fmt.Printf("    - chapters/ 目录 %02d.md..%02d.md: %s\n", genStart, genEnd, md)
	if ev.lowAfterBlock {
		fmt.Println("    - ⚠ chapter<genStart 的条目出现在段之后（异常）")
	} else {
		fmt.Println("    - ch<genStart 条目全部位于段之前，无低章污染 ✓")
	}
	if ev.dupKeys > 0 {
		fmt.Printf("    - 全数组按稳定 key 完全重复 %d 条（仅提示；同章同 time 不同事件不是重复，本工具不去重）\n", ev.dupKeys)
	}
	fmt.Println("  排除法:")
	fmt.Println("    - 不能按 (chapter,time) 去重：同章同 time 下可有多事件（如 ch56 三条），会误删真实事件")
	fmt.Println("    - 不能只过滤 chapter<=94：ch40-94 旧分支另有大量条目散布在数组中部，会被误保留")
}

func printCandidateStats(cand, candMax, total, wantCount, wantMax int) {
	fmt.Println("\n[3] 候选结果")
	fmt.Printf("  候选条数: %d", cand)
	if wantCount >= 0 {
		mark := "✓"
		if cand != wantCount {
			mark = "✗"
		}
		fmt.Printf("（期望 %d %s）", wantCount, mark)
	}
	fmt.Println()
	fmt.Printf("  候选 max chapter: %d", candMax)
	if wantMax >= 0 {
		mark := "✓"
		if candMax != wantMax {
			mark = "✗"
		}
		fmt.Printf("（期望 %d %s）", wantMax, mark)
	}
	fmt.Println()
	fmt.Printf("  将移除: %d 条（%d → %d）\n", total-cand, total, cand)
}

func printSpotChecks(candidate []domain.TimelineEvent, genStart, genEnd int) {
	fmt.Println("\n[4] 抽查：候选保留的事件（ch40/50/60/80/94 各前 2 条）")
	probes := []int{40, 50, 60, 80, 94}
	for _, ch := range probes {
		if ch < genStart || ch > genEnd {
			continue
		}
		fmt.Printf("  ch%d:\n", ch)
		n := 0
		for _, e := range candidate {
			if e.Chapter != ch {
				continue
			}
			chars := ""
			if len(e.Characters) > 0 {
				chars = "（" + strings.Join(e.Characters, "、") + "）"
			}
			fmt.Printf("    %d) [%s] %s%s\n", n+1, e.Time, clip(e.Event, 60), chars)
			n++
			if n >= 2 {
				break
			}
		}
		if n == 0 {
			fmt.Println("    （无候选事件）")
		}
	}
}

func clip(s string, n int) string {
	r := []rune(strings.ReplaceAll(s, "\n", " "))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n]) + "…"
}

func intList(v []int) string {
	parts := make([]string, len(v))
	for i, x := range v {
		parts[i] = fmt.Sprintf("%d", x)
	}
	return strings.Join(parts, ", ")
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "✗ 错误: "+format+"\n", args...)
	os.Exit(1)
}
