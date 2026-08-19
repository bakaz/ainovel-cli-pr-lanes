package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/voocel/ainovel-cli/assets"
	"github.com/voocel/ainovel-cli/internal/bootstrap"
	"github.com/voocel/ainovel-cli/internal/rules"
	"github.com/voocel/ainovel-cli/internal/store"
)

type customizationInspection struct {
	Schema          string                `json:"schema"`
	CWD             string                `json:"cwd"`
	OutputDir       string                `json:"output_dir"`
	Style           string                `json:"style"`
	Prompts         []inspectedResource   `json:"prompts"`
	SelectedStyle   inspectedResource     `json:"selected_style"`
	RuleFiles       []inspectedRuleFile   `json:"rule_files"`
	RuleSnapshot    inspectedRuleSnapshot `json:"rule_snapshot"`
	StyleRules      inspectedFile         `json:"style_rules"`
	StyleAnchors    inspectedStyleAnchors `json:"style_anchors"`
	OverlayWarnings []string              `json:"overlay_warnings,omitempty"`
	AntiRefusal     inspectedAntiRefusal  `json:"anti_refusal"`
}

type inspectedAntiRefusal struct {
	Path    string `json:"path"`
	Status  string `json:"status"`
	SHA256  string `json:"sha256,omitempty"`
	Warning string `json:"warning,omitempty"`
}

type inspectedResource struct {
	Key             string `json:"key"`
	Kind            string `json:"kind"`
	Path            string `json:"path"`
	SourceSHA256    string `json:"source_sha256"`
	EffectiveSHA256 string `json:"effective_sha256"`
}

type inspectedRuleFile struct {
	Label  string `json:"label"`
	Kind   string `json:"kind"`
	SHA256 string `json:"sha256"`
}

type inspectedRuleSnapshot struct {
	Path            string   `json:"path"`
	Exists          bool     `json:"exists"`
	Status          string   `json:"status,omitempty"`
	Sources         []string `json:"sources,omitempty"`
	CoversRuleFiles bool     `json:"covers_rule_files"`
	MissingSources  []string `json:"missing_sources,omitempty"`
	SHA256          string   `json:"sha256,omitempty"`
}

type inspectedFile struct {
	Path   string `json:"path"`
	Exists bool   `json:"exists"`
	SHA256 string `json:"sha256,omitempty"`
}

