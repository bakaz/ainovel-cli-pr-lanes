package tools

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"

	"github.com/voocel/agentcore"
	corecontext "github.com/voocel/agentcore/context"
	"github.com/voocel/ainovel-cli/internal/store"
)

const toolResultPersistMinBytes = 512

// ResultViewGate 为工具结果维护磁盘全文，并按剩余上下文窗口决定
// 写入对话的是全文还是摘要。
type ResultViewGate struct {
	mu      sync.Mutex
	store   *store.Store
	mgr     agentcore.ContextManager
	window  int
	reserve int
}

func NewResultViewGate(st *store.Store) *ResultViewGate {
	return &ResultViewGate{store: st}
}

// Bind 在 ContextManager 建好后注入占用查询。
func (g *ResultViewGate) Bind(mgr agentcore.ContextManager, window, reserve int) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.mgr = mgr
	g.window = window
	g.reserve = reserve
}

// Middleware 包装 tool.Execute：先落盘全文，再按剩余窗口选择返回体。
func (g *ResultViewGate) Middleware() agentcore.ToolMiddleware {
	return func(ctx context.Context, call agentcore.ToolCall, next agentcore.ToolExecuteFunc) (json.RawMessage, error) {
		out, err := next(ctx, call.Args)
		if err != nil || len(out) == 0 {
			return out, err
		}
		return g.Select(call.Name, out), nil
	}
}

// Select 落盘全文；剩余窗口够则原样返回，否则返回摘要 JSON。
func (g *ResultViewGate) Select(toolName string, full json.RawMessage) json.RawMessage {
	if g == nil || len(full) == 0 {
		return full
	}
	need := corecontext.EstimateTokens(agentcore.UserMsg(string(full)))
	if need <= g.remaining() {
		return full
	}
	id := newToolResultID()
	if len(full) >= toolResultPersistMinBytes && g.store != nil && g.store.Sessions != nil {
		if err := g.store.Sessions.SaveToolResult(id, toolName, full); err != nil {
			slog.Warn("save tool result sidecar", "tool", toolName, "err", err)
		}
	}
	return summarizeToolResult(toolName, id, full)
}

func (g *ResultViewGate) remaining() int {
	g.mu.Lock()
	mgr, window, reserve := g.mgr, g.window, g.reserve
	g.mu.Unlock()
	if window <= 0 {
		return 1 << 30
	}
	used := 0
	if mgr != nil {
		if u := mgr.Usage(); u != nil {
			used = u.Tokens
			if u.ContextWindow > 0 {
				window = u.ContextWindow
			}
		}
	}
	left := window - used - reserve
	if left < 0 {
		return 0
	}
	return left
}

func summarizeToolResult(toolName, id string, full json.RawMessage) json.RawMessage {
	rec := map[string]any{
		"_view": "summary",
		"tool":  toolName,
		"id":    id,
		"bytes": len(full),
		"hint":  "完整结果已另存。需要细节请再次调用该工具。",
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(full, &obj) == nil {
		keys := make([]string, 0, 12)
		for k := range obj {
			if strings.HasPrefix(k, "_") {
				continue
			}
			keys = append(keys, k)
			if len(keys) >= 12 {
				break
			}
		}
		if len(keys) > 0 {
			rec["keys"] = keys
		}
		if raw, ok := obj["_loading_summary"]; ok {
			var s string
			if json.Unmarshal(raw, &s) == nil && s != "" {
				rec["_loading_summary"] = s
			}
		}
	}
	out, err := json.Marshal(rec)
	if err != nil {
		return json.RawMessage(`{"_view":"summary","hint":"完整结果已另存。需要细节请再次调用该工具。"}`)
	}
	return out
}

func newToolResultID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return hex.EncodeToString([]byte("fallback"))
	}
	return hex.EncodeToString(b[:])
}
