package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/voocel/ainovel-cli/assets"
	"github.com/voocel/ainovel-cli/internal/bootstrap"
	"github.com/voocel/ainovel-cli/internal/entry/headless"
	"github.com/voocel/ainovel-cli/internal/entry/tui"
	"github.com/voocel/ainovel-cli/internal/eval"
	"github.com/voocel/ainovel-cli/internal/rules"
	buildversion "github.com/voocel/ainovel-cli/internal/version"
)

var (
	version = "dev"
	commit  = "unknown"
	date    = "unknown"
)

// headlessMode 记录本次是否 headless 启动，供 die 决定错误退出时是否暂停。
var headlessMode bool

func main() {
	// 子命令在常规 flag 解析之前拦截：eval 是离线评测 harness，参数体系独立。
	if len(os.Args) > 1 && os.Args[1] == "eval" {
		os.Exit(eval.Command(os.Args[2:]))
	}
	// inspect-customizations 是只读的生产装配诊断：它与 TUI/headless 共用
	// LoadProduction，但不构造模型、不发 Provider 请求、不写小说状态。
	if len(os.Args) > 1 && os.Args[1] == "inspect-customizations" {
		os.Exit(inspectCustomizations(os.Args[2:]))
	}
	// migrate-legacy-rooms 是一次性迁移工具：加载配置、创建 Store、调用
	// Store.MigrateLegacyRooms()，打印状态后退出。不在 TUI/Host 启动时运行，
	// 不触发 long 审批。
	if len(os.Args) > 1 && os.Args[1] == "migrate-legacy-rooms" {
		os.Exit(migrateLegacyRooms(os.Args[2:]))
	}

	opts, args, err := parseCLIOptions(os.Args[1:])
	if err != nil {
		die("flags: %v", err)
	}
	if opts.Version {
		buildversion.Print(os.Stdout, versionInfo())
		return
	}
	if opts.Update {
		if err := runSelfUpdate(opts.UpdateVersion); err != nil {
			fmt.Fprintf(os.Stderr, "update: %v\n", err)
			os.Exit(1)
		}
		return
	}
	headlessMode = opts.Headless

	// 首次引导
	if bootstrap.NeedsSetup(opts.ConfigPath) {
		if opts.Headless {
			die("error: headless 模式不支持首次引导，请先运行一次 TUI 完成配置")
		}
		setupCfg, err := bootstrap.RunSetup()
		if err != nil {
			die("setup: %v", err)
		}
		// 引导完成后使用生成的配置继续
		runWithConfig(setupCfg, opts, args)
		return
	}

	// 加载配置
	cfg, err := bootstrap.LoadConfig(opts.ConfigPath)
	if err != nil {
		die("config: %v", err)
	}

	runWithConfig(cfg, opts, args)
}

// die 统一处理致命错误退出：打印到 stderr、落盘到 ~/.ainovel/last-error.log，
// 并在交互式终端（非 headless）下暂停等待回车——双击启动时控制台会随进程退出
// 立即关闭，不暂停的话错误一闪而过，正是 issue #37 里用户无从排查的根因。
func die(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintln(os.Stderr, msg)
	if path := bootstrap.WriteStartupError(msg); path != "" {
		fmt.Fprintf(os.Stderr, "（详细错误已记录到 %s）\n", path)
	}
	if !headlessMode && stdinIsTerminal() {
		fmt.Fprint(os.Stderr, "\n按回车键退出...")
		fmt.Fscanln(os.Stdin)
	}
	os.Exit(1)
}

// stdinIsTerminal 判断标准输入是否连接到终端（字符设备）。双击启动 / 交互式终端
// 为 true；管道、重定向、CI 为 false。零依赖近似，足够区分要不要暂停。
func stdinIsTerminal() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

