package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/voocel/agentcore"
	"github.com/voocel/ainovel-cli/internal/store"
)

// fakeChat 是可编程的 ChatModel 桩：记录每次调用，按配置返回错误或成功。
// 并发测试用字段：
//   - block/blockN：前 blockN 次 Generate 调用在返回前阻塞（模拟慢请求）；
//   - onBlock：首次阻塞前 close（测试同步用，单 goroutine 触发）；
//   - failOn/failOnErr：第 failOn 次 Generate 调用（1-based）返回 failOnErr
//     （用于同一实例对不同调用表现不同，如 A 阻塞成功、B 失败）。
//     failOn 在阻塞判定**之前**检查：阻塞中的调用（A，n=1）不会因后续调用（B）
//     把共享计数推过 failOn 而在释放后误命中——A 释放后按成功返回，B 独立命中
//     failOn 失败，两者互不串扰（并发测试依赖此确定性）。
type fakeChat struct {
	name      string
	err       error
	streamErr error
	// streamDeltas：流式调用中先发多少条 text_delta 再发 streamErr（模拟已转发
	// 部分输出后报错）；为 0 时错误立即发生（现有语义）。
	streamDeltas int
	calls        *[]string

	block           chan struct{}
	blockN          int
	blockedSignaled bool
	onBlock         chan struct{}
	failOn          int
	failOnErr       error
	n               int
}

func (f *fakeChat) Generate(ctx context.Context, messages []agentcore.Message, tools []agentcore.ToolSpec, opts ...agentcore.CallOption) (*agentcore.LLMResponse, error) {
	if f.calls != nil {
		*f.calls = append(*f.calls, f.name)
	}
	f.n++
	// failOn 在阻塞前判定：第 failOn 次调用是确定性语义。若放在阻塞后判定，
	// 共享计数被后续并发调用（B）推进后，阻塞中的 A 释放时会误命中 failOn——
	// 并发测试的空真来源（A 本应成功却失败，`idx < affinityIdx` 丢弃分支永不触发）。
	if f.failOn > 0 && f.n == f.failOn {
		return nil, f.failOnErr
	}
	if f.block != nil && f.n <= f.blockN {
		if f.onBlock != nil && !f.blockedSignaled {
			f.blockedSignaled = true
			close(f.onBlock)
		}
		<-f.block
	}
	if f.err != nil {
		return nil, f.err
	}
	return &agentcore.LLMResponse{Message: agentcore.Message{Role: agentcore.RoleAssistant}}, nil
}

func (f *fakeChat) GenerateStream(ctx context.Context, messages []agentcore.Message, tools []agentcore.ToolSpec, opts ...agentcore.CallOption) (<-chan agentcore.StreamEvent, error) {
	if f.calls != nil {
		*f.calls = append(*f.calls, f.name)
	}
	ch := make(chan agentcore.StreamEvent, 1+f.streamDeltas)
	if f.streamErr != nil {
		for i := 0; i < f.streamDeltas; i++ {
			ch <- agentcore.StreamEvent{Type: agentcore.StreamEventTextDelta, Delta: "partial"}
		}
		ch <- agentcore.StreamEvent{Type: agentcore.StreamEventError, Err: f.streamErr}
		close(ch)
		return ch, nil
	}
	ch <- agentcore.StreamEvent{Type: agentcore.StreamEventDone, Message: agentcore.Message{Role: agentcore.RoleAssistant}}
	close(ch)
	return ch, nil
}

func (f *fakeChat) SupportsTools() bool { return false }

var rateLimitErr = errors.New("rate limit exceeded")

// newAffinityFailover 构造带 provider 亲和的 failoverModel：primary 为 go0，
// fallbacks 依次命名 go1/go2/...。calls 记录每次底层调用的模型名。
func newAffinityFailover(primary *SwappableModel, fbs ...*fakeChat) (*failoverModel, *[]string) {
	var calls []string
	wireCalls := func(f *fakeChat) {
		if f != nil {
			f.calls = &calls
		}
	}
	wireCalls(primary.SwappableModel.Current().(*fakeChat))
	targets := make([]modelTarget, 0, len(fbs))
	for i, fb := range fbs {
		wireCalls(fb)
		targets = append(targets, modelTarget{provider: "go" + string(rune('1'+i)), name: fb.name, model: fb})
	}
	m := &failoverModel{
		role:       "writer",
		primary:    primary,
		fallbacks:  targets,
		protocolOf: func(string) string { return "openai" },
	}
	return m, &calls
}

