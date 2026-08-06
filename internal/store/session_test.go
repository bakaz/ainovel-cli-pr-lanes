package store

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/voocel/agentcore"
)

// TestSessionStore_MetaInjected_AssistantWithUsage 验证只有"assistant + has Usage"
// 的消息才被附加 _meta，这是 replay 路径精确算价的前提。
func TestSessionStore_MetaInjected_AssistantWithUsage(t *testing.T) {
	dir := t.TempDir()
	s := NewSessionStore(newIO(dir))
	lookup := ModelLookup(func(agentName string) (string, string) {
		return "meme", "gpt-5.4"
	})
	logger := s.SubAgentLogger(lookup)

	logger("writer", "写第 1 章", agentcore.Message{
		Role:  agentcore.RoleUser,
		Usage: nil,
	})
	logger("writer", "写第 1 章", agentcore.Message{
		Role: agentcore.RoleAssistant,
		Usage: &agentcore.Usage{
			Input: 1000, Output: 200, CacheRead: 800, TotalTokens: 1200,
		},
	})
	logger("writer", "写第 1 章", agentcore.Message{
		Role:  agentcore.RoleAssistant,
		Usage: nil, // assistant 但无 usage（流式未带 final usage chunk）
	})

	entries := readJSONL(t, filepath.Join(dir, "meta/sessions/agents/writer-ch01.jsonl"))
	if len(entries) != 3 {
		t.Fatalf("entries=%d want 3", len(entries))
	}
	if _, has := entries[0]["_meta"]; has {
		t.Errorf("user message should NOT have _meta")
	}
	if _, has := entries[2]["_meta"]; has {
		t.Errorf("assistant without Usage should NOT have _meta")
	}
	meta, ok := entries[1]["_meta"].(map[string]any)
	if !ok {
		t.Fatalf("assistant+Usage should have _meta map, got %T %v", entries[1]["_meta"], entries[1]["_meta"])
	}
	if meta["provider"] != "meme" || meta["model"] != "gpt-5.4" {
		t.Errorf("_meta = %v want provider=meme model=gpt-5.4", meta)
	}
}

// TestSessionStore_MetaModelSwitch 验证运行中切换模型后，后续消息的 _meta 也跟着变。
// 这是 B 方案对"同进程内 /model 切换"的精确支持。
func TestSessionStore_MetaModelSwitch(t *testing.T) {
	dir := t.TempDir()
	s := NewSessionStore(newIO(dir))

	current := "model-a"
	lookup := ModelLookup(func(agentName string) (string, string) {
		return "meme", current
	})
	logger := s.SubAgentLogger(lookup)

	logger("writer", "写第 1 章", makeAssistantWithUsage())
	current = "model-b" // 模拟 /model 切换
	logger("writer", "写第 1 章", makeAssistantWithUsage())

	entries := readJSONL(t, filepath.Join(dir, "meta/sessions/agents/writer-ch01.jsonl"))
	if len(entries) != 2 {
		t.Fatalf("entries=%d want 2", len(entries))
	}
	for i, want := range []string{"model-a", "model-b"} {
		meta, ok := entries[i]["_meta"].(map[string]any)
		if !ok {
			t.Fatalf("entry[%d] missing _meta", i)
		}
		if got := meta["model"]; got != want {
			t.Errorf("entry[%d] model = %v want %s", i, got, want)
		}
	}
}

// TestSessionStore_NilLookup 验证 lookup=nil 时写入仍然正常，
// 只是不带 _meta。
func TestSessionStore_NilLookup(t *testing.T) {
	dir := t.TempDir()
	s := NewSessionStore(newIO(dir))
	logger := s.SubAgentLogger(nil)
	logger("writer", "写第 1 章", makeAssistantWithUsage())

	entries := readJSONL(t, filepath.Join(dir, s.subAgentPath("writer", "写第 1 章")))
	if len(entries) != 1 {
		t.Fatalf("entries=%d want 1", len(entries))
	}
	if _, has := entries[0]["_meta"]; has {
		t.Errorf("nil lookup should not produce _meta")
	}
	// 但其他字段（role/usage）必须正常
	if entries[0]["role"] != "assistant" {
		t.Errorf("role lost: %v", entries[0]["role"])
	}
}

