package diag

import (
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"

	"github.com/voocel/agentcore"
	"github.com/voocel/ainovel-cli/internal/errclass"
)

// ── Error Category Constants ────────────────────────────────────────────────

// ErrCategory is a stable, short label classifying a runtime error.
// These labels are part of the diagnostic API contract — they will not change
// between minor versions, so monitoring and alert routing can key on them.
//
// The string values are defined in the shared errclass package; this type
// provides a local wrapper for the diag API surface.
type ErrCategory string

const (
	CatToolArgsMalformed    ErrCategory = errclass.CatToolArgsMalformed
	CatToolSchemaValidation ErrCategory = errclass.CatToolSchemaValidation
	CatToolSemanticVal      ErrCategory = errclass.CatToolSemanticVal
	CatToolNoop             ErrCategory = errclass.CatToolNoop
	CatStyleReviewExhausted ErrCategory = errclass.CatStyleReviewExhausted
	CatMaxTurns             ErrCategory = errclass.CatMaxTurns
	CatStreamIdle           ErrCategory = errclass.CatStreamIdle
)

// ── Desensitized Error Observation ──────────────────────────────────────────

// ErrObs is a single desensitized error observation. It carries only structured
// metadata — no body text, no raw arguments, no trailing fragments, no provider
// SSE. Every string field is either a stable label, a structural identifier
// (agent/tool name), a schema path, or a short fingerprint.
type ErrObs struct {
	Agent        string      `json:"agent"`                   // agent that produced the error (structural)
	Tool         string      `json:"tool,omitempty"`          // tool name, if error is tool-related
	Category     ErrCategory `json:"category"`                // stable error category
	Turn         int         `json:"turn,omitempty"`          // turn number (from ProgressPayload.Attempt or session)
	FinishReason string      `json:"finish_reason,omitempty"` // stop reason from Message (stop/length/error/…)
	// Schema validation details — only populated from agenTcore ValidationIssue.
	SchemaPath string `json:"schema_path,omitempty"`
	Expected   string `json:"expected,omitempty"`
	Received   string `json:"received,omitempty"`
	// Args fingerprint — byte length and short content hash, NOT the raw args.
	ArgsBytes int    `json:"args_bytes,omitempty"`
	ArgsHash  string `json:"args_hash,omitempty"` // sha256-prefix hash of raw args; same hash ≈ same args
	// Stream/tool-call metrics. Only populated when source data provides them.
	DeltaCount  int  `json:"delta_count,omitempty"`
	ToolUseDone bool `json:"tool_use_done,omitempty"`
	// Count is the number of times this exact error was observed (deduped by
	// agent+tool+category+argsHash). Populated during aggregation, not capture.
	Count int `json:"count,omitempty"`
}

// ── Classification ──────────────────────────────────────────────────────────

// ClassifyErrMsg maps a tool error message string to a stable ErrCategory.
// Returns "" when no known category matches.
//
// Delegates text pattern matching to errclass.ClassifyMsg so the single shared
// pattern table stays in sync with host/observer.errorKind.
func ClassifyErrMsg(msg string) ErrCategory {
	return ErrCategory(errclass.ClassifyMsg(msg))
}

// ── Session Error Extraction ────────────────────────────────────────────────

// extractSessionErrors walks session messages and produces a list of
// desensitized ErrObs entries. It looks for tool-result error messages and
// pairs them with the preceding assistant message's tool-call metadata.
//
// For parallel tool calls in a single assistant message, tool_name is bound
// from the tool result's own metadata (set by agentcore loop.go) rather than
// guessing from the nearest assistant message. When metadata is unavailable,
// Tool is set to "unknown" to avoid misattribution.
//
// Args fingerprint (ArgsBytes / ArgsHash) is resolved by tool_call_id from
// metadata, so parallel calls each get the correct args. When tool_call_id is
// unavailable and the assistant message contains multiple tool calls, args
// is left empty to avoid taking the first call's args.
//
// Sensitive content (body text, raw args, thinking) is never captured — only
// structural fields and error first-line classifiers pass through.
func extractSessionErrors(agent string, msgs []agentcore.Message) []ErrObs {
	var obs []ErrObs
	for i, m := range msgs {
		if m.Role != agentcore.RoleTool {
			continue
		}
		isErr, _ := m.Metadata["is_error"].(bool)
		if !isErr {
			continue
		}
		// Get first line of error text (already truncated by redactMessage).
		var errText string
		for _, b := range m.Content {
			if b.Type == agentcore.ContentText {
				errText = b.Text
				break
			}
		}
		if errText == "" {
			continue
		}
		cat := ClassifyErrMsg(errText)
		if cat == "" {
			continue // unclassified, skip (reduce noise)
		}
		o := ErrObs{
			Agent:    agent,
			Category: cat,
			// Prefer tool_name from tool result metadata (set by agentcore
			// loop.go). For parallel tool calls this is the only reliable
			// source. Fall back to "unknown" when metadata is absent.
			Tool: "unknown",
		}
		if tn, ok := m.Metadata["tool_name"].(string); ok && tn != "" {
			o.Tool = tn
		}
		// Read tool_call_id from metadata for precise args matching.
		toolCallID, _ := m.Metadata["tool_call_id"].(string)

		// Check if this is after a Message.StopReason error/aborted
		// (the Message carrying stop reason is not the tool result itself,
		// we look at the last assistant message in the list).
		if j := lastAssistantBefore(i, msgs); j >= 0 {
			o.Turn = approxTurn(msgs[:j+1])
			o.FinishReason = string(msgs[j].StopReason)
			// Precise args fingerprint: use tool_call_id when available;
			// otherwise only fall back when there is exactly one tool call
			// (no ambiguity).
			if toolCallID != "" {
				o.ArgsBytes, o.ArgsHash = argsFingerprintByID(msgs[j], toolCallID)
			} else {
				o.ArgsBytes, o.ArgsHash = argsFingerprintSingleton(msgs[j])
			}
			o.ToolUseDone = msgs[j].StopReason == agentcore.StopReasonStop ||
				msgs[j].StopReason == agentcore.StopReasonToolUse
		}
		obs = append(obs, o)
	}
	return obs
}