// TestFailoverAffinity_StaysOnFallbackAfterFailover 是核心行为：一次 run 内
// primary 失败 failover 到备用后，后续调用直接从备用开始，不回跳 primary。
func TestFailoverAffinity_StaysOnFallbackAfterFailover(t *testing.T) {
	primary := NewSwappableModel("go0", "model-a", &fakeChat{name: "go0", err: rateLimitErr})
	fb := &fakeChat{name: "go1"}
	m, calls := newAffinityFailover(primary, fb)

	resp, err := m.Generate(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("failover 后应成功: %v", err)
	}
	if resp == nil {
		t.Fatal("resp 不应为 nil")
	}
	if len(*calls) != 2 || (*calls)[0] != "go0" || (*calls)[1] != "go1" {
		t.Fatalf("首次调用应 go0→go1, calls=%v", *calls)
	}

	// 第二次调用：亲和生效，直接 go1，不再碰 primary
	if _, err := m.Generate(context.Background(), nil, nil); err != nil {
		t.Fatalf("第二次调用应成功: %v", err)
	}
	if len(*calls) != 3 || (*calls)[2] != "go1" {
		t.Fatalf("第二次调用应从 go1 开始（不回跳 primary）, calls=%v", *calls)
	}

	if got := m.Epoch(); got != 2 {
		t.Errorf("Epoch = %d, want 2", got)
	}
	rep := m.selectionReport()
	if rep.ConfigKey != "go1" || rep.Epoch != 2 || rep.Protocol != "openai" {
		t.Errorf("selectionReport = %+v, want configKey=go1 epoch=2 protocol=openai", rep)
	}
}

// TestFailoverAffinity_ExpiryResetsToPrimary 验证窗口过期（≈ 新 run）后重新从
// primary 选择。
func TestFailoverAffinity_ExpiryResetsToPrimary(t *testing.T) {
	primary := NewSwappableModel("go0", "model-a", &fakeChat{name: "go0", err: rateLimitErr})
	fb := &fakeChat{name: "go1"}
	m, calls := newAffinityFailover(primary, fb)

	if _, err := m.Generate(context.Background(), nil, nil); err != nil {
		t.Fatalf("首次调用应成功: %v", err)
	}

	// 手动把亲和窗口过期
	m.mu.Lock()
	m.affinityExp = time.Now().Add(-time.Second)
	m.mu.Unlock()

	if _, err := m.Generate(context.Background(), nil, nil); err != nil {
		t.Fatalf("过期后调用应成功: %v", err)
	}
	if len(*calls) != 4 || (*calls)[2] != "go0" || (*calls)[3] != "go1" {
		t.Fatalf("窗口过期应回到 primary 重试: calls=%v", *calls)
	}
}

// TestFailoverAffinity_PrimarySwapInvalidates 验证 /model 热切换（primary Swap）
// 使亲和失效，下个调用从新 primary 开始。
func TestFailoverAffinity_PrimarySwapInvalidates(t *testing.T) {
	primary := NewSwappableModel("go0", "model-a", &fakeChat{name: "go0", err: rateLimitErr})
	fb := &fakeChat{name: "go1"}
	m, calls := newAffinityFailover(primary, fb)

	if _, err := m.Generate(context.Background(), nil, nil); err != nil {
		t.Fatalf("首次调用应成功: %v", err)
	}

	// /model 热切换：primary 换成 go2（同样失败，验证从新 primary 重新走 failover）
	go2 := &fakeChat{name: "go2", err: rateLimitErr}
	go2.calls = calls
	primary.Swap("go2", "model-c", go2)

	if _, err := m.Generate(context.Background(), nil, nil); err != nil {
		t.Fatalf("切换后调用应成功: %v", err)
	}
	if len(*calls) != 4 || (*calls)[2] != "go2" || (*calls)[3] != "go1" {
		t.Fatalf("primary 切换后应从新 primary 开始: calls=%v", *calls)
	}
}

