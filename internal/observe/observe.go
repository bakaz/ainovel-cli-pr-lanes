package observe

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/voocel/agentcore"
	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/store"
)

type Options struct {
	Dir     string
	Timeout time.Duration
	Model   agentcore.ChatModel
}

type Result struct {
	Success bool
	Reason  string
}

var recoverablePhases = map[domain.Phase]bool{
	domain.PhaseInit: true, domain.PhasePremise: true,
	domain.PhaseOutline: true, domain.PhaseWriting: true,
}

// CanonicalDir validates the observe target without creating anything.
func CanonicalDir(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("observe --dir must be an absolute path")
	}
	abs := filepath.Clean(path)
	info, err := os.Stat(abs)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("observe target is not a directory: %s", abs)
	}
	return abs, nil
}

// Preflight is read-only. It deliberately uses NewStore only for reads; it
// never calls Store.Init or any clearing/initialization method.
func Preflight(dir string) error {
	s := store.NewStore(dir)
	p, err := s.Progress.Load()
	if err != nil {
		return fmt.Errorf("read progress: %w", err)
	}
	if p == nil || p.Phase == domain.PhaseComplete || !recoverablePhases[p.Phase] {
		return fmt.Errorf("observe refuses progress phase %q", phaseOf(p))
	}
	if p.Phase != domain.PhaseWriting {
		return fmt.Errorf("observe refuses non-writing phase %q", p.Phase)
	}
	if p.InProgressChapter > 0 || len(p.CompletedScenes) > 0 {
		return fmt.Errorf("observe refuses active chapter work")
	}
	if p.Flow != "" && p.Flow != domain.FlowWriting {
		return fmt.Errorf("observe refuses non-writing flow %q", p.Flow)
	}
	meta, err := s.RunMeta.Load()
	if err != nil {
		return fmt.Errorf("read run metadata: %w", err)
	}
	if meta != nil && strings.TrimSpace(meta.PendingSteer) != "" {
		return fmt.Errorf("observe refuses pending steer")
	}
	pending, err := s.Signals.LoadPendingCommit()
	if err != nil {
		return fmt.Errorf("read pending commit: %w", err)
	}
	if pending != nil {
		return fmt.Errorf("observe refuses pending commit")
	}
	rules, err := s.UserRules.Load()
	if err != nil {
		return fmt.Errorf("read rules snapshot: %w", err)
	}
	if rules == nil {
		return fmt.Errorf("observe requires meta/user_rules.json")
	}
	return nil
}

func phaseOf(p *domain.Progress) string {
	if p == nil {
		return "missing"
	}
	return string(p.Phase)
}

// Run performs one bounded, observe-only probe request. It creates no
// Store-owned runtime, observer, logger, session writer, or usage autosave.
func Run(ctx context.Context, opts Options) (Result, error) {
	if opts.Timeout <= 0 {
		return Result{}, fmt.Errorf("observe timeout must be positive")
	}
	dir, err := CanonicalDir(opts.Dir)
	if err != nil {
		return Result{}, err
	}
	if err := Preflight(dir); err != nil {
		return Result{}, err
	}
	if opts.Model == nil {
		return Result{}, fmt.Errorf("observe model is required")
	}
	return runOne(ctx, opts.Timeout, opts.Model)
}

type observeTool struct{}

func (observeTool) Name() string           { return "check_consistency" }
func (observeTool) Description() string    { return "observe-only consistency probe" }
func (observeTool) Schema() map[string]any { return map[string]any{"type": "object"} }
func (observeTool) Execute(context.Context, json.RawMessage) (json.RawMessage, error) {
	return nil, fmt.Errorf("observe tool execution is forbidden")
}

func executeObserveTool(ctx context.Context, policy Policy, tool agentcore.Tool, call agentcore.ToolCall) (json.RawMessage, error) {
	decision, err := policy.ToolGate()(ctx, agentcore.GateRequest{Tool: tool, Call: call})
	if err != nil {
		return nil, err
	}
	if decision != nil && !decision.Allowed {
		return nil, fmt.Errorf("%s", decision.Reason)
	}
	return tool.Execute(ctx, call.Args)
}

// guardedModel ensures provider streaming obeys cancellation. Tool calls are
// still passed to the agent core so the ToolGate is the genuine pre-execution
// authorization boundary.
type guardedModel struct {
	model        agentcore.ChatModel
	policy       Policy
	tools        []agentcore.Tool
	done         chan struct{}
	once         sync.Once
	scopeFailure atomic.Bool
}

const observeJoinGrace = 50 * time.Millisecond

func (m *guardedModel) markDone() { m.once.Do(func() { close(m.done) }) }

