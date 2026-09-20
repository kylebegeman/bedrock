package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/kylebegeman/bedrock/internal/kernel"
)

// MaintainKind is the maintenance window: install updates, reboot if the
// machine asks for one, bring everything back and check it.
const MaintainKind = "host.maintain"

// Maintain is the Definition for MaintainKind.
type Maintain struct {
	Env    Env
	Socket string
}

// MaintainInput configures a maintenance run.
type MaintainInput struct {
	// Reboot allows a reboot when the machine needs one. Off, the run
	// installs updates and reports that a reboot is pending.
	Reboot bool `json:"reboot"`
}

// Kind implements kernel.Definition.
func (Maintain) Kind() string { return MaintainKind }

// Plan implements kernel.Definition.
func (m Maintain) Plan(ctx context.Context, raw json.RawMessage) (*kernel.Plan, error) {
	var in MaintainInput
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &in); err != nil {
			return nil, fmt.Errorf("maintain input: %w", err)
		}
	}
	env := m.Env
	if !env.Privileged {
		return nil, errors.New("maintenance needs root")
	}
	f := Gather(ctx, env, m.Socket)
	hostname, _ := env.Run(ctx, "hostname")
	plan := &kernel.Plan{Target: strings.TrimSpace(hostname), Recovery: kernel.Resume}
	plan.Steps = append(plan.Steps, kernel.Step{
		Name: "updates", Change: "install pending updates and remove packages nothing needs",
		Note: updatesNote(f),
		Apply: func(ctx context.Context, out io.Writer) error {
			if _, err := env.Run(ctx, "apt-get", "update"); err != nil {
				return err
			}
			result, err := env.Run(ctx, "apt-get", "-y", "-o", "Dpkg::Options::=--force-confdef", "-o", "Dpkg::Options::=--force-confold", "dist-upgrade")
			if err != nil {
				return err
			}
			fmt.Fprintln(out, summarizeApt(result))
			_, err = env.Run(ctx, "apt-get", "-y", "autoremove")
			return err
		},
	})
	if in.Reboot {
		plan.Steps = append(plan.Steps, kernel.Step{
			Name: "reboot", Change: "reboot if the machine asks for it",
			Note: doneIf(f.RebootRequired, "a reboot is pending now", "none pending before updates"),
			// After a reboot the marker is gone (it lives in /run), so the
			// resumed step sees nothing to do. That is how the step ends.
			Apply: func(ctx context.Context, out io.Writer) error {
				if !env.Exists("/var/run/reboot-required") {
					fmt.Fprintln(out, "no reboot needed")
					return nil
				}
				fmt.Fprintln(out, "rebooting; the operation resumes when the machine is back")
				return env.Reboot(ctx)
			},
		})
	} else {
		plan.Steps = append(plan.Steps, kernel.Step{
			Name: "reboot", Change: "report whether a reboot is pending (not allowed this run)",
			Apply: func(_ context.Context, out io.Writer) error {
				if env.Exists("/var/run/reboot-required") {
					fmt.Fprintln(out, "a reboot is pending; run maintain with --reboot")
				} else {
					fmt.Fprintln(out, "no reboot needed")
				}
				return nil
			},
		})
	}
	plan.Steps = append(plan.Steps, kernel.Step{
		Name: "verify", Change: "wait for Docker and the registry, then run the doctor",
		Apply: func(ctx context.Context, out io.Writer) error {
			if err := waitFor(ctx, 2*time.Minute, func() bool {
				active, _ := env.Run(ctx, "systemctl", "is-active", "docker")
				running, _ := env.Run(ctx, "docker", "inspect", "-f", "{{.State.Running}}", RegistryContainer)
				return strings.TrimSpace(active) == "active" && strings.TrimSpace(running) == "true"
			}); err != nil {
				return fmt.Errorf("docker and the registry didn't come back: %w", err)
			}
			results := Diagnose(Gather(ctx, env, m.Socket))
			var failed []string
			for _, r := range results {
				if r.Verdict == Fail && r.Name != "daemon" {
					failed = append(failed, r.Name+": "+r.Detail)
				}
			}
			if len(failed) > 0 {
				return errors.New("the doctor found: " + strings.Join(failed, "; "))
			}
			fmt.Fprintf(out, "doctor: %d checks, none failing\n", len(results))
			return nil
		},
	})
	return plan, nil
}

func updatesNote(f Facts) string {
	switch {
	case f.UpdatesPending > 0:
		return fmt.Sprintf("%d pending", f.UpdatesPending)
	case f.UpdatesPending == 0:
		return "none pending"
	}
	return ""
}

func summarizeApt(output string) string {
	for _, line := range strings.Split(output, "\n") {
		if strings.Contains(line, "upgraded,") && strings.Contains(line, "newly installed") {
			return strings.TrimSpace(line)
		}
	}
	return "updates applied"
}

// waitFor polls until ok returns true or the wait runs out.
func waitFor(ctx context.Context, limit time.Duration, ok func() bool) error {
	deadline := time.Now().Add(limit)
	for {
		if ok() {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("gave up after %s", limit)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}
