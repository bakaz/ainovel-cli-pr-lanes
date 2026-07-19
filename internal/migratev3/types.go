// Package migratev3 implements the standalone, review-only SceneBeat v3
// migration draft protocol. It deliberately does not depend on Host, Store,
// Engine, workers, flow, or the normal CLI runtime.
package migratev3

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/projectprofile"
)

const (
	ProtocolVersion = "scene-beat-v3-draft/v7"
	MaxApprovedCost = 20.0
)

func approvedSourcePaths() []string {
	paths := []string{
		"premise.md",
		"outline.json",
		"outline.md",
		"layered_outline.json",
		"layered_outline.md",
	}
	for i := 1; i <= 34; i++ {
		paths = append(paths, fmt.Sprintf("chapters/%02d.md", i))
	}
	return paths
}

type Pricing struct {
	Known               bool    `json:"-"`
	InputUSDPerMillion  float64 `json:"input_usd_per_million"`
	OutputUSDPerMillion float64 `json:"output_usd_per_million"`
	MaxOutputTokens     int     `json:"max_output_tokens"`
}

type GeneratorDescriptor struct {
	Provider     string  `json:"provider"`
	Model        string  `json:"model"`
	Pricing      Pricing `json:"pricing"`
	RealProvider bool    `json:"-"`
}

type Usage struct {
	Known        bool `json:"-"`
	InputTokens  int  `json:"input_tokens"`
	OutputTokens int  `json:"output_tokens"`
}

type GenerateResult struct {
	RawResponse      json.RawMessage
	ProviderResponse json.RawMessage
	Usage            Usage
	FinishReason     string
}

// PreparedRequest is the exact credential-free HTTP/body payload that a
// generator will consume. Its UTF-8 byte length is the conservative input
// token upper bound and is persisted so verify can independently recompute it.
type PreparedRequest struct {
	OutboundBody  string
	retryFeedback string
}

// Generator is intentionally narrow: one chapter enters, one raw JSON result
// exits. Protocol validation and all persistence remain owned by this package.
type Generator interface {
	Descriptor() GeneratorDescriptor
	Prepare(ChapterRequest) (PreparedRequest, error)
	Generate(context.Context, ChapterRequest, PreparedRequest, int) (GenerateResult, error)
}

// RetryPreparer lets a generator strengthen a retry request without weakening
// protocol validation. Draft reserves and persists the exact retry body before
// the Provider call, just as it does for the first attempt.
type RetryPreparer interface {
	PrepareRetry(ChapterRequest, int, string) (PreparedRequest, error)
}

type SceneRequest struct {
	ID              string            `json:"id"`
	Source          domain.SceneBeat  `json:"source"`
	LegacyAction    string            `json:"legacy_action,omitempty"`
	MissingFields   []string          `json:"missing_fields"`
	PreservedFields map[string]string `json:"preserved_fields"`
}

type ChapterRequest struct {
	Chapter int            `json:"chapter"`
	Title   string         `json:"title"`
	Scenes  []SceneRequest `json:"scenes"`
}

type proposalEnvelope struct {
	Items []ProposalItem `json:"items"`
}

type ProposalItem struct {
	ID     string            `json:"id"`
	Fields map[string]string `json:"fields"`
}

type FileHash struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type SourceManifest struct {
	Protocol    string                     `json:"protocol"`
	BookDir     string                     `json:"book_dir"`
	Fingerprint projectprofile.Fingerprint `json:"fingerprint"`
	Files       []FileHash                 `json:"files"`
}

type ProviderAttempt struct {
	Chapter             int     `json:"chapter"`
	Attempt             int     `json:"attempt"`
	Provider            string  `json:"provider"`
	Model               string  `json:"model"`
	UsageKnown          bool    `json:"usage_known"`
	InputTokens         int     `json:"input_tokens"`
	ReservedInputTokens int     `json:"reserved_input_tokens"`
	OutputTokens        int     `json:"output_tokens"`
	CostUSD             float64 `json:"cost_usd"`
	ReservedUSD         float64 `json:"reserved_usd"`
	Retry               bool    `json:"retry"`
	ErrorCategory       string  `json:"error_category,omitempty"`
	ErrorDetail         string  `json:"error_detail,omitempty"`
	FinishReason        string  `json:"finish_reason,omitempty"`
}

type AuditEntry struct {
	Chapter          int `json:"chapter"`
	SceneCount       int `json:"scene_count"`
	LegacyScenes     int `json:"legacy_scenes"`
	StructuredScenes int `json:"structured_scenes"`
	FilledFields     int `json:"filled_fields"`
	Attempts         int `json:"attempts"`
}

type ReviewDiffEntry struct {
	ID        string            `json:"id"`
	Preserved map[string]string `json:"preserved"`
	Added     map[string]string `json:"added"`
}

type ArtifactManifest struct {
	Protocol         string                     `json:"protocol"`
	Status           string                     `json:"status"`
	RunID            string                     `json:"run_id"`
	BookDir          string                     `json:"book_dir"`
	CreatedAt        time.Time                  `json:"created_at"`
	Fingerprint      projectprofile.Fingerprint `json:"fingerprint"`
	LogicalBatches   int                        `json:"logical_batches"`
	TotalScenes      int                        `json:"total_scenes"`
	LegacyScenes     int                        `json:"legacy_scenes"`
	StructuredScenes int                        `json:"structured_scenes"`
	Attempts         int                        `json:"attempts"`
	CostUSD          float64                    `json:"cost_usd"`
	MaxCostUSD       float64                    `json:"max_cost_usd"`
	Provider         string                     `json:"provider"`
	Model            string                     `json:"model"`
	Pricing          ManifestPricing            `json:"pricing"`
	ProviderCalls    bool                       `json:"provider_calls"`
	Artifacts        []FileHash                 `json:"artifacts"`
}

type ManifestPricing struct {
	InputUSDPerMillion  float64 `json:"input_usd_per_million"`
	OutputUSDPerMillion float64 `json:"output_usd_per_million"`
	MaxOutputTokens     int     `json:"max_output_tokens"`
}

type DraftOptions struct {
	BookDir            string
	MaxCostUSD         float64
	MaxAttempts        int
	Generator          Generator
	ProviderConfigPath string
	AllowProviderCalls bool
	ExpectedEnrolled   *projectprofile.Fingerprint // tests only; production leaves nil
	Now                func() time.Time
	RandomBytes        func([]byte) error
	AfterBatch         func(int) // tests only
}

type DraftResult struct {
	RunDir         string
	ManifestSHA256 string
	CostUSD        float64
}

type PreflightResult struct {
	Provider         string
	Model            string
	LogicalBatches   int
	MaxAttempts      int
	WorstCaseCostUSD float64
}

type VerifyOptions struct {
	BookDir          string
	RunDir           string
	ExpectedEnrolled *projectprofile.Fingerprint // tests only; production leaves nil
}

type VerifyResult struct {
	ManifestSHA256 string
	LogicalBatches int
	TotalScenes    int
}
