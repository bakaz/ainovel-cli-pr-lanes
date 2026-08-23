package tui

import "strings"

// streamRound stores stream output as append-only chunks. Bubble Tea copies
// Model values on every update, so a strings.Builder cannot live here safely:
// its copy would retain mutable builder state. Chunks keep token updates O(1);
// the renderer joins them only when the viewport is actually refreshed.
type streamRound struct {
	chunks   []string
	bytes    int
	nonBlank bool
	render   string
	renderW  int
}

func (r *streamRound) append(text string) {
	if text == "" {
		return
	}
	r.chunks = append(r.chunks, text)
	r.bytes += len(text)
	r.render = ""
	r.renderW = 0
	if !r.nonBlank && strings.TrimSpace(text) != "" {
		r.nonBlank = true
	}
}

func (r streamRound) empty() bool { return !r.nonBlank }

func (r streamRound) text() string {
	if len(r.chunks) == 0 {
		return ""
	}
	var b strings.Builder
	b.Grow(r.bytes)
	for _, chunk := range r.chunks {
		b.WriteString(chunk)
	}
	return b.String()
}

type streamOp struct {
	clear bool
	text  string
}

// streamBatchMsg is one bounded, ordered group read from Host.Stream. A batch
// may contain delta/clear/delta; Update applies the operations in order, so a
// clear sentinel can never be merged across its boundary.
type streamBatchMsg struct {
	ops    []streamOp
	closed bool
}