// TestSessionStore_MetaPrefersConfigKey 验证 _meta.provider 优先配置键
// （go0/go1/go2）而非 Usage.Provider 的协议名（"openai"），协议名挪到 protocol
// 字段——账号切换诊断的关键修复。
func TestSessionStore_MetaPrefersConfigKey(t *testing.T) {
	dir := t.TempDir()
	s := NewSessionStore(newIO(dir))
	lookup := ModelLookup(func(agentName string) (string, string) {
		return "go0", "gpt-4o"
	})
	logger := s.SubAgentLogger(lookup)

	logger("writer", "写第 1 章", agentcore.Message{
		Role: agentcore.RoleAssistant,
		Usage: &agentcore.Usage{
			Provider: "openai", Model: "gpt-4o",
			Input: 1000, Output: 200, CacheRead: 800, TotalTokens: 1200,
		},
	})

	entries := readJSONL(t, filepath.Join(dir, "meta/sessions/agents/writer-ch01.jsonl"))
	if len(entries) != 1 {
		t.Fatalf("entries=%d want 1", len(entries))
	}
	meta, ok := entries[0]["_meta"].(map[string]any)
	if !ok {
		t.Fatalf("assistant+Usage 应有 _meta, got %v", entries[0]["_meta"])
	}
	if meta["provider"] != "go0" {
		t.Errorf("_meta.provider = %v, want go0（配置键）", meta["provider"])
	}
	if meta["protocol"] != "openai" {
		t.Errorf("_meta.protocol = %v, want openai", meta["protocol"])
	}
	if meta["model"] != "gpt-4o" {
		t.Errorf("_meta.model = %v, want gpt-4o", meta["model"])
	}
}

// TestMergeSessionMetaPriority 验证 mergeSessionMeta 的合并优先级。
func TestMergeSessionMetaPriority(t *testing.T) {
	// lookup 配置键 + usage 协议名 → 配置键进 provider，协议名进 protocol
	m := mergeSessionMeta(&sessionLogMeta{Provider: "go0", Model: "m"},
		&sessionLogMeta{Provider: "openai", Model: "m"})
	if m == nil || m.Provider != "go0" || m.Protocol != "openai" || m.Model != "m" {
		t.Errorf("lookup+usage 合并异常: %+v", m)
	}

	// 无 lookup → usage 原样（旧行为）
	m = mergeSessionMeta(nil, &sessionLogMeta{Provider: "openai", Model: "m"})
	if m == nil || m.Provider != "openai" || m.Protocol != "" {
		t.Errorf("仅 usage 合并异常: %+v", m)
	}

	// 无 usage → lookup
	m = mergeSessionMeta(&sessionLogMeta{Provider: "go0", Model: "m"}, nil)
	if m == nil || m.Provider != "go0" || m.Protocol != "" {
		t.Errorf("仅 lookup 合并异常: %+v", m)
	}

	// 全空 → nil
	if got := mergeSessionMeta(nil, nil); got != nil {
		t.Errorf("全空应返回 nil, got %+v", got)
	}

	// 相同 provider（配置键恰好等于协议名）→ 不产生 protocol
	m = mergeSessionMeta(&sessionLogMeta{Provider: "openai", Model: "m"},
		&sessionLogMeta{Provider: "openai", Model: "m"})
	if m == nil || m.Provider != "openai" || m.Protocol != "" {
		t.Errorf("同名 provider 不应产生 protocol: %+v", m)
	}
}

func makeAssistantWithUsage() agentcore.Message {
	return agentcore.Message{
		Role:  agentcore.RoleAssistant,
		Usage: &agentcore.Usage{Input: 1000, Output: 200, TotalTokens: 1200},
	}
}

func readJSONL(t *testing.T, path string) []map[string]any {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	var out []map[string]any
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(line, &m); err != nil {
			t.Fatalf("unmarshal line: %v\n%s", err, string(line))
		}
		out = append(out, m)
	}
	return out
}
