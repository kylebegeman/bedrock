package docker

import (
	"context"
	"fmt"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// RestartPolicy is the durable Docker policy restored after a migration abort.
type RestartPolicy struct {
	Name              string `json:"name"`
	MaximumRetryCount int    `json:"maximum_retry_count,omitempty"`
}

func (e *Engine) RestartPolicy(ctx context.Context, id string) (RestartPolicy, error) {
	res, err := e.cli.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		return RestartPolicy{}, err
	}
	if res.Container.HostConfig == nil {
		return RestartPolicy{}, nil
	}
	p := res.Container.HostConfig.RestartPolicy
	return RestartPolicy{Name: string(p.Name), MaximumRetryCount: p.MaximumRetryCount}, nil
}

// SetRestartPolicy persists before a writer is stopped, so a host reboot cannot
// restart it while the source is fenced for migration.
func (e *Engine) SetRestartPolicy(ctx context.Context, id string, policy RestartPolicy) error {
	_, err := e.cli.ContainerUpdate(ctx, id, client.ContainerUpdateOptions{RestartPolicy: &container.RestartPolicy{
		Name: container.RestartPolicyMode(policy.Name), MaximumRetryCount: policy.MaximumRetryCount,
	}})
	if err != nil {
		return fmt.Errorf("set %s restart policy: %w", id, err)
	}
	return nil
}
