// Package errclass provides shared error classification rules used by both
// host (observer.errorKind) and diag (ClassifyErrMsg / extractSessionErrors).
//
// This is a zero-dependency, pure pattern-matching helper. It does not import
// agentcore or any other internal package, so it is safe to import from both
// host and diag without creating circular dependencies.
//
// The message-pattern table is the single source of truth for text-based
// classification; call sites that also check error chains (e.g. host's
// errorKind via errors.Is) maintain their own priority logic on top.
package errclass

import "strings"

// ── Category Constants ──────────────────────────────────────────────────────

// These strings are the canonical category labels shared across observer and
// diag. They are part of the diagnostic API contract — they will not change
// between minor versions, so monitoring and alert routing can key on them.
const (
	CatToolArgsMalformed    = "tool_args_malformed"      // JSON parse failure in tool arguments
	CatToolSchemaValidation = "tool_schema_validation"   // Schema-level validation (missing/type-mismatch fields)
	CatToolSemanticVal      = "tool_semantic_validation" // Business-logic validation (write-before-read, etc.)
	CatToolNoop             = "tool_noop"                // Tool reported no changes needed
	CatStyleReviewExhausted = "style_review_exhausted"   // Style review iterations exhausted
	CatMaxTurns             = "max_turns"                // Agent loop reached max turn limit
	CatStreamIdle           = "stream_idle"              // Provider stream idle timeout
)

// ── Message Pattern Classification ──────────────────────────────────────────

// streamIdleMsgPattern is the canonical substring that identifies a provider
// stream idle timeout in a flattened error message. Matches agentcore's
// internal constant so the two stay in sync.
const streamIdleMsgPattern = "stream idle timeout"

// isStreamIdleMessage reports whether s matches the stream idle pattern.
// Replicates agentcore.IsStreamIdleMessage without importing the package.
func isStreamIdleMessage(s string) bool {
	return strings.Contains(strings.ToLower(s), streamIdleMsgPattern)
}

// ClassifyMsg classifies an error message string into a stable category label
// using only text pattern matching. Returns "" when no known pattern matches.
//
// This is the single source of truth for message-pattern-based classification.
// Both host/observer.errorKind and diag/ClassifyErrMsg delegate to it.
func ClassifyMsg(msg string) string {
	if msg == "" {
		return ""
	}
	// Provider-level patterns first (higher specificity).
	if isStreamIdleMessage(msg) {
		return CatStreamIdle
	}
	lower := strings.ToLower(msg)
	switch {
	case containsAny(lower, "max turns"):
		return CatMaxTurns
	case containsAny(lower, "inputvalidationerror", "the required parameter", "type is expected as"):
		return CatToolSchemaValidation
	case containsAny(lower, "argsparseerror", "cannot parse", "unexpected token", "unexpected end of json input"):
		return CatToolArgsMalformed
	case containsAny(lower, "invalid character") && containsAny(lower, "looking for"):
		return CatToolArgsMalformed
	case containsAny(lower, "received malformed json arguments"):
		return CatToolArgsMalformed
	case containsAny(lower, "received invalid json arguments"):
		return CatToolArgsMalformed
	case containsAny(lower, "semantic validation", "write before read", "no such chapter"):
		return CatToolSemanticVal
	case containsAny(lower, "noop", "no changes", "nothing to update", "nothing to do"):
		return CatToolNoop
	case containsAny(lower, "style review") && (containsAny(lower, "exhaust", "max", "limit")):
		return CatStyleReviewExhausted
	case containsAny(lower, "评审") && containsAny(lower, "耗尽"):
		return CatStyleReviewExhausted
	}
	return ""
}

// containsAny reports whether s contains any of the given substrings.
func containsAny(s string, substrs ...string) bool {
	for _, sub := range substrs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
