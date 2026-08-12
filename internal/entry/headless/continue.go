package headless

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/voocel/ainovel-cli/internal/host"
	"github.com/voocel/ainovel-cli/internal/store"
)

// continueStartTimeout 是 --continue-prompt-file 模式下从提交干预到引擎进入
// running 状态的墙钟超时；超时判定 no_run（退出码 3）并优雅中止。
// 包级 var 供测试缩短超时。
var continueStartTimeout = 120 * time.Second

// ExitCodeError 携带明确进程退出码的错误，供 headless 自动化区分失败语义：
// 2=干预失败，3=引擎未启动（含超时），4=拒绝执行，5=恢复未完成。
// main 用 errors.As 取出并落码。
type ExitCodeError struct {
	Code int
	Err  error
}

func (e *ExitCodeError) Error() string {
	if e.Err == nil {
		return fmt.Sprintf("exit code %d", e.Code)
	}
	return e.Err.Error()
}

func (e *ExitCodeError) Unwrap() error { return e.Err }

// continueEngine 是 runContinue 依赖的 Host 面（生产实现是 *host.Host；
// 测试用假实现注入干预结果与事件通道）。
type continueEngine interface {
	engineSource
	ContinueAndWait(text string) (host.InterventionOutcome, error)
	Close()
}

// runContinue 向现有书注入继续指令（--continue-prompt-file 的入口）：
//
//  1. 执行前检查 RunMeta.PendingSteer，非空则拒绝执行（退出码 4），不得静默覆盖；
//  2. 同步等待 arbiter 干预裁定与引擎启动尝试（ContinueAndWait，非 fire-and-forget），
//     墙钟超时优雅中止并判 no_run（退出码 3）；
//  3. 引擎启动成功 → 复用 consume 直到 Done，随后打印退出摘要
//     （PendingRewrites / AdvanceHold / error 事件）——不能仅凭 Done 判成功；
//     consume 检测到 error 事件 / 重写未排空 / hold 未消费时返回 ExitCodeError
//     （退出码 5），摘要先打印再上抛，诊断信息不丢失。
//
// 本路径绝不触碰新建书路径（PrepareUserRules / StartPrepared / Checkpoints.Reset /
// Progress.Init / ReplayQueue / Resume），也不安装阻塞式终端 AskUser。
func runContinue(eng continueEngine, stdout, stderr io.Writer, prompt string) error {
	// P0-4：执行前检查 PendingSteer，非空拒绝执行（退出码 4）。
	// 复核阻塞项 2 只读模式：引擎 store 持有写锁时也可安全读取。
	st := store.NewReadOnlyStore(eng.Dir())
	meta, err := st.RunMeta.Load()
	if err != nil {
		return &ExitCodeError{Code: 3, Err: fmt.Errorf("读取运行元信息失败: %w", err)}
	}
	if meta != nil && strings.TrimSpace(meta.PendingSteer) != "" {
		return &ExitCodeError{Code: 4, Err: fmt.Errorf(
			"存在未处理的继续指令 %q，拒绝执行 --continue-prompt-file（请先恢复会话处理该指令后再注入）",
			meta.PendingSteer)}
	}

	fmt.Fprintf(stderr, "headless 继续: %s\n", eng.Dir())

	// P0-2：同步等待干预结果。ContinueAndWait 在返回前完成裁定、动作应用与引擎
	// 启动尝试；这里用带超时的等待通道把墙钟超时（no_run）与结果明确区分开。
	type waitResult struct {
		outcome host.InterventionOutcome
		err     error
	}
	ch := make(chan waitResult, 1)
	go func() {
		outcome, err := eng.ContinueAndWait(prompt)
		ch <- waitResult{outcome: outcome, err: err}
	}()

	var outcome host.InterventionOutcome
	select {
	case r := <-ch:
		outcome = r.outcome
		if r.err != nil {
			// 前置校验失败（迁移门/共创/恢复中/预算拒绝）：引擎未启动。
			return &ExitCodeError{Code: 3, Err: fmt.Errorf("继续指令无法执行: %w", r.err)}
		}
		if !outcome.OK {
			return &ExitCodeError{Code: 2, Err: fmt.Errorf("干预失败（未做任何修改）: %w", outcome.Failure)}
		}
		if !outcome.EngineRunning {
			return &ExitCodeError{Code: 3, Err: fmt.Errorf("干预已应用但引擎未启动: %v", outcome.Failure)}
		}
	case <-time.After(continueStartTimeout):
		// 优雅中止：Close 取消在途裁定（runCtx）并中止引擎循环；随后有界等待
		// 干预收尾，避免超时判定后引擎仍被启动写盘。
		eng.Close()
		select {
		case <-ch:
		case <-time.After(5 * time.Second):
		}
		return &ExitCodeError{Code: 3, Err: fmt.Errorf(
			"超时：%s 内引擎未进入运行状态，已中止继续指令", continueStartTimeout)}
	}

	stats, err := consume(eng, stdout, stderr, false)
	// 摘要先打印再上抛错误：即使恢复未完成（退出码 5），诊断信息也不丢失。
	printContinueSummary(stderr, eng, stats)
	if err != nil {
		return err
	}
	return nil
}

// printContinueSummary 打印 --continue-prompt-file 的退出摘要：引擎收到 Done
// 不等于成功，需核对 error 事件、PendingRewrites 是否排空与 AdvanceHold 状态。
// 状态读取失败/关键文件缺失时打印"状态读取失败"，绝不误报为"已排空/无"。
func printContinueSummary(stderr io.Writer, eng engineSource, stats *consumeStats) {
	// 复核阻塞项 2 只读模式：只读校验，不取 workspace 写锁。
	st := store.NewReadOnlyStore(eng.Dir())
	fmt.Fprintln(stderr, "── 退出摘要 ──")
	if stats.errorEvents > 0 {
		fmt.Fprintf(stderr, "- error 事件: %d（引擎可能异常停止，请检查上方日志）\n", stats.errorEvents)
	} else {
		fmt.Fprintln(stderr, "- error 事件: 0")
	}
	if p, err := st.Progress.Load(); err != nil || p == nil {
		fmt.Fprintln(stderr, "- PendingRewrites: 状态读取失败")
	} else if len(p.PendingRewrites) > 0 {
		fmt.Fprintf(stderr, "- PendingRewrites: 未排空（剩余 %d 章: %v）\n", len(p.PendingRewrites), p.PendingRewrites)
	} else {
		fmt.Fprintln(stderr, "- PendingRewrites: 已排空")
	}
	if meta, err := st.RunMeta.Load(); err != nil || meta == nil {
		fmt.Fprintln(stderr, "- AdvanceHold: 状态读取失败")
	} else if meta.AdvanceHold != nil {
		fmt.Fprintf(stderr, "- AdvanceHold: 生效中（after=%s, reason=%s）\n",
			meta.AdvanceHold.After, meta.AdvanceHold.Reason)
	} else {
		fmt.Fprintln(stderr, "- AdvanceHold: 无")
	}
}
