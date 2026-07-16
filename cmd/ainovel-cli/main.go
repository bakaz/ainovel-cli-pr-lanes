package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/voocel/agentcore"
	"github.com/voocel/ainovel-cli/assets"
	"github.com/voocel/ainovel-cli/internal/bootstrap"
	"github.com/voocel/ainovel-cli/internal/entry/headless"
	"github.com/voocel/ainovel-cli/internal/entry/tui"
	"github.com/voocel/ainovel-cli/internal/eval"
	"github.com/voocel/ainovel-cli/internal/observe"
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

	// observe 是只读探活子命令。
	if name := earlyCommand(os.Args); name == "observe" {
		os.Exit(observeCommand(os.Args[2:]))
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
	bundle := assets.Load(cfg.Style, assets.DefaultLoadOptions(cfg.OutputDir))
	if opts.Headless {
		prompt, err := loadPrompt(opts)
		if err != nil {
			die("error: %v", err)
		}
		if err := headless.Run(cfg, bundle, headless.Options{Prompt: prompt}); err != nil {
			die("error: %v", err)
		}
		return
	}
	if opts.Prompt != "" || opts.PromptFile != "" {
		die("error: --prompt/--prompt-file 仅能在 --headless 模式下使用")
	}
	if err := tui.Run(cfg, bundle, versionInfo().Version); err != nil {
		die("error: %v", err)
	}
}

type cliOptions struct {
	ConfigPath    string
	Headless      bool
	Prompt        string
	PromptFile    string
	Version       bool
	Update        bool
	UpdateVersion string
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
			opts.Prompt = argv[i+1]
			i++
		case "--prompt-file":
			if i+1 >= len(argv) {
				return opts, nil, fmt.Errorf("--prompt-file 缺少值")
			}
			opts.PromptFile = argv[i+1]
			i++
		default:
			args = append(args, argv[i])
		}
	}
	if opts.Prompt != "" && opts.PromptFile != "" {
		return opts, nil, fmt.Errorf("--prompt 和 --prompt-file 不能同时使用")
	}
	if opts.Version && (opts.Update || opts.ConfigPath != "" || opts.Headless || opts.Prompt != "" || opts.PromptFile != "" || len(args) > 0) {
		return opts, nil, fmt.Errorf("version 不能与其他启动参数混用")
	}
	if opts.Update && (opts.ConfigPath != "" || opts.Headless || opts.Prompt != "" || opts.PromptFile != "" || len(args) > 0) {
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

// ── observe command ─────────────────────────────────────────

// earlyCommand returns the subcommand name if the first non-program argument
// is a recognized early-production subcommand (observe), or "" otherwise.
func earlyCommand(argv []string) string {
	if len(argv) < 2 {
		return ""
	}
	switch argv[1] {
	case "observe":
		return "observe"
	}
	return ""
}

// observeConfigLoader is a test-observable variable for loading config.
// Production loads the real user config (not an empty stub).
var observeConfigLoader = func() (bootstrap.Config, error) {
	cfg, err := bootstrap.LoadConfig("")
	if err != nil {
		return bootstrap.Config{}, err
	}
	cfg.FillDefaults()
	if err := cfg.ValidateBase(); err != nil {
		return bootstrap.Config{}, err
	}
	return cfg, nil
}

// observeModelSetFactory is a test-observable variable for creating a ModelSet.
var observeModelSetFactory = func(cfg bootstrap.Config) (observeModelSet, error) {
	return bootstrap.NewModelSet(cfg)
}

// observeModelBuilder builds a default model from config (no coordinator role).
var observeModelBuilder = func(cfg bootstrap.Config) (agentcore.ChatModel, error) {
	ms, err := observeModelSetFactory(cfg)
	if err != nil {
		return nil, err
	}
	return ms.ForRole("default"), nil
}

// observeModelSet matches the ForRole method used by observe.
type observeModelSet interface {
	ForRole(role string) agentcore.ChatModel
}

// observeCommand implements the "observe" subcommand.
func observeCommand(argv []string) int {
	var dir string
	var timeout time.Duration
	dupDir := false
	dupTimeout := false

	for i := 0; i < len(argv); i++ {
		switch argv[i] {
		case "--dir":
			if dupDir {
				fmt.Fprintln(os.Stderr, "error: --dir 只能指定一次")
				return 1
			}
			dupDir = true
			if i+1 >= len(argv) {
				fmt.Fprintln(os.Stderr, "error: --dir 缺少值")
				return 1
			}
			i++
			dir = argv[i]
		case "--timeout":
			if dupTimeout {
				fmt.Fprintln(os.Stderr, "error: --timeout 只能指定一次")
				return 1
			}
			dupTimeout = true
			if i+1 >= len(argv) {
				fmt.Fprintln(os.Stderr, "error: --timeout 缺少值")
				return 1
			}
			i++
			d, err := time.ParseDuration(argv[i])
			if err != nil {
				fmt.Fprintf(os.Stderr, "error: --timeout 无效: %v\n", err)
				return 1
			}
			timeout = d
		default:
			if strings.HasPrefix(argv[i], "-") {
				fmt.Fprintf(os.Stderr, "error: 未知 flag %q\n", argv[i])
				return 1
			}
			fmt.Fprintf(os.Stderr, "error: 不支持参数 %q\n", argv[i])
			return 1
		}
	}

	if dir == "" {
		fmt.Fprintln(os.Stderr, "error: --dir 是必需的")
		return 1
	}
	if !filepath.IsAbs(dir) {
		fmt.Fprintln(os.Stderr, "error: --dir 必须是绝对路径")
		return 1
	}
	if timeout <= 0 {
		fmt.Fprintln(os.Stderr, "error: --timeout 必须是正数时长（如 30s）")
		return 1
	}

	cfg, err := observeConfigLoader()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: 加载配置失败: %v\n", err)
		return 1
	}
	model, err := observeModelBuilder(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: 构建模型失败: %v\n", err)
		return 1
	}
	result, err := observe.Run(context.Background(), observe.Options{
		Dir:     dir,
		Timeout: timeout,
		Model:   model,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	if !result.Success {
		fmt.Fprintf(os.Stderr, "observe 失败: %s\n", result.Reason)
		return 1
	}
	return 0
}

func loadPrompt(opts cliOptions) (string, error) {
	if opts.PromptFile == "" {
		return strings.TrimSpace(opts.Prompt), nil
	}

	var data []byte
	var err error
	if opts.PromptFile == "-" {
		data, err = os.ReadFile("/dev/stdin")
	} else {
		data, err = os.ReadFile(opts.PromptFile)
	}
	if err != nil {
		return "", fmt.Errorf("读取 prompt 失败: %w", err)
	}
	return strings.TrimSpace(string(data)), nil
}