type inspectedStyleAnchors struct {
	Path     string   `json:"path"`
	Exists   bool     `json:"exists"`
	Valid    bool     `json:"valid"`
	Status   string   `json:"status"`
	Count    int      `json:"count"`
	SHA256   string   `json:"sha256,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
}

func inspectCustomizations(argv []string) int {
	var configPath, explicitRoot string
	for i := 0; i < len(argv); i++ {
		switch argv[i] {
		case "--config":
			if i+1 >= len(argv) {
				fmt.Fprintln(os.Stderr, "error: --config 缺少值")
				return 2
			}
			i++
			configPath = argv[i]
		case "--prompts-dir":
			if i+1 >= len(argv) {
				fmt.Fprintln(os.Stderr, "error: --prompts-dir 缺少值")
				return 2
			}
			i++
			explicitRoot = argv[i]
		default:
			fmt.Fprintf(os.Stderr, "error: inspect-customizations 不支持参数 %q\n", argv[i])
			return 2
		}
	}

	cfg, err := bootstrap.LoadConfig(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: config: %v\n", err)
		return 1
	}
	cfg.FillDefaults()
	bundle, overlay, err := assets.LoadProduction(cfg.Style, cfg.OutputDir, explicitRoot)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	cwd, _ := os.Getwd()
	outputDir, _ := filepath.Abs(cfg.OutputDir)
	report := customizationInspection{
		Schema: "ainovel-customizations/v1", CWD: cwd, OutputDir: outputDir, Style: cfg.Style,
		OverlayWarnings: overlay.Warnings,
	}

	keys := make([]string, 0, len(bundle.Sources))
	for key := range bundle.Sources {
		if strings.HasPrefix(filepath.ToSlash(key), "prompts/") {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		src := bundle.Sources[key]
		effective, _ := bundle.EffectiveResource(key)
		report.Prompts = append(report.Prompts, inspectedResource{
			Key: key, Kind: src.Kind, Path: src.Path, SourceSHA256: src.SHA256, EffectiveSHA256: textSHA256(effective),
		})
	}
	styleKey := "styles/" + cfg.Style + ".md"
	styleSource := bundle.Sources[styleKey]
	report.SelectedStyle = inspectedResource{
		Key: styleKey, Kind: styleSource.Kind, Path: styleSource.Path, SourceSHA256: styleSource.SHA256,
		EffectiveSHA256: textSHA256(bundle.Styles[cfg.Style]),
	}

	ruleOptions := rules.DefaultOptions()
	rawRules := rules.RawFileSources(ruleOptions)
	wantedSources := make(map[string]bool, len(rawRules))
	for _, raw := range rawRules {
		report.RuleFiles = append(report.RuleFiles, inspectedRuleFile{Label: raw.Label, Kind: raw.Kind.String(), SHA256: textSHA256(raw.Text)})
		wantedSources[raw.Label] = true
	}

	// 复核阻塞项 2 只读模式：inspect 只读，不取 workspace 排他锁。
	st := store.NewReadOnlyStore(outputDir)
	snapshotPath := filepath.Join(outputDir, "meta", "user_rules.json")
	report.RuleSnapshot = inspectedRuleSnapshot{Path: snapshotPath, CoversRuleFiles: len(wantedSources) == 0}
	if snap, loadErr := st.UserRules.Load(); loadErr == nil && snap != nil {
		report.RuleSnapshot.Exists = true
		report.RuleSnapshot.Status = string(snap.Status)
		report.RuleSnapshot.Sources = append([]string(nil), snap.Sources...)
		report.RuleSnapshot.SHA256, _ = fileSHA256(snapshotPath)
		present := make(map[string]bool, len(snap.Sources))
		for _, source := range snap.Sources {
			present[source] = true
		}
		for source := range wantedSources {
			if !present[source] {
				report.RuleSnapshot.MissingSources = append(report.RuleSnapshot.MissingSources, source)
			}
		}
		sort.Strings(report.RuleSnapshot.MissingSources)
		report.RuleSnapshot.CoversRuleFiles = len(report.RuleSnapshot.MissingSources) == 0
	}

	styleRulesPath := filepath.Join(outputDir, "meta", "style_rules.json")
	report.StyleRules = inspectFile(styleRulesPath)
	anchorsPath := filepath.Join(outputDir, "meta", "style_anchors.json")
	anchorResult := st.StyleAnchors.LoadManual()
	report.StyleAnchors = inspectedStyleAnchors{
		Path: anchorsPath, Exists: anchorResult.Status != store.StatusNotExist,
		Status: manualStatusName(anchorResult.Status), Warnings: anchorResult.Warnings,
	}
	if anchorResult.Anchors != nil {
		report.StyleAnchors.Count = len(anchorResult.Anchors.Anchors)
	}
	report.StyleAnchors.Valid = anchorResult.Status == store.StatusValid || anchorResult.Status == store.StatusEmptyValid || anchorResult.Status == store.StatusLegacyFormat
	report.StyleAnchors.SHA256, _ = fileSHA256(anchorsPath)

	anti := st.AntiRefusal.Load()
	report.AntiRefusal = inspectedAntiRefusal{
		Path: anti.Path, Status: string(anti.Status), SHA256: anti.SHA256, Warning: anti.Warning,
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(report); err != nil {
		fmt.Fprintf(os.Stderr, "error: encode report: %v\n", err)
		return 1
	}
	return 0
}

func manualStatusName(status store.ManualFileStatus) string {
	switch status {
	case store.StatusNotExist:
		return "not_exist"
	case store.StatusEmptyValid:
		return "empty_valid"
	case store.StatusValid:
		return "valid"
	case store.StatusCorrupted:
		return "corrupted"
	case store.StatusLegacyFormat:
		return "legacy_valid"
	default:
		return "unknown"
	}
}

func inspectFile(path string) inspectedFile {
	hash, err := fileSHA256(path)
	return inspectedFile{Path: path, Exists: err == nil, SHA256: hash}
}

func fileSHA256(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return textSHA256(string(data)), nil
}

func textSHA256(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
