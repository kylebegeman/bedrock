package app

import (
	"context"
	"io"

	"github.com/kylebegeman/helios/internal/kernel"
	"github.com/kylebegeman/helios/internal/state"
)

// A checkpoint fence may appear after an operation was reviewed. Recheck
// before every move or ordinary restore step without blocking compensation
// cleanup or the explicit rollback that owns the checkpoint fence.
func guardReleaseRestoreSteps(plan *kernel.Plan, store *state.Store) *kernel.Plan {
	for i := range plan.Steps {
		apply := plan.Steps[i].Apply
		plan.Steps[i].Apply = func(ctx context.Context, out io.Writer) error {
			if err := checkReleaseStoreAccess(ctx, store, plan.Target); err != nil {
				return err
			}
			return apply(ctx, out)
		}
	}
	return plan
}