func runWithConfig(cfg bootstrap.Config, opts cliOptions, args []string) {
	rules.EnsureHomeRulesDir()

	if len(args) > 0 {
		die("error: 不再支持命令行直接传入小说需求，请启动后在 TUI 输入框中输入")
	}

	// FillDefaults 必须先于资产加载:OutputDir 是运行时字段,默认值在此归一——
	// 否则默认配置下 <书目录>/style/ 的本书级文风覆盖永远不会被加载。
	cfg.FillDefaults()
	bundle, report, err := assets.LoadProduction(cfg.Style, cfg.OutputDir, opts.PromptsDir)
	if err != nil {
		die("error: %v", err)
	}
	for _, warning := range report.Warnings {
		fmt.Fprintln(os.Stderr, "warning:", warning)
		slog.Warn("asset overlay warning", "warning", warning)
	}
	for _, applied := range report.Applied {
		slog.Debug("asset overlay applied", "key", applied.Key, "kind", applied.Kind, "path", applied.Path, "sha256", applied.SHA256)
	}
	if opts.Headless {
		prompt, err := loadPrompt(opts)
		if err != nil {
			die("error: %v", err)
		}
		continuePrompt, err := loadContinuePrompt(opts)
		if err != nil {
			die("error: %v", err)
		}
		if err := headless.Run(cfg, bundle, headless.Options{Prompt: prompt, ContinuePrompt: continuePrompt}); err != nil {
			// headless 自动化失败语义：2=干预失败 / 3=引擎未启动 / 4=拒绝执行 / 5=恢复未完成。
			var ec *headless.ExitCodeError
			if errors.As(err, &ec) {
				exitWithCode(ec.Code, "error: %v", ec)
			}
			die("error: %v", err)
		}
		return
	}
	if opts.PromptSet || opts.PromptFileSet || opts.ContinuePromptSet {
		die("error: --prompt/--prompt-file/--continue-prompt-file 仅能在 --headless 模式下使用")
	}
	if err := tui.Run(cfg, bundle, versionInfo().Version); err != nil {
		die("error: %v", err)
	}
}

type cliOptions struct {
	ConfigPath         string
	Headless           bool
	Prompt             string
	PromptSet          bool // --prompt 是否显式出现（空值也记录，显式空值拒绝）
	PromptFile         string
	PromptFileSet      bool // --prompt-file 是否显式出现（空值也记录，显式空值拒绝）
	ContinuePromptFile string
	ContinuePromptSet  bool // --continue-prompt-file 是否显式出现（空值也记录，拒绝静默落入普通 resume）
	PromptsDir         string
	Version            bool
	Update             bool
	UpdateVersion      string
}

// parseCLIOptions 提取 CLI flag，返回选项和剩余参数。
func parseCLIOptions(argv []string) (cliOptions, []string, error) {
	var opts cliOptions
	var args []string
	for i := 0; i < len(argv); i++ {
		switch argv[i] {
		case "--version", "-v":
			opts.Version = true
		case "version":
			if i+1 < len(argv) {
				return opts, nil, fmt.Errorf("version 不接受参数")
			}
			opts.Version = true
		case "update":
			if opts.Update {
				return opts, nil, fmt.Errorf("update 只能指定一次")
			}
			opts.Update = true
			if i+1 < len(argv) {
				if strings.HasPrefix(argv[i+1], "-") {
					return opts, nil, fmt.Errorf("update 只接受一个可选版本参数")
				}
				opts.UpdateVersion = argv[i+1]
				i++
			}
			if i+1 < len(argv) {
				return opts, nil, fmt.Errorf("update 只接受一个可选版本参数")
			}
		case "--config":
			if i+1 >= len(argv) {
				return opts, nil, fmt.Errorf("--config 缺少值")
			}
			opts.ConfigPath = argv[i+1]
			i++
		case "--headless":
			opts.Headless = true
		case "--prompt":
			if i+1 >= len(argv) {
				return opts, nil, fmt.Errorf("--prompt 缺少值")
			}
			opts.PromptSet = true
			opts.Prompt = argv[i+1]
			i++
		case "--prompt-file":
			if i+1 >= len(argv) {
				return opts, nil, fmt.Errorf("--prompt-file 缺少值")
			}
			opts.PromptFileSet = true
			opts.PromptFile = argv[i+1]
			i++
		case "--continue-prompt-file":
			if i+1 >= len(argv) {
				return opts, nil, fmt.Errorf("--continue-prompt-file 缺少值")
			}
			opts.ContinuePromptSet = true
			opts.ContinuePromptFile = argv[i+1]
			i++
		case "--prompts-dir":
			if i+1 >= len(argv) {
				return opts, nil, fmt.Errorf("--prompts-dir 缺少值")
			}
			opts.PromptsDir = argv[i+1]
			i++
		default:
			args = append(args, argv[i])
		}
	}
	// 冲突校验用显式性（Set 标志）而非字符串值：显式空值同样计入冲突，
	// 防止 `--continue-prompt-file x --prompt ""` 之类的组合静默放行。
	if opts.PromptSet && opts.PromptFileSet {
		return opts, nil, fmt.Errorf("--prompt 和 --prompt-file 不能同时使用")
	}
	if opts.ContinuePromptSet && (opts.PromptSet || opts.PromptFileSet) {
		return opts, nil, fmt.Errorf("--continue-prompt-file 与 --prompt/--prompt-file 不能同时使用")
	}
	if opts.Version && (opts.Update || opts.ConfigPath != "" || opts.Headless || opts.PromptSet || opts.PromptFileSet || opts.ContinuePromptSet || opts.PromptsDir != "" || len(args) > 0) {
		return opts, nil, fmt.Errorf("version 不能与其他启动参数混用")
	}
	if opts.Update && (opts.ConfigPath != "" || opts.Headless || opts.PromptSet || opts.PromptFileSet || opts.ContinuePromptSet || opts.PromptsDir != "" || len(args) > 0) {
		return opts, nil, fmt.Errorf("update 不能与其他启动参数混用")
	}
	return opts, args, nil
}