func (m *guardedModel) Generate(ctx context.Context, msgs []agentcore.Message, tools []agentcore.ToolSpec, opts ...agentcore.CallOption) (*agentcore.LLMResponse, error) {
	return m.model.Generate(ctx, msgs, tools, opts...)
}
func (m *guardedModel) GenerateStream(ctx context.Context, msgs []agentcore.Message, tools []agentcore.ToolSpec, opts ...agentcore.CallOption) (<-chan agentcore.StreamEvent, error) {
	in, err := m.model.GenerateStream(ctx, msgs, tools, opts...)
	if err != nil {
		m.markDone()
		return nil, err
	}
	out := make(chan agentcore.StreamEvent, 4)
	go func() {
		defer close(out)
		defer m.markDone()
		for {
			select {
			case <-ctx.Done():
				grace := time.NewTimer(observeJoinGrace)
				defer grace.Stop()
				for {
					select {
					case _, ok := <-in:
						if !ok {
							return
						}
					case <-grace.C:
						m.scopeFailure.Store(true)
						return
					}
				}
			case ev, ok := <-in:
				if !ok {
					return
				}
				if ev.Type == agentcore.StreamEventToolCallEnd && ev.CompletedToolCall != nil {
					var tool agentcore.Tool
					for _, candidate := range m.tools {
						if candidate.Name() == ev.CompletedToolCall.Name {
							tool = candidate
							break
						}
					}
					_, err := executeObserveTool(ctx, m.policy, tool, *ev.CompletedToolCall)
					if err == nil {
						err = fmt.Errorf("observe denies tool call")
					}
					out <- agentcore.StreamEvent{Type: agentcore.StreamEventError, Err: err}
					return
				}
				select {
				case out <- ev:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out, nil
}
func (m *guardedModel) SupportsTools() bool { return false }

func runOne(parent context.Context, timeout time.Duration, model agentcore.ChatModel) (Result, error) {
	return runOneWithTools(parent, timeout, model, []agentcore.Tool{observeTool{}})
}

func runOneWithTools(parent context.Context, timeout time.Duration, model agentcore.ChatModel, tools []agentcore.Tool) (Result, error) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	deadline := time.Now().Add(timeout)
	if parentDeadline, ok := parent.Deadline(); ok && parentDeadline.Before(deadline) {
		deadline = parentDeadline
	}
	guard := &guardedModel{model: model, policy: DefaultDenyPolicy(), tools: tools, done: make(chan struct{})}
	agent := agentcore.NewAgent(
		agentcore.WithModel(guard),
		agentcore.WithSystemPrompt("You are an observe-only liveness probe. Return a brief assistant response. Never use tools."),
		// Tools are handled by guardedModel at the observe boundary, never by
		// agentcore's normal executor.
		agentcore.WithMaxTurns(1),
		agentcore.WithMaxRetries(0),
		agentcore.WithToolGate(DefaultDenyPolicy().ToolGate()),
	)

	var mu sync.Mutex
	result := Result{}
	decided := false
	decide := func(success bool, reason string) bool {
		mu.Lock()
		defer mu.Unlock()
		if success && !time.Now().Before(deadline) {
			success = false
			reason = "timeout"
		}
		if !decided {
			decided = true
			result = Result{Success: success, Reason: reason}
			return true
		}
		return false
	}
	abort := func(success bool, reason string) {
		if decide(success, reason) {
			agent.Abort()
		}
	}
	agent.Subscribe(func(ev agentcore.Event) {
		switch ev.Type {
		case agentcore.EventMessageUpdate:
			if ev.Delta != "" && (ev.DeltaKind == agentcore.DeltaText || ev.DeltaKind == agentcore.DeltaThinking) {
				abort(true, "first assistant/thinking delta")
			}
		case agentcore.EventToolExecStart:
			// The gate has already denied this call. Let the executor publish its
			// terminal denied result; no underlying tool execution is possible.
			decide(false, "tool attempted")
		case agentcore.EventRetry:
			abort(false, "retry attempted")
		case agentcore.EventError:
			abort(false, "provider error")
		case agentcore.EventAgentEnd:
			mu.Lock()
			wasDecided := decided
			mu.Unlock()
			if !wasDecided {
				abort(false, "probe ended without assistant delta")
			}
		}
	})
	timeoutAbort := time.AfterFunc(timeout, func() { abort(false, "timeout") })
	defer timeoutAbort.Stop()
	if err := agent.Prompt(ctx, "Perform one liveness response now."); err != nil {
		return Result{Success: false, Reason: "prompt error"}, err
	}
	joined := make(chan struct{})
	go func() {
		agent.WaitForIdle()
		close(joined)
	}()
	select {
	case <-joined:
	case <-ctx.Done():
		abort(false, "timeout")
	}
	join := time.NewTimer(observeJoinGrace)
	defer join.Stop()
	select {
	case <-joined:
	case <-join.C:
		return Result{Success: false, Reason: "scope failure: agent join grace exceeded"}, nil
	}
	providerJoin := time.NewTimer(observeJoinGrace)
	defer providerJoin.Stop()
	select {
	case <-guard.done:
	case <-providerJoin.C:
		return Result{Success: false, Reason: "scope failure: provider join grace exceeded"}, nil
	}
	if guard.scopeFailure.Load() {
		return Result{Success: false, Reason: "scope failure: provider ignored cancellation"}, nil
	}
	if !isClosed(guard.done) {
		return Result{Success: false, Reason: "provider join timeout"}, nil
	}
	mu.Lock()
	deferred := result
	wasDecided := decided
	mu.Unlock()
	if !wasDecided {
		if ctx.Err() != nil {
			return Result{Success: false, Reason: "timeout"}, nil
		}
		return Result{Success: false, Reason: "probe ended without result"}, nil
	}
	return deferred, nil
}

func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}