// TestFailoverAffinity_AllFailThenRetriesPrimary 验证目标链全部失败时清空亲和，
// 下个调用重新从 primary 开始（自愈，不会卡死在死备用上）。
func TestFailoverAffinity_AllFailThenRetriesPrimary(t *testing.T) {
	primary := NewSwappableModel("go0", "model-a", &fakeChat{name: "go0", err: rateLimitErr})
	fb := &fakeChat{name: "go1", err: rateLimitErr}
	m, calls := newAffinityFailover(primary, fb)

	if _, err := m.Generate(context.Background(), nil, nil); err == nil {
		t.Fatal("全部失败应返回错误")
	}
	if got := m.Epoch(); got != 1 {
		t.Errorf("亲和清空后 Epoch = %d, want 1", got)
	}
	if _, err := m.Generate(context.Background(), nil, nil); err == nil {
		t.Fatal("第二次全部失败应返回错误")
	}
	if len(*calls) != 4 {
		t.Fatalf("应 go0,go1,go0,go1 各两次尝试, calls=%v", *calls)
	}
}

// TestFailoverAffinity_AffinityTargetDiesResetsToPrimary 是 ora-1 必补测试 1：
// 亲和已落到 go1 后，go1 全链失败（无后续）会清空亲和，下个请求重新从 primary
// 评估——go0 恢复后能回来，不会在 10 分钟窗口内卡死在已死的 go1 上。
func TestFailoverAffinity_AffinityTargetDiesResetsToPrimary(t *testing.T) {
	go0 := &fakeChat{name: "go0", err: rateLimitErr}
	primary := NewSwappableModel("go0", "model-a", go0)
	fb := &fakeChat{name: "go1"}
	m, calls := newAffinityFailover(primary, fb)

	// 第一次：go0 失败 → go1 成功，亲和落到 go1
	if _, err := m.Generate(context.Background(), nil, nil); err != nil {
		t.Fatalf("首次调用应成功: %v", err)
	}
	if got := m.Epoch(); got != 2 {
		t.Fatalf("首次调用后 Epoch = %d, want 2", got)
	}

	// go1 死亡（rate limit），且无后续目标 → 全链失败应清空亲和
	fb.err = rateLimitErr
	if _, err := m.Generate(context.Background(), nil, nil); err == nil {
		t.Fatal("go1 失败且无后续应返回错误")
	}
	if got := m.Epoch(); got != 1 {
		t.Fatalf("全链失败后 Epoch = %d, want 1（亲和应清空）", got)
	}

	// go0 恢复 → 下个请求重新从 primary 评估，回到 go0
	go0.err = nil
	if _, err := m.Generate(context.Background(), nil, nil); err != nil {
		t.Fatalf("恢复后调用应成功: %v", err)
	}
	if len(*calls) != 4 || (*calls)[2] != "go1" || (*calls)[3] != "go0" {
		t.Fatalf("下个请求应从 primary 重新评估: calls=%v", *calls)
	}
	if got := m.Epoch(); got != 1 {
		t.Fatalf("恢复后 Epoch = %d, want 1", got)
	}
}

// TestFailoverAffinity_GenerateFullChain 是 ora-1 必补测试 2：非流式 Generate
// 遍历完整 fallback 链（与 GenerateStream 一致）——go0→go1→go2 全失败后 go3
// 成功，亲和落到 go3，下个调用直接从 go3 开始。
func TestFailoverAffinity_GenerateFullChain(t *testing.T) {
	primary := NewSwappableModel("go0", "model-a", &fakeChat{name: "go0", err: rateLimitErr})
	fb1 := &fakeChat{name: "go1", err: rateLimitErr}
	fb2 := &fakeChat{name: "go2", err: rateLimitErr}
	fb3 := &fakeChat{name: "go3"}
	m, calls := newAffinityFailover(primary, fb1, fb2, fb3)

	if _, err := m.Generate(context.Background(), nil, nil); err != nil {
		t.Fatalf("应遍历全链成功: %v", err)
	}
	if len(*calls) != 4 || (*calls)[0] != "go0" || (*calls)[1] != "go1" || (*calls)[2] != "go2" || (*calls)[3] != "go3" {
		t.Fatalf("非流式应遍历 go0→go1→go2→go3, calls=%v", *calls)
	}
	if got := m.Epoch(); got != 4 {
		t.Errorf("Epoch = %d, want 4", got)
	}
	if _, err := m.Generate(context.Background(), nil, nil); err != nil {
		t.Fatalf("第二次调用应成功: %v", err)
	}
	if len(*calls) != 5 || (*calls)[4] != "go3" {
		t.Fatalf("第二次应直接从 go3 开始, calls=%v", *calls)
	}
}

