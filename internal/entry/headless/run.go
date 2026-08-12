package headless

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/voocel/ainovel-cli/assets"
	"github.com/voocel/ainovel-cli/internal/bootstrap"
	"github.com/voocel/ainovel-cli/internal/diag"
	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/entry/startup"
	"github.com/voocel/ainovel-cli/internal/host"
	"github.com/voocel/ainovel-cli/internal/logger"
	"github.com/voocel/ainovel-cli/internal/store"
)

type Options struct {
	Prompt         string
	ContinuePrompt string
	Stdin          io.Reader
	Stdout         io.Writer
	Stderr         io.Writer
}

// Run 以无界面模式运行会话内核，直接消费 Engine 事件与流式输出。
// 未来若新增“续写已有小说”等共享启动方式，不应直接堆到这里，
// 而应先落到 internal/entry/startup，再由 headless 入口调用。
func Run(cfg bootstrap.Config, bundle assets.Bundle, opts Options) error {
	stdout := opts.Stdout
	if stdout == nil {
		stdout = os.Stdout
	}
	stderr := opts.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}
	stdin := opts.Stdin
	if stdin == nil {
		stdin = os.Stdin
	}

	eng, err := host.New(cfg, bundle)
	if err != nil {
		return err
	}
	continuePrompt := strings.TrimSpace(opts.ContinuePrompt)
	if continuePrompt == "" {
		// 自动化继续模式（--continue-prompt-file）禁用阻塞式终端 AskUser：
		// ask_user 走 tools 默认的非阻塞降级，避免无人值守时挂死在 stdin 读取。
		eng.AskUser().SetHandler(newTerminalAskUser(stdin, stderr).handle)
	}
	cleanup := logger.SetupFile(eng.Dir(), "headless.log", false)
	defer cleanup()
	defer eng.Close()
	// 运行结束 / 出错返回时落一份脱敏诊断，方便 headless 用户贴 issue。
	// （外部 kill 的挂死不走 defer，仍需在 TUI 里手动 /diag。）
	// 复核阻塞项 2 只读模式：诊断导出只读，用 NewReadOnlyStore（不取 workspace
	// 排他锁，引擎关闭前仍可执行；避免同进程第二可写 Store 被拒）。
	defer func() { _, _ = diag.Export(store.NewReadOnlyStore(eng.Dir())) }()

	prompt := strings.TrimSpace(opts.Prompt)
	if continuePrompt != "" && prompt != "" {
		return fmt.Errorf("--continue-prompt-file 与 --prompt/--prompt-file 不能同时使用")
	}
	if continuePrompt != "" {
		// 向现有书注入继续指令：绝不触碰新建书路径（PrepareUserRules/
		// StartPrepared/Checkpoints.Reset/Progress.Init/ReplayQueue/Resume）。
		return runContinue(eng, stdout, stderr, continuePrompt)
	}
	if prompt != "" {
		plan, err := startup.PrepareQuick(startup.Request{
			Mode:        startup.ModeQuick,
			UserPrompt:  prompt,
			OutputDir:   eng.Dir(),
			Interactive: true,
		})
		if err != nil {
			return err
		}
		fmt.Fprintf(stderr, "headless 启动: %s\n", eng.Dir())
		// 启动侧确定性生成本书用户规则快照（用原始 prompt 归一化），须在 StartPrepared 前。
		if err := eng.PrepareUserRules(plan.RawPrompt); err != nil {
			return err
		}
		if err := eng.StartPrepared(plan.RawPrompt); err != nil {
			return err
		}
	} else {
		items, err := eng.ReplayQueue(0)
		if err != nil {
			return err
		}
		roundHasContent, err := replayQueue(items, stdout, stderr)
		if err != nil {
			return err
		}
		label, err := eng.Resume()
		if err != nil {
			return err
		}
		if label == "" {
			return fmt.Errorf("headless 模式需要 --prompt，或输出目录 %q 下已有可恢复会话", eng.Dir())
		}
		fmt.Fprintf(stderr, "headless 恢复: %s (%s)\n", eng.Dir(), label)
		if _, err := consume(eng, stdout, stderr, roundHasContent); err != nil {
			return err
		}
		return nil
	}

	if _, err := consume(eng, stdout, stderr, false); err != nil {
		return err
	}
	return nil
}

