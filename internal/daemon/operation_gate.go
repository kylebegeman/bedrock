package daemon

import (
	"context"
	"encoding/json"

	"github.com/kylebegeman/bedrock/internal/app"
	"github.com/kylebegeman/bedrock/internal/kernel"
)

// The operation lock serializes execution; a persistent recovery reservation
// retains authority across restarts and between private restore and cutover.
type recoveryGuard struct {
	stateFile  string
	definition kernel.Definition
}

func (g recoveryGuard) Kind() string { return g.definition.Kind() }
func (g recoveryGuard) Plan(ctx context.Context, raw json.RawMessage) (*kernel.Plan, error) {
	if err := app.CheckMachineRecoveryAccess(g.stateFile, g.Kind(), raw); err != nil {
		return nil, err
	}
	return g.definition.Plan(ctx, raw)
}