// TestFailoverAffinity_ConcurrentCompletionNoRegression 是 ora-1 必补测试 3：
// 并发完成不能让亲和倒退——A 在 go1 上慢执行，B 从 go1 失败续到 go2（亲和=go2）；
// A 稍后完成时不能把亲和写回 go1（单调推进，旧完成结果被丢弃）。
func TestFailoverAffinity_ConcurrentCompletionNoRegression(t *testing.T) {
	primary := NewSwappableModel("go0", "model-a", &fakeChat{name: "go0", err: rateLimitErr})
	// go1：A 的调用（第 1 次）阻塞后成功；B 的调用（第 2 次）返回 rateLimitErr
	go1 := &fakeChat{name: "go1", block: make(chan struct{}), blockN: 1, onBlock: make(chan struct{}), failOn: 2, failOnErr: rateLimitErr}
	go2 := &fakeChat{name: "go2"}
	m, calls := newAffinityFailover(primary, go1, go2)

	done := make(chan error, 1)
	go func() {
		_, err := m.Generate(context.Background(), nil, nil)
		done <- err
	}()

	// 等 A 阻塞在 go1（A 已尝试 go0 失败、go1 在途）
	select {
	case <-go1.onBlock:
	case <-time.After(5 * time.Second):
		t.Fatalf("A 未阻塞在 go1, calls=%v", *calls)
	}

	// B：go0 失败 → go1 失败（第 2 次调用）→ go2 成功，亲和=go2
	if _, err := m.Generate(context.Background(), nil, nil); err != nil {
		t.Fatalf("B 调用应成功: %v", err)
	}
	if got := m.Epoch(); got != 3 {
		t.Fatalf("B 完成后 Epoch = %d, want 3", got)
	}

	// 释放 A：A 在 go1 上成功完成，但亲和必须保持 go2（不回跳）
	close(go1.block)
	if err := <-done; err != nil {
		t.Fatalf("A 调用应成功: %v", err)
	}
	if got := m.Epoch(); got != 3 {
		t.Fatalf("A 完成后 Epoch = %d, want 3（旧完成不得回跳）", got)
	}
	rep := m.selectionReport()
	if rep.ConfigKey != "go2" || rep.Epoch != 3 {
		t.Errorf("selectionReport = %+v, want configKey=go2 epoch=3", rep)
	}
}

// TestFailoverAffinity_SwapDuringInflightNoRebuild 是 ora-1 必补测试 4：在途
// fallback 请求在 /model Swap 之后完成时，不得用旧 primary 语义重建亲和——下个
// 调用从新 primary 重新评估。
func TestFailoverAffinity_SwapDuringInflightNoRebuild(t *testing.T) {
	go0 := &fakeChat{name: "go0", err: rateLimitErr}
	primary := NewSwappableModel("go0", "model-a", go0)
	// go1：唯一一次调用阻塞（A 在途），释放后成功
	go1 := &fakeChat{name: "go1", block: make(chan struct{}), blockN: 1, onBlock: make(chan struct{})}
	m, calls := newAffinityFailover(primary, go1)

	done := make(chan error, 1)
	go func() {
		_, err := m.Generate(context.Background(), nil, nil)
		done <- err
	}()

	select {
	case <-go1.onBlock:
	case <-time.After(5 * time.Second):
		t.Fatalf("A 未阻塞在 go1, calls=%v", *calls)
	}

	// /model 热切换：primary go0 → go2（也失败，验证下个调用从新 primary 走链）
	go2 := &fakeChat{name: "go2", err: rateLimitErr}
	go2.calls = calls
	primary.Swap("go2", "model-c", go2)

	// 释放 A：A 在 go1 上成功，但亲和不得重建（primary 快照已变）
	close(go1.block)
	if err := <-done; err != nil {
		t.Fatalf("A 调用应成功: %v", err)
	}
	if got := m.Epoch(); got != 1 {
		t.Fatalf("Swap 后在途完成不应重建亲和, Epoch = %d, want 1", got)
	}

	// 下个请求：从新 primary go2 开始 → 失败 → go1 成功，亲和以新 primary 语义建立
	if _, err := m.Generate(context.Background(), nil, nil); err != nil {
		t.Fatalf("Swap 后调用应成功: %v", err)
	}
	if len(*calls) != 4 || (*calls)[2] != "go2" || (*calls)[3] != "go1" {
		t.Fatalf("Swap 后应从新 primary 重新走链: calls=%v", *calls)
	}
	if got := m.Epoch(); got != 2 {
		t.Fatalf("Swap 后 Epoch = %d, want 2", got)
	}
}