func versionInfo() buildversion.Info {
	return buildversion.Resolve(buildversion.Info{
		Version: version,
		Commit:  commit,
		Date:    date,
	})
}

func runSelfUpdate(target string) error {
	info := versionInfo()
	result, err := buildversion.Update(context.Background(), buildversion.UpdateOptions{
		Repo:           "voocel/ainovel-cli",
		BinaryName:     "ainovel-cli",
		TargetVersion:  target,
		CurrentVersion: info.Version,
	})
	if err != nil {
		return err
	}
	if !result.Updated {
		fmt.Printf("ainovel-cli 已是最新版本 %s\n", result.Version)
		return nil
	}
	fmt.Printf("ainovel-cli 已更新到 %s\n", result.Version)
	fmt.Printf("安装位置：%s\n", result.Path)
	return nil
}

// loadPrompt 读取 --prompt / --prompt-file 的内容。flag 未显式出现 → 返回空
// （headless 落入普通恢复路径）；显式出现但值为空 / 路径为空 / 内容为空 → 错误，
// 防止显式空值被当成"未指定"而静默落入普通恢复（与注入意图不符）。
func loadPrompt(opts cliOptions) (string, error) {
	if !opts.PromptSet && !opts.PromptFileSet {
		return "", nil
	}
	if opts.PromptSet && opts.PromptFileSet {
		return "", fmt.Errorf("--prompt 和 --prompt-file 不能同时使用")
	}
	if opts.PromptSet {
		if strings.TrimSpace(opts.Prompt) == "" {
			return "", fmt.Errorf("--prompt 内容为空")
		}
		return strings.TrimSpace(opts.Prompt), nil
	}
	if strings.TrimSpace(opts.PromptFile) == "" {
		return "", fmt.Errorf("--prompt-file 缺少文件路径")
	}
	return readPromptFile(opts.PromptFile)
}

// loadContinuePrompt 读取 --continue-prompt-file 的内容。flag 未显式出现 → 返回空
// （headless 落入普通恢复路径）；显式出现但路径为空 / 内容为空 → 错误，防止空值
// 被当成"未指定"而静默落入普通 resume（与注入意图不符）。
func loadContinuePrompt(opts cliOptions) (string, error) {
	if !opts.ContinuePromptSet {
		return "", nil
	}
	if strings.TrimSpace(opts.ContinuePromptFile) == "" {
		return "", fmt.Errorf("--continue-prompt-file 缺少文件路径")
	}
	text, err := readPromptFile(opts.ContinuePromptFile)
	if err != nil {
		return "", err
	}
	if text == "" {
		return "", fmt.Errorf("--continue-prompt-file 内容为空")
	}
	return text, nil
}

// readPromptFile 读取 prompt 文件内容（"-" 表示 stdin），与既有 --prompt-file 同规则。
func readPromptFile(path string) (string, error) {
	var data []byte
	var err error
	if path == "-" {
		data, err = os.ReadFile("/dev/stdin")
	} else {
		data, err = os.ReadFile(path)
	}
	if err != nil {
		return "", fmt.Errorf("读取 prompt 失败: %w", err)
	}
	return strings.TrimSpace(string(data)), nil
}

// exitWithCode 以指定退出码结束进程（headless 自动化失败语义：
// 2=干预失败 / 3=引擎未启动 / 4=拒绝执行 / 5=恢复未完成），与 die 相同地打印并落盘错误日志。
func exitWithCode(code int, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintln(os.Stderr, msg)
	if path := bootstrap.WriteStartupError(msg); path != "" {
		fmt.Fprintf(os.Stderr, "（详细错误已记录到 %s）\n", path)
	}
	os.Exit(code)
}