// engineSource 是 consume / drainPending / incompleteRecoveryErr 依赖的 Host 最小
// 事件面（生产实现是 *host.Host；测试用假实现注入通道与目录）。
type engineSource interface {
	Dir() string
	Events() <-chan host.Event
	Stream() <-chan string
	Done() <-chan struct{}
}

// consumeStats 汇总 consume 过程中的观测数据，供 --continue-prompt-file 的退出摘要使用。
type consumeStats struct {
	errorEvents int // 写往 stderr 的 error 级事件数
}

func consume(eng engineSource, stdout, stderr io.Writer, roundHasContent bool) (*consumeStats, error) {
	stats := &consumeStats{}
	for {
		select {
		case ev, ok := <-eng.Events():
			if !ok {
				return stats, incompleteRecoveryErr(eng, stats)
			}
			if ev.Level == "error" {
				stats.errorEvents++
			}
			writeEvent(stderr, ev)
		case delta, ok := <-eng.Stream():
			if !ok {
				continue
			}
			if delta == host.StreamClearSentinel {
				if roundHasContent {
					if _, err := io.WriteString(stdout, "\n\n"); err != nil {
						return stats, err
					}
					roundHasContent = false
				}
				continue
			}
			if delta == "" {
				continue
			}
			if _, err := io.WriteString(stdout, delta); err != nil {
				return stats, err
			}
			roundHasContent = true
		case _, ok := <-eng.Done():
			if !ok {
				// 引擎结束（done 关闭）：先排空已缓冲的事件再判恢复完整性，
				// 避免 select 随机抢先导致 error 事件漏计（漏计 → 误判成功）。
				var err error
				stats, err = drainPending(eng, stdout, stderr, roundHasContent, stats)
				if err != nil {
					return stats, err
				}
				return stats, incompleteRecoveryErr(eng, stats)
			}
			// drain 阶段与主循环共享同一 stats：Done 前已累计的 error 事件不得丢弃。
			var err error
			stats, err = drainPending(eng, stdout, stderr, roundHasContent, stats)
			if err != nil {
				return stats, err
			}
			return stats, incompleteRecoveryErr(eng, stats)
		}
	}
}

func drainPending(eng engineSource, stdout, stderr io.Writer, roundHasContent bool, stats *consumeStats) (*consumeStats, error) {
	if stats == nil {
		stats = &consumeStats{}
	}
	// 关闭的 channel 永远 ready：若任一通道已关闭仍继续 select，default 永不触发，
	// 会退化为 100% CPU 自旋。用标志位记录关闭状态，两侧都关闭后直接收尾。
	eventsClosed, streamClosed := false, false
	for {
		if eventsClosed && streamClosed {
			if roundHasContent {
				if _, err := io.WriteString(stdout, "\n"); err != nil {
					return stats, err
				}
			}
			return stats, nil
		}
		select {
		case ev, ok := <-eng.Events():
			if !ok {
				eventsClosed = true
				continue
			}
			if ev.Level == "error" {
				stats.errorEvents++
			}
			writeEvent(stderr, ev)
		case delta, ok := <-eng.Stream():
			if !ok {
				streamClosed = true
				continue
			}
			if delta == host.StreamClearSentinel {
				if roundHasContent {
					if _, err := io.WriteString(stdout, "\n\n"); err != nil {
						return stats, err
					}
					roundHasContent = false
				}
				continue
			}
			if delta != "" {
				if _, err := io.WriteString(stdout, delta); err != nil {
					return stats, err
				}
				roundHasContent = true
			}
		default:
			if roundHasContent {
				if _, err := io.WriteString(stdout, "\n"); err != nil {
					return stats, err
				}
			}
			return stats, nil
		}
	}
}