// TestFailoverAffinity_NonEligibleErrorNoFallback 验证非 failover 错误（如 auth）
// 不切换 provider，且不建立亲和。
func TestFailoverAffinity_NonEligibleErrorNoFallback(t *testing.T) {
	primary := NewSwappableModel("go0", "model-a", &fakeChat{name: "go0", err: errors.New("invalid api key")})
	fb := &fakeChat{name: "go1"}
	m, calls := newAffinityFailover(primary, fb)

	if _, err := m.Generate(context.Background(), nil, nil); err == nil {
		t.Fatal("应返回错误")
	}
	if len(*calls) != 1 || (*calls)[0] != "go0" {
		t.Fatalf("非 failover 错误不应切换 provider, calls=%v", *calls)
	}
}

// TestFailoverAffinity_NonEligibleErrorClearsAffinity 是 ora-1 中-3 必补测试：
// 亲和落到 go1 后，go1 返回非 failover 类错误（quota——不在 IsFailoverEligible
// 清单里）时，亲和必须立即清空，下个调用从 primary 重新评估——此前只在
// eligible 错误时清空，quota/auth 类错误会让亲和卡死在配额耗尽的备用上直到
// 10 分钟窗口过期（旧实现每次从 primary 评估可立即恢复）。
func TestFailoverAffinity_NonEligibleErrorClearsAffinity(t *testing.T) {
	go0 := &fakeChat{name: "go0", err: rateLimitErr}
	primary := NewSwappableModel("go0", "model-a", go0)
	fb := &fakeChat{name: "go1"}
	m, calls := newAffinityFailover(primary, fb)

	// 第一次：go0 限流失败 → go1 成功，亲和落到 go1（epoch 2）
	if _, err := m.Generate(context.Background(), nil, nil); err != nil {
		t.Fatalf("首次调用应成功: %v", err)
	}
	if got := m.Epoch(); got != 2 {
		t.Fatalf("首次调用后 Epoch = %d, want 2", got)
	}

	// go1 配额耗尽：quota 错误不触发 failover 前进（非 eligible），但亲和目标
	// 自身已证明不可用 → 亲和清空（与错误是否 eligible 无关）
	fb.err = agentcore.ErrProviderQuota
	if _, err := m.Generate(context.Background(), nil, nil); err == nil {
		t.Fatal("go1 配额错误应返回错误（非 eligible 不继续 failover）")
	}
	if got := m.Epoch(); got != 1 {
		t.Fatalf("非 eligible 错误后 Epoch = %d, want 1（亲和应清空）", got)
	}

	// 下个调用：从 primary 重新评估（go0 恢复成功），不再碰已死的 go1
	go0.err = nil
	if _, err := m.Generate(context.Background(), nil, nil); err != nil {
		t.Fatalf("恢复后调用应成功: %v", err)
	}
	if len(*calls) != 4 || (*calls)[2] != "go1" || (*calls)[3] != "go0" {
		t.Fatalf("亲和清空后应从 primary 重新评估: calls=%v", *calls)
	}
	if got := m.Epoch(); got != 1 {
		t.Fatalf("恢复后 Epoch = %d, want 1", got)
	}
}

