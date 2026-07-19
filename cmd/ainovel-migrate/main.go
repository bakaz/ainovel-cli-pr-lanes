// ainovel-migrate is intentionally separate from the normal application
// runtime. Draft/verify create review artifacts; apply is an explicit,
// hash-bound post-review transition with a recoverable receipt.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"

	"github.com/voocel/ainovel-cli/internal/migratev3"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "ainovel-migrate:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) < 2 || args[0] != "scene-beat-v3" {
		return fmt.Errorf("usage: ainovel-migrate scene-beat-v3 draft|verify|apply|verify-applied [flags]")
	}
	switch args[1] {
	case "draft":
		fs := flag.NewFlagSet("scene-beat-v3 draft", flag.ContinueOnError)
		fs.SetOutput(stderr)
		bookDir := fs.String("book-dir", "", "canonical book directory (required)")
		maxCost := fs.Float64("max-cost-usd", migratev3.MaxApprovedCost, "hard upper cost bound")
		maxAttempts := fs.Int("max-attempts", 2, "maximum attempts per chapter (1-3)")
		providerConfig := fs.String("provider-config", "", "explicit Provider config; rejected unless --allow-provider-calls is set")
		allowProvider := fs.Bool("allow-provider-calls", false, "enable the reviewed Stage 3b real Provider adapter")
		preflightOnly := fs.Bool("preflight-only", false, "validate source and worst-case cost without creating a run or calling Provider")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return fmt.Errorf("unexpected positional arguments: %v", fs.Args())
		}
		var generator migratev3.Generator = migratev3.FakeGenerator{}
		if *allowProvider {
			realGenerator, err := migratev3.NewOpenAIChatGenerator(*providerConfig)
			if err != nil {
				return err
			}
			generator = realGenerator
		}
		opts := migratev3.DraftOptions{
			BookDir: *bookDir, MaxCostUSD: *maxCost, MaxAttempts: *maxAttempts,
			Generator: generator, ProviderConfigPath: *providerConfig,
			AllowProviderCalls: *allowProvider,
		}
		if *preflightOnly {
			preflight, err := migratev3.Preflight(opts)
			if err != nil {
				return err
			}
			fmt.Fprintf(stdout, "preflight passed\nprovider/model: %s/%s\nlogical batches: %d\nmax attempts: %d\nworst-case cost: $%.6f\n", preflight.Provider, preflight.Model, preflight.LogicalBatches, preflight.MaxAttempts, preflight.WorstCaseCostUSD)
			return nil
		}
		result, err := migratev3.Draft(ctx, opts)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "review run: %s\nmanifest sha256: %s\nrecorded cost: $%.6f\n", result.RunDir, result.ManifestSHA256, result.CostUSD)
		return nil
	case "verify":
		fs := flag.NewFlagSet("scene-beat-v3 verify", flag.ContinueOnError)
		fs.SetOutput(stderr)
		bookDir := fs.String("book-dir", "", "canonical book directory (required)")
		runDir := fs.String("run-dir", "", "completed review run directory (required)")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return fmt.Errorf("unexpected positional arguments: %v", fs.Args())
		}
		result, err := migratev3.Verify(migratev3.VerifyOptions{BookDir: *bookDir, RunDir: *runDir})
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "verified complete review run\nmanifest sha256: %s\nlogical batches: %d\nscenes: %d\n", result.ManifestSHA256, result.LogicalBatches, result.TotalScenes)
		return nil
	case "apply":
		fs := flag.NewFlagSet("scene-beat-v3 apply", flag.ContinueOnError)
		fs.SetOutput(stderr)
		bookDir := fs.String("book-dir", "", "canonical book directory (required)")
		runDir := fs.String("run-dir", "", "verified review run directory (required)")
		approvedHash := fs.String("approved-manifest-sha256", "", "exact reviewed manifest SHA-256 (required)")
		keepThrough := fs.Int("keep-through", 0, "last approved chapter; this transition requires 34")
		confirm := fs.Bool("confirm-apply-reviewed-proposal", false, "confirm canonical apply after human review")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return fmt.Errorf("unexpected positional arguments: %v", fs.Args())
		}
		if !*confirm {
			return fmt.Errorf("apply requires --confirm-apply-reviewed-proposal")
		}
		result, err := migratev3.ApplyReviewed(migratev3.ApplyOptions{
			BookDir: *bookDir, RunDir: *runDir, ApprovedManifestSHA256: *approvedHash, KeepThrough: *keepThrough,
		})
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "applied reviewed proposal through chapter %d\nremoved planned chapters: %v\nkept scenes: %d\nreceipt: %s\nreceipt sha256: %s\n", result.KeptChapters, result.Removed, result.KeptScenes, result.ReceiptDir, result.ReceiptSHA256)
		return nil
	case "verify-applied":
		fs := flag.NewFlagSet("scene-beat-v3 verify-applied", flag.ContinueOnError)
		fs.SetOutput(stderr)
		bookDir := fs.String("book-dir", "", "canonical book directory (required)")
		receiptDir := fs.String("receipt-dir", "", "completed apply receipt directory (required)")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return fmt.Errorf("unexpected positional arguments: %v", fs.Args())
		}
		result, err := migratev3.VerifyApplied(*bookDir, *receiptDir)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "verified applied SceneBeat v3 profile\nreceipt sha256: %s\nchapters: %d\nscenes: %d\n", result.ReceiptSHA256, result.KeptChapters, result.KeptScenes)
		return nil
	default:
		return fmt.Errorf("unknown scene-beat-v3 command %q; expected draft, verify, apply, or verify-applied", args[1])
	}
}
