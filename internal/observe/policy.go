package observe

import (
	"context"
	"fmt"

	"github.com/voocel/agentcore"
	"github.com/voocel/ainovel-cli/internal/flow"
)

// Policy is the explicit default-deny authorization surface for observe mode.
// It is intentionally independent from the normal flow Dispatcher so a future
// operation policy can be composed here without weakening observe's boundary.
type Policy struct{}

func DefaultDenyPolicy() Policy { return Policy{} }

func (Policy) Authorize(tool string) error {
	return fmt.Errorf("observe mode denies tool %q", tool)
}

func (Policy) DispatchAuthorization(*flow.Instruction) error {
	return fmt.Errorf("observe mode denies flow dispatch")
}

func (p Policy) ToolGate() agentcore.ToolGate {
	return func(_ context.Context, req agentcore.GateRequest) (*agentcore.GateDecision, error) {
		return &agentcore.GateDecision{Allowed: false, Reason: p.Authorize(req.Call.Name).Error()}, nil
	}
}