// TestFailoverAffinity_StreamStaysOnFallback 验证流式路径同样受亲和约束。
func TestFailoverAffinity_StreamStaysOnFallback(t *testing.T) {
	primary := NewSwappableModel("go0", "model-a", &fakeChat{name: "go0", streamErr: rateLimitErr})
	fb := &fakeChat{name: "go1"}
	m, calls := newAffinityFailover(primary, fb)

	ch, err := m.GenerateStream(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("GenerateStream: %v", err)
	}
	for ev := range ch {
		if ev.Type == agentcore.StreamEventError {
			t.Fatalf("流式应成功: %v", ev.Err)
		}
	}
	if len(*calls) != 2 || (*calls)[0] != "go0" || (*calls)[1] != "go1" {
		t.Fatalf("流式应 go0→go1, calls=%v", *calls)
	}

	// 第二次：直接 go1
	ch, err = m.GenerateStream(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("第二次 GenerateStream: %v", err)
	}
	for ev := range ch {
		if ev.Type == agentcore.StreamEventError {
			t.Fatalf("第二次流式应成功: %v", ev.Err)
		}
	}
	if len(*calls) != 3 || (*calls)[2] != "go1" {
		t.Fatalf("第二次流式应从 go1 开始, calls=%v", *calls)
	}
}

// TestFailoverAffinity_StreamForwardedErrorClearsAffinity 是 ora-1 必补测试：
// 亲和=go1 后，go1 在流式调用中先转发部分输出（forwarded=true）再返回 failover
// 类错误（stream idle）——错误必须原样传播（已转发内容不得在同一次 stream 内
// 透明切 provider），但亲和必须被清空：下个调用从 primary 重新评估，避免外层
// AgentLoop retry 连续钉死在已失败的 go1 上（此前的回退：affinity 保留 go1，
// retry 再次从 go1 开始，stream idle 可连续命中 7 次）。
func TestFailoverAffinity_StreamForwardedErrorClearsAffinity(t *testing.T) {
	go0 := &fakeChat{name: "go0", err: rateLimitErr}
	primary := NewSwappableModel("go0", "model-a", go0)
	fb := &fakeChat{name: "go1", streamErr: rateLimitErr, streamDeltas: 1}
	m, calls := newAffinityFailover(primary, fb)

	// 第一次（非流式）：go0 限流失败 → go1 成功，亲和落到 go1（epoch 2）
	if _, err := m.Generate(context.Background(), nil, nil); err != nil {
		t.Fatalf("首次调用应成功: %v", err)
	}
	if got := m.Epoch(); got != 2 {
		t.Fatalf("首次调用后 Epoch = %d, want 2", got)
	}

	// 第二次（流式）：从亲和 go1 开始，go1 先发 1 条 delta 再报 eligible 错误
	ch, err := m.GenerateStream(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("GenerateStream: %v", err)
	}
	var gotDelta, gotErr bool
	for ev := range ch {
		switch ev.Type {
		case agentcore.StreamEventTextDelta:
			gotDelta = true
		case agentcore.StreamEventError:
			gotErr = ev.Err != nil
		}
	}
	if !gotDelta {
		t.Fatal("应先转发正文增量")
	}
	if !gotErr {
		t.Fatal("流式应返回错误")
	}
	// 已转发后不得在同一次 stream 内切 provider：流式调用只碰到 go1
	// （calls 含首次非流式调用的 go0→go1 failover 记录）
	if len(*calls) != 3 || (*calls)[2] != "go1" {
		t.Fatalf("已转发后不得切 provider, calls=%v", *calls)
	}
	// 但亲和必须清空：下个调用从 primary 重新评估
	if got := m.Epoch(); got != 1 {
		t.Fatalf("forwarded 错误后 Epoch = %d, want 1（亲和应清空）", got)
	}

	// 下个调用：从 primary 重新评估（go0 恢复成功），不再碰已死的 go1
	go0.err = nil
	if _, err := m.Generate(context.Background(), nil, nil); err != nil {
		t.Fatalf("恢复后调用应成功: %v", err)
	}
	if len(*calls) != 4 || (*calls)[3] != "go0" {
		t.Fatalf("亲和清空后应从 primary 重新评估: calls=%v", *calls)
	}
	if got := m.Epoch(); got != 1 {
		t.Fatalf("恢复后 Epoch = %d, want 1", got)
	}
}