// incompleteRecoveryErr 检查恢复是否真正完成：error 事件、PendingRewrites 未排空、
// AdvanceHold 生效均视为"恢复未完成"。任一存在 → 返回 ExitCodeError{Code: 5}
// （main 经 errors.As 落为退出码 5），防止 headless 自动化把不完整恢复误判为成功。
// 仅在引擎事件流结束后调用（此时状态已落盘，可安全读取）。
//
// 读取失败/关键状态文件缺失同样视为恢复未完成（fail-closed）：无法核验状态就
// 不能宣称成功——corrupted JSON 与缺失文件均返回 ExitCodeError{Code: 5}。
func incompleteRecoveryErr(eng engineSource, stats *consumeStats) error {
	var problems []string
	if stats.errorEvents > 0 {
		problems = append(problems, fmt.Sprintf("error 事件 %d 个", stats.errorEvents))
	}
	// 只读校验恢复状态：用 NewReadOnlyStore（复核阻塞项 2 只读模式，引擎 store
	// 仍持有写锁时也可安全读取）。
	st := store.NewReadOnlyStore(eng.Dir())
	p, err := st.Progress.Load()
	if err != nil {
		return &ExitCodeError{Code: 5, Err: fmt.Errorf("恢复状态读取失败（meta/progress.json）: %w", err)}
	}
	if p == nil {
		return &ExitCodeError{Code: 5, Err: fmt.Errorf("恢复状态读取失败：meta/progress.json 缺失")}
	}
	if len(p.PendingRewrites) > 0 {
		problems = append(problems, fmt.Sprintf("PendingRewrites 未排空（剩余 %d 章: %v）", len(p.PendingRewrites), p.PendingRewrites))
	}
	meta, err := st.RunMeta.Load()
	if err != nil {
		return &ExitCodeError{Code: 5, Err: fmt.Errorf("恢复状态读取失败（meta/run.json）: %w", err)}
	}
	if meta == nil {
		return &ExitCodeError{Code: 5, Err: fmt.Errorf("恢复状态读取失败：meta/run.json 缺失")}
	}
	if meta.AdvanceHold != nil {
		problems = append(problems, fmt.Sprintf("AdvanceHold 生效中（after=%s, reason=%s）", meta.AdvanceHold.After, meta.AdvanceHold.Reason))
	}
	if len(problems) == 0 {
		return nil
	}
	return &ExitCodeError{Code: 5, Err: fmt.Errorf("恢复未完成: %s", strings.Join(problems, "；"))}
}

func writeEvent(w io.Writer, ev host.Event) {
	if w == nil || strings.TrimSpace(ev.Summary) == "" {
		return
	}
	ts := ev.Time.Format("15:04:05")
	if ts == "00:00:00" {
		ts = "--:--:--"
	}
	fmt.Fprintf(w, "[%s] [%s] %s\n", ts, ev.Category, ev.Summary)
}

func replayQueue(items []domain.RuntimeQueueItem, stdout, stderr io.Writer) (bool, error) {
	var roundHasContent bool
	for _, item := range items {
		switch item.Kind {
		case domain.RuntimeQueueUIEvent:
			writeEvent(stderr, host.Event{
				Time:     item.Time,
				Category: item.Category,
				Summary:  item.Summary,
			})
		case domain.RuntimeQueueStreamClear:
			if roundHasContent {
				if _, err := io.WriteString(stdout, "\n\n"); err != nil {
					return roundHasContent, err
				}
				roundHasContent = false
			}
		case domain.RuntimeQueueStreamDelta:
			text := host.ReplayDeltaText(item)
			if text == "" {
				continue
			}
			if _, err := io.WriteString(stdout, text); err != nil {
				return roundHasContent, err
			}
			roundHasContent = true
		}
	}
	return roundHasContent, nil
}