// lastAssistantBefore scans backwards from pos to find the most recent
// assistant message (which may carry tool call metadata). Returns -1 if none.
func lastAssistantBefore(pos int, msgs []agentcore.Message) int {
	for i := pos - 1; i >= 0; i-- {
		if msgs[i].Role == agentcore.RoleAssistant {
			return i
		}
	}
	return -1
}

// argsFingerprintByID returns the args fingerprint for the tool call with the
// given call ID in the assistant message. Returns empty when no matching call
// is found — this is correct for parallel calls when the ID doesn't match.
func argsFingerprintByID(m agentcore.Message, id string) (bytes int, hash string) {
	for _, b := range m.Content {
		if b.Type == agentcore.ContentToolCall && b.ToolCall != nil &&
			b.ToolCall.ID == id && len(b.ToolCall.Args) > 0 {
			raw := b.ToolCall.Args
			bytes = len(raw)
			h := sha256.Sum256(raw)
			hash = fmt.Sprintf("%016x", h[:8])
			return
		}
	}
	return 0, ""
}

// argsFingerprintSingleton returns the args fingerprint only when the assistant
// message contains exactly one tool call. For zero or multiple calls (parallel),
// returns empty to avoid misattribution — never takes the first call's args
// when the true source is ambiguous.
func argsFingerprintSingleton(m agentcore.Message) (bytes int, hash string) {
	var match *agentcore.ToolCall
	count := 0
	for _, b := range m.Content {
		if b.Type == agentcore.ContentToolCall && b.ToolCall != nil && len(b.ToolCall.Args) > 0 {
			count++
			if count == 1 {
				match = b.ToolCall
			}
		}
	}
	if count == 1 && match != nil {
		raw := match.Args
		bytes = len(raw)
		h := sha256.Sum256(raw)
		hash = fmt.Sprintf("%016x", h[:8])
	}
	return
}

// approxTurn approximates the turn number from the message sequence. In a
// well-formed session, user/tool messages and assistant messages alternate.
// This counts the number of assistant messages up to (and including) the given
// index as the turn estimate. Accurate for typical sessions; may be off by
// one in edge cases (steering injections).
func approxTurn(msgs []agentcore.Message) int {
	n := 0
	for _, m := range msgs {
		if m.Role == agentcore.RoleAssistant {
			n++
		}
	}
	return n
}

// ── Aggregation ─────────────────────────────────────────────────────────────

// aggErrObsKey returns a dedup key for ErrObs: agent+tool+category+argsHash.
// Two observations with the same fingerprint represent the same root cause.
func aggErrObsKey(o ErrObs) string {
	return fmt.Sprintf("%s\x00%s\x00%s\x00%s", o.Agent, o.Tool, o.Category, o.ArgsHash)
}

// aggregateErrObs deduplicates a slice of ErrObs by their fingerprint key,
// summing Count for duplicates. The returned slice is stable-sorted by count
// descending (most frequent first).
func aggregateErrObs(obs []ErrObs) []ErrObs {
	if len(obs) == 0 {
		return nil
	}
	agg := make(map[string]*ErrObs)
	for _, o := range obs {
		key := aggErrObsKey(o)
		if existing, ok := agg[key]; ok {
			existing.Count++
			continue
		}
		o.Count = 1
		agg[key] = &o
	}
	out := make([]ErrObs, 0, len(agg))
	for _, o := range agg {
		out = append(out, *o)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Count > out[j].Count
	})
	return out
}

// ── Sensitive Content Guard ─────────────────────────────────────────────────

// ErrObsContainsSensitive reports true if any ErrObs field carries content
// that should not appear in a desensitized export. Used by tests to verify
// the guard.
//
// Currently checks for known sensitive patterns (Chinese prose, obvious body
// text). This is a best-effort test helper, not a security boundary.
func ErrObsContainsSensitive(obs []ErrObs) bool {
	for _, o := range obs {
		for _, s := range []string{
			// Chinese prose markers (common novel content)
			"雪夜", "主角", "反派", "阴谋",
			// Raw args / provider markers
			"args_raw", "sse:", "provider:",
		} {
			if strings.Contains(string(o.Agent), s) ||
				strings.Contains(string(o.Tool), s) ||
				strings.Contains(string(o.Category), s) ||
				strings.Contains(o.SchemaPath, s) ||
				strings.Contains(o.Expected, s) ||
				strings.Contains(o.Received, s) ||
				strings.Contains(o.FinishReason, s) ||
				strings.Contains(o.ArgsHash, s) {
				return true
			}
		}
	}
	return false
}