// TestFailoverAffinity_StreamForwardedNonEligibleErrorClearsAffinity 是 ora-1
// 必补测试：已转发部分输出后 go1 返回非 failover 类错误（quota）——错误传播且
// 亲和同样清空（best-known 目标已证明不可用，与错误是否 eligible 无关，与
// nextOrStall 的非 eligible 清空语义一致）。
func TestFailoverAffinity_StreamForwardedNonEligibleErrorClearsAffinity(t *testing.T) {
	go0 := &fakeChat{name: "go0", err: rateLimitErr}
	primary := NewSwappableModel("go0", "model-a", go0)
	fb := &fakeChat{name: "go1", streamErr: agentcore.ErrProviderQuota, streamDeltas: 1}
	m, calls := newAffinityFailover(primary, fb)

	// 第一次（非流式）：go0 限流失败 → go1 成功，亲和落到 go1（epoch 2）
	if _, err := m.Generate(context.Background(), nil, nil); err != nil {
		t.Fatalf("首次调用应成功: %v", err)
	}
	if got := m.Epoch(); got != 2 {
		t.Fatalf("首次调用后 Epoch = %d, want 2", got)
	}

	// 第二次（流式）：从亲和 go1 开始，go1 先发 1 条 delta 再报非 eligible 错误
	ch, err := m.GenerateStream(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("GenerateStream: %v", err)
	}
	var gotDelta, gotErr bool
	for ev := range ch {
		switch ev.Type {
		case agentcore.StreamEventTextDelta:
			gotDelta = true
		case agentcore.StreamEventError:
			gotErr = ev.Err != nil
		}
	}
	if !gotDelta || !gotErr {
		t.Fatalf("应转发 delta 后报错, delta=%v err=%v", gotDelta, gotErr)
	}
	// 已转发后不得在同一次 stream 内切 provider：流式调用只碰到 go1
	// （calls 含首次非流式调用的 go0→go1 failover 记录）
	if len(*calls) != 3 || (*calls)[2] != "go1" {
		t.Fatalf("已转发后不得切 provider, calls=%v", *calls)
	}
	if got := m.Epoch(); got != 1 {
		t.Fatalf("forwarded 非 eligible 错误后 Epoch = %d, want 1（亲和应清空）", got)
	}
}

// TestFailoverDrivesSessionMeta 端到端回归（ora-1 必补测试 5）：go0 失败
// failover 到 go1 后，session _meta 的 provider 必须记 go1（配置键）而非
// go0——此前 modelLookup 用 CurrentSelection 只读 primary，把 fallback 请求
// 记成 go0，甚至产生不存在的 go0/备用模型组合。验证数据源（SelectionReport
// 读租约目标）与写入路径（store.SessionStore.SubAgentLogger）的完整链路。
func TestFailoverDrivesSessionMeta(t *testing.T) {
	cfg := Config{
		Provider:  "go0",
		ModelName: "model-a",
		Providers: map[string]ProviderConfig{
			"go0": {Type: "openai", APIKey: "sk-x"},
			"go1": {Type: "openai", APIKey: "sk-y"},
		},
		Roles: map[string]RoleConfig{
			"writer": {Provider: "go0", Model: "model-a",
				Fallbacks: []ModelRef{{Provider: "go1", Model: "model-b"}}},
		},
	}
	ms, err := NewModelSet(cfg)
	if err != nil {
		t.Fatalf("NewModelSet: %v", err)
	}
	fm := ms.ForRoleWithFailover("writer", nil).(*failoverModel)
	// 换成可编程桩：primary go0 限流失败，fallback go1 成功（真实 failover）。
	fm.primary = NewSwappableModel("go0", "model-a", &fakeChat{name: "go0", err: rateLimitErr})
	fm.fallbacks = []modelTarget{{provider: "go1", name: "model-b", model: &fakeChat{name: "go1"}}}
	if _, err := fm.Generate(context.Background(), nil, nil); err != nil {
		t.Fatalf("failover 后应成功: %v", err)
	}

	// 与 agents.BuildWorkers 的 modelLookup 同构：SelectionReport → (provider, model)。
	lookup := store.ModelLookup(func(agentName string) (string, string) {
		rep := ms.SelectionReport(agentName)
		return rep.ConfigKey, rep.Model
	})
	dir := t.TempDir()
	sess := store.NewStore(dir).Sessions
	logger := sess.SubAgentLogger(lookup)
	logger("writer", "写第 1 章", agentcore.Message{
		Role: agentcore.RoleAssistant,
		Usage: &agentcore.Usage{
			Provider: "openai", Model: "model-b",
			Input: 1000, Output: 200, CacheRead: 800, TotalTokens: 1200,
		},
	})

	data, err := os.ReadFile(filepath.Join(dir, "meta/sessions/agents/writer-ch01.jsonl"))
	if err != nil {
		t.Fatalf("读 session jsonl: %v", err)
	}
	var rec struct {
		Meta *struct {
			Provider string `json:"provider"`
			Model    string `json:"model"`
			Protocol string `json:"protocol"`
		} `json:"_meta"`
	}
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatalf("解析 session: %v", err)
	}
	if rec.Meta == nil {
		t.Fatal("assistant+Usage 消息应有 _meta")
	}
	if rec.Meta.Provider != "go1" {
		t.Errorf("_meta.provider = %q, want go1（failover 租约目标配置键，而非 primary go0）", rec.Meta.Provider)
	}
	if rec.Meta.Model != "model-b" {
		t.Errorf("_meta.model = %q, want model-b（实际响应模型）", rec.Meta.Model)
	}
	if rec.Meta.Protocol != "openai" {
		t.Errorf("_meta.protocol = %q, want openai（协议名保留）", rec.Meta.Protocol)
	}
}

// TestModelSet_SelectionReportAndBaseline 验证 SelectionReport 的配置键/协议/
// epoch 映射与前缀基线注册/读取。
func TestModelSet_SelectionReportAndBaseline(t *testing.T) {
	cfg := Config{
		Provider:  "go0",
		ModelName: "model-a",
		Providers: map[string]ProviderConfig{
			"go0": {Type: "openai", APIKey: "sk-x"},
			"go1": {Type: "openai", APIKey: "sk-y"},
			"go2": {Type: "anthropic", APIKey: "sk-z"},
		},
		Roles: map[string]RoleConfig{
			"writer": {Provider: "go0", Model: "model-a",
				Fallbacks: []ModelRef{{Provider: "go1", Model: "model-b"}, {Provider: "go2", Model: "model-c"}}},
		},
	}
	ms, err := NewModelSet(cfg)
	if err != nil {
		t.Fatalf("NewModelSet: %v", err)
	}

	// 无显式配置的角色 → 默认选择
	rep := ms.SelectionReport("editor")
	if rep.ConfigKey != "go0" || rep.Model != "model-a" || rep.Epoch != 1 || rep.Protocol != "openai" {
		t.Errorf("editor SelectionReport = %+v, want go0/model-a/1/openai", rep)
	}

	// failover 角色初始：primary，epoch 1
	if ms.ForRoleWithFailover("writer", nil) == nil {
		t.Fatal("ForRoleWithFailover 不应返回 nil")
	}
	rep = ms.SelectionReport("writer")
	if rep.ConfigKey != "go0" || rep.Epoch != 1 || rep.Protocol != "openai" {
		t.Errorf("writer 初始 SelectionReport = %+v, want go0/1/openai", rep)
	}

	// 亲和推到第二个备用（go2）：epoch 3，protocol 解析为 anthropic
	fm := ms.failoverModels["writer"]
	if fm == nil {
		t.Fatal("failover wrapper 未登记到 ModelSet")
	}
	fm.mu.Lock()
	fm.affinityIdx = 2
	fm.affinityExp = time.Now().Add(time.Minute)
	fm.affinityPrimProv, fm.affinityPrimName = "go0", "model-a"
	fm.mu.Unlock()
	rep = ms.SelectionReport("writer")
	if rep.ConfigKey != "go2" || rep.Model != "model-c" || rep.Epoch != 3 || rep.Protocol != "anthropic" {
		t.Errorf("亲和到 go2 时 SelectionReport = %+v, want go2/model-c/3/anthropic", rep)
	}

	// 前缀基线注册/读取
	ms.SetAgentPrefixBaseline("writer", PrefixBaseline{SystemHash: "s", SystemEstTokens: 10, ToolsHash: "t", ToolsEstTokens: 20})
	b := ms.AgentPrefixBaseline("writer")
	if b.SystemHash != "s" || b.SystemEstTokens != 10 || b.ToolsHash != "t" || b.ToolsEstTokens != 20 {
		t.Errorf("baseline = %+v", b)
	}
	if got := ms.AgentPrefixBaseline("nope"); got != (PrefixBaseline{}) {
		t.Errorf("未注册 agent 应返回零值, got %+v", got)
	}
}
